package pkg

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestGithubBackoffIsScopedToTheDeniedEndpoint(t *testing.T) {
	jobs := githubEndpointKey("https://api.github.com/repos/acme/repo/actions/jobs/10")
	runners := githubEndpointKey("https://api.github.com/repos/acme/repo/actions/runners/42")
	require.NotEqual(t, jobs, runners)
	require.Equal(t,
		githubEndpointKey("https://api.github.com/repos/acme/repo/actions/runners?per_page=100&page=1"),
		githubEndpointKey("https://api.github.com/repos/acme/repo/actions/runners?per_page=100&page=2"))

	s, m, _, _ := lifecycleTestScaler()
	ctx := context.Background()
	var delayed retryAtError
	require.ErrorAs(t, s.githubFailure(ctx, "https://api.github.com/repos/acme/repo/actions/jobs/10", &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}), &delayed)
	reached := 0
	s.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		reached++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"runners":[]}`))}, nil
	})}
	var result interface{}
	require.NoError(t, s.githubGet(ctx, "test", "https://api.github.com/repos/acme/repo/actions/runners?per_page=100&page=1", &result))
	require.Equal(t, 1, reached)
	require.Len(t, m.backoffs, 1)
}

func TestDeleteCallbackReleasesFleetReservation(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, j, false))
	require.NoError(t, s.processJob(ctx, src, j))
	key := jobKey(j)
	name := m.get(key).VMName
	require.NotEmpty(t, name)
	require.Equal(t, 1, m.fleet.Runners)
	completed := j
	completed.RunnerName = name
	require.NoError(t, s.observe(ctx, src, completed, true))
	var task Job
	s.queueFn = func(_ context.Context, route, _ string, p interface{}, _ time.Duration) error {
		require.Equal(t, s.conf.RouteDeleteVm, route)
		task = p.(Job)
		return nil
	}
	require.NoError(t, s.enqueueJob(ctx, src, completed, 0))
	s.deleteInZoneFn = func(context.Context, string, string) (bool, error) { return true, nil }
	code, err := s.deleteRunner(ctx, src, task)
	require.NoError(t, err)
	require.Equal(t, 200, code)
	after := m.get(key)
	require.Empty(t, after.VMName)
	require.Empty(t, after.PendingDelete)
	require.Equal(t, 0, m.fleet.Runners)
	require.False(t, after.NeedsReconcile)
}

func TestDiscoveryListsRunsOncePerRepository(t *testing.T) {
	s, _, src, _ := lifecycleTestScaler()
	ctx := context.Background()
	var queued []discoveryPage
	s.queueFn = func(_ context.Context, _, _ string, p interface{}, _ time.Duration) error {
		queued = append(queued, p.(discoveryPage))
		return nil
	}
	var requests []string
	s.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.URL.String())
		body := `{"workflow_runs":[{"id":1,"status":"queued"},{"id":2,"status":"completed"},{"id":3,"status":"in_progress"},{"id":4,"status":"waiting"}]}`
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	require.NoError(t, s.discoverWithPAT(ctx, src, "test", discoveryPage{Source: src.Name, Repository: "acme/repo"}))
	require.Len(t, requests, 1)
	require.Contains(t, requests[0], "/repos/acme/repo/actions/runs?")
	require.Contains(t, requests[0], "created=")
	require.NotContains(t, requests[0], "status=")
	var runs []int64
	for _, p := range queued {
		runs = append(runs, p.RunID)
	}
	require.ElementsMatch(t, []int64{1, 3, 4}, runs)
}

func TestSweepDrainsMoreThanOnePageOfDueRunners(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	m.runners = map[string]runnerRecord{}
	for i := 0; i < 150; i++ {
		name := fmt.Sprintf("runner-%d-0123456789abcdef", i)
		m.runners[name] = runnerRecord{SchemaVersion: stateVersion, Name: name, Pool: poolKey(src.Name, j), NextActionAt: time.Now().Add(-time.Minute), Record: lifecycleRecord{SchemaVersion: stateVersion, Source: src.Name, Job: j, VMName: name}}
	}
	m.fleet.Runners = 150
	require.NoError(t, s.reconcileRunners(ctx))
	require.Empty(t, m.runners)
	require.Equal(t, 0, m.fleet.Runners)
}

func TestAmbiguousInsertResolvesFromOperationState(t *testing.T) {
	type tc struct {
		name        string
		operation   *computepb.Operation
		attemptedAt time.Time
		replaced    bool
	}
	cases := []tc{
		{"done operation without a VM releases the generation", &computepb.Operation{Status: computepb.Operation_DONE.Enum()}, time.Time{}, true},
		{"running operation keeps the generation", &computepb.Operation{Status: computepb.Operation_RUNNING.Enum()}, time.Time{}, false},
		{"missing operation keeps a recent attempt", nil, time.Now(), false},
		{"missing operation expires an old attempt", nil, time.Now().Add(-time.Hour), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, m, src, j := lifecycleTestScaler()
			ctx := context.Background()
			require.NoError(t, s.observe(ctx, src, j, false))
			var names []string
			s.tryInsertFn = func(_ context.Context, _ creationAttempt, name string, _ []*computepb.Items) error {
				names = append(names, name)
				if len(names) == 1 {
					return context.DeadlineExceeded
				}
				return nil
			}
			require.Error(t, s.processJob(ctx, src, j))
			key := jobKey(j)
			if !c.attemptedAt.IsZero() {
				require.NoError(t, m.UpdateJob(ctx, key, func(r *lifecycleRecord, _ *fleetState) error { r.AttemptedAt = c.attemptedAt; return nil }))
			}
			looked := 0
			s.operationLookupFn = func(_ context.Context, r lifecycleRecord) (*computepb.Operation, error) {
				looked++
				require.Equal(t, m.get(key).VMName, r.VMName)
				return c.operation, nil
			}
			require.NoError(t, s.processJob(ctx, src, j))
			require.Equal(t, 1, looked)
			require.Len(t, names, 2)
			require.Equal(t, c.replaced, names[0] != names[1])
			require.Equal(t, 1, m.fleet.Runners)
		})
	}
}

func TestDispatchWindowFollowsConfiguredRetryPolicy(t *testing.T) {
	// Attempt-bound chain: 4 attempts of 185s and three 30s backoffs, plus margin.
	require.Equal(t, time.Duration(4*185+3*30+60)*time.Second, dispatchWindow(180, 4, 30, 120))
	// Duration-bound chain: fast failures keep retrying until max_retry_duration elapses.
	require.Equal(t, time.Duration(600+35+60)*time.Second, dispatchWindow(30, 2, 10, 600))
	// A retuned queue policy changes the marker without touching Go arithmetic.
	require.Equal(t, time.Duration(16*185+15*600+60)*time.Second, dispatchWindow(180, 16, 600, 7200))

	s, m, src, j := lifecycleTestScaler()
	s.conf.TaskRetryAttempts, s.conf.TaskRetryMaxBackoff, s.conf.TaskRetryMaxDuration = 16, 600, 7200
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, j, false))
	require.NoError(t, s.enqueueJob(ctx, src, j, 0))
	until := m.get(jobKey(j)).EnqueuedUntil
	require.WithinDuration(t, time.Now().Add(dispatchWindow(30, 16, 600, 7200)), until, 5*time.Second)
}

func TestSweepSkipsRunnerWithUnknownSourceAndContinues(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	m.runners = map[string]runnerRecord{}
	orphan := lifecycleRecord{SchemaVersion: stateVersion, Source: "removed-org", Job: j, VMName: "runner-1-0123456789abcdef"}
	healthy := lifecycleRecord{SchemaVersion: stateVersion, Source: src.Name, Job: j, VMName: "runner-2-0123456789abcdef"}
	for _, r := range []lifecycleRecord{orphan, healthy} {
		m.runners[r.VMName] = runnerRecord{SchemaVersion: stateVersion, Name: r.VMName, Pool: poolKey(r.Source, r.Job), NextActionAt: time.Now().Add(-time.Minute), Record: r}
	}
	s.instanceStateFn = func(context.Context, string) (bool, State, error) { return true, RUNNING, nil }
	require.NoError(t, s.reconcileRunners(ctx))
	require.True(t, m.runners[healthy.VMName].Available)
	require.True(t, m.runners[healthy.VMName].NextActionAt.After(time.Now()))
	require.False(t, m.runners[orphan.VMName].Available)
	require.True(t, m.runners[orphan.VMName].NextActionAt.After(time.Now()))
}

func TestDiscoveryRejectsLegacyMagicLabelLikeWebhooks(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	sent := 0
	s.queueFn = func(context.Context, string, string, interface{}, time.Duration) error { sent++; return nil }
	legacy := j
	legacy.Labels = []string{"spock", "@machine:n2-standard-8"}
	require.NoError(t, s.observeDiscoveredJobs(context.Background(), src, j.RepositoryFullName, []Job{legacy}))
	require.Empty(t, m.rows)
	require.Equal(t, 0, sent)
}

func TestDiscoveryPagesRepositoriesAndRunJobs(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	var queued []discoveryPage
	s.queueFn = func(_ context.Context, route, _ string, p interface{}, _ time.Duration) error {
		if route == "/discover" {
			queued = append(queued, p.(discoveryPage))
		}
		return nil
	}
	s.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		page := req.URL.Query().Get("page")
		var body string
		switch {
		case strings.HasSuffix(req.URL.Path, "/repos") && page == "1":
			body = `[` + strings.TrimSuffix(strings.Repeat(`{"full_name":"acme/live","archived":false},`, 99), ",") + `,{"full_name":"acme/old","archived":true}]`
		case strings.HasSuffix(req.URL.Path, "/repos"):
			body = `[{"full_name":"acme/last","archived":false}]`
		case strings.HasSuffix(req.URL.Path, "/jobs") && page == "1":
			body = `{"jobs":[` + strings.TrimSuffix(strings.Repeat(fmt.Sprintf(`{"id":%d,"status":"queued","labels":["spock"]},`, j.Id), 100), ",") + `]}`
		default:
			body = fmt.Sprintf(`{"jobs":[{"id":%d,"status":"queued","labels":["spock"]}]}`, j.Id+1)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	require.NoError(t, s.discoverWithPAT(ctx, src, "test", discoveryPage{Source: src.Name}))
	repos := map[string]int{}
	continued := 0
	for _, p := range queued {
		if p.Repository != "" {
			repos[p.Repository]++
		} else {
			continued++
			require.Equal(t, 2, p.Page)
		}
	}
	require.Equal(t, 99, repos["acme/live"])
	require.Zero(t, repos["acme/old"])
	require.Equal(t, 1, continued)
	queued = nil
	require.NoError(t, s.discoverWithPAT(ctx, src, "test", discoveryPage{Source: src.Name, Repository: "acme/repo", RunID: 7}))
	require.Len(t, queued, 1)
	require.Equal(t, 2, queued[0].Page)
	require.NoError(t, s.discoverWithPAT(ctx, src, "test", discoveryPage{Source: src.Name, Repository: "acme/repo", RunID: 7, Page: 2}))
	require.Len(t, m.rows, 2)
}

func TestOverlappingWebhookScopesShareOneDemandRecord(t *testing.T) {
	s, m, org, j := lifecycleTestScaler()
	ctx := context.Background()
	repo := Source{Name: j.RepositoryFullName, SourceType: TypeRepository, Secret: "repo-secret"}
	s.conf.RegisteredSources[repo.Name] = repo
	require.NoError(t, s.observe(ctx, org, j, false))
	require.NoError(t, s.observe(ctx, repo, j, false))
	require.Len(t, m.rows, 1)
	for _, r := range m.rows {
		require.Equal(t, org.Name, r.Source)
	}
	require.NoError(t, s.processJob(ctx, repo, j))
	require.NoError(t, s.processJob(ctx, org, j))
	require.Equal(t, 1, m.fleet.Runners)
	delete(s.conf.RegisteredSources, org.Name)
	require.NoError(t, s.observe(ctx, repo, j, false))
	for _, r := range m.rows {
		require.Equal(t, repo.Name, r.Source)
	}
}

// A retry after an ambiguous insert learns from Compute that the operation
// settled with a stockout; the plan must continue past that attempt instead of
// returning the same error until reconciliation restarts from the first zone.
func TestSettledStockoutAdvancesPastPinnedAttempt(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, j, false))
	key := jobKey(j)
	var attempts []creationAttempt
	s.tryInsertFn = func(_ context.Context, a creationAttempt, _ string, _ []*computepb.Items) error {
		attempts = append(attempts, a)
		if len(attempts) == 1 {
			return context.DeadlineExceeded
		}
		if a.provisioningModel == "spot" {
			return operationError{fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED")}
		}
		return nil
	}
	require.Error(t, s.processJob(ctx, src, j))
	first := m.get(key)
	require.NoError(t, m.UpdateJob(ctx, key, func(r *lifecycleRecord, _ *fleetState) error { r.Operation = "insert-op"; return nil }))
	require.NoError(t, s.processJob(ctx, src, j))
	after := m.get(key)
	require.Equal(t, first.VMName, after.VMName)
	require.Equal(t, "standard", after.Model)
	require.Equal(t, 1, m.fleet.Runners)
	require.Equal(t, 1, m.fleet.Standard)
	var models []string
	for _, a := range attempts {
		models = append(models, a.provisioningModel+"/"+a.zone)
	}
	require.Equal(t, []string{"spot/" + first.Zone, "spot/" + first.Zone}, models[:2])
	require.Equal(t, "standard", attempts[len(attempts)-1].provisioningModel)
	require.Len(t, attempts, 4)
}

// Reconciliation that finds the recorded operation settled with a stockout
// keeps the generation and its JIT credential and continues the plan after the
// failed attempt rather than releasing and restarting from the first zone.
func TestResolvedStockoutKeepsGenerationAndSkipsFailedAttempt(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, j, false))
	key := jobKey(j)
	jitCalls := 0
	s.jitConfigFn = func(context.Context, string, string, int64, []string) (string, error) { jitCalls++; return "jit", nil }
	var attempts []creationAttempt
	s.tryInsertFn = func(_ context.Context, a creationAttempt, _ string, _ []*computepb.Items) error {
		attempts = append(attempts, a)
		if len(attempts) == 1 {
			return context.DeadlineExceeded
		}
		if a.provisioningModel == "spot" {
			return operationError{fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED")}
		}
		return nil
	}
	require.Error(t, s.processJob(ctx, src, j))
	first := m.get(key)
	s.operationLookupFn = func(context.Context, lifecycleRecord) (*computepb.Operation, error) {
		return &computepb.Operation{Status: computepb.Operation_DONE.Enum(), HttpErrorStatusCode: proto.Int32(403), HttpErrorMessage: proto.String("Forbidden"), Error: &computepb.Error{Errors: []*computepb.Errors{{Code: proto.String("ZONE_RESOURCE_POOL_EXHAUSTED")}}}}, nil
	}
	require.NoError(t, s.processJob(ctx, src, j))
	after := m.get(key)
	require.Equal(t, first.VMName, after.VMName)
	require.Equal(t, 1, jitCalls)
	require.Equal(t, "standard", after.Model)
	require.Equal(t, 1, m.fleet.Runners)
	require.Equal(t, 1, m.fleet.Standard)
	require.NotEqual(t, first.Zone, attempts[1].zone)
	require.Equal(t, "spot", attempts[1].provisioningModel)
	require.Len(t, attempts, 3)
}
