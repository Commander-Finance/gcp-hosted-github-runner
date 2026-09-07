package pkg

import (
	"context"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/stretchr/testify/require"
)

func emulatorStore(t *testing.T) *firestoreStore {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("requires local Firestore emulator")
	}
	client, err := firestore.NewClientWithDatabase(context.Background(), "runner-test-"+nonce(), "github-runners")
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	f := &firestoreStore{client}
	require.NoError(t, f.ensureSchema(context.Background()))
	return f
}
func TestFirestoreCapacityTransferAndVersionContract(t *testing.T) {
	f := emulatorStore(t)
	ctx := context.Background()
	s, _, src, a := lifecycleTestScaler()
	s.store = f
	b := a
	b.Id++
	for _, j := range []Job{a, b} {
		require.NoError(t, s.observe(ctx, src, j, false))
	}
	ak, bk := jobKey(src.Name, a), jobKey(src.Name, b)
	_, err := s.claim(ctx, ak, "a")
	require.NoError(t, err)
	require.NoError(t, s.mutate(ctx, ak, "a", func(r *lifecycleRecord, c *fleetState) error {
		r.VMName = "runner-transfer"
		r.Model = "standard"
		r.CreatedAt = time.Now()
		c.Runners++
		c.Standard++
		return nil
	}))
	require.NoError(t, f.Detach(ctx, ak, "a", true, time.Now()))
	_, err = s.claim(ctx, bk, "b")
	require.NoError(t, err)
	adopted, err := f.Adopt(ctx, bk, "b", poolKey(src.Name, b))
	require.NoError(t, err)
	require.True(t, adopted)
	count, rows, err := f.AuditSnapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count.Runners)
	require.Equal(t, 1, count.Standard)
	require.Len(t, rows, 1)
	require.Equal(t, bk, rows[0].Owner)
	require.NoError(t, f.RememberRunnerID(ctx, "runner-transfer", 123))
	registered, err := f.Runner(ctx, "runner-transfer")
	require.NoError(t, err)
	require.EqualValues(t, 123, registered.Record.RunnerID)
	require.NoError(t, f.ReleaseRunner(ctx, "runner-transfer")) // Attached: must not release.
	require.NoError(t, f.Detach(ctx, bk, "b", false, time.Now()))
	due, err := f.RunnerPage(ctx, time.Now())
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.EqualValues(t, 123, due[0].Record.RunnerID)
	require.NoError(t, f.ReleaseRunner(ctx, "runner-transfer"))
	require.NoError(t, f.ReleaseRunner(ctx, "runner-transfer"))
	count, rows, err = f.AuditSnapshot(ctx)
	require.NoError(t, err)
	require.Zero(t, count.Runners)
	require.Empty(t, rows)
	_, err = f.client.Collection("control").Doc("schema").Set(ctx, map[string]interface{}{"Version": 2})
	require.NoError(t, err)
	require.ErrorIs(t, f.ensureSchema(ctx), errSchema)
}
func TestFirestoreDuePaginationAndJobOnlyTransactions(t *testing.T) {
	f := emulatorStore(t)
	ctx := context.Background()
	s, _, src, j := lifecycleTestScaler()
	s.store = f
	for id := int64(1); id <= 60; id++ {
		job := j
		job.Id = id
		require.NoError(t, s.observe(ctx, src, job, true))
	}
	for id := int64(100); id <= 102; id++ {
		job := j
		job.Id = id
		require.NoError(t, s.observe(ctx, src, job, false))
	}
	first, next, err := f.Page(ctx, "", 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.NotEmpty(t, next)
	second, _, err := f.Page(ctx, next, 2)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.NotEqual(t, first[1].Key, second[0].Key)
	// Job writes work even when the fleet document is not decodable. Only capacity
	// transitions may read this aggregate; this catches accidental global coupling.
	_, err = f.client.Collection("control").Doc("fleet").Set(ctx, map[string]interface{}{"Runners": "invalid"})
	require.NoError(t, err)
	require.NoError(t, f.UpdateJob(ctx, first[0].Key, func(r *lifecycleRecord, _ *fleetState) error { r.NextActionAt = time.Now().Add(time.Hour); return nil }))
	require.Error(t, f.Update(ctx, first[0].Key, func(*lifecycleRecord, *fleetState) error { return nil }))
	_, err = f.client.Collection("jobs").Doc(first[0].Key).Update(ctx, []firestore.Update{{Path: "SchemaVersion", Value: 99}})
	require.NoError(t, err)
	require.ErrorIs(t, f.UpdateJob(ctx, first[0].Key, func(*lifecycleRecord, *fleetState) error { return nil }), errSchema)
}

func TestFirestoreRejectsUnversionedFleet(t *testing.T) {
	f := emulatorStore(t)
	ctx := context.Background()
	_, err := f.client.Collection("control").Doc("schema").Delete(ctx)
	require.NoError(t, err)
	_, err = f.client.Collection("control").Doc("fleet").Set(ctx, fleetState{Runners: 3})
	require.NoError(t, err)
	require.ErrorIs(t, f.ensureSchema(ctx), errSchema)
	_, err = f.client.Collection("control").Doc("schema").Get(ctx)
	require.Error(t, err)
}
