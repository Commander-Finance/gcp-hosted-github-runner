package pkg

import (
	"context"
	"fmt"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These white-box tests pin the SPOT-vs-on-demand behaviour: SPOT must be attempted
// in every configured zone before any on-demand (STANDARD) fallback, the fallback is
// reached only on capacity errors, and a non-capacity error aborts without falling
// back.

func spotFallbackScaler(zones []string) *Autoscaler {
	return &Autoscaler{conf: AutoscalerConfig{
		Zones:                    zones,
		InstanceTemplate:         "primary",
		FallbackInstanceTemplate: "ondemand", // non-empty => primary is SPOT
	}}
}

func TestCreationPlanSpotFirstThenStandard(t *testing.T) {

	s := spotFallbackScaler([]string{"z1", "z2", "z3"})
	plan := s.creationPlan("runner-7")

	require.Len(t, plan, 6) // 2 templates x 3 zones
	// All SPOT/primary attempts come first, then all STANDARD/ondemand attempts.
	for i := 0; i < 3; i++ {
		assert.Equal(t, "spot", plan[i].provisioningModel, "attempt %d", i)
		assert.Equal(t, "primary", plan[i].template, "attempt %d", i)
	}
	for i := 3; i < 6; i++ {
		assert.Equal(t, "standard", plan[i].provisioningModel, "attempt %d", i)
		assert.Equal(t, "ondemand", plan[i].template, "attempt %d", i)
	}
	// Both the SPOT block and the STANDARD block cover every configured zone.
	spotZones := map[string]bool{}
	standardZones := map[string]bool{}
	for i := 0; i < 3; i++ {
		spotZones[plan[i].zone] = true
		standardZones[plan[i+3].zone] = true
	}
	all := map[string]bool{"z1": true, "z2": true, "z3": true}
	assert.Equal(t, all, spotZones)
	assert.Equal(t, all, standardZones)
}

func TestCreationPlanEmptyWhenNoZones(t *testing.T) {

	s := &Autoscaler{conf: AutoscalerConfig{InstanceTemplate: "primary", FallbackInstanceTemplate: "ondemand"}}
	assert.Empty(t, s.creationPlan("runner-7"))
}

func TestIsStoppedPredicate(t *testing.T) {

	for _, st := range []State{STOPPING, TERMINATED, SUSPENDING, SUSPENDED} {
		assert.True(t, st.isStopped(), "%s should be reclaimable", st)
	}
	// Never reclaim a VM that may be running (or about to run) a job.
	for _, st := range []State{PROVISIONING, STAGING, RUNNING, REPAIRING, Unknown} {
		assert.False(t, st.isStopped(), "%s must NOT be reclaimable", st)
	}
}

func TestDecideCreate(t *testing.T) {

	// No existing VM -> create.
	assert.Equal(t, createProceed, decideCreate(false, Unknown))

	// A live VM (running or coming up) is left alone so we never duplicate or
	// disturb a runner that may still take the job.
	for _, st := range []State{PROVISIONING, STAGING, RUNNING, REPAIRING} {
		assert.Equal(t, createSkip, decideCreate(true, st), "state %s should be left alone", st)
	}

	// A stopped leftover must be replaced, otherwise the queued job is stranded.
	for _, st := range []State{STOPPING, SUSPENDING, SUSPENDED, TERMINATED} {
		assert.Equal(t, createReplace, decideCreate(true, st), "state %s should be replaced", st)
	}
}

func TestCreationPlanStandardOnlyWhenNotPreemptible(t *testing.T) {

	// No fallback template => the primary is already on-demand; SPOT must never appear.
	s := &Autoscaler{conf: AutoscalerConfig{
		Zones:            []string{"z1", "z2"},
		InstanceTemplate: "primary",
	}}
	plan := s.creationPlan("runner-7")

	require.Len(t, plan, 2)
	for _, a := range plan {
		assert.Equal(t, "standard", a.provisioningModel)
		assert.Equal(t, "primary", a.template)
	}
}

func TestCreateInstanceUsesSpotWhenAvailable(t *testing.T) {

	s := spotFallbackScaler([]string{"z1", "z2", "z3"})
	var attempts []creationAttempt
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, mt *string, md []*computepb.Items) error {
		attempts = append(attempts, a)
		return nil // first (SPOT) attempt succeeds
	}

	require.NoError(t, s.CreateInstanceFromTemplate(context.Background(), "runner-7", nil))
	// Stops at the first successful SPOT attempt - no on-demand fallback.
	require.Len(t, attempts, 1)
	assert.Equal(t, "spot", attempts[0].provisioningModel)
}

