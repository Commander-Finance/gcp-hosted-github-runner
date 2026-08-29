package pkg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// deleteHandlerScaler builds an Autoscaler whose delete-vm callback can be driven
// without touching GCP: deletes are intercepted by the deleteInZoneFn seam, and
// the detached orphan sweep is neutered by pre-seeding lastSweep so it short-
// circuits on the throttle instead of constructing a real compute client.
func deleteHandlerScaler(secret string, deleted *[]string) *Autoscaler {
	gin.SetMode(gin.TestMode)
	s := &Autoscaler{conf: AutoscalerConfig{
		RunnerPrefix:      "runner",
		Zones:             []string{"z1"},
		SourceQueryParam:  "src",
		RegisteredSources: map[string]Source{"repo": {Name: "repo", Secret: secret}},
	}}
	s.deleteInZoneFn = func(ctx context.Context, name string, zone string) (bool, error) {
		*deleted = append(*deleted, name)
		return true, nil // found + deleted in this zone
	}
	s.lastSweep = time.Now() // suppress the detached maybeSweepOrphans goroutine
	// Defaults hold the starvation gate inert: with no originating job recoverable
	// from metadata, a delete can never be classified as theft, so no top-up fires.
	s.instanceJobFn = func(ctx context.Context, name string) (*Job, error) { return nil, nil }
	s.needsRunnerFn = func(ctx context.Context, job Job, src Source, exclude string) (bool, error) { return false, nil }
	return s
}

// postDelete drives handleDeleteVm with a correctly signed delete-vm payload and
// returns the recorded response.
func postDelete(s *Autoscaler, secret string, job Job) *httptest.ResponseRecorder {
	body, _ := json.Marshal(job)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/delete?src=repo", bytes.NewReader(body))
	c.Request.Header.Set(SHA_HEADER, SHA_PREFIX+CalcSigHex([]byte(secret), body))
	s.handleDeleteVm(c)
	return w
}

// A completed job whose VM carries a *different* job's id (cross-assignment) must
// still be deleted, by the webhook-supplied runner_name. This pins the actual
// fix at the handler boundary: the guard routes an owned name into DeleteInstance
// rather than skipping it (which is the leak the change addresses).
func TestHandleDeleteVmDeletesOwnedRunner(t *testing.T) {

	secret := "topsecret"
	var deleted []string
	s := deleteHandlerScaler(secret, &deleted)

	w := postDelete(s, secret, Job{Id: 79130065907, RunnerName: "runner-79130066573"})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"runner-79130066573"}, deleted, "the webhook-named VM must be deleted")
}

// An empty runner_name (job cancelled while queued) or a foreign name must be
// acknowledged (200, so the task isn't retried) without issuing any delete.
func TestHandleDeleteVmSkipsEmptyAndForeignNames(t *testing.T) {

	secret := "topsecret"
	for _, name := range []string{"", "other-42"} {
		var deleted []string
		s := deleteHandlerScaler(secret, &deleted)

		w := postDelete(s, secret, Job{Id: 42, RunnerName: name})

		assert.Equal(t, http.StatusOK, w.Code, "empty/foreign name must be acked, not retried")
		assert.Empty(t, deleted, "must not issue a delete for runner_name %q", name)
	}
}

// recreateHandlerScaler builds an Autoscaler whose recreate-vm callback can be
// driven without touching GCP or Cloud Tasks: the createTaskFn seam intercepts
// all task enqueue attempts so tests can assert on kind, url, delay, and job
// without requiring real GCP credentials. lastSweep is pre-seeded to suppress
// any goroutine-detached orphan sweep that would otherwise try to construct a
// compute client.
func recreateHandlerScaler(secret string, capturedKind *string, capturedUrl *string, capturedDelay *time.Duration, capturedJob *Job, taskErr error) *Autoscaler {
	gin.SetMode(gin.TestMode)
	s := &Autoscaler{conf: AutoscalerConfig{
		RunnerPrefix:     "runner",
		Zones:            []string{"z1"},
		SourceQueryParam: "src",
		RouteCreateVm:    "/create_vm",
		RouteRecreateVm:  "/recreate_vm",
		RegisteredSources: map[string]Source{
			"repo": {Name: "repo", Secret: secret, SourceType: TypeOrganization},
		},
	}}
	s.createTaskFn = func(ctx context.Context, kind string, url string, secret string, job Job, delay time.Duration) error {
		*capturedKind = kind
		*capturedUrl = url
		*capturedDelay = delay
		*capturedJob = job
		return taskErr
	}
	s.lastSweep = time.Now() // suppress the detached maybeSweepOrphans goroutine
	// Default to "a runner is needed" so the pre-gate recreate contract still holds;
	// the tests that exercise the gate override this.
	s.needsRunnerFn = func(ctx context.Context, job Job, src Source, exclude string) (bool, error) { return true, nil }
	return s
}

