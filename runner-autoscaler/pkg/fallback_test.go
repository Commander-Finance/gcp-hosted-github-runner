package pkg

import (
	"context"
	"encoding/json"
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
	plan := s.creationPlan("runner-7", nil)

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
	assert.Empty(t, s.creationPlan("runner-7", nil))
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

func TestParseMachineTypeFallbacks(t *testing.T) {

	assert.Empty(t, ParseMachineTypeFallbacks(""), "blank => no fallback (legacy)")
	assert.Empty(t, ParseMachineTypeFallbacks("  ,  ,"), "only separators/space => empty")
	assert.Equal(t,
		[]string{"n4-standard-2", "c4-standard-2", "c4d-standard-2"},
		ParseMachineTypeFallbacks(" n4-standard-2 , c4-standard-2,c4d-standard-2 "),
		"trims whitespace, drops empties, preserves order",
	)
}

// A per-minute API rate-limit must be classified as a rate-limit (→ back off and let
// Cloud Tasks retry), NOT as a capacity error (which would cycle families and amplify
// the very write-request rate that tripped the quota). Resource/capacity quotas must
// still NOT be rate-limits — a different family has different CPU/disk quota, so those
// should keep triggering family fallback.
func TestIsRateLimitError(t *testing.T) {

	rate := fmt.Errorf("googleapi: Error 403: Quota exceeded for quota metric 'Write requests' and limit 'Write requests per minute per region' of service 'compute.googleapis.com'")
	assert.True(t, IsRateLimitError(rate), "per-minute write-rate 403 must be a rate-limit error")
	assert.True(t, IsRateLimitError(fmt.Errorf("rpc error: code = ResourceExhausted desc = RATE_LIMIT_EXCEEDED")))

	assert.False(t, IsRateLimitError(fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED")), "stockout is capacity, not rate-limit")
	assert.False(t, IsRateLimitError(fmt.Errorf("Quota 'N4_CPUS' exceeded. Limit: 24.0 in region us-central1")), "a resource quota should still trigger family fallback")
	assert.False(t, IsRateLimitError(fmt.Errorf("does not have enough resources")))
	assert.False(t, IsRateLimitError(nil))
}

// On a rate-limit, the create must NOT cycle families (that amplifies the write rate);
// it returns after the current attempt so Cloud Tasks backs off and retries.
func TestCreateInstanceRateLimitReturnsWithoutCycling(t *testing.T) {

	s := familyScaler([]string{"z1", "z2", "z3", "z4"}, testFamilies)
	var attempts []creationAttempt
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
		attempts = append(attempts, a)
		return fmt.Errorf("googleapi: Error 403: Quota exceeded for quota metric 'Write requests' and limit 'Write requests per minute per region'")
	}

	err := s.CreateInstanceFromTemplate(context.Background(), "runner-7", nil)
	require.Error(t, err)
	assert.Len(t, attempts, 1, "a rate-limit must return immediately, not cycle all the families")
	assert.True(t, IsRateLimitError(err), "the surfaced error must be the rate-limit (so Cloud Tasks retries)")
}

// familyScaler builds a SPOT-primary scaler with a configured machine-type fallback list.
func familyScaler(zones, fams []string) *Autoscaler {
	return &Autoscaler{conf: AutoscalerConfig{
		Zones:                    zones,
		InstanceTemplate:         "primary",
		FallbackInstanceTemplate: "ondemand", // non-empty => primary is SPOT
		MachineTypeFallbacks:     fams,
	}}
}

var testFamilies = []string{
	"n4-standard-2", "c4-standard-2", "c4d-standard-2", "c4d-highmem-2",
	"n4d-standard-2", "c3d-standard-4", "c3-standard-4",
}

// With a fallback list and no magic label, the plan tries each family once on the SPOT
// (primary) template rotating zones, then the first standardFallbackFamilies on-demand.
func TestCreationPlanFamilyFallbackOrder(t *testing.T) {

	s := familyScaler([]string{"z1", "z2", "z3", "z4"}, testFamilies)
	plan := s.creationPlan("runner-7", nil)
	ordered := s.OrderedZones("runner-7")

	require.Len(t, plan, len(testFamilies)+standardFallbackFamilies) // 7 + 2 = 9

	// SPOT pass: one attempt per family, in list order, rotating zones.
	for i, fam := range testFamilies {
		assert.Equal(t, "spot", plan[i].provisioningModel, "attempt %d", i)
		assert.Equal(t, "primary", plan[i].template, "attempt %d", i)
		assert.Equal(t, fam, plan[i].machineType, "attempt %d", i)
		assert.Equal(t, ordered[i%len(ordered)], plan[i].zone, "attempt %d zone", i)
	}
	// STANDARD pass: first standardFallbackFamilies families on the on-demand template.
	for i := 0; i < standardFallbackFamilies; i++ {
		a := plan[len(testFamilies)+i]
		assert.Equal(t, "standard", a.provisioningModel, "standard attempt %d", i)
		assert.Equal(t, "ondemand", a.template, "standard attempt %d", i)
		assert.Equal(t, testFamilies[i], a.machineType, "standard attempt %d", i)
	}
}

// A magic-label machine type disables family fallback: that exact type is stamped on
// every attempt and the plan keeps the legacy template×zone shape.
func TestCreationPlanMagicLabelDisablesFamilyFallback(t *testing.T) {

	s := familyScaler([]string{"z1", "z2", "z3", "z4"}, testFamilies)
	override := "c2d-standard-16"
	plan := s.creationPlan("runner-7", &override)

	require.Len(t, plan, 8) // legacy 2 templates x 4 zones, NOT the family shape
	for i, a := range plan {
		assert.Equal(t, override, a.machineType, "attempt %d", i)
	}
	for i := 0; i < 4; i++ {
		assert.Equal(t, "spot", plan[i].provisioningModel, "attempt %d", i)
	}
	for i := 4; i < 8; i++ {
		assert.Equal(t, "standard", plan[i].provisioningModel, "attempt %d", i)
	}
}

// An empty fallback list with no magic label is byte-for-byte the legacy plan: the
// template default machine type (empty) on the legacy template×zone shape.
func TestCreationPlanEmptyFallbackListIsLegacy(t *testing.T) {

	s := spotFallbackScaler([]string{"z1", "z2", "z3"}) // no MachineTypeFallbacks
	plan := s.creationPlan("runner-7", nil)

	require.Len(t, plan, 6) // 2 templates x 3 zones
	for i, a := range plan {
		assert.Equal(t, "", a.machineType, "attempt %d must use the template default", i)
	}
}

// When the primary is already on-demand (no fallback template), family fallback still
// works on the primary template with no extra STANDARD pass.
func TestCreationPlanFamilyFallbackNoSpotTemplate(t *testing.T) {

	fams := []string{"n4-standard-2", "c4-standard-2", "c4d-standard-2"}
	s := &Autoscaler{conf: AutoscalerConfig{
		Zones:                []string{"z1", "z2"},
		InstanceTemplate:     "primary",
		MachineTypeFallbacks: fams, // no FallbackInstanceTemplate => primary is on-demand
	}}
	plan := s.creationPlan("runner-7", nil)

	require.Len(t, plan, len(fams)) // no STANDARD pass appended
	for i, fam := range fams {
		assert.Equal(t, "standard", plan[i].provisioningModel, "attempt %d", i)
		assert.Equal(t, "primary", plan[i].template, "attempt %d", i)
		assert.Equal(t, fam, plan[i].machineType, "attempt %d", i)
	}
}

// Deadline-budget guard. The create-vm callback runs under autoscaler_timeout (240s in
// prod). At ~12s per capacity-failed Insert and ~40s for a success, the safe budget is
// roughly (240-40)/12 ≈ 16 attempts. Keep the worst-case plan well under that so a
// near-deadline success still acks before Cloud Tasks retries and spawns a duplicate VM.
// If a future family-list/timeout change breaks this, revisit the budget deliberately.
func TestCreationPlanFamilyFallbackBoundsAttemptCount(t *testing.T) {

	const maxCreateAttemptsBudget = 12
	s := familyScaler([]string{"z1", "z2", "z3", "z4"}, testFamilies)
	plan := s.creationPlan("runner-7", nil)

	assert.LessOrEqual(t, len(plan), len(testFamilies)+standardFallbackFamilies, "no attempt explosion (e.g. reverting to family×zone)")
	assert.Less(t, len(plan), maxCreateAttemptsBudget, "plan must fit the deadline budget")
}

// Family fallback stops at the first successful family and never reaches the STANDARD pass.
func TestCreateInstanceFamilyFallbackStopsAtFirstSuccess(t *testing.T) {

	fams := []string{"f1", "f2", "f3", "f4", "f5"}
	s := familyScaler([]string{"z1", "z2", "z3", "z4"}, fams)
	var attempts []creationAttempt
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
		attempts = append(attempts, a)
		if len(attempts) < 4 {
			return fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED")
		}
		return nil // the 4th family succeeds
	}

	require.NoError(t, s.CreateInstanceFromTemplate(context.Background(), "runner-7", nil))
	require.Len(t, attempts, 4)
	assert.Equal(t, "f4", attempts[3].machineType)
	for i, a := range attempts {
		assert.Equal(t, "spot", a.provisioningModel, "attempt %d should be SPOT", i)
	}
}

// When every family is capacity-exhausted, all 9 attempts run and a capacity error is returned.
func TestCreateInstanceFamilyFallbackExhaustionReturnsCapacityError(t *testing.T) {

	s := familyScaler([]string{"z1", "z2", "z3", "z4"}, testFamilies)
	var attempts []creationAttempt
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
		attempts = append(attempts, a)
		return fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED")
	}

	err := s.CreateInstanceFromTemplate(context.Background(), "runner-7", nil)
	require.Error(t, err)
	assert.True(t, IsCapacityError(err), "must surface a capacity error after exhausting all fallbacks")
	assert.Len(t, attempts, len(testFamilies)+standardFallbackFamilies) // 7 spot + 2 standard
}