func TestCreateInstanceFallsBackToStandardOnlyAfterAllSpotZones(t *testing.T) {

	s := spotFallbackScaler([]string{"z1", "z2", "z3"})
	var attempts []creationAttempt
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, mt *string, md []*computepb.Items) error {
		attempts = append(attempts, a)
		if a.provisioningModel == "spot" {
			return fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED") // SPOT exhausted everywhere
		}
		return nil // first STANDARD attempt succeeds
	}

	require.NoError(t, s.CreateInstanceFromTemplate(context.Background(), "runner-7", nil))
	// All three SPOT zones tried first, then exactly one STANDARD attempt (which won).
	require.Len(t, attempts, 4)
	for i := 0; i < 3; i++ {
		assert.Equal(t, "spot", attempts[i].provisioningModel, "attempt %d should still be SPOT", i)
	}
	assert.Equal(t, "standard", attempts[3].provisioningModel)
	assert.Equal(t, "ondemand", attempts[3].template)
}

func TestCreateInstanceAbortsOnNonCapacityErrorWithoutFallback(t *testing.T) {

	s := spotFallbackScaler([]string{"z1", "z2", "z3"})
	var attempts []creationAttempt
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, mt *string, md []*computepb.Items) error {
		attempts = append(attempts, a)
		return fmt.Errorf("PERMISSION_DENIED: caller lacks compute.instances.create")
	}

	err := s.CreateInstanceFromTemplate(context.Background(), "runner-7", nil)
	require.Error(t, err)
	// A non-capacity error is not retryable: abort immediately, no other zone, no
	// on-demand fallback.
	assert.Len(t, attempts, 1)
	assert.Equal(t, "spot", attempts[0].provisioningModel)
}

func TestCreateInstanceReturnsCapacityErrorWhenAllExhausted(t *testing.T) {

	s := spotFallbackScaler([]string{"z1", "z2", "z3"})
	var attempts []creationAttempt
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, mt *string, md []*computepb.Items) error {
		attempts = append(attempts, a)
		return fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED")
	}

	err := s.CreateInstanceFromTemplate(context.Background(), "runner-7", nil)
	require.Error(t, err)
	// Every SPOT zone and every STANDARD zone was tried (3 + 3) before giving up.
	assert.Len(t, attempts, 6)
}

func TestCreateInstanceTreatsAlreadyExistsAsSuccess(t *testing.T) {

	s := spotFallbackScaler([]string{"z1", "z2", "z3"})
	var attempts []creationAttempt
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, mt *string, md []*computepb.Items) error {
		attempts = append(attempts, a)
		return fmt.Errorf("The resource 'runner-7' already exists")
	}

	// A retried create that finds the VM already present is idempotent success - it
	// must NOT keep trying other zones/models.
	require.NoError(t, s.CreateInstanceFromTemplate(context.Background(), "runner-7", nil))
	assert.Len(t, attempts, 1)
}

func TestCreateInstanceFailsWithNoZones(t *testing.T) {

	s := &Autoscaler{conf: AutoscalerConfig{InstanceTemplate: "primary"}}
	called := false
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, mt *string, md []*computepb.Items) error {
		called = true
		return nil
	}

	err := s.CreateInstanceFromTemplate(context.Background(), "runner-7", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no zones configured")
	assert.False(t, called, "no creation attempt should run when there are no zones")
}

