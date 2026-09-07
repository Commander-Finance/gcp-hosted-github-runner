package pkg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/stretchr/testify/require"
)

func TestCrossAssignmentAndPreemptionDoNotStrandDemand(t *testing.T) {
	s, m, src, a := lifecycleTestScaler()
	ctx := context.Background()
	b := a
	b.Id++
	for _, j := range []Job{a, b} {
		require.NoError(t, s.observe(ctx, src, j, false))
		require.NoError(t, s.processJob(ctx, src, j))
	}
	ra, rb := m.get(jobKey(src.Name, a)).VMName, m.get(jobKey(src.Name, b)).VMName
	s.jobStatusFn = func(_ context.Context, j Job) (string, error) {
		if j.Id == b.Id {
			return "in_progress", nil
		}
		return "queued", nil
	}
	s.instanceStateFn = func(_ context.Context, name string) (bool, State, error) { return name != rb, RUNNING, nil }
	s.runnerBusyFn = func(_ context.Context, _ Source, name string) (bool, error) { return name == ra, nil }
	// B is served by RA; its own RB was lost. Free RB's reservation first.
	require.NoError(t, s.processJob(ctx, src, b))
	require.NoError(t, s.processJob(ctx, src, a))
	require.Empty(t, m.get(jobKey(src.Name, a)).VMName)
	require.Equal(t, 1, m.fleet.Runners) // RA still accounted while it serves B.
	require.NoError(t, s.processJob(ctx, src, a))
	replacement := m.get(jobKey(src.Name, a)).VMName
	require.NotEmpty(t, replacement)
	require.NotEqual(t, ra, replacement)
	require.Equal(t, 2, m.fleet.Runners)
	retained, err := m.Runner(ctx, ra)
	require.NoError(t, err)
	require.Empty(t, retained.Owner)
	require.False(t, retained.Available)
}
func TestSpareRunnerIsAdoptedWithoutNewCapacity(t *testing.T) {
	s, m, src, a := lifecycleTestScaler()
	ctx := context.Background()
	b := a
	b.Id++
	for _, j := range []Job{a, b} {
		require.NoError(t, s.observe(ctx, src, j, false))
		require.NoError(t, s.processJob(ctx, src, j))
	}
	ra, rb := m.get(jobKey(src.Name, a)).VMName, m.get(jobKey(src.Name, b)).VMName
	s.jobStatusFn = func(_ context.Context, j Job) (string, error) {
		if j.Id == b.Id {
			return "in_progress", nil
		}
		return "queued", nil
	}
	s.instanceStateFn = func(context.Context, string) (bool, State, error) { return true, RUNNING, nil }
	s.runnerBusyFn = func(_ context.Context, _ Source, name string) (bool, error) { return name == ra, nil }
	require.NoError(t, s.processJob(ctx, src, b)) // RB is idle, B is on RA.
	require.NoError(t, s.processJob(ctx, src, a)) // RA is busy, detach it from A.
	s.tryInsertFn = func(context.Context, creationAttempt, string, []*computepb.Items) error {
		t.Fatal("spare capacity should be adopted")
		return nil
	}
	require.NoError(t, s.processJob(ctx, src, a))
	require.Equal(t, rb, m.get(jobKey(src.Name, a)).VMName)
	require.Equal(t, 2, m.fleet.Runners)
	require.NoError(t, m.ReleaseRunner(ctx, ra))
	require.NoError(t, m.ReleaseRunner(ctx, ra))
	require.Equal(t, 1, m.fleet.Runners)
}
func TestDueQueryExcludesTombstonesAndStableJobs(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	for i := int64(1); i <= 14000; i++ {
		job := j
		job.Id = i
		require.NoError(t, s.observe(ctx, src, job, true))
	}
	j.Id = 20000
	require.NoError(t, s.observe(ctx, src, j, false))
	future := j
	future.Id++
	require.NoError(t, s.observe(ctx, src, future, false))
	require.NoError(t, m.UpdateJob(ctx, jobKey(src.Name, future), func(r *lifecycleRecord, _ *fleetState) error { r.NextActionAt = time.Now().Add(time.Hour); return nil }))
	cleanup := j
	cleanup.Id += 2
	cleanup.RunnerName = "runner-1-0123456789abcdef"
	require.NoError(t, s.observe(ctx, src, cleanup, true))
	rows, next, err := m.Page(ctx, "", 50)
	require.NoError(t, err)
	require.Empty(t, next)
	require.Len(t, rows, 2)
	require.True(t, m.get(jobKey(src.Name, cleanup)).NeedsReconcile)
}
func TestOneRetryChainSurvivesRepeatedReconciliation(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, j, false))
	sent := 0
	var task Job
	s.queueFn = func(_ context.Context, _, _ string, p interface{}, _ time.Duration) error {
		sent++
		task = p.(Job)
		return nil
	}
	require.NoError(t, s.enqueueJob(ctx, src, j, 0))
	code, err := s.finishTask(ctx, src, task, errors.New("temporary API outage"))
	require.NoError(t, err)
	require.Equal(t, 503, code)
	for i := 0; i < 20; i++ {
		require.NoError(t, s.enqueueJob(ctx, src, j, 0))
	}
	require.Equal(t, 1, sent)
	code, err = s.finishTask(ctx, src, task, errFleetFull)
	require.NoError(t, err)
	require.Equal(t, 200, code)
	require.True(t, m.get(jobKey(src.Name, j)).NextActionAt.After(time.Now()))
	require.True(t, m.get(jobKey(src.Name, j)).EnqueuedUntil.IsZero())
	require.ErrorIs(t, s.activeTask(ctx, src, task), errTaskObsolete)
}
func TestLeaseAndPermanentFailuresDoNotHotRetry(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, j, false))
	before := m.capacityWrites
	_, err := s.claim(ctx, jobKey(src.Name, j), "owner")
	require.NoError(t, err)
	code, err := s.finishTask(ctx, src, j, errLeaseBusy)
	require.NoError(t, err)
	require.Equal(t, 200, code)
	require.Equal(t, before, m.capacityWrites)
	require.NoError(t, m.Update(ctx, jobKey(src.Name, j), func(r *lifecycleRecord, f *fleetState) error { r.VMName = "unsubmitted"; f.Runners++; return nil }))
	code, err = s.finishTask(ctx, src, j, permanentError{"invalid machine"})
	require.NoError(t, err)
	require.Equal(t, 200, code)
	require.Zero(t, m.fleet.Runners)
	require.False(t, m.get(jobKey(src.Name, j)).NeedsReconcile)
}