// postRecreate drives handleRecreateVm with a correctly signed recreate-vm
// payload and returns the recorded response.
func postRecreate(s *Autoscaler, secret string, job Job) *httptest.ResponseRecorder {
	body, _ := json.Marshal(job)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/recreate_vm?src=repo", bytes.NewReader(body))
	c.Request.Header.Set(SHA_HEADER, SHA_PREFIX+CalcSigHex([]byte(secret), body))
	s.handleRecreateVm(c)
	return w
}

// A VM that died without accepting a job calls back with the original job
// payload; the handler must re-enqueue a create-vm Cloud Task so the job gets
// another runner. Verifying that kind == TaskKindCreate and url contains the
// create-vm route pins the routing contract between the shutdown script and the
// re-creation path.
func TestHandleRecreateVmEnqueuesCreateTask(t *testing.T) {

	secret := "recreatesecret"
	var kind string
	var url string
	var delay time.Duration
	var job Job
	s := recreateHandlerScaler(secret, &kind, &url, &delay, &job, nil)

	w := postRecreate(s, secret, Job{Id: 99999, Labels: []string{"spock"}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, TaskKindCreate, kind, "recreate must enqueue a create task, not a delete task")
	assert.True(t, strings.Contains(url, "/create_vm"), "task url must route to the create-vm handler")
	assert.Greater(t, delay, time.Duration(0), "create task must be delayed (non-zero) to allow deduplication window")
	assert.Equal(t, int64(99999), job.Id, "the enqueued task must carry the original job id")
}

// An incorrect signature on the recreate callback must be rejected with 401 so
// a forged shutdown signal cannot trick the autoscaler into creating unlimited
// VMs. The task enqueue function must not be called at all.
func TestHandleRecreateVmRejectsBadSignature(t *testing.T) {

	secret := "recreatesecret"
	var kind string
	var url string
	var delay time.Duration
	var job Job
	s := recreateHandlerScaler(secret, &kind, &url, &delay, &job, nil)

	body, _ := json.Marshal(Job{Id: 99999})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/recreate_vm?src=repo", bytes.NewReader(body))
	c.Request.Header.Set(SHA_HEADER, SHA_PREFIX+CalcSigHex([]byte("wrongsecret"), body))
	s.handleRecreateVm(c)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, kind, "createTaskFn must not be called on bad signature")
}

// A recreate payload with job.Id == 0 means the shutdown script fired with a
// corrupt or missing payload. Acknowledging with 200 (rather than 4xx/5xx) is
// correct because retrying a corrupt payload will never succeed, and 200 causes
// Cloud Tasks to stop retrying. No VM must be created.
func TestHandleRecreateVmRejectsZeroJobId(t *testing.T) {

	secret := "recreatesecret"
	var kind string
	var url string
	var delay time.Duration
	var job Job
	s := recreateHandlerScaler(secret, &kind, &url, &delay, &job, nil)

	w := postRecreate(s, secret, Job{Id: 0})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, kind, "createTaskFn must not be called for a zero-id job")
}

// A task-enqueue failure (e.g. Cloud Tasks quota exceeded or transient API
// error) must surface as a 500 so Cloud Tasks retries the recreate callback
// rather than silently losing the re-creation request and stranding the job.
func TestHandleRecreateVmReturns500OnTaskFailure(t *testing.T) {

	secret := "recreatesecret"
	var kind string
	var url string
	var delay time.Duration
	var job Job
	s := recreateHandlerScaler(secret, &kind, &url, &delay, &job, fmt.Errorf("cloud tasks unavailable"))

	w := postRecreate(s, secret, Job{Id: 99999})

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// handleCreateVm with a zero job id must acknowledge with 200 and must not
// attempt VM creation or JIT-config generation. A zero id means the payload is
// corrupt or the task was mis-enqueued; retrying (via a 5xx) would just repeat
// the failure. The validation guard belongs at the handler boundary so it fires
// before any GCP call.
func TestHandleCreateVmRejectsZeroJobId(t *testing.T) {

	secret := "createsecret"
	gin.SetMode(gin.TestMode)

	jitCalled := false
	insertCalled := false

	s := &Autoscaler{conf: AutoscalerConfig{
		RunnerPrefix:     "runner",
		Zones:            []string{"z1"},
		SourceQueryParam: "src",
		RouteCreateVm:    "/create_vm",
		RegisteredSources: map[string]Source{
			"repo": {Name: "repo", Secret: secret, SourceType: TypeOrganization},
		},
	}}
	s.instanceStateFn = func(ctx context.Context, instanceName string) (bool, State, error) {
		// If the handler reaches instanceState it has not short-circuited on the
		// zero id — the test should catch this.
		return false, Unknown, nil
	}
	s.jitConfigFn = func(ctx context.Context, url string, runnerName string, runnerGroupId int64, labels []string) (string, error) {
		jitCalled = true
		return "fake-jit-config", nil
	}
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
		insertCalled = true
		return nil
	}
	s.lastSweep = time.Now()

	body, _ := json.Marshal(Job{Id: 0})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/create_vm?src=repo", bytes.NewReader(body))
	c.Request.Header.Set(SHA_HEADER, SHA_PREFIX+CalcSigHex([]byte(secret), body))
	s.handleCreateVm(c)

	assert.Equal(t, http.StatusOK, w.Code, "zero-id job must be acked without a 5xx")
	assert.False(t, jitCalled, "jit config must not be generated for a zero-id job")
	assert.False(t, insertCalled, "no VM must be inserted for a zero-id job")
}