// DeleteInstance zone-iteration behaviour, exercised through the deleteInZoneFn seam.

func deleteSeamScaler(zones []string, fn func(zone string, call int) (bool, error)) *Autoscaler {
	s := &Autoscaler{conf: AutoscalerConfig{Zones: zones}}
	call := 0
	s.deleteInZoneFn = func(ctx context.Context, name string, zone string) (bool, error) {
		call++
		return fn(zone, call)
	}
	return s
}

func TestDeleteInstanceStopsAtFirstSuccess(t *testing.T) {

	calls := 0
	s := deleteSeamScaler([]string{"z1", "z2", "z3"}, func(zone string, call int) (bool, error) {
		calls = call
		return true, nil // found+deleted in the very first zone
	})
	require.NoError(t, s.DeleteInstance(context.Background(), "runner-7"))
	assert.Equal(t, 1, calls, "should stop at the first zone where the VM is found")
}

func TestDeleteInstanceContinuesPastNotFoundZones(t *testing.T) {

	calls := 0
	s := deleteSeamScaler([]string{"z1", "z2", "z3"}, func(zone string, call int) (bool, error) {
		calls = call
		if call < 3 {
			return false, nil // not in this zone
		}
		return true, nil // found in the third zone
	})
	require.NoError(t, s.DeleteInstance(context.Background(), "runner-7"))
	assert.Equal(t, 3, calls)
}

func TestDeleteInstanceWaitFailureIsNotSwallowed(t *testing.T) {

	calls := 0
	s := deleteSeamScaler([]string{"z1", "z2", "z3"}, func(zone string, call int) (bool, error) {
		calls = call
		return true, fmt.Errorf("operation failed") // was here, deletion failed
	})
	// The instance was found, so a deletion failure must surface (not be treated as
	// a false success that acks the task and leaks the VM).
	require.Error(t, s.DeleteInstance(context.Background(), "runner-7"))
	assert.Equal(t, 1, calls, "must not fall through to other zones once the VM is found")
}

func TestDeleteInstanceAllNotFoundIsIdempotent(t *testing.T) {

	s := deleteSeamScaler([]string{"z1", "z2", "z3"}, func(zone string, call int) (bool, error) {
		return false, nil // not in any zone
	})
	assert.NoError(t, s.DeleteInstance(context.Background(), "runner-7"))
}

func TestDeleteInstanceReturnsErrorWhenNoZoneSucceeds(t *testing.T) {

	s := deleteSeamScaler([]string{"z1", "z2", "z3"}, func(zone string, call int) (bool, error) {
		return false, fmt.Errorf("transient API error") // unknown error, not 404
	})
	require.Error(t, s.DeleteInstance(context.Background(), "runner-7"))
}

func TestCallbackTaskNameDeleteTargetsCreateTask(t *testing.T) {

	// DeleteCallbackTask cancels the *create* callback; this pins the exact name it
	// builds so the create/delete namespacing can't silently drift.
	queue := "projects/p/locations/r/queues/q"
	assert.Equal(t, queue+"/tasks/create-42-0", CallbackTaskName(queue, TaskKindCreate, 42, 0))
}

func TestAutoscalerConfigValidateRejectsEmptyRunnerPrefix(t *testing.T) {

	// An empty RunnerPrefix is a misconfiguration: InstanceName would produce
	// "-<jobId>" (an invalid GCP instance name) and the delete guard would match
	// nothing it created. Validate must fail fast at startup rather than let the
	// autoscaler run in a broken state.
	require.Error(t, AutoscalerConfig{RunnerPrefix: ""}.Validate())
	require.NoError(t, AutoscalerConfig{RunnerPrefix: "runner"}.Validate())
}
