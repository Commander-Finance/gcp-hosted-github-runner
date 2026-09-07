package pkg

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const stateVersion = 1

var errSchema = errors.New("unsupported durable state version; migration required")

type runnerRecord struct {
	SchemaVersion int
	Name          string
	Owner         string // A capacity claim, never an assertion of GitHub job assignment.
	Pool          string
	Available     bool
	NextActionAt  time.Time
	Record        lifecycleRecord
}

func checkRecordVersion(r *lifecycleRecord, exists bool) error {
	if exists && r.SchemaVersion != stateVersion {
		return errSchema
	}
	r.SchemaVersion = stateVersion
	return nil
}
func prepareRecord(r *lifecycleRecord) {
	r.NeedsReconcile = (r.Failure == "" && !r.Terminal) || r.VMName != "" || r.PendingDelete != ""
	if r.NeedsReconcile {
		r.ExpiresAt = time.Time{}
		if r.NextActionAt.IsZero() {
			r.NextActionAt = time.Now()
		}
		if r.EnqueuedUntil.After(r.NextActionAt) {
			r.NextActionAt = r.EnqueuedUntil
		}
	} else if r.Terminal && r.VMName == "" && r.PendingDelete == "" {
		if r.ExpiresAt.IsZero() {
			r.ExpiresAt = time.Now().Add(7 * 24 * time.Hour)
		}
	}
}
func (f *firestoreStore) DeferRunner(ctx context.Context, name string, due time.Time) error {
	return f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		ref := f.client.Collection("runners").Doc(name)
		doc, err := tx.Get(ref)
		if status.Code(err) == codes.NotFound {
			return nil
		}
		if err != nil {
			return err
		}
		var r runnerRecord
		if err = doc.DataTo(&r); err != nil {
			return err
		}
		if r.SchemaVersion != stateVersion {
			return errSchema
		}
		if r.Owner != "" {
			return nil
		}
		r.NextActionAt = due
		return tx.Set(ref, r)
	})
}

// Exact label sets and repository scope are deliberately conservative: capacity
// is shared only where both repository eligibility and requested labels agree.
func poolKey(source string, job Job) string {
	labels := append([]string(nil), job.Labels...)
	for i := range labels {
		labels[i] = strings.ToLower(labels[i])
	}
	sort.Strings(labels)
	return source + ":" + job.RepositoryFullName + ":" + strings.Join(labels, ",")
}

type dueCursor struct {
	Key string
	At  time.Time
}

func encodeCursor(key string, at time.Time) string {
	b, _ := json.Marshal(dueCursor{key, at})
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeCursor(s string) (dueCursor, error) {
	var c dueCursor
	b, e := base64.RawURLEncoding.DecodeString(s)
	if e == nil {
		e = json.Unmarshal(b, &c)
	}
	return c, e
}

func (f *firestoreStore) ensureSchema(ctx context.Context) error {
	return f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		ref := f.client.Collection("control").Doc("schema")
		doc, err := tx.Get(ref)
		if err == nil {
			var v struct{ Version int }
			if err = doc.DataTo(&v); err != nil {
				return err
			}
			if v.Version != stateVersion {
				return errSchema
			}
			return nil
		}
		if status.Code(err) != codes.NotFound {
			return err
		}
		// Never silently reinterpret pre-versioned state or overwrite a newer schema.
		for _, collection := range []string{"jobs", "runners"} {
			it := tx.Documents(f.client.Collection(collection).Limit(1))
			_, err = it.Next()
			it.Stop()
			if err != iterator.Done {
				if err == nil {
					return errSchema
				}
				return err
			}
		}
		return tx.Set(ref, map[string]interface{}{"Version": stateVersion})
	})
}