// handleDeleteVm with a zero job id must be acknowledged with 200 and no
// delete must be issued. A zero id is not a valid job; retrying it would not
// help, so 200 is the correct ack-and-drop response. This mirrors the
// zero-id guard on the create path.
func TestHandleDeleteVmRejectsZeroJobId(t *testing.T) {

	secret := "topsecret"
	var deleted []string
	s := deleteHandlerScaler(secret, &deleted)

	w := postDelete(s, secret, Job{Id: 0, RunnerName: "runner-123"})

	assert.Equal(t, http.StatusOK, w.Code, "zero-id job must be acked without a 5xx")
	assert.Empty(t, deleted, "no delete must be issued for a zero-id job")
}

// createMetadataScaler builds an Autoscaler whose create-vm callback runs the full
// happy path (instance not found → createProceed) without touching GCP: instance
// state, JIT config, and the Insert are all seam-stubbed, with the Insert capturing
// the metadata items for assertion.
func createMetadataScaler(secret string, jobLogPattern string, capturedMetadata *[]*computepb.Items) *Autoscaler {
	gin.SetMode(gin.TestMode)
	s := &Autoscaler{conf: AutoscalerConfig{
		RunnerPrefix:     "runner",
		Zones:            []string{"z1"},
		SourceQueryParam: "src",
		RouteCreateVm:    "/create_vm",
		RouteRecreateVm:  "/recreate_vm",
		RegisteredSources: map[string]Source{
			"repo": {Name: "repo", Secret: secret, SourceType: TypeOrganization},
		},
		RunnerJobLogPattern: jobLogPattern,
	}}
	s.instanceStateFn = func(ctx context.Context, instanceName string) (bool, State, error) {
		// Return not-found so decideCreate → createProceed (happy path create).
		return false, Unknown, nil
	}
	s.jitConfigFn = func(ctx context.Context, url string, runnerName string, runnerGroupId int64, labels []string) (string, error) {
		return "fake-jit-config-value", nil
	}
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
		*capturedMetadata = md
		return nil
	}
	s.lastSweep = time.Now()
	return s
}

// metadataByKey indexes captured instance-metadata items by key for easy lookup.
func metadataByKey(items []*computepb.Items) map[string]string {
	mdMap := map[string]string{}
	for _, item := range items {
		if item != nil && item.Key != nil && item.Value != nil {
			mdMap[*item.Key] = *item.Value
		}
	}
	return mdMap
}