type replayedEnqueueStore struct{ lifecycleStore }

func (r replayedEnqueueStore) UpdateJob(ctx context.Context, key string, change func(*lifecycleRecord, *fleetState) error) error {
	// Simulate an aborted first attempt followed by a competing dispatch winning.
	first := lifecycleRecord{NeedsReconcile: true}
	if err := change(&first, &fleetState{}); err != nil {
		return err
	}
	second := lifecycleRecord{NeedsReconcile: true, EnqueuedUntil: time.Now().Add(time.Hour)}
	return change(&second, &fleetState{})
}

func TestReplayedEnqueueDoesNotSendAbortedDispatch(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	s.store = replayedEnqueueStore{m}
	s.queueFn = func(context.Context, string, string, interface{}, time.Duration) error {
		t.Fatal("aborted transaction must not enqueue its stale dispatch")
		return nil
	}
	require.NoError(t, s.enqueueJob(context.Background(), src, j, 0))
}
func TestStateVersionFailsClosed(t *testing.T) {
	for _, version := range []int{0, 2, 99} {
		r := lifecycleRecord{SchemaVersion: version}
		require.ErrorIs(t, checkRecordVersion(&r, true), errSchema)
	}
	r := lifecycleRecord{}
	require.NoError(t, checkRecordVersion(&r, false))
	require.Equal(t, stateVersion, r.SchemaVersion)
}
func TestCounterAuditDistinguishesPendingFromDrift(t *testing.T) {
	r := runnerRecord{Name: "runner-pending", Record: lifecycleRecord{Model: "spot"}}
	require.NoError(t, ledgerDiscrepancy(fleetState{Runners: 1}, []runnerRecord{r}, map[string]string{}))
	require.Error(t, ledgerDiscrepancy(fleetState{Runners: 0}, []runnerRecord{r}, map[string]string{}))
	require.Error(t, ledgerDiscrepancy(fleetState{Runners: 2}, []runnerRecord{r}, map[string]string{}))
	require.Error(t, ledgerDiscrepancy(fleetState{Runners: 1}, []runnerRecord{r}, map[string]string{"unmanaged": "spot"}))
	r.Record.CreatedAt = time.Now().Add(-time.Hour)
	require.Error(t, ledgerDiscrepancy(fleetState{Runners: 1}, []runnerRecord{r}, map[string]string{}))
}
func TestGithubBackpressurePersistsAcrossWorkers(t *testing.T) {
	s, m, _, _ := lifecycleTestScaler()
	ctx := context.Background()
	endpoint := "https://api.github.com/repos/acme/repo/actions/jobs/10"
	cases := []struct {
		code    int
		headers http.Header
		body    string
		global  bool
	}{
		{429, http.Header{"Retry-After": []string{"120"}}, "", true},
		{403, http.Header{"X-Ratelimit-Remaining": []string{"0"}, "X-Ratelimit-Reset": []string{fmt.Sprint(time.Now().Add(time.Hour).Unix())}}, "", true},
		{403, http.Header{}, `{"message":"You have exceeded a secondary rate limit"}`, true},
		{404, http.Header{}, "", false},
	}
	for _, tc := range cases {
		m.backoffs = map[string]time.Time{}
		e := s.githubFailure(ctx, endpoint, &http.Response{StatusCode: tc.code, Header: tc.headers, Body: io.NopCloser(strings.NewReader(tc.body))})
		var delayed retryAtError
		require.ErrorAs(t, e, &delayed)
		require.True(t, delayed.Until.After(time.Now()))
		other, _, _, _ := lifecycleTestScaler()
		other.store = m
		other.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("backoff must avoid network"); return nil, nil })}
		var result interface{}
		require.ErrorAs(t, other.githubGet(ctx, "test", endpoint, &result), &delayed)
	}
	require.Error(t, s.githubFailure(ctx, endpoint, &http.Response{StatusCode: 503, Header: http.Header{}}))
}