func (f *firestoreStore) Detach(ctx context.Context, key, token string, available bool, due time.Time) error {
	return f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		ref := f.client.Collection("jobs").Doc(key)
		doc, err := tx.Get(ref)
		if err != nil {
			return err
		}
		var r lifecycleRecord
		if err = doc.DataTo(&r); err != nil {
			return err
		}
		if err = checkRecordVersion(&r, true); err != nil {
			return err
		}
		if r.Lease != token || time.Now().After(r.LeaseUntil) {
			return errLeaseBusy
		}
		if r.VMName == "" {
			return nil
		}
		rr := runnerRecord{SchemaVersion: stateVersion, Name: r.VMName, Pool: poolKey(r.Source, r.Job), Available: available, NextActionAt: due, Record: r}
		if err = tx.Set(f.client.Collection("runners").Doc(r.VMName), rr); err != nil {
			return err
		}
		// The runner ledger retains the reservation; detaching is not a release.
		discard := fleetState{}
		releaseReservation(&r, &discard)
		r.NextActionAt = time.Now().Add(30 * time.Second)
		prepareRecord(&r)
		return tx.Set(ref, r)
	})
}
func (f *firestoreStore) Adopt(ctx context.Context, key, token, pool string) (bool, error) {
	adopted := false
	err := f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		adopted = false
		ref := f.client.Collection("jobs").Doc(key)
		doc, err := tx.Get(ref)
		if err != nil {
			return err
		}
		var r lifecycleRecord
		if err = doc.DataTo(&r); err != nil {
			return err
		}
		if err = checkRecordVersion(&r, true); err != nil {
			return err
		}
		if r.Lease != token || time.Now().After(r.LeaseUntil) {
			return errLeaseBusy
		}
		if r.Terminal || r.VMName != "" {
			return nil
		}
		it := tx.Documents(f.client.Collection("runners").Where("Available", "==", true).Where("Pool", "==", pool).Limit(1))
		defer it.Stop()
		candidate, err := it.Next()
		if err == iterator.Done {
			return nil
		}
		if err != nil {
			return err
		}
		var rr runnerRecord
		if err = candidate.DataTo(&rr); err != nil {
			return err
		}
		if rr.SchemaVersion != stateVersion {
			return errSchema
		}
		job, source, lease, until, seen, dispatch, enqueued := r.Job, r.Source, r.Lease, r.LeaseUntil, r.UpdatedAt, r.EnqueueToken, r.EnqueuedUntil
		r = rr.Record
		r.Job = job
		r.Source = source
		r.Lease = lease
		r.LeaseUntil = until
		r.UpdatedAt = seen
		r.Terminal = false
		r.PendingDelete = ""
		r.EnqueueToken = dispatch
		r.EnqueuedUntil = enqueued
		r.Failure = ""
		r.NextActionAt = time.Now()
		prepareRecord(&r)
		rr.Owner = key
		rr.Available = false
		rr.Record = r
		if err = tx.Set(candidate.Ref, rr); err != nil {
			return err
		}
		if err = tx.Set(ref, r); err != nil {
			return err
		}
		adopted = true
		return nil
	})
	return adopted, err
}
func (f *firestoreStore) Runner(ctx context.Context, name string) (runnerRecord, error) {
	var r runnerRecord
	doc, err := f.client.Collection("runners").Doc(name).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	err = doc.DataTo(&r)
	if err == nil && r.SchemaVersion != stateVersion {
		err = errSchema
	}
	return r, err
}
func (f *firestoreStore) RememberRunnerID(ctx context.Context, name string, id int64) error {
	return f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		ref := f.client.Collection("runners").Doc(name)
		doc, err := tx.Get(ref)
		if status.Code(err) == codes.NotFound {
			return nil
		}
		if err != nil {
			return err
		}
		var rr runnerRecord
		if err = doc.DataTo(&rr); err != nil {
			return err
		}
		if rr.SchemaVersion != stateVersion {
			return errSchema
		}
		var owner *firestore.DocumentRef
		var r lifecycleRecord
		if rr.Owner != "" {
			owner = f.client.Collection("jobs").Doc(rr.Owner)
			doc, err = tx.Get(owner)
			if err != nil {
				return err
			}
			if err = doc.DataTo(&r); err != nil {
				return err
			}
			if err = checkRecordVersion(&r, true); err != nil {
				return err
			}
			if r.VMName != name {
				return fmt.Errorf("runner owner disagrees with generation")
			}
		}
		rr.Record.RunnerID = id
		if owner != nil {
			r.RunnerID = id
			if err = tx.Set(owner, r); err != nil {
				return err
			}
		}
		return tx.Set(ref, rr)
	})
}

