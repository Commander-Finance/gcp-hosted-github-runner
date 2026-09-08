package pkg

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPositiveAssignmentOverridesFalseIdle(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued origin", true: "completed origin"}[terminal], func(t *testing.T) {
			s, m, src, a := lifecycleTestScaler()
			ctx := context.Background()
			b := a
			b.Id++
			for _, job := range []Job{a, b} {
				require.NoError(t, s.observe(ctx, src, job, false))
				require.NoError(t, s.processJob(ctx, src, job))
			}
			ra, rb := m.get(jobKey(a)).VMName, m.get(jobKey(b)).VMName
			b.Status, b.RunnerName = "in_progress", ra
			// Discovery reconstructs assignment even when the webhook was missed.
			require.NoError(t, s.observeDiscoveredJobs(ctx, src, b.RepositoryFullName, []Job{b}))
			s.jobStatusFn = func(_ context.Context, j Job) (string, error) {
				if j.Id == b.Id {
					return "in_progress", nil
				}
				if terminal {
					return "completed", nil
				}
				return "queued", nil
			}
			s.instanceStateFn = func(_ context.Context, name string) (bool, State, error) { return name != rb, RUNNING, nil }
			s.runnerStateFn = func(context.Context, Source, string) (runnerRegistration, error) { return runnerIdle, nil }
			s.deleteInZoneFn = func(context.Context, string, string) (bool, error) {
				t.Fatal("assigned RA must not be deleted")
				return false, nil
			}
			require.NoError(t, s.processJob(ctx, src, b))
			require.NoError(t, s.processJob(ctx, src, a))
			require.Empty(t, m.get(jobKey(a)).VMName)
			require.Equal(t, 1, m.fleet.Runners)
			require.NoError(t, m.DeferRunner(ctx, ra, time.Now(), true))
			require.NoError(t, s.reconcileRunners(ctx))
			r, err := m.Runner(ctx, ra)
			require.NoError(t, err)
			require.False(t, r.Available)
			if !terminal {
				require.NoError(t, s.processJob(ctx, src, a))
				require.NotEmpty(t, m.get(jobKey(a)).VMName)
				require.NotEqual(t, ra, m.get(jobKey(a)).VMName)
			}
		})
	}
}

func TestAssignmentCompletionIsMonotonic(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	j.Status, j.RunnerName = "in_progress", "runner-10-0123456789abcdef"
	require.NoError(t, s.observe(ctx, src, j, false))
	require.NoError(t, s.observe(ctx, src, j, true))
	require.NoError(t, s.observe(ctx, src, j, false))
	a, err := m.Assignment(ctx, j.RunnerName)
	require.NoError(t, err)
	require.Equal(t, "completed", a.Job.Status)
}

func TestMissedAssignmentCompletionConsumesGeneration(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	j.Status, j.RunnerName = "in_progress", "runner-10-0123456789abcdef"
	require.NoError(t, s.observe(ctx, src, j, false))
	s.runnerStateFn = func(context.Context, Source, string) (runnerRegistration, error) {
		t.Fatal("REST idle must not override assignment")
		return runnerIdle, nil
	}
	s.jobStatusFn = func(context.Context, Job) (string, error) { return "completed", nil }
	state, err := s.capacityRegistration(ctx, src, j.RunnerName)
	require.NoError(t, err)
	require.Equal(t, runnerGone, state)
	a, err := m.Assignment(ctx, j.RunnerName)
	require.NoError(t, err)
	require.Equal(t, "completed", a.Job.Status)
}

// A re-run attempt replays every carried-over completed job under a new job ID
// but with the runner name from the earlier attempt. That generation belongs to
// the job that actually ran on it, so the replay is ignored rather than failed,
// while a live claim on another job's generation is still a conflict.
func TestRerunReplayDoesNotClaimAnotherJobsGeneration(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	j.Status, j.RunnerName = "in_progress", "runner-10-0123456789abcdef"
	require.NoError(t, s.observe(ctx, src, j, false))
	require.NoError(t, s.observe(ctx, src, j, true))
	replay := j
	replay.Id++
	require.NoError(t, s.observe(ctx, src, replay, true))
	a, err := m.Assignment(ctx, j.RunnerName)
	require.NoError(t, err)
	require.Equal(t, jobKey(j), a.JobKey)
	require.Equal(t, "completed", a.Job.Status)
	live := replay
	live.Id++
	live.Status = "in_progress"
	require.Error(t, s.observe(ctx, src, live, false))
}
