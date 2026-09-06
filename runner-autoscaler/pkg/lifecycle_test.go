package pkg

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/option"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type memoryStore struct {
	mu    sync.Mutex
	rows  map[string]lifecycleRecord
	fleet fleetState
}

func (m *memoryStore) Update(_ context.Context, key string, fn func(*lifecycleRecord, *fleetState) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, f := m.rows[key], m.fleet
	if err := fn(&r, &f); err != nil {
		return err
	}
	m.rows[key], m.fleet = r, f
	return nil
}
func (m *memoryStore) Page(_ context.Context, after string, n int) ([]storedRecord, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := []string{}
	for k := range m.rows {
		if k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	next := ""
	if len(keys) >= n {
		keys = keys[:n]
		next = keys[n-1]
	}
	rows := []storedRecord{}
	for _, k := range keys {
		rows = append(rows, storedRecord{k, m.rows[k]})
	}
	return rows, next, nil
}
func (m *memoryStore) Close() error { return nil }
func (m *memoryStore) get(key string) lifecycleRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[key]
}
func lifecycleTestScaler() (*Autoscaler, *memoryStore, Source, Job) {
	gin.SetMode(gin.TestMode)
	src := Source{Name: "acme", SourceType: TypeOrganization, Secret: "secret"}
	job := Job{Id: 10, RepositoryFullName: "acme/repo", Labels: []string{"spock"}, Status: "queued"}
	m := &memoryStore{rows: map[string]lifecycleRecord{}}
	s := NewAutoscaler(AutoscalerConfig{StateDatabase: "test", RouteWebhook: "/webhook", RouteCreateVm: "/create_vm", RouteDeleteVm: "/delete_vm", RouteRecreateVm: "/recreate_vm", SourceQueryParam: "src", RunnerPrefix: "runner", RunnerLabelGroups: [][]string{{"spock"}}, RegisteredSources: map[string]Source{src.Name: src}, CallbackBaseURL: "https://trusted.example", TaskTimeout: 30, MaxRunners: 2, MaxOnDemandRunners: 1, AllowOnDemand: true, Zones: []string{"z1", "z2"}, InstanceTemplate: "spot", FallbackInstanceTemplate: "standard", RunnerJobLogPattern: DefaultRunnerJobLogPattern})
	s.store = m
	s.jobStatusFn = func(context.Context, Job) (string, error) { return "queued", nil }
	s.instanceStateFn = func(context.Context, string) (bool, State, error) { return false, Unknown, nil }
	s.jitConfigFn = func(context.Context, string, string, int64, []string) (string, error) { return "jit", nil }
	s.tryInsertFn = func(context.Context, creationAttempt, string, []*computepb.Items) error { return nil }
	s.queueFn = func(context.Context, string, string, interface{}, time.Duration) error { return nil }
	return s, m, src, job
}
func TestConcurrentCreateLeaseAcrossWorkers(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, j, false))
	entered, unblock := make(chan struct{}), make(chan struct{})
	s.jitConfigFn = func(context.Context, string, string, int64, []string) (string, error) {
		close(entered)
		<-unblock
		return "only-config", nil
	}
	done := make(chan error, 1)
	go func() { done <- s.processJob(ctx, src, j) }()
	<-entered
	other, _, _, _ := lifecycleTestScaler()
	other.store = m
	require.ErrorIs(t, other.processJob(ctx, src, j), errLeaseBusy)
	close(unblock)
	require.NoError(t, <-done)
	require.Equal(t, 1, m.fleet.Runners)
	require.Equal(t, "only-config", m.get(jobKey(src.Name, j)).JIT)
}
func TestCancelledUnassignedAndReorderedWebhook(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	s.queueFn = func(context.Context, string, string, interface{}, time.Duration) error {
		return errors.New("queue unavailable")
	}
	for _, action := range []Action{QUEUED, COMPLETED, QUEUED} {
		body, _ := json.Marshal(Payload{Action: action, Job: j, Repository: Repository{FullName: j.RepositoryFullName}})
		req := httptest.NewRequest("POST", "/webhook?src=acme", bytes.NewReader(body))
		req.Header.Set(SHA_HEADER, SHA_PREFIX+CalcSigHex([]byte(src.Secret), body))
		req.Header.Set(EVENT_HEADER, WEBHOOK_JOB_EVENT)
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, req)
		require.Equal(t, 200, w.Code)
	}
	require.True(t, m.get(jobKey(src.Name, j)).Terminal)
	require.NoError(t, s.processJob(context.Background(), src, j))
	require.Zero(t, m.fleet.Runners)
}
func TestIndependentQueuedDemandAndFleetCap(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	// Presence of an idle runner for a third job cannot erase either queued job.
	for id := int64(1); id <= 3; id++ {
		j.Id = id
		require.NoError(t, s.observe(ctx, src, j, false))
		err := s.processJob(ctx, src, j)
		if id <= 2 {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, errFleetFull)
		}
	}
	require.Equal(t, 2, m.fleet.Runners)
}
func TestRecoveryRetainsOriginAfterDeletionAndGitHubFailure(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, j, false))
	require.NoError(t, s.processJob(ctx, src, j))
	old := m.get(jobKey(src.Name, j)).VMName
	s.instanceStateFn = func(context.Context, string) (bool, State, error) { return false, Unknown, nil } // VM already deleted after serving another job
	s.jobStatusFn = func(context.Context, Job) (string, error) { return "", errors.New("GitHub unavailable") }
	require.Error(t, s.processJob(ctx, src, j))
	require.Equal(t, j, m.get(jobKey(src.Name, j)).Job)
	require.Zero(t, m.fleet.Runners)
	// A different process recovers without reading deleted VM metadata.
	other, _, _, _ := lifecycleTestScaler()
	other.store = m
	require.NoError(t, other.processJob(ctx, src, j))
	require.NotEqual(t, old, m.get(jobKey(src.Name, j)).VMName)
	require.Equal(t, 1, m.fleet.Runners)
}
func TestAmbiguousInsertKeepsNameZoneAndCredentials(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, j, false))
	names := []string{}
	zones := []string{}
	requests := []string{}
	jitCalls := 0
	s.jitConfigFn = func(context.Context, string, string, int64, []string) (string, error) {
		jitCalls++
		return "same-jit", nil
	}
	s.tryInsertFn = func(_ context.Context, a creationAttempt, name string, _ []*computepb.Items) error {
		names = append(names, name)
		zones = append(zones, a.zone)
		requests = append(requests, insertRequestID(name, a))
		if len(names) == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}
	require.Error(t, s.processJob(ctx, src, j))
	require.NoError(t, s.processJob(ctx, src, j))
	require.Len(t, names, 2)
	require.Equal(t, names[0], names[1])
	require.Equal(t, zones[0], zones[1])
	require.Equal(t, requests[0], requests[1])
	require.Equal(t, 1, jitCalls)
	require.Equal(t, 1, m.fleet.Runners)
}
func TestStandardReservationLimit(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	s.tryInsertFn = func(_ context.Context, a creationAttempt, _ string, _ []*computepb.Items) error {
		if a.provisioningModel == "spot" {
			return fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED")
		}
		return nil
	}
	require.NoError(t, s.observe(ctx, src, j, false))
	require.NoError(t, s.processJob(ctx, src, j))
	j.Id++
	require.NoError(t, s.observe(ctx, src, j, false))
	require.ErrorIs(t, s.processJob(ctx, src, j), errFleetFull)
	require.Equal(t, 1, m.fleet.Standard)
}
func TestExpiredLeaseCannotChangeReplacement(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	key := jobKey(src.Name, j)
	require.NoError(t, s.observe(ctx, src, j, false))
	_, err := s.claim(ctx, key, "old")
	require.NoError(t, err)
	require.NoError(t, m.Update(ctx, key, func(r *lifecycleRecord, _ *fleetState) error { r.LeaseUntil = time.Now().Add(-time.Second); return nil }))
	_, err = s.claim(ctx, key, "new")
	require.NoError(t, err)
	require.ErrorIs(t, s.mutate(ctx, key, "old", func(r *lifecycleRecord, _ *fleetState) error { r.Terminal = true; return nil }), errLeaseBusy)
	require.False(t, m.get(key).Terminal)
}
func TestWebhookBodyBoundAndUnknownSourceNotRead(t *testing.T) {
	s, _, _, _ := lifecycleTestScaler()
	s.conf.MaxRequestBytes = 32
	req := httptest.NewRequest("POST", "/webhook?src=acme", strings.NewReader(strings.Repeat("x", 33)))
	req.Header.Set(SHA_HEADER, SHA_PREFIX+strings.Repeat("a", 64))
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	require.Equal(t, 413, w.Code)
	read := false
	req = httptest.NewRequest("POST", "/webhook?src=unknown", nil)
	req.Body = &readDetector{read: &read}
	req.Header.Set(SHA_HEADER, SHA_PREFIX+strings.Repeat("a", 64))
	w = httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	require.Equal(t, 401, w.Code)
	require.False(t, read)
}