// Called only after confirmed deletion/absence. Attached reservations are released
// by their owner, whose operation-recovery state must not be bypassed.
func (f *firestoreStore) ReleaseRunner(ctx context.Context, name string) error {
	return f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		ref := f.client.Collection("runners").Doc(name)
		doc, err := tx.Get(ref)
		if status.Code(err) == codes.NotFound {
			return nil
		}
		if err != nil {
			return err
		}
		var rr runnerRecord
		if err = doc.DataTo(&rr); err != nil {
			return err
		}
		if rr.SchemaVersion != stateVersion {
			return errSchema
		}
		if rr.Owner != "" {
			return nil
		}
		countRef := f.client.Collection("control").Doc("fleet")
		doc, err = tx.Get(countRef)
		if err != nil {
			return err
		}
		var count fleetState
		if err = doc.DataTo(&count); err != nil {
			return err
		}
		count.Runners--
		if rr.Record.Model == "standard" {
			count.Standard--
		}
		count.Revision++
		if err = tx.Delete(ref); err != nil {
			return err
		}
		return tx.Set(countRef, count)
	})
}
func (f *firestoreStore) RunnerPage(ctx context.Context, due time.Time) ([]runnerRecord, error) {
	it := f.client.Collection("runners").Where("Owner", "==", "").Where("NextActionAt", "<=", due).Limit(100).Documents(ctx)
	defer it.Stop()
	var out []runnerRecord
	for {
		doc, err := it.Next()
		if err == iterator.Done {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var r runnerRecord
		if err = doc.DataTo(&r); err != nil {
			return nil, err
		}
		if r.SchemaVersion != stateVersion {
			return nil, errSchema
		}
		out = append(out, r)
	}
}
func (f *firestoreStore) AuditSnapshot(ctx context.Context) (fleetState, []runnerRecord, error) {
	// Read-only transaction gives a consistent ledger/counter snapshot. No tombstones.
	var counts fleetState
	var rows []runnerRecord
	err := f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		rows = nil
		doc, err := tx.Get(f.client.Collection("control").Doc("fleet"))
		if err == nil {
			err = doc.DataTo(&counts)
		} else if status.Code(err) == codes.NotFound {
			counts = fleetState{}
			err = nil
		}
		if err != nil {
			return err
		}
		it := tx.Documents(f.client.Collection("runners"))
		defer it.Stop()
		for {
			doc, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			var r runnerRecord
			if err = doc.DataTo(&r); err != nil {
				return err
			}
			if r.SchemaVersion != stateVersion {
				return errSchema
			}
			rows = append(rows, r)
			if len(rows) > 2000 {
				return fmt.Errorf("runner ledger exceeds audit bound")
			}
		}
	}, firestore.ReadOnly)
	return counts, rows, err
}
func (f *firestoreStore) Backoff(ctx context.Context, key string, until time.Time) (time.Time, error) {
	var result time.Time
	err := f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		ref := f.client.Collection("backoff").Doc(key)
		doc, err := tx.Get(ref)
		var r struct{ Until time.Time }
		if err == nil {
			err = doc.DataTo(&r)
		} else if status.Code(err) == codes.NotFound {
			err = nil
		}
		if err != nil {
			return err
		}
		if until.After(r.Until) {
			r.Until = until
			if err = tx.Set(ref, r); err != nil {
				return err
			}
		}
		result = r.Until
		return nil
	})
	return result, err
}