// An invalid/typo'd family yields a non-capacity error, which is fatal: the walk aborts
// immediately rather than silently limping along on a later family.
func TestCreateInstanceFamilyFallbackInvalidTypeIsFatal(t *testing.T) {

	s := familyScaler([]string{"z1", "z2"}, []string{"bogus-type", "f2"})
	var attempts []creationAttempt
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
		attempts = append(attempts, a)
		return fmt.Errorf("Invalid value for field 'resource.machineType': 'bogus-type'")
	}

	err := s.CreateInstanceFromTemplate(context.Background(), "runner-7", nil)
	require.Error(t, err)
	assert.False(t, IsCapacityError(err), "invalid machine types must stay non-capacity fatal errors")
	assert.Contains(t, err.Error(), "resource.machineType")
	assert.Len(t, attempts, 1, "a non-capacity error must abort without trying the next family")
}

// A duplicated fallback entry must not waste a creation attempt: the plan is one attempt
// per DISTINCT family (and the capped on-demand pass still reaches a later distinct one).
func TestCreationPlanFamilyFallbackDedupes(t *testing.T) {

	s := familyScaler([]string{"z1", "z2", "z3", "z4"}, []string{"n4-standard-2", "n4-standard-2", "c4-standard-2"})
	plan := s.creationPlan("runner-7", nil)

	// 2 distinct SPOT attempts + 2 on-demand (clamped to the 2 distinct families).
	require.Len(t, plan, 4)
	assert.Equal(t, "n4-standard-2", plan[0].machineType)
	assert.Equal(t, "c4-standard-2", plan[1].machineType)
	assert.Equal(t, "spot", plan[1].provisioningModel)
	assert.Equal(t, "standard", plan[2].provisioningModel)
	assert.Equal(t, "n4-standard-2", plan[2].machineType)
	assert.Equal(t, "c4-standard-2", plan[3].machineType)
}