type readDetector struct{ read *bool }

func (r *readDetector) Read([]byte) (int, error) {
	*r.read = true
	return 0, errors.New("must not read")
}
func (r *readDetector) Close() error { return nil }
func TestRecreateCapabilityCannotAuthorizeWorkers(t *testing.T) {
	s, _, src, j := lifecycleTestScaler()
	body, _ := json.Marshal(recreateCapability{Job: j, Runner: "runner-old", Purpose: "recreate", Expires: time.Now().Add(time.Hour).Unix()})
	for _, route := range []string{"/create_vm", "/delete_vm", "/sweep", "/reconcile", "/discover"} {
		req := httptest.NewRequest("POST", route+"?src=acme", bytes.NewReader(body))
		req.Header.Set(SHA_HEADER, SHA_PREFIX+CalcSigHex([]byte(src.Secret), append([]byte("recreate\n"), body...)))
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	}
}
func TestRecreateRejectsExpiredOrOldGeneration(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, j, false))
	require.NoError(t, s.processJob(ctx, src, j))
	name := m.get(jobKey(src.Name, j)).VMName
	for _, cap := range []recreateCapability{{Job: j, Runner: name, Purpose: "recreate", Expires: time.Now().Add(-time.Second).Unix()}, {Job: j, Runner: "runner-old", Purpose: "recreate", Expires: time.Now().Add(time.Hour).Unix()}} {
		body, _ := json.Marshal(cap)
		req := httptest.NewRequest("POST", "/recreate_vm?src=acme", bytes.NewReader(body))
		req.Header.Set(SHA_HEADER, SHA_PREFIX+CalcSigHex([]byte(src.Secret), append([]byte("recreate\n"), body...)))
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, req)
		require.Contains(t, []int{401, 409}, w.Code)
	}
}
func TestMixedCaseLabelsAndOverride(t *testing.T) {
	j := Job{Labels: []string{"SpOcK", "GCE-MACHINE-N4-STANDARD-2"}}
	ok, _ := j.HasAnyLabelGroup([][]string{{"spock"}})
	require.True(t, ok)
	require.Equal(t, "n4-standard-2", *j.GetMagicLabelValue(MagicLabelMachine))
}

