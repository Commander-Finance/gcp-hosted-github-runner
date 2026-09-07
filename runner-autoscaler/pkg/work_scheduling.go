package pkg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

type permanentError struct{ message string }

func (e permanentError) Error() string { return e.message }

type retryAtError struct {
	Until   time.Time
	Message string
}

func (e retryAtError) Error() string { return e.Message }

var errTaskObsolete = errors.New("task is no longer the active dispatch")

// dispatchWindow bounds one Cloud Tasks retry chain. Cloud Tasks keeps retrying
// until both max_attempts and max_retry_duration are exhausted, so the chain is
// the longer of the attempt-bound and duration-bound spans, plus a margin for
// the final attempt and propagation. Each attempt may run for the dispatch
// deadline (TaskTimeout plus five seconds).
func dispatchWindow(taskTimeout, attempts, maxBackoff, maxRetryDuration int64) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	attempt := taskTimeout + 5
	byAttempts := attempts*attempt + (attempts-1)*maxBackoff
	byDuration := maxRetryDuration + attempt
	chain := byAttempts
	if byDuration > chain {
		chain = byDuration
	}
	return time.Duration(chain+60) * time.Second
}

// One durable outbox marker covers a complete bounded Cloud Tasks retry chain.
// Write before enqueue: if a worker dies here, the marker expires and redrives.
func (s *Autoscaler) enqueueJob(ctx context.Context, src Source, job Job, delay time.Duration) error {
	key := jobKey(src.Name, job)
	token := nonce()
	route := s.conf.RouteCreateVm
	send := false
	err := s.store.UpdateJob(ctx, key, func(r *lifecycleRecord, _ *fleetState) error {
		// Firestore may replay this callback after a transaction conflict.
		send = false
		route = s.conf.RouteCreateVm
		if !r.NeedsReconcile || r.EnqueuedUntil.After(time.Now()) || (r.Lease != "" && r.LeaseUntil.After(time.Now())) {
			return nil
		}
		if r.NextActionAt.After(time.Now().Add(delay)) {
			return nil
		}
		r.EnqueueToken = token
		r.EnqueuedUntil = time.Now().Add(delay + dispatchWindow(s.conf.TaskTimeout, s.conf.TaskRetryAttempts, s.conf.TaskRetryMaxBackoff, s.conf.TaskRetryMaxDuration))
		job = r.Job
		job.TaskToken = token
		if r.PendingDelete != "" {
			route = s.conf.RouteDeleteVm
			job.RunnerName = r.PendingDelete
		}
		send = true
		return nil
	})
	if err != nil || !send {
		return err
	}
	if err = s.queue(ctx, route, src.Name, job, delay); err != nil {
		_ = s.store.UpdateJob(ctx, key, func(r *lifecycleRecord, _ *fleetState) error {
			if r.EnqueueToken == token {
				r.EnqueuedUntil = time.Time{}
				r.NextActionAt = time.Now().Add(time.Minute)
			}
			return nil
		})
	}
	return err
}
func (s *Autoscaler) activeTask(ctx context.Context, src Source, job Job) error {
	return s.store.UpdateJob(ctx, jobKey(src.Name, job), func(r *lifecycleRecord, _ *fleetState) error {
		if job.TaskToken == "" || r.EnqueueToken != job.TaskToken {
			return errTaskObsolete
		}
		return nil
	})
}

