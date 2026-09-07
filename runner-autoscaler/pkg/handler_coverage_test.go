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
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

// authenticatedRequest builds a request carrying a signed OIDC bearer token
// that satisfies s.privateRequest: a fresh RSA key backs both the JWKS s.tokenValidator
// fetches and the token's signature, with claims matching s.conf.CallbackBaseURL /
// CallbackServiceAccount.
func authenticatedRequest(t *testing.T, s *Autoscaler, method, target string, body []byte) *http.Request {
	t.Helper()
	if s.conf.CallbackServiceAccount == "" {
		s.conf.CallbackServiceAccount = "callback@example.iam.gserviceaccount.com"
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	b64 := base64.RawURLEncoding.EncodeToString
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":"test","alg":"RS256","use":"sig","n":"%s","e":"%s"}]}`, b64(key.N.Bytes()), b64(big.NewInt(int64(key.E)).Bytes()))
	validator, err := idtoken.NewValidator(context.Background(), option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(jwks))}, nil
	})}))
	require.NoError(t, err)
	s.tokenValidator = validator
	claims, _ := json.Marshal(map[string]interface{}{
		"aud": s.conf.CallbackBaseURL, "email": s.conf.CallbackServiceAccount, "email_verified": true,
		"iss": "https://accounts.google.com", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	})
	unsigned := b64([]byte(`{"alg":"RS256","kid":"test"}`)) + "." + b64(claims)
	hash := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	require.NoError(t, err)
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+unsigned+"."+b64(signature))
	return req
}

func TestReconcileHandlerSkipsSettledLeasedAndUnknownSourceRows(t *testing.T) {
	s, m, src, _ := lifecycleTestScaler()
	now := time.Now()

	seed := func(id int64, r lifecycleRecord) {
		job := Job{Id: id, RepositoryFullName: "acme/repo", Labels: []string{"spock"}, Status: "queued"}
		r.Job = job
		r.SchemaVersion = stateVersion
		prepareRecord(&r)
		m.rows[jobKey(r.Source, job)] = r
	}
	// Settled terminal tombstone: NeedsReconcile computes false, so Page never
	// surfaces it to the handler at all.
	seed(9001, lifecycleRecord{Source: src.Name, Terminal: true})
	// A live lease reaches the handler but is skipped without enqueueing.
	seed(9002, lifecycleRecord{Source: src.Name, Lease: "held", LeaseUntil: now.Add(time.Hour), NextActionAt: now.Add(-20 * time.Second)})
	// An unregistered source reaches the handler but is skipped and logged.
	seed(9003, lifecycleRecord{Source: "removed-org", NextActionAt: now.Add(-15 * time.Second)})
	const normalCount = 55
	for i := int64(0); i < normalCount; i++ {
		seed(2000+i, lifecycleRecord{Source: src.Name, NextActionAt: now.Add(-10 * time.Second)})
	}

	var created []Job
	var continued []pageRequest
	s.queueFn = func(_ context.Context, route, _ string, p interface{}, delay time.Duration) error {
		switch route {
		case s.conf.RouteCreateVm:
			require.Zero(t, delay)
			created = append(created, p.(Job))
		case "/reconcile":
			continued = append(continued, p.(pageRequest))
		default:
			t.Fatalf("unexpected queued route %s", route)
		}
		return nil
	}

	unauth := httptest.NewRequest("POST", "/reconcile", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, unauth)
	require.Equal(t, 401, w.Code)

	req := authenticatedRequest(t, s, "POST", "/reconcile", []byte("{}"))
	w = httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)

	// The page limit is 50; 3 skip rows + 55 normal rows exceed it, so only part
	// of the normal rows is enqueued on this page and one continuation is queued.
	require.Len(t, created, 48)
	for _, job := range created {
		require.NotEqual(t, int64(9002), job.Id)
		require.NotEqual(t, int64(9003), job.Id)
	}
	require.Len(t, continued, 1)
	require.NotEmpty(t, continued[0].After)
}

func spotInstance(name string) *computepb.Instance {
	return &computepb.Instance{Name: proto.String(name), Scheduling: &computepb.Scheduling{ProvisioningModel: proto.String("SPOT")}}
}

