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
	// SPOT covers every configured zone.
	spotZones := map[string]bool{}
	for i := 0; i < 3; i++ {
		spotZones[plan[i].zone] = true
	}
	assert.Equal(t, map[string]bool{"z1": true, "z2": true, "z3": true}, spotZones)
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