// createVmWithJitConfig must inject shutdown-script and recreate metadata items
// alongside the JIT config so that the VM can call back if it dies without
// accepting a job. The recreate_callback_sig must be a valid HMAC of the
// recreate_callback_payload so the handler can verify the origin. Testing this
// at the tryInsertFn boundary is the only way to assert on the exact metadata
// items without running real GCP calls.
func TestHandleCreateVmSetsShutdownScriptMetadata(t *testing.T) {

	secret := "metadatasecret"
	var capturedMetadata []*computepb.Items
	s := createMetadataScaler(secret, "job_accepted", &capturedMetadata)

	job := Job{Id: 4242, Labels: []string{"spock"}}
	body, _ := json.Marshal(job)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/create_vm?src=repo", bytes.NewReader(body))
	c.Request.Header.Set(SHA_HEADER, SHA_PREFIX+CalcSigHex([]byte(secret), body))
	s.handleCreateVm(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotEmpty(t, capturedMetadata, "tryInsertFn must have been called with metadata")
	mdMap := metadataByKey(capturedMetadata)

	// A jit_config_* key must be present (the exact suffix is random).
	hasJitKey := false
	for k := range mdMap {
		if strings.HasPrefix(k, RUNNER_JIT_CONFIG_ATTR) {
			hasJitKey = true
			break
		}
	}
	assert.True(t, hasJitKey, "metadata must contain a jit_config_* key")

	// All recreate-mechanism metadata keys must be present. startup-script and
	// shutdown-script are GCE-reserved key names with no constants.
	for _, requiredKey := range []string{
		"startup-script",
		"shutdown-script",
		RECREATE_CALLBACK_URL_ATTR,
		RECREATE_CALLBACK_PAYLOAD_ATTR,
		RECREATE_CALLBACK_SIG_ATTR,
		RECREATE_CALLBACK_JOB_PATTERN_ATTR,
	} {
		assert.Contains(t, mdMap, requiredKey, "metadata must contain key %q", requiredKey)
	}

	// The callback URL must route to the recreate endpoint and carry the source
	// query param, otherwise the dying VM's POST can't be attributed/verified.
	assert.Contains(t, mdMap[RECREATE_CALLBACK_URL_ATTR], "/recreate_vm", "callback url must route to the recreate endpoint")
	assert.Contains(t, mdMap[RECREATE_CALLBACK_URL_ATTR], "src=repo", "callback url must carry the source query param")

	// The recreate_callback_payload must unmarshal to a Job with the original id.
	var payloadJob Job
	err := json.Unmarshal([]byte(mdMap[RECREATE_CALLBACK_PAYLOAD_ATTR]), &payloadJob)
	assert.NoError(t, err, "recreate_callback_payload must be valid JSON")
	assert.Equal(t, int64(4242), payloadJob.Id, "recreate_callback_payload must carry the original job id")

	// The recreate_callback_sig must be a valid HMAC over the payload bytes so
	// handleRecreateVm's verifySignature check will pass.
	expectedSig := CalcSigHex([]byte(secret), []byte(mdMap[RECREATE_CALLBACK_PAYLOAD_ATTR]))
	assert.Equal(t, expectedSig, mdMap[RECREATE_CALLBACK_SIG_ATTR], "recreate_callback_sig must be HMAC of recreate_callback_payload with the source secret")

	// The configured job-accepted log pattern must be stored verbatim so the
	// shutdown script greps for exactly what the startup script waits on.
	assert.Equal(t, "job_accepted", mdMap[RECREATE_CALLBACK_JOB_PATTERN_ATTR], "configured pattern must be stored verbatim")
}

// An empty RunnerJobLogPattern must fall back to the default rather than being
// stored empty: an empty grep pattern matches every journal line, which would
// make every dying VM think it accepted a job and silently suppress all
// recreate callbacks - disabling the orphaned-job fix.
func TestHandleCreateVmDefaultsJobPatternWhenConfigEmpty(t *testing.T) {

	secret := "metadatasecret"
	var capturedMetadata []*computepb.Items
	s := createMetadataScaler(secret, "", &capturedMetadata)

	job := Job{Id: 4243, Labels: []string{"spock"}}
	body, _ := json.Marshal(job)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/create_vm?src=repo", bytes.NewReader(body))
	c.Request.Header.Set(SHA_HEADER, SHA_PREFIX+CalcSigHex([]byte(secret), body))
	s.handleCreateVm(c)

	assert.Equal(t, http.StatusOK, w.Code)
	mdMap := metadataByKey(capturedMetadata)
	assert.Equal(t, DefaultRunnerJobLogPattern, mdMap[RECREATE_CALLBACK_JOB_PATTERN_ATTR], "empty config must fall back to the default pattern, never empty")
}

// conflictScaler builds a create-vm scaler whose jit-config returns 409 on the first call
// (a stale registration) and a valid config thereafter; deleteRunnerFn and tryInsertFn are
// recorded so the recovery path can be asserted.
func conflictScaler(secret string, jitCalls, deleteCalls *int, deletedName *string, inserted *bool, deleteErr error) *Autoscaler {
	gin.SetMode(gin.TestMode)
	s := &Autoscaler{conf: AutoscalerConfig{
		RunnerPrefix:     "runner",
		Zones:            []string{"z1"},
		SourceQueryParam: "src",
		RouteCreateVm:    "/create_vm",
		RegisteredSources: map[string]Source{
			"repo": {Name: "repo", Secret: secret, SourceType: TypeOrganization},
		},
	}}
	s.instanceStateFn = func(ctx context.Context, name string) (bool, State, error) {
		return false, Unknown, nil // no live VM backs the stale registration -> proceed to jit-config
	}
	s.jitConfigFn = func(ctx context.Context, url, runnerName string, gid int64, labels []string) (string, error) {
		*jitCalls++
		if *jitCalls == 1 {
			return "", ErrRunnerNameConflict
		}
		return "fake-jit", nil
	}
	s.deleteRunnerFn = func(ctx context.Context, jitURL, name string) error {
		*deleteCalls++
		*deletedName = name
		return deleteErr
	}
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
		*inserted = true
		return nil
	}
	s.lastSweep = time.Now()
	return s
}

