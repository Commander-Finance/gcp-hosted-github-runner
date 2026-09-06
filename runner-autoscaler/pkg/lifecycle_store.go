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
	PendingDelete string
	Template      string
	JITIssuedAt   time.Time
	Operation     string
	AttemptedAt   time.Time
	Job           Job
	Source        string
	Terminal      bool
	Lease         string
	LeaseUntil    time.Time
	VMName        string
	Zone          string
	Model         string
	Machine       string
	JIT           string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ExpiresAt     time.Time `firestore:"expires_at,omitempty"`
}
type fleetState struct {
	Runners  int
	Standard int
}
type lifecycleStore interface {
	Update(context.Context, string, func(*lifecycleRecord, *fleetState) error) error
	Page(context.Context, string, int) ([]storedRecord, string, error)
	Close() error
}
type storedRecord struct {
	Key    string
	Record lifecycleRecord
}
type firestoreStore struct{ client *firestore.Client }

func jobKey(source string, job Job) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", source, job.RepositoryFullName, job.Id))))
}
func (f *firestoreStore) Update(ctx context.Context, key string, change func(*lifecycleRecord, *fleetState) error) error {
	return f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		doc := f.client.Collection("jobs").Doc(key)
		fleet := f.client.Collection("control").Doc("fleet")
		r, counts := lifecycleRecord{}, fleetState{}
		snap, err := tx.Get(doc)
		if err == nil {
			if err = snap.DataTo(&r); err != nil {
				return err
			}
		} else if status.Code(err) != codes.NotFound {
			return err
		}
		snap, err = tx.Get(fleet)
		if err == nil {
			if err = snap.DataTo(&counts); err != nil {
				return err
			}
		} else if status.Code(err) != codes.NotFound {
			return err
		}
		before := counts
		if err = change(&r, &counts); err != nil {
			return err
		}
		if err = tx.Set(doc, r); err != nil {
			return err
		}
		if counts != before {
			return tx.Set(fleet, counts)
		}
		return nil
	})
}
func (f *firestoreStore) Page(ctx context.Context, after string, limit int) ([]storedRecord, string, error) {
	q := f.client.Collection("jobs").OrderBy(firestore.DocumentID, firestore.Asc).Limit(limit)
	if after != "" {
		q = q.StartAfter(after)
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
		rows = append(rows, storedRecord{doc.Ref.ID, r})
	}
	next := ""
	if len(rows) == limit {
		next = rows[len(rows)-1].Key
	}
	return rows, next, nil
}
func (f *firestoreStore) Close() error { return f.client.Close() }