// With a single-family fallback list and a SPOT template, the STANDARD pass is clamped
// to the family count (1) rather than standardFallbackFamilies — guards the index clamp.
func TestCreationPlanFamilyFallbackClampsStandardPassToFamilyCount(t *testing.T) {

	s := familyScaler([]string{"z1", "z2"}, []string{"only-fam"})
	plan := s.creationPlan("runner-7", nil)

	require.Len(t, plan, 2) // 1 SPOT + 1 (clamped) STANDARD
	assert.Equal(t, "spot", plan[0].provisioningModel)
	assert.Equal(t, "only-fam", plan[0].machineType)
	assert.Equal(t, "standard", plan[1].provisioningModel)
	assert.Equal(t, "ondemand", plan[1].template)
	assert.Equal(t, "only-fam", plan[1].machineType)
}

// End-to-end: a magic-label machine type passed to CreateInstanceFromTemplate must reach
// the Insert (the attempt the seam receives), proving the override travels through the
// plan and is not dropped by the refactor — even when a fallback list is also configured.
func TestCreateInstanceMagicLabelStampsMachineTypeOnInsert(t *testing.T) {

	s := familyScaler([]string{"z1", "z2", "z3"}, testFamilies) // fallback list present...
	var attempts []creationAttempt
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
		attempts = append(attempts, a)
		return nil
	}

	override := "c2d-standard-16"
	require.NoError(t, s.CreateInstanceFromTemplate(context.Background(), "runner-7", &override))
	// ...but the magic label wins: a single attempt stamped with the requested type.
	require.Len(t, attempts, 1)
	assert.Equal(t, override, attempts[0].machineType)
}