func postCreate(s *Autoscaler, secret string, job Job) *httptest.ResponseRecorder {
	body, _ := json.Marshal(job)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/create_vm?src=repo", bytes.NewReader(body))
	c.Request.Header.Set(SHA_HEADER, SHA_PREFIX+CalcSigHex([]byte(secret), body))
	s.handleCreateVm(c)
	return w
}

// A 409 Conflict from generate-jitconfig (a stale runner registration from a prior create
// whose VM never materialized) must be recovered: delete the stale registration by name,
// retry, and create the VM - so the queued job isn't stranded behind a phantom runner.
func TestHandleCreateVmRecoversFromJitConfigConflict(t *testing.T) {

	secret := "conflictsecret"
	jitCalls, deleteCalls := 0, 0
	deletedName := ""
	inserted := false
	s := conflictScaler(secret, &jitCalls, &deleteCalls, &deletedName, &inserted, nil)

	w := postCreate(s, secret, Job{Id: 777, Labels: []string{"spock"}})

	assert.Equal(t, http.StatusOK, w.Code, "should delete the stale registration, retry, and create the VM")
	assert.Equal(t, 1, deleteCalls, "the stale registration must be deleted exactly once")
	assert.Equal(t, "runner-777", deletedName, "must delete by the deterministic runner name")
	assert.Equal(t, 2, jitCalls, "jit-config must be retried after the stale registration is removed")
	assert.True(t, inserted, "the VM must be created after recovery")
}

// If deleting the stale registration fails, surface a 500 (Cloud Tasks retries) rather than
// retrying jit-config or creating a VM with no clean registration.
func TestHandleCreateVmJitConflictDeleteFailureReturns500(t *testing.T) {

	secret := "conflictsecret2"
	jitCalls, deleteCalls := 0, 0
	deletedName := ""
	inserted := false
	s := conflictScaler(secret, &jitCalls, &deleteCalls, &deletedName, &inserted, fmt.Errorf("github API error"))

	w := postCreate(s, secret, Job{Id: 778, Labels: []string{"spock"}})

	assert.Equal(t, http.StatusInternalServerError, w.Code, "delete failure should surface as 500")
	assert.Equal(t, 1, deleteCalls, "the stale registration delete should be attempted once")
	assert.Equal(t, "runner-778", deletedName, "must attempt deletion by the deterministic runner name")
	assert.Equal(t, 1, jitCalls, "no second jit-config attempt when the delete failed")
	assert.False(t, inserted, "no VM should be created")
}

// Only an offline, not-busy registration (a stale phantom) is deletable; an online or busy
// runner must never be deleted (it may be live mid-job or a fresh concurrent registration).
func TestRunnerIsDeletable(t *testing.T) {

	assert.True(t, runnerIsDeletable("offline", false), "offline + idle is a stale phantom - deletable")
	assert.False(t, runnerIsDeletable("online", false), "an online runner must not be deleted")
	assert.False(t, runnerIsDeletable("offline", true), "a busy runner must not be deleted")
	assert.False(t, runnerIsDeletable("online", true))
	// Fail closed on unknown/empty status - only an explicit "offline" is deletable.
	assert.False(t, runnerIsDeletable("", false), "empty status must fail closed")
	assert.False(t, runnerIsDeletable("unknown", false), "unknown status must fail closed")
}

func TestRunnersBaseFromJitURL(t *testing.T) {

	assert.Equal(t, "https://api.github.com/orgs/acme/actions/runners",
		runnersBaseFromJitURL("https://api.github.com/orgs/acme/actions/runners/generate-jitconfig"))
	assert.Equal(t, "https://api.github.com/repos/acme/app/actions/runners",
		runnersBaseFromJitURL("https://api.github.com/repos/acme/app/actions/runners/generate-jitconfig"))
}

// --- starvation gate -------------------------------------------------------