func TestAuditFleetMatchesDiscrepancyAndConcurrentCapacityWrite(t *testing.T) {
	t.Run("ledger matches inventory", func(t *testing.T) {
		s, m, _, _ := lifecycleTestScaler()
		m.runners = map[string]runnerRecord{"runner-1-0123456789abcdef": {SchemaVersion: stateVersion, Name: "runner-1-0123456789abcdef", Record: lifecycleRecord{Model: "spot"}}}
		m.fleet = fleetState{Runners: 1, Standard: 0}
		s.listInstancesFn = func(_ context.Context, zone string) ([]*computepb.Instance, error) {
			if zone != "z1" {
				return nil, nil
			}
			return []*computepb.Instance{spotInstance("runner-1-0123456789abcdef")}, nil
		}
		hook := logtest.NewGlobal()
		defer hook.Reset()
		req := authenticatedRequest(t, s, "POST", "/audit", nil)
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, req)
		require.Equal(t, 200, w.Code)
		require.Empty(t, messagesWithPrefix(hook, "Lifecycle invariant failed"))
		require.Len(t, messagesWithPrefix(hook, "Fleet invariant audit passed"), 1)
	})

	t.Run("a genuine discrepancy is logged, not rewritten", func(t *testing.T) {
		s, m, _, _ := lifecycleTestScaler()
		m.runners = map[string]runnerRecord{"runner-1-0123456789abcdef": {SchemaVersion: stateVersion, Name: "runner-1-0123456789abcdef", Record: lifecycleRecord{Model: "spot"}}}
		m.fleet = fleetState{Runners: 1, Standard: 0}
		s.listInstancesFn = func(_ context.Context, zone string) ([]*computepb.Instance, error) {
			if zone != "z1" {
				return nil, nil
			}
			// A second VM exists in GCE with no matching durable reservation.
			return []*computepb.Instance{spotInstance("runner-1-0123456789abcdef"), spotInstance("runner-9-0123456789abcdef")}, nil
		}
		hook := logtest.NewGlobal()
		defer hook.Reset()
		req := authenticatedRequest(t, s, "POST", "/audit", nil)
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, req)
		require.Equal(t, 200, w.Code)
		before := fleetState{Runners: m.fleet.Runners, Standard: m.fleet.Standard}
		lines := messagesWithPrefix(hook, "Lifecycle invariant failed")
		require.Len(t, lines, 1)
		require.Contains(t, lines[0], "no durable reservation")
		// Counters are never rewritten from the GCE inventory.
		require.Equal(t, before, fleetState{Runners: m.fleet.Runners, Standard: m.fleet.Standard})
	})

	t.Run("a capacity write mid-inventory defers rather than reports drift", func(t *testing.T) {
		s, m, _, _ := lifecycleTestScaler()
		m.runners = map[string]runnerRecord{"runner-1-0123456789abcdef": {SchemaVersion: stateVersion, Name: "runner-1-0123456789abcdef", Record: lifecycleRecord{Model: "spot"}}}
		m.fleet = fleetState{Runners: 1, Standard: 0}
		bumped := false
		s.listInstancesFn = func(_ context.Context, zone string) ([]*computepb.Instance, error) {
			if zone == "z1" && !bumped {
				bumped = true
				m.mu.Lock()
				m.fleet.Revision++
				m.mu.Unlock()
			}
			if zone != "z1" {
				return nil, nil
			}
			// This VM would be reported as drift if the audit did not defer.
			return []*computepb.Instance{spotInstance("runner-9-0123456789abcdef")}, nil
		}
		hook := logtest.NewGlobal()
		defer hook.Reset()
		req := authenticatedRequest(t, s, "POST", "/audit", nil)
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, req)
		require.Equal(t, 200, w.Code)
		require.Len(t, messagesWithPrefix(hook, "Fleet audit deferred"), 1)
		require.Empty(t, messagesWithPrefix(hook, "Lifecycle invariant failed"))
	})
}

// A valid capability re-dispatches the job after recreateVmDelay even though
// the record's ordinary due time is further out.
func TestDurableRecreateAcceptsCapabilityMatchingCurrentRunner(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, j, false))
	require.NoError(t, s.processJob(ctx, src, j))
	s.instanceStateFn = func(context.Context, string) (bool, State, error) { return true, RUNNING, nil }
	require.NoError(t, s.processJob(ctx, src, j))
	key := jobKey(src.Name, j)
	name := m.get(key).VMName
	require.NotEmpty(t, name)
	require.True(t, m.get(key).NextActionAt.After(time.Now().Add(recreateVmDelay)))
	var routes []string
	var delays []time.Duration
	s.queueFn = func(_ context.Context, route, _ string, _ interface{}, delay time.Duration) error {
		routes = append(routes, route)
		delays = append(delays, delay)
		return nil
	}

	cap := recreateCapability{Job: j, Runner: name, Purpose: "recreate", Expires: time.Now().Add(time.Hour).Unix()}
	body, _ := json.Marshal(cap)
	req := httptest.NewRequest("POST", "/recreate_vm?src=acme", bytes.NewReader(body))
	req.Header.Set(SHA_HEADER, SHA_PREFIX+CalcSigHex([]byte(src.Secret), append([]byte("recreate\n"), body...)))
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)

	require.Equal(t, 200, w.Code)
	require.Equal(t, []string{s.conf.RouteCreateVm}, routes)
	require.Equal(t, []time.Duration{recreateVmDelay}, delays)
	require.Equal(t, m.get(key).EnqueuedUntil, m.get(key).NextActionAt)
}