func TestCreationPlanStandardOnlyWhenNotPreemptible(t *testing.T) {

	// No fallback template => the primary is already on-demand; SPOT must never appear.
	s := &Autoscaler{conf: AutoscalerConfig{
		Zones:            []string{"z1", "z2"},
		InstanceTemplate: "primary",
	}}
	plan := s.creationPlan("runner-7", nil)

	require.Len(t, plan, 2)
	for _, a := range plan {
		assert.Equal(t, "standard", a.provisioningModel)
		assert.Equal(t, "primary", a.template)
	}
}

func TestCreateInstanceUsesSpotWhenAvailable(t *testing.T) {

	s := spotFallbackScaler([]string{"z1", "z2", "z3"})
	var attempts []creationAttempt
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
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
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
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
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
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
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
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
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
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
	s.tryInsertFn = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
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

// RepositoryFullName must survive a JSON marshal/unmarshal round-trip and must
// be keyed "repository_full_name" in the wire format. The recreate-vm flow
// embeds the whole Job struct in VM instance metadata as JSON, and the recreate
// callback handler unmarshals it back — if the field were dropped or renamed the
// handler would reconstruct a Job that looks valid (non-zero Id) but lacks the
// repo context needed for JIT-config generation.
func TestJobRepositoryFullNameSurvivesJsonRoundTrip(t *testing.T) {

	original := Job{Id: 42, RepositoryFullName: "owner/repo"}
	data, err := json.Marshal(original)
	require.NoError(t, err)

	// Wire format must use the snake_case JSON tag, not the Go field name.
	assert.Contains(t, string(data), `"repository_full_name"`, "JSON must use the repository_full_name key")

	var result Job
	require.NoError(t, json.Unmarshal(data, &result))
	assert.Equal(t, "owner/repo", result.RepositoryFullName, "RepositoryFullName must survive the round-trip")
	assert.Equal(t, int64(42), result.Id, "Id must survive the round-trip alongside RepositoryFullName")
}