// needsRunner gates every replacement create. A recreate for a job that has since
// ended - or that an already-idle runner could serve - must not re-enqueue,
// otherwise a VM that boots with nothing to pick up loops idle -> recreate ->
// idle until the task-name suffix budget silently runs out.
func TestHandleRecreateVmSkipsWhenRunnerNotNeeded(t *testing.T) {

	secret := "topsecret"
	var kind, url string
	var delay time.Duration
	var job Job
	s := recreateHandlerScaler(secret, &kind, &url, &delay, &job, nil)
	s.needsRunnerFn = func(ctx context.Context, j Job, src Source, exclude string) (bool, error) { return false, nil }

	w := postRecreate(s, secret, Job{Id: 42, RepositoryFullName: "org/repo"})

	assert.Equal(t, http.StatusOK, w.Code, "a declined recreate must still be acked")
	assert.Empty(t, kind, "no create task may be enqueued when no runner is needed")
}

// A stolen VM's delete is the only signal that the job it was built for lost its
// runner. The original job's id and repo come from the VM's own metadata, because
// the completed webhook names a different job - usually in another repository.
func TestHandleDeleteVmTopsUpStolenRunner(t *testing.T) {

	secret := "topsecret"
	var deleted []string
	s := deleteHandlerScaler(secret, &deleted)
	s.conf.RouteCreateVm = "/create_vm"
	s.instanceJobFn = func(ctx context.Context, name string) (*Job, error) {
		return &Job{Id: 111, RepositoryFullName: "org/mccoy", Labels: []string{"spock"}}, nil
	}
	s.needsRunnerFn = func(ctx context.Context, j Job, src Source, exclude string) (bool, error) { return true, nil }
	var kinds, urls, secrets []string
	var jobs []Job
	var delays []time.Duration
	s.createTaskFn = func(ctx context.Context, kind string, url string, secret string, job Job, delay time.Duration) error {
		kinds = append(kinds, kind)
		urls = append(urls, url)
		secrets = append(secrets, secret)
		delays = append(delays, delay)
		jobs = append(jobs, job)
		return nil
	}

	w := postDelete(s, secret, Job{Id: 222, RunnerName: "runner-111"})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"runner-111"}, deleted, "the webhook-named VM must still be deleted")
	assert.Equal(t, []string{TaskKindTopUp}, kinds, "a stolen VM tops up under its own task kind")
	if assert.Len(t, urls, 1) {
		assert.Contains(t, urls[0], "/create_vm", "the top-up must post to the create route")
		assert.Equal(t, secret, secrets[0], "and be signed with the source's secret")
		assert.Equal(t, topUpVmDelay, delays[0], "the name is already free, so it must not wait recreateVmDelay")
	}
	if assert.Len(t, jobs, 1) {
		assert.Equal(t, int64(111), jobs[0].Id, "the top-up targets the original job, not the completed one")
		assert.Equal(t, "org/mccoy", jobs[0].RepositoryFullName, "and carries the original job's repo")
	}
}

// The mutual-swap case: two VMs run each other's jobs. When the first job
// finishes, its own VM is deleted by webhook-supplied name - but the job that VM
// was built for is already running on the other VM, so nothing may be topped up.
func TestHandleDeleteVmNoTopUpWhenOriginalJobRunning(t *testing.T) {

	secret := "topsecret"
	var deleted []string
	s := deleteHandlerScaler(secret, &deleted)
	s.instanceJobFn = func(ctx context.Context, name string) (*Job, error) {
		return &Job{Id: 111, RepositoryFullName: "org/mccoy"}, nil
	}
	s.needsRunnerFn = func(ctx context.Context, j Job, src Source, exclude string) (bool, error) { return false, nil }
	var kinds []string
	s.createTaskFn = func(ctx context.Context, kind string, url string, secret string, job Job, delay time.Duration) error {
		kinds = append(kinds, kind)
		return nil
	}

	w := postDelete(s, secret, Job{Id: 222, RunnerName: "runner-111"})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"runner-111"}, deleted)
	assert.Empty(t, kinds, "the original job is already running - no top-up")
}

// A VM that ran the job it was built for is an ordinary completion, not theft.
// The gate must not even be consulted, so a normal delete costs no GitHub calls.
func TestHandleDeleteVmNoTopUpForSameJob(t *testing.T) {

	secret := "topsecret"
	var deleted []string
	s := deleteHandlerScaler(secret, &deleted)
	s.instanceJobFn = func(ctx context.Context, name string) (*Job, error) {
		return &Job{Id: 222, RepositoryFullName: "org/backend"}, nil
	}
	gateCalls := 0
	s.needsRunnerFn = func(ctx context.Context, j Job, src Source, exclude string) (bool, error) { gateCalls++; return true, nil }
	var kinds []string
	s.createTaskFn = func(ctx context.Context, kind string, url string, secret string, job Job, delay time.Duration) error {
		kinds = append(kinds, kind)
		return nil
	}

	w := postDelete(s, secret, Job{Id: 222, RunnerName: "runner-222"})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"runner-222"}, deleted)
	assert.Zero(t, gateCalls, "a same-job delete must not consult the gate")
	assert.Empty(t, kinds)
}