// Expected contention is acknowledged. Only transient API errors retain the
// Cloud Tasks retry chain; reconciliation cannot create another during its lease.
func (s *Autoscaler) finishTask(ctx context.Context, src Source, job Job, workErr error) (int, error) {
	if errors.Is(workErr, errTaskObsolete) || errors.Is(workErr, errLeaseBusy) {
		return 200, nil
	}
	var permanent permanentError
	var later retryAtError
	status := 200
	transient := workErr != nil && !errors.Is(workErr, errLeaseBusy) && !errors.Is(workErr, errFleetFull) && !errors.As(workErr, &permanent) && !errors.As(workErr, &later)
	if transient {
		status = 503
	}
	update := s.store.UpdateJob
	if errors.As(workErr, &permanent) {
		update = s.store.Update
	}
	err := update(ctx, jobKey(src.Name, job), func(r *lifecycleRecord, f *fleetState) error {
		if job.TaskToken != "" && job.TaskToken != r.EnqueueToken {
			return nil
		}
		if !transient {
			r.EnqueuedUntil = time.Time{}
			r.EnqueueToken = ""
		}
		if !transient {
			r.NextActionAt = time.Now().Add(2 * time.Minute)
		}
		if workErr == nil && r.Job.Status == "in_progress" {
			r.NextActionAt = time.Now().Add(10 * time.Minute)
		}
		if errors.As(workErr, &later) {
			r.NextActionAt = later.Until
		}
		if errors.As(workErr, &permanent) {
			if r.Zone == "" && r.VMName != "" {
				releaseReservation(r, f)
			}
			r.Failure = permanent.Error()
			r.NextActionAt = time.Now().Add(time.Hour)
		}
		return nil
	})
	if err != nil {
		return 503, err
	}
	if workErr != nil && !errors.Is(workErr, errLeaseBusy) && !errors.Is(workErr, errFleetFull) {
		log.WithField("job_id", job.Id).Errorf("Lifecycle create failed: %v", workErr)
	}
	return status, nil
}

type runnerRegistration int

const (
	runnerIdle runnerRegistration = iota
	runnerBusy
	runnerGone
	runnerOffline
)

func registrationState(status string, busy bool) runnerRegistration {
	if !strings.EqualFold(status, "online") {
		return runnerOffline
	}
	if busy {
		return runnerBusy
	}
	return runnerIdle
}

func (s *Autoscaler) offlineExpired(r lifecycleRecord) bool {
	started := r.CreatedAt
	if started.IsZero() {
		started = r.AttemptedAt
	}
	if started.IsZero() {
		started = r.JITIssuedAt
	}
	timeout := s.conf.RunnerRegisterTimeout
	if timeout <= 0 {
		timeout = 120
	}
	return started.IsZero() || !time.Now().Before(started.Add(time.Duration(timeout)*time.Second))
}

var errRunnerLookupMissing = errors.New("runner lookup requires inventory verification")

// Registration is observed independently of the VM's originating demand.
func (s *Autoscaler) runnerState(ctx context.Context, src Source, name string) (runnerRegistration, error) {
	pat, err := s.readPat(ctx)
	if err != nil {
		return runnerIdle, err
	}
	return s.runnerStateWithPAT(ctx, src, name, pat)
}

func (s *Autoscaler) runnerStateWithPAT(ctx context.Context, src Source, name, pat string) (runnerRegistration, error) {
	endpoint := jitEndpoint(src)
	endpoint = strings.TrimSuffix(endpoint, "/generate-jitconfig")
	if s.store != nil {
		r, err := s.store.Runner(ctx, name)
		if err != nil {
			return runnerIdle, err
		}
		if r.Record.RunnerID > 0 {
			var result struct {
				Busy   bool   `json:"busy"`
				Status string `json:"status"`
			}
			if err = s.githubGetWithRunner404(ctx, pat, fmt.Sprintf("%s/%d", endpoint, r.Record.RunnerID), &result, true); err == nil {
				return registrationState(result.Status, result.Busy), nil
			} else if !errors.Is(err, errRunnerLookupMissing) {
				return runnerIdle, err
			}
		}
	}
	for page := 1; page <= 100; page++ {
		var result struct {
			Runners []struct {
				ID     int64  `json:"id"`
				Name   string `json:"name"`
				Busy   bool   `json:"busy"`
				Status string `json:"status"`
			} `json:"runners"`
		}
		if err := s.githubGet(ctx, pat, fmt.Sprintf("%s?per_page=100&page=%d", endpoint, page), &result); err != nil {
			return runnerIdle, err
		}
		for _, runner := range result.Runners {
			if runner.Name == name {
				if s.store != nil && runner.ID > 0 {
					if err := s.store.RememberRunnerID(ctx, name, runner.ID); err != nil {
						return runnerIdle, err
					}
				}
				return registrationState(runner.Status, runner.Busy), nil
			}
		}
		if len(result.Runners) < 100 {
			return runnerGone, nil
		}
	}
	return runnerIdle, fmt.Errorf("runner registration inventory exceeds page bound")
}
