package pkg

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Records outlive both the originating VM and Cloud Tasks retry windows. The
// source key refers to configuration, never a copy of its webhook secret.
type lifecycleRecord struct {
	SchemaVersion  int
	RunnerID       int64
	NextActionAt   time.Time
	NeedsReconcile bool
	EnqueuedUntil  time.Time
	EnqueueToken   string
	Failure        string
	PendingDelete  string
	Template       string
	JITIssuedAt    time.Time
	Operation      string
	AttemptedAt    time.Time
	Job            Job
	Source         string
	Terminal       bool
	Lease          string
	LeaseUntil     time.Time
	VMName         string
	Zone           string
	Model          string
	Machine        string
	JIT            string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ExpiresAt      time.Time `firestore:"expires_at,omitempty"`
}
type fleetState struct {
	Runners  int
	Standard int
	Revision int64
}
type lifecycleStore interface {
	Update(context.Context, string, func(*lifecycleRecord, *fleetState) error) error
	UpdateJob(context.Context, string, func(*lifecycleRecord, *fleetState) error) error
	Detach(context.Context, string, string, bool, time.Time) error
	Adopt(context.Context, string, string, string) (bool, error)
	Runner(context.Context, string) (runnerRecord, error)
	RecordAssignment(context.Context, string, string, Job) error
	Assignment(context.Context, string) (runnerAssignment, error)
	RememberRunnerID(context.Context, string, int64) error
	ReleaseRunner(context.Context, string) error
	DeferRunner(context.Context, string, time.Time, bool) error
	RunnerPage(context.Context, time.Time) ([]runnerRecord, error)
	AuditSnapshot(context.Context) (fleetState, []runnerRecord, error)
	Backoff(context.Context, string, time.Time) (time.Time, error)
	Page(context.Context, string, int) ([]storedRecord, string, error)
	Close() error
}
type storedRecord struct {
	Key    string
	Record lifecycleRecord
}
type firestoreStore struct{ client *firestore.Client }

// jobKey identifies demand by the GitHub job alone. Overlapping webhook scopes
// (an organization webhook plus a repository webhook) deliver the same job
// under different sources and must converge on one record.
func jobKey(job Job) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d", job.RepositoryFullName, job.Id))))
}
func (f *firestoreStore) Update(ctx context.Context, key string, change func(*lifecycleRecord, *fleetState) error) error {
	return f.update(ctx, key, true, change)
}
func (f *firestoreStore) UpdateJob(ctx context.Context, key string, change func(*lifecycleRecord, *fleetState) error) error {
	return f.update(ctx, key, false, change)
}
func (f *firestoreStore) update(ctx context.Context, key string, capacity bool, change func(*lifecycleRecord, *fleetState) error) error {
	return f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		doc := f.client.Collection("jobs").Doc(key)
		fleet := f.client.Collection("control").Doc("fleet")
		r, counts := lifecycleRecord{}, fleetState{}
		snap, err := tx.Get(doc)
		recordExists := err == nil
		if err == nil {
			if err = snap.DataTo(&r); err != nil {
				return err
			}
		} else if status.Code(err) != codes.NotFound {
			return err
		}
		if err = checkRecordVersion(&r, recordExists); err != nil {
			return err
		}
		oldName := r.VMName
		if capacity {
			snap, err = tx.Get(fleet)
			if err == nil {
				if err = snap.DataTo(&counts); err != nil {
					return err
				}
			} else if status.Code(err) != codes.NotFound {
				return err
			}
		}
		before := counts
		if err = change(&r, &counts); err != nil {
			return err
		}
		prepareRecord(&r)
		if err = tx.Set(doc, r); err != nil {
			return err
		}
		if capacity && oldName != "" && oldName != r.VMName {
			if err = tx.Delete(f.client.Collection("runners").Doc(oldName)); err != nil {
				return err
			}
		}
		if r.VMName != "" {
			if err = tx.Set(f.client.Collection("runners").Doc(r.VMName), runnerRecord{SchemaVersion: stateVersion, Name: r.VMName, Owner: key, Pool: poolKey(r.Source, r.Job), Record: r}); err != nil {
				return err
			}
		}
		if counts != before {
			if !capacity {
				return fmt.Errorf("job-only update changed capacity")
			}
			counts.Revision++
			return tx.Set(fleet, counts)
		}
		return nil
	})
}
func (f *firestoreStore) Page(ctx context.Context, after string, limit int) ([]storedRecord, string, error) {
	q := f.client.Collection("jobs").Where("NeedsReconcile", "==", true).Where("NextActionAt", "<=", time.Now()).OrderBy("NextActionAt", firestore.Asc).OrderBy(firestore.DocumentID, firestore.Asc).Limit(limit)
	if after != "" {
		cursor, err := decodeCursor(after)
		if err != nil {
			return nil, "", err
		}
		q = q.StartAfter(cursor.At, cursor.Key)
	}
	it := q.Documents(ctx)
	defer it.Stop()
	rows := []storedRecord{}
	for {
		doc, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, "", err
		}
		var r lifecycleRecord
		if err = doc.DataTo(&r); err != nil {
			return nil, "", err
		}
		if err = checkRecordVersion(&r, true); err != nil {
			return nil, "", err
		}
		rows = append(rows, storedRecord{doc.Ref.ID, r})
	}
	next := ""
	if len(rows) == limit {
		last := rows[len(rows)-1]
		next = encodeCursor(last.Key, last.Record.NextActionAt)
	}
	return rows, next, nil
}
func (f *firestoreStore) Close() error { return f.client.Close() }