// A VM created before the metadata attribute existed - or with the recreate route
// unset - carries no originating payload. The delete must still succeed, and the
// unknown origin must fail closed to no top-up rather than guessing.
func TestHandleDeleteVmNoTopUpWhenMetadataAbsent(t *testing.T) {

	secret := "topsecret"
	var deleted []string
	s := deleteHandlerScaler(secret, &deleted)
	s.instanceJobFn = func(ctx context.Context, name string) (*Job, error) { return nil, nil }
	var kinds []string
	s.createTaskFn = func(ctx context.Context, kind string, url string, secret string, job Job, delay time.Duration) error {
		kinds = append(kinds, kind)
		return nil
	}

	w := postDelete(s, secret, Job{Id: 222, RunnerName: "runner-111"})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"runner-111"}, deleted, "an unreadable origin must not block the delete")
	assert.Empty(t, kinds, "unknown origin fails closed")
}

// --- the gate's own GitHub calls -------------------------------------------

// GitHub answers 404 for a resource the token cannot see, not 403, so a PAT
// without actions:read is indistinguishable from a deleted job. Reading 404 as
// "not queued" would make the gate decline every recreate and silently disable
// the fleet's only self-heal, so it must surface as an error and let each caller
// apply its own fail-open / fail-closed policy.
func TestJobIsQueuedTreatsNotFoundAsError(t *testing.T) {

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	s := &Autoscaler{githubAPIBase: srv.URL}

	queued, err := s.jobIsQueued(context.Background(), "pat", Job{Id: 7, RepositoryFullName: "o/r"})

	assert.Error(t, err, "404 is ambiguous (missing scope vs missing job) - it must not read as a confident negative")
	assert.False(t, queued)
}

func TestJobIsQueuedReadsStatus(t *testing.T) {

	for _, tc := range []struct {
		status string
		want   bool
	}{{"queued", true}, {"in_progress", false}, {"completed", false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"status":%q}`, tc.status)
		}))
		s := &Autoscaler{githubAPIBase: srv.URL}

		queued, err := s.jobIsQueued(context.Background(), "pat", Job{Id: 7, RepositoryFullName: "o/r"})

		assert.NoError(t, err)
		assert.Equal(t, tc.want, queued, "status %q", tc.status)
		srv.Close()
	}
}

// runnersPage serves a single runners page built from the given entries.
func runnersJSON(entries ...string) string {
	return `{"runners":[` + strings.Join(entries, ",") + `]}`
}

func runnerEntry(name string, status string, busy bool, labels ...string) string {
	l := make([]string, 0, len(labels))
	for _, n := range labels {
		l = append(l, fmt.Sprintf(`{"name":%q}`, n))
	}
	return fmt.Sprintf(`{"name":%q,"status":%q,"busy":%v,"labels":[%s]}`, name, status, busy, strings.Join(l, ","))
}

// A JIT runner registers with its own job's labels, so an idle runner only
// counts as supply for jobs whose labels it carries. Treating a plain "spock"
// runner as supply for a magic-label job strands that job: the gate is confident,
// so no fail-open path rescues it.
func TestHasIdleRunnerRequiresMatchingLabels(t *testing.T) {

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, runnersJSON(runnerEntry("runner-1", "online", false, "spock")))
	}))
	defer srv.Close()
	s := &Autoscaler{conf: AutoscalerConfig{RunnerPrefix: "runner"}}
	jit := srv.URL + "/orgs/x/actions/runners/generate-jitconfig"

	plain, err := s.hasIdleRunner(context.Background(), "pat", jit, []string{"spock"}, "")
	assert.NoError(t, err)
	assert.True(t, plain, "an idle runner carrying the job's labels is supply")

	magic, err := s.hasIdleRunner(context.Background(), "pat", jit, []string{"spock", "gce-machine-c4d-highmem-2"}, "")
	assert.NoError(t, err)
	assert.False(t, magic, "it cannot serve a job needing a label it does not carry")
}

func TestHasIdleRunnerIgnoresBusyOfflineAndExcluded(t *testing.T) {

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, runnersJSON(
			runnerEntry("runner-1", "online", true, "spock"),
			runnerEntry("runner-2", "offline", false, "spock"),
			runnerEntry("runner-3", "online", false, "spock"),
			runnerEntry("other-4", "online", false, "spock"),
		))
	}))
	defer srv.Close()
	s := &Autoscaler{conf: AutoscalerConfig{RunnerPrefix: "runner"}}
	jit := srv.URL + "/orgs/x/actions/runners/generate-jitconfig"

	idle, err := s.hasIdleRunner(context.Background(), "pat", jit, []string{"spock"}, "")
	assert.NoError(t, err)
	assert.True(t, idle, "runner-3 is online and free")

	// The VM we just deleted may still be registered; it is not supply.
	idle, err = s.hasIdleRunner(context.Background(), "pat", jit, []string{"spock"}, "runner-3")
	assert.NoError(t, err)
	assert.False(t, idle, "the excluded runner must not count, and busy/offline/foreign never do")
}

func TestHasIdleRunnerPaginates(t *testing.T) {

	full := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		full = append(full, runnerEntry(fmt.Sprintf("runner-%d", i), "online", true, "spock"))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, runnersJSON(full...))
			return
		}
		fmt.Fprint(w, runnersJSON(runnerEntry("runner-late", "online", false, "spock")))
	}))
	defer srv.Close()
	s := &Autoscaler{conf: AutoscalerConfig{RunnerPrefix: "runner"}}

	idle, err := s.hasIdleRunner(context.Background(), "pat",
		srv.URL+"/orgs/x/actions/runners/generate-jitconfig", []string{"spock"}, "")

	assert.NoError(t, err)
	assert.True(t, idle, "a full first page must not end the scan - the idle runner is on page 2")
}

func TestHasIdleRunnerSurfacesHTTPError(t *testing.T) {

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"nope"}`)
	}))
	defer srv.Close()
	s := &Autoscaler{conf: AutoscalerConfig{RunnerPrefix: "runner"}}

	_, err := s.hasIdleRunner(context.Background(), "pat",
		srv.URL+"/orgs/x/actions/runners/generate-jitconfig", []string{"spock"}, "")

	assert.Error(t, err, "a non-200 must never be read as 'no idle runners'")
}