func TestAutoscalerConfigValidateRejectsEachInvariant(t *testing.T) {
	baseline := func() AutoscalerConfig {
		return AutoscalerConfig{
			StateDatabase:            "test",
			CallbackBaseURL:          "https://trusted.example",
			CallbackServiceAccount:   "callback@example.iam.gserviceaccount.com",
			DeleteTaskQueue:          "delete-queue",
			MaintenanceTaskQueue:     "maintenance-queue",
			RunnerPrefix:             "runner",
			MaxRunners:               2,
			MaxOnDemandRunners:       1,
			AllowOnDemand:            true,
			FallbackInstanceTemplate: "standard",
			TaskTimeout:              30,
			MachineTimeout:           3600,
			MaxRequestBytes:          1 << 20,
			TaskRetryAttempts:        4,
			TaskRetryMaxBackoff:      30,
			TaskRetryMaxDuration:     120,
		}
	}
	require.NoError(t, baseline().Validate())

	for _, tc := range []struct {
		name    string
		mutate  func(*AutoscalerConfig)
		wantErr string
	}{
		{"http scheme", func(c *AutoscalerConfig) { c.CallbackBaseURL = "http://trusted.example" }, "CallbackBaseURL must be an HTTPS origin"},
		{"URL with a path", func(c *AutoscalerConfig) { c.CallbackBaseURL = "https://trusted.example/hook" }, "CallbackBaseURL must be an HTTPS origin"},
		{"URL with a query", func(c *AutoscalerConfig) { c.CallbackBaseURL = "https://trusted.example?src=acme" }, "CallbackBaseURL must be an HTTPS origin"},
		{"STANDARD-only fleet without on-demand", func(c *AutoscalerConfig) { c.FallbackInstanceTemplate = ""; c.AllowOnDemand = false }, "STANDARD-only"},
		{"MaxRunners zero", func(c *AutoscalerConfig) { c.MaxRunners = 0 }, "invalid fleet limits"},
		{"MaxOnDemandRunners exceeds MaxRunners", func(c *AutoscalerConfig) { c.MaxOnDemandRunners = 3 }, "invalid fleet limits"},
		{"missing CallbackServiceAccount", func(c *AutoscalerConfig) { c.CallbackServiceAccount = "" }, "worker identity and queues are required"},
		{"missing DeleteTaskQueue", func(c *AutoscalerConfig) { c.DeleteTaskQueue = "" }, "worker identity and queues are required"},
		{"missing MaintenanceTaskQueue", func(c *AutoscalerConfig) { c.MaintenanceTaskQueue = "" }, "worker identity and queues are required"},
		{"uppercase RunnerPrefix", func(c *AutoscalerConfig) { c.RunnerPrefix = "Runner" }, "runner prefix must be a GCE-safe prefix"},
		{"RunnerPrefix over 20 characters", func(c *AutoscalerConfig) { c.RunnerPrefix = strings.Repeat("a", 21) }, "runner prefix must be a GCE-safe prefix"},
		{"TaskTimeout below range", func(c *AutoscalerConfig) { c.TaskTimeout = 10 }, "invalid deadlines or body limit"},
		{"TaskTimeout above range", func(c *AutoscalerConfig) { c.TaskTimeout = 1800 }, "invalid deadlines or body limit"},
		{"MachineTimeout below 60", func(c *AutoscalerConfig) { c.MachineTimeout = 59 }, "invalid deadlines or body limit"},
		{"MaxRequestBytes zero", func(c *AutoscalerConfig) { c.MaxRequestBytes = 0 }, "invalid deadlines or body limit"},
		{"TaskRetryAttempts zero", func(c *AutoscalerConfig) { c.TaskRetryAttempts = 0 }, "invalid task retry policy"},
		{"negative TaskRetryMaxBackoff", func(c *AutoscalerConfig) { c.TaskRetryMaxBackoff = -1 }, "invalid task retry policy"},
		{"ZoneBenchMinRatio out of range", func(c *AutoscalerConfig) { c.ZoneBenchMinVMs = 3; c.ZoneBenchMinRatio = 0 }, "ZoneBenchMinRatio must be in (0, 1]"},
		{"empty RunnerPrefix", func(c *AutoscalerConfig) { c.RunnerPrefix = "" }, "RunnerPrefix must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := baseline()
			tc.mutate(&c)
			require.ErrorContains(t, c.Validate(), tc.wantErr)
		})
	}
}
