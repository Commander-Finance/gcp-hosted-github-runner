package pkg

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// An observation subdocument belongs to the immutable runner generation. It is
// independent of capacity snapshots, so owner updates/adoption cannot erase it.
type runnerAssignment struct {
	SchemaVersion int
	JobKey        string
	Job           Job
	ExpiresAt     time.Time `firestore:"expires_at,omitempty"`
}

func (f *firestoreStore) assignmentRef(name string) *firestore.DocumentRef {
	return f.client.Collection("runners").Doc(name).Collection("assignments").Doc("job")
}

func (f *firestoreStore) hasAssignment(tx *firestore.Transaction, name string) (bool, error) {
	doc, err := tx.Get(f.assignmentRef(name))
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var r runnerAssignment
	if err = doc.DataTo(&r); err != nil {
		return false, err
	}
	if r.SchemaVersion != stateVersion {
		return false, errSchema
	}
	return true, nil
}

func (f *firestoreStore) RecordAssignment(ctx context.Context, key, name string, job Job) error {
	return f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		// Read durable demand to prevent a delayed in_progress event resurrecting
		// an assignment after completion, including a crash between the two writes.
		doc, err := tx.Get(f.client.Collection("jobs").Doc(key))
		if err != nil {
			return err
		}
		var demand lifecycleRecord
		if err = doc.DataTo(&demand); err != nil {
			return err
		}
		if err = checkRecordVersion(&demand, true); err != nil {
			return err
		}
		ref := f.assignmentRef(name)
		doc, err = tx.Get(ref)
		var existing runnerAssignment
		if err == nil {
			if err = doc.DataTo(&existing); err != nil {
				return err
			}
			if existing.SchemaVersion != stateVersion {
				return errSchema
			}
			if existing.JobKey != key {
				// A re-run attempt replays carried-over completed jobs under new IDs
				// with the runner name from the earlier attempt. The generation
				// belongs to the job that ran on it; only a live claim conflicts.
				if job.Status == "completed" {
					return nil
				}
				return fmt.Errorf("conflicting assignment for JIT generation %s", name)
			}
		} else if status.Code(err) != codes.NotFound {
			return err
		}
		ledger := f.client.Collection("runners").Doc(name)
		ledgerDoc, ledgerErr := tx.Get(ledger)
		if ledgerErr != nil && status.Code(ledgerErr) != codes.NotFound {
			return ledgerErr
		}
		if demand.Terminal || existing.Job.Status == "completed" {
			job.Status = "completed"
		}
		r := runnerAssignment{SchemaVersion: stateVersion, JobKey: key, Job: job}
		if job.Status == "completed" {
			r.ExpiresAt = time.Now().Add(7 * 24 * time.Hour)
		}
		if ledgerErr == nil && ledgerDoc.Exists() {
			if err = tx.Update(ledger, []firestore.Update{{Path: "Available", Value: false}}); err != nil {
				return err
			}
		}
		return tx.Set(ref, r)
	})
}

func (f *firestoreStore) Assignment(ctx context.Context, name string) (runnerAssignment, error) {
	doc, err := f.assignmentRef(name).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return runnerAssignment{}, nil
	}
	if err != nil {
		return runnerAssignment{}, err
	}
	var r runnerAssignment
	if err = doc.DataTo(&r); err != nil {
		return r, err
	}
	if r.SchemaVersion != stateVersion {
		return r, errSchema
	}
	return r, nil
}

func (s *Autoscaler) assignmentActive(ctx context.Context, name string) (bool, error) {
	assignment, err := s.store.Assignment(ctx, name)
	if err != nil {
		return false, err
	}
	return s.checkAssignment(ctx, name, assignment)
}

func (s *Autoscaler) checkAssignment(ctx context.Context, name string, assignment runnerAssignment) (bool, error) {
	job := assignment.Job
	if job.Id == 0 || job.Status == "completed" {
		return false, nil
	}
	// A positive assignment overrides REST busy/status. Only the assigned job's
	// confirmed completion can retire it; API errors retain the protection.
	statusFn := s.jobStatusFn
	if statusFn == nil {
		statusFn = s.currentJobStatus
	}
	current, err := statusFn(ctx, job)
	if err != nil {
		return true, err
	}
	if current == "completed" {
		job.Status = "completed"
		if err = s.store.RecordAssignment(ctx, assignment.JobKey, name, job); err != nil {
			return true, err
		}
	}
	return current != "completed", nil
}

func (s *Autoscaler) capacityRegistration(ctx context.Context, src Source, name string) (runnerRegistration, error) {
	a, err := s.store.Assignment(ctx, name)
	if err != nil {
		return runnerOffline, err
	}
	if a.Job.Id != 0 {
		active, err := s.checkAssignment(ctx, name, a)
		if err != nil {
			return runnerBusy, err
		}
		if active {
			return runnerBusy, nil
		}
		// A JIT generation that completed its assigned job is consumed, even
		// if the registration endpoint transiently advertises online/idle.
		return runnerGone, nil
	}
	fn := s.runnerStateFn
	if fn == nil {
		fn = s.runnerState
	}
	return fn(ctx, src, name)
}