// --- gate error policy and ordering ----------------------------------------

// Fail open: a demand check that errors must leave the pre-gate recreate
// behaviour untouched, so a GitHub outage cannot strand every dead VM.
func TestHandleRecreateVmRecreatesWhenGateErrors(t *testing.T) {

	secret := "topsecret"
	var kind, url string
	var delay time.Duration
	var job Job
	s := recreateHandlerScaler(secret, &kind, &url, &delay, &job, nil)
	s.needsRunnerFn = func(ctx context.Context, j Job, src Source, exclude string) (bool, error) {
		return false, fmt.Errorf("github unreachable")
	}

	w := postRecreate(s, secret, Job{Id: 42, RepositoryFullName: "org/repo"})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, TaskKindCreate, kind, "an unreadable gate must not suppress the recreate")
}

// Fail closed: an unreadable gate must never authorise a VM nobody asked for.
func TestHandleDeleteVmNoTopUpWhenGateErrors(t *testing.T) {

	secret := "topsecret"
	var deleted []string
	s := deleteHandlerScaler(secret, &deleted)
	s.conf.RouteCreateVm = "/create_vm"
	s.instanceJobFn = func(ctx context.Context, name string) (*Job, error) {
		return &Job{Id: 111, RepositoryFullName: "org/mccoy"}, nil
	}
	s.needsRunnerFn = func(ctx context.Context, j Job, src Source, exclude string) (bool, error) {
		return false, fmt.Errorf("github unreachable")
	}
	var kinds []string
	s.createTaskFn = func(ctx context.Context, kind string, url string, secret string, job Job, delay time.Duration) error {
		kinds = append(kinds, kind)
		return nil
	}

	w := postDelete(s, secret, Job{Id: 222, RunnerName: "runner-111"})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"runner-111"}, deleted)
	assert.Empty(t, kinds, "an unreadable gate must not authorise a VM")
}

// The originating job lives in the VM's metadata, which dies with the VM. If the
// read ever moves after the delete the feature silently stops working, so pin
// the ordering rather than trusting the call site to stay put.
func TestHandleDeleteVmReadsMetadataBeforeDeleting(t *testing.T) {

	secret := "topsecret"
	var deleted []string
	s := deleteHandlerScaler(secret, &deleted)
	readWhileAlive := false
	s.instanceJobFn = func(ctx context.Context, name string) (*Job, error) {
		readWhileAlive = len(deleted) == 0
		return &Job{Id: 111, RepositoryFullName: "org/mccoy"}, nil
	}

	postDelete(s, secret, Job{Id: 222, RunnerName: "runner-111"})

	assert.True(t, readWhileAlive, "the metadata must be read before the VM is deleted")
	assert.Equal(t, []string{"runner-111"}, deleted)
}