func TestCompletionDeletionIntentSurvivesReorderedQueue(t *testing.T) {
	s, m, src, job := lifecycleTestScaler()
	completed := job
	completed.RunnerName = "runner-20-0123456789abcdef"
	require.NoError(t, s.observe(context.Background(), src, completed, true))
	require.NoError(t, s.observe(context.Background(), src, job, false))
	r := m.get(jobKey(src.Name, job))
	require.True(t, r.Terminal)
	require.Equal(t, completed.RunnerName, r.PendingDelete)
	require.Equal(t, completed.RunnerName, r.Job.RunnerName)
	require.True(t, r.ExpiresAt.IsZero())
}
func TestExpiredUnsubmittedJITGetsNewGeneration(t *testing.T) {
	s, m, src, job := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, job, false))
	require.NoError(t, m.Update(ctx, jobKey(src.Name, job), func(r *lifecycleRecord, f *fleetState) error {
		r.VMName = "runner-10-old"
		r.JIT = "expired"
		r.JITIssuedAt = time.Now().Add(-time.Hour)
		f.Runners = 1
		return nil
	}))
	require.NoError(t, s.processJob(ctx, src, job))
	r := m.get(jobKey(src.Name, job))
	require.NotEqual(t, "runner-10-old", r.VMName)
	require.Equal(t, "jit", r.JIT)
	require.Equal(t, 1, m.fleet.Runners)
}
func TestCancelledUnsubmittedReservationIsReleased(t *testing.T) {
	s, m, src, job := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, job, true))
	require.NoError(t, m.Update(ctx, jobKey(src.Name, job), func(r *lifecycleRecord, f *fleetState) error { r.VMName = "runner-10-old"; f.Runners = 1; return nil }))
	require.NoError(t, s.processJob(ctx, src, job))
	require.Empty(t, m.get(jobKey(src.Name, job)).VMName)
	require.Zero(t, m.fleet.Runners)
}
func TestAmbiguousRetryPreservesTemplateAndOriginalAttemptTime(t *testing.T) {
	s, m, src, job := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, job, false))
	s.tryInsertFn = func(context.Context, creationAttempt, string, []*computepb.Items) error {
		return context.DeadlineExceeded
	}
	require.Error(t, s.processJob(ctx, src, job))
	before := m.get(jobKey(src.Name, job))
	s.conf.InstanceTemplate = "new-deployment-template"
	s.tryInsertFn = func(_ context.Context, a creationAttempt, _ string, _ []*computepb.Items) error {
		require.Equal(t, before.Template, a.template)
		return context.DeadlineExceeded
	}
	require.Error(t, s.processJob(ctx, src, job))
	after := m.get(jobKey(src.Name, job))
	require.Equal(t, before.AttemptedAt, after.AttemptedAt)
	require.Equal(t, before.VMName, after.VMName)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestWorkerOIDCSignatureAudienceAndIdentity(t *testing.T) {
	s, _, _, _ := lifecycleTestScaler()
	s.conf.CallbackServiceAccount = "callback@example.iam.gserviceaccount.com"
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	b64 := base64.RawURLEncoding.EncodeToString
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":"test","alg":"RS256","use":"sig","n":"%s","e":"%s"}]}`, b64(key.N.Bytes()), b64(big.NewInt(int64(key.E)).Bytes()))
	s.tokenValidator, err = idtoken.NewValidator(context.Background(), option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(jwks))}, nil
	})}))
	require.NoError(t, err)
	for _, tc := range []struct {
		name, audience, email, issuer string
		verified, valid               bool
	}{
		{"valid", s.conf.CallbackBaseURL, s.conf.CallbackServiceAccount, "https://accounts.google.com", true, true},
		{"wrong audience", "https://other.example", s.conf.CallbackServiceAccount, "https://accounts.google.com", true, false},
		{"wrong identity", s.conf.CallbackBaseURL, "other@example.com", "https://accounts.google.com", true, false},
		{"unverified", s.conf.CallbackBaseURL, s.conf.CallbackServiceAccount, "https://accounts.google.com", false, false},
		{"wrong issuer", s.conf.CallbackBaseURL, s.conf.CallbackServiceAccount, "https://other.example", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, _ := json.Marshal(map[string]interface{}{"aud": tc.audience, "email": tc.email, "email_verified": tc.verified, "iss": tc.issuer, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()})
			unsigned := b64([]byte(`{"alg":"RS256","kid":"test"}`)) + "." + b64(claims)
			hash := sha256.Sum256([]byte(unsigned))
			signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
			require.NoError(t, err)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/reconcile", nil)
			c.Request.Header.Set("Authorization", "Bearer "+unsigned+"."+b64(signature))
			require.Equal(t, tc.valid, s.privateRequest(c))
			if !tc.valid {
				require.Equal(t, 401, w.Code)
			}
		})
	}
}

func TestStandardOnlyFleetRejectsImpossibleAdmission(t *testing.T) {
	s, _, _, _ := lifecycleTestScaler()
	c := s.conf
	c.CallbackServiceAccount = "callback@example.com"
	c.DeleteTaskQueue = "delete"
	c.MaintenanceTaskQueue = "maintenance"
	c.MachineTimeout = 3600
	c.MaxRequestBytes = 1 << 20
	for _, tc := range []struct {
		name     string
		fallback string
		allow    bool
		limit    int
		valid    bool
	}{
		{"standard disabled", "", false, 1, false},
		{"standard zero budget", "", true, 0, false},
		{"standard allowed", "", true, 1, true},
		{"spot without standard", "standard", false, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := c
			config.FallbackInstanceTemplate = tc.fallback
			config.AllowOnDemand = tc.allow
			config.MaxOnDemandRunners = tc.limit
			if tc.valid {
				require.NoError(t, config.Validate())
			} else {
				require.ErrorContains(t, config.Validate(), "STANDARD-only")
			}
		})
	}
}
