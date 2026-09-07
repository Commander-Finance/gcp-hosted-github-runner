package pkg

import (
	"context"
	"fmt"
	"time"
)

func (m *memoryStore) RecordAssignment(_ context.Context, key, name string, job Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.assignments == nil {
		m.assignments = map[string]runnerAssignment{}
	}
	old := m.assignments[name]
	if old.JobKey != "" && old.JobKey != key {
		return fmt.Errorf("conflicting assignment")
	}
	if m.rows[key].Terminal || old.Job.Status == "completed" {
		job.Status = "completed"
	}
	m.assignments[name] = runnerAssignment{SchemaVersion: stateVersion, JobKey: key, Job: job}
	if r, ok := m.runners[name]; ok {
		r.Available = false
		m.runners[name] = r
	}
	return nil
}

func (m *memoryStore) Assignment(_ context.Context, name string) (runnerAssignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.assignments[name], nil
}

func (m *memoryStore) RememberRunnerID(_ context.Context, name string, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rr, ok := m.runners[name]
	if !ok {
		return nil
	}
	rr.Record.RunnerID = id
	m.runners[name] = rr
	if rr.Owner != "" {
		r := m.rows[rr.Owner]
		r.RunnerID = id
		m.rows[rr.Owner] = r
	}
	return nil
}

func (m *memoryStore) Detach(_ context.Context, key, token string, available bool, due time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[key]
	if r.Lease != token || time.Now().After(r.LeaseUntil) {
		return errLeaseBusy
	}
	m.runners[r.VMName] = runnerRecord{SchemaVersion: stateVersion, Name: r.VMName, Pool: poolKey(r.Source, r.Job), Available: available && m.assignments[r.VMName].Job.Id == 0, NextActionAt: due, Record: r}
	discard := fleetState{}
	releaseReservation(&r, &discard)
	r.NextActionAt = time.Now().Add(30 * time.Second)
	prepareRecord(&r)
	m.rows[key] = r
	return nil
}
func (m *memoryStore) Adopt(_ context.Context, key, token, pool string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[key]
	if r.Lease != token || time.Now().After(r.LeaseUntil) {
		return false, errLeaseBusy
	}
	if r.Terminal || r.VMName != "" {
		return false, nil
	}
	for name, rr := range m.runners {
		if !rr.Available || rr.Pool != pool || m.assignments[rr.Name].Job.Id != 0 {
			continue
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
		m.rows[key] = r
		m.runners[name] = rr
		return true, nil
	}
	return false, nil
}
func (m *memoryStore) Runner(_ context.Context, name string) (runnerRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runners[name], nil
}
func (m *memoryStore) ReleaseRunner(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runners[name]
	if !ok || r.Owner != "" {
		return nil
	}
	delete(m.runners, name)
	m.fleet.Runners--
	if r.Record.Model == "standard" {
		m.fleet.Standard--
	}
	m.fleet.Revision++
	return nil
}
func (m *memoryStore) DeferRunner(_ context.Context, name string, due time.Time, available bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runners[name]
	if ok && r.Owner == "" {
		r.NextActionAt = due
		r.Available = available && !r.Record.Terminal && m.assignments[name].Job.Id == 0
		m.runners[name] = r
	}
	return nil
}
func (m *memoryStore) RunnerPage(_ context.Context, due time.Time) ([]runnerRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rows []runnerRecord
	for _, r := range m.runners {
		if r.Owner == "" && !r.NextActionAt.After(due) {
			rows = append(rows, r)
		}
		if len(rows) == runnerPageSize {
			break
		}
	}
	return rows, nil
}
func (m *memoryStore) AuditSnapshot(_ context.Context) (fleetState, []runnerRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rows []runnerRecord
	for _, r := range m.runners {
		rows = append(rows, r)
	}
	return m.fleet, rows, nil
}
func (m *memoryStore) Backoff(_ context.Context, key string, until time.Time) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.backoffs == nil {
		m.backoffs = map[string]time.Time{}
	}
	if until.After(m.backoffs[key]) {
		m.backoffs[key] = until
	}
	return m.backoffs[key], nil
}
