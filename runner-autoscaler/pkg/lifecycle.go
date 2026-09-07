package pkg

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2/apierror"
	log "github.com/sirupsen/logrus"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

var errLeaseBusy = errors.New("job lease is held by another callback")
var errFleetFull = errors.New("runner admission limit reached")

func nonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (s *Autoscaler) observe(ctx context.Context, src Source, job Job, terminal bool) error {
	job.TaskToken = ""
	err := s.store.UpdateJob(ctx, jobKey(job), func(r *lifecycleRecord, _ *fleetState) error {
		changed := r.Job.Status != job.Status || (!r.Terminal && terminal)
		if !r.Terminal || terminal {
			r.Job = job
		}
		// The first delivering source owns the record so its JIT scope stays
		// stable; a later source takes over only if the first was unregistered.
		if _, registered := s.conf.RegisteredSources[r.Source]; r.Source == "" || !registered {
			r.Source = src.Name
		}
		if terminal && IsOwnedRunnerName(s.conf.RunnerPrefix, job.RunnerName) {
			r.PendingDelete = job.RunnerName
			r.ExpiresAt = time.Time{}
		}
		r.Terminal = r.Terminal || terminal
		if changed {
			r.NextActionAt = time.Now()
			r.EnqueuedUntil = time.Time{}
			r.EnqueueToken = ""
		}
		if r.UpdatedAt.IsZero() {
			r.UpdatedAt = time.Now()
		}
		if r.Terminal && r.VMName == "" && r.PendingDelete == "" {
			r.ExpiresAt = time.Now().Add(7 * 24 * time.Hour)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if IsOwnedRunnerName(s.conf.RunnerPrefix, job.RunnerName) && (job.Status == "in_progress" || terminal) {
		if terminal {
			job.Status = "completed"
		}
		return s.store.RecordAssignment(ctx, jobKey(job), job.RunnerName, job)
	}
	return nil
}

// claim does NOT reserve a VM. Demand must be checked before admission. The lease
// outlasts the worker's operation deadline, including time spent waiting on APIs.
func (s *Autoscaler) claim(ctx context.Context, key, token string) (lifecycleRecord, error) {
	var record lifecycleRecord
	err := s.store.UpdateJob(ctx, key, func(r *lifecycleRecord, _ *fleetState) error {
		if r.Job.Id == 0 {
			return fmt.Errorf("unknown job")
		}
		if r.Lease != "" && time.Now().Before(r.LeaseUntil) {
			return errLeaseBusy
		}
		r.Lease, r.LeaseUntil = token, time.Now().Add(time.Duration(s.conf.TaskTimeout+60)*time.Second)
		record = *r
		return nil
	})
	return record, err
}
func (s *Autoscaler) mutateJob(ctx context.Context, key, token string, change func(*lifecycleRecord, *fleetState) error) error {
	return s.store.UpdateJob(ctx, key, func(r *lifecycleRecord, f *fleetState) error {
		if r.Lease != token || !time.Now().Before(r.LeaseUntil) {
			return errLeaseBusy
		}
		return change(r, f)
	})
}
func (s *Autoscaler) mutate(ctx context.Context, key, token string, change func(*lifecycleRecord, *fleetState) error) error {
	return s.store.Update(ctx, key, func(r *lifecycleRecord, f *fleetState) error {
		if r.Lease != token || !time.Now().Before(r.LeaseUntil) {
			return errLeaseBusy
		}
		return change(r, f)
	})
}
func releaseReservation(r *lifecycleRecord, f *fleetState) {
	if r.VMName != "" {
		f.Runners--
		if r.Model == "standard" {
			f.Standard--
		}
	}
	r.VMName, r.Zone, r.Model, r.JIT = "", "", "", ""
	r.CreatedAt = time.Time{}
	r.Operation = ""
	r.Template = ""
	r.JITIssuedAt = time.Time{}
	r.RunnerID = 0
	r.AttemptedAt = time.Time{}
	if r.Terminal && r.PendingDelete == "" {
		r.ExpiresAt = time.Now().Add(7 * 24 * time.Hour)
	}
}

// operationError reports an insert whose zonal operation Compute has settled
// with an error: the attempt is over and no VM will appear for it.
type operationError struct{ err error }

func (e operationError) Error() string { return e.err.Error() }
func (e operationError) Unwrap() error { return e.err }

// processJob retains a durable record through deletion, API outages and enqueue
// failures. There is intentionally no boolean idle-runner gate: each queued job
// has at most one reservation and is rechecked until GitHub serves it, regardless
// of which job its original runner actually accepted.
func (s *Autoscaler) processJob(ctx context.Context, src Source, job Job) error {
	if s.conf.Simulate {
		return nil
	}
	key, token := jobKey(job), nonce()
	r, err := s.claim(ctx, key, token)
	if err != nil {
		return err
	}
	defer func() {
		finish, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := s.mutateJob(finish, key, token, func(r *lifecycleRecord, _ *fleetState) error { r.Lease = ""; return nil }); e != nil {
			log.Warnf("Lifecycle lease release failed for job %d: %v", job.Id, e)
		}
	}()
	// A settled insert failure found during reconciliation; the creation plan
	// resumes after it instead of restarting from the first attempt.
	var failedAttempt *creationAttempt
	// GitHub assignment, not VM ownership, determines whether demand is served.
	statusFn := s.jobStatusFn
	if statusFn == nil {
		statusFn = s.currentJobStatus
	}
	currentStatus := "completed"
	var statusErr error
	if !r.Terminal {
		currentStatus, statusErr = statusFn(ctx, job)
	}
	if err = s.mutateJob(ctx, key, token, func(current *lifecycleRecord, _ *fleetState) error {
		if !current.Terminal && statusErr == nil {
			current.Job.Status = currentStatus
			current.Terminal = currentStatus == "completed"
		}
		r = *current
		return nil
	}); err != nil {
		return err
	}
	if currentStatus == "queued" && r.VMName == "" {
		adopted, e := s.store.Adopt(ctx, key, token, poolKey(src.Name, job))
		if e != nil {
			return e
		}
		if adopted {
			if e = s.mutateJob(ctx, key, token, func(current *lifecycleRecord, _ *fleetState) error { r = *current; return nil }); e != nil {
				return e
			}
		}
	}
	// A VM's immutable generation name can never target a later replacement.
	// Reclaim a stopped VM even if GitHub is unavailable, retaining its job record.
	if r.VMName != "" {
		stateFn := s.instanceStateFn
		if stateFn == nil {
			stateFn = s.instanceState
		}
		found, state, e := stateFn(ctx, r.VMName)
		if e != nil {
			return e
		}
		if found && !state.isStopped() {
			if statusErr != nil {
				return statusErr
			}
			registration, e := s.capacityRegistration(ctx, src, r.VMName)
			if e != nil {
				return e
			}
			if registration == runnerGone || (r.Terminal && registration != runnerBusy) || (registration == runnerOffline && s.offlineExpired(r)) {
				if e = s.DeleteInstance(ctx, r.VMName); e != nil {
					return e
				}
				// Deletion must succeed before capacity can be released. A queued
				// job remains due and receives replacement on the next dispatch.
				return s.mutate(ctx, key, token, func(current *lifecycleRecord, f *fleetState) error {
					releaseReservation(current, f)
					return nil
				})
			}
			busy := registration == runnerBusy
			if currentStatus != "queued" || busy {
				// Keep the actual running VM accounted independently. In particular,
				// busy RA cannot suppress A when it accepted B and RB was preempted.
				return s.store.Detach(ctx, key, token, registration == runnerIdle && !r.Terminal, time.Now().Add(2*time.Minute))
			}
			return s.mutateJob(ctx, key, token, func(current *lifecycleRecord, _ *fleetState) error {
				if current.CreatedAt.IsZero() {
					current.CreatedAt = time.Now()
				}
				current.NextActionAt = time.Now().Add(2 * time.Minute)
				return nil
			})
		}
		if found {
			if e = s.DeleteInstance(ctx, r.VMName); e != nil {
				return e
			}
		}
		if !found && r.CreatedAt.IsZero() && r.Zone != "" && s.canResolveAttempt() {
			done, settled, e := s.resolveAttempt(ctx, r)
			if e != nil {
				return e
			}
			if done {
				// The operation may finish after the first instance lookup.
				stillPresent, _, checkErr := stateFn(ctx, r.VMName)
				if checkErr != nil {
					return checkErr
				}
				if stillPresent {
					return nil
				}
				if settled != nil && IsCapacityError(settled) {
					// Compute settled the insert with a stockout. Keep the generation
					// and its JIT credential; the plan continues after this attempt.
					pinned := creationAttempt{template: r.Template, zone: r.Zone, provisioningModel: r.Model, machineType: rMachine(r)}
					failedAttempt = &pinned
					change := s.mutateJob
					if r.Model == "standard" {
						change = s.mutate
					}
					if e = change(ctx, key, token, func(current *lifecycleRecord, f *fleetState) error {
						clearAttempt(current, f)
						r = *current
						return nil
					}); e != nil {
						return e
					}
				} else if e = s.mutate(ctx, key, token, func(current *lifecycleRecord, f *fleetState) error {
					// A completed operation and no VM: it either failed or the VM was
					// already removed by a delete/sweep before creation could be recorded.
					releaseReservation(current, f)
					r = *current
					return nil
				}); e != nil {
					return e
				}
			}
		}
		// For an ambiguous insert, absence is NOT proof that the insert failed.
		// Retry the exact zone/name/JIT rather than freeing the reservation.
		if found || !r.CreatedAt.IsZero() {
			if e = s.mutate(ctx, key, token, func(current *lifecycleRecord, f *fleetState) error {
				releaseReservation(current, f)
				r = *current
				return nil
			}); e != nil {
				return e
			}
		}
	}
	if statusErr != nil {
		return statusErr
	}
	if r.Terminal {
		if r.VMName != "" && r.Zone == "" {
			return s.mutate(ctx, key, token, func(current *lifecycleRecord, f *fleetState) error { releaseReservation(current, f); return nil })
		}
		return nil
	}
	if r.Failure != "" {
		return permanentError{r.Failure}
	}
	status := currentStatus
	if err = s.mutateJob(ctx, key, token, func(current *lifecycleRecord, _ *fleetState) error {
		if !current.Terminal {
			current.Job.Status = status
		}
		current.NextActionAt = time.Now().Add(10 * time.Minute)
		return nil
	}); err != nil {
		return err
	}
	if status != "queued" {
		// in_progress may still be using a runner created for a different job. Keep
		// observing it; only a confirmed completion becomes a terminal tombstone.
		if status == "completed" {
			if err = s.observe(ctx, src, job, true); err != nil {
				return err
			}
		}
		if r.VMName != "" && r.Zone == "" {
			return s.mutate(ctx, key, token, func(current *lifecycleRecord, f *fleetState) error { releaseReservation(current, f); return nil })
		}
		return nil
	}
	// A JIT configuration must not survive a prolonged capacity outage. No
	// submitted attempt may be replaced until its operation has been resolved.
	if r.VMName != "" && r.Zone == "" && !r.JITIssuedAt.IsZero() && time.Since(r.JITIssuedAt) > 45*time.Minute {
		if err = s.mutate(ctx, key, token, func(current *lifecycleRecord, f *fleetState) error {
			releaseReservation(current, f)
			r = *current
			return nil
		}); err != nil {
			return err
		}
	}
	override := job.GetMagicLabelValue(MagicLabelMachine)
	if override != nil && !s.allowedMachine(*override) {
		return permanentError{fmt.Sprintf("machine type %q is not allowed", *override)}
	}
	if r.VMName == "" {
		name := fmt.Sprintf("%s-%d-%s", s.conf.RunnerPrefix, job.Id, nonce())
		err = s.mutate(ctx, key, token, func(current *lifecycleRecord, f *fleetState) error {
			if current.Terminal {
				return nil
			}
			if f.Runners >= s.conf.MaxRunners {
				return errFleetFull
			}
			current.VMName = name
			current.ExpiresAt = time.Time{}
			f.Runners++
			r = *current
			return nil
		})
		if err != nil || r.VMName == "" {
			return err
		}
	}
	group := s.conf.RunnerGroupId
	if src.SourceType == TypeRepository {
		group = 1
	}
	endpoint := jitEndpoint(src)
	if r.JIT == "" {
		generate := s.jitConfigFn
		if generate == nil {
			generate = s.GenerateRunnerJitConfig
		}
		jit, e := generate(ctx, endpoint, r.VMName, group, job.Labels)
		if errors.Is(e, ErrRunnerNameConflict) {
			// The prior lease has expired and no live VM backs this generation. Only
			// this lease holder can recover a registration lost before its state write.
			if e = s.deleteRunnerByName(ctx, endpoint, r.VMName); e == nil {
				jit, e = generate(ctx, endpoint, r.VMName, group, job.Labels)
			}
		}
		if e != nil {
			return e
		}
		if e = s.mutateJob(ctx, key, token, func(current *lifecycleRecord, _ *fleetState) error {
			current.JIT = jit
			current.JITIssuedAt = time.Now()
			r = *current
			return nil
		}); e != nil {
			return e
		}
	}
	full := s.creationPlan(r.VMName, override, s.benchedZonesCached(ctx))
	plan := full
	if r.Zone != "" {
		// Retry the saved attempt first; the persisted machine, not the job
		// override, is what was inserted. A settled failure then continues
		// through the rest of the plan.
		pinned := creationAttempt{template: r.Template, zone: r.Zone, provisioningModel: r.Model, machineType: rMachine(r)}
		plan = append([]creationAttempt{pinned}, attemptsAfter(full, pinned)...)
	} else if failedAttempt != nil {
		plan = attemptsAfter(full, *failedAttempt)
	}
	insert := s.tryInsertFn
	if insert == nil {
		client, closeClient := s.compute(ctx)
		defer closeClient()
		insert = func(ctx context.Context, a creationAttempt, name string, md []*computepb.Items) error {
			var machine *string
			if a.machineType != "" {
				machine = proto.String(fmt.Sprintf("zones/%s/machineTypes/%s", a.zone, a.machineType))
			}
			op, e := client.Insert(ctx, &computepb.InsertInstanceRequest{Project: s.conf.ProjectId, Zone: a.zone, RequestId: proto.String(insertRequestID(name, a)), SourceInstanceTemplate: proto.String(a.template), InstanceResource: &computepb.Instance{Name: proto.String(name), MachineType: machine, Metadata: &computepb.Metadata{Items: md}}})
			if e != nil {
				return e
			}
			if e = s.mutateJob(ctx, key, token, func(current *lifecycleRecord, _ *fleetState) error {
				current.Operation = op.Name()
				r = *current
				return nil
			}); e != nil {
				return e
			}
			if e = op.Wait(ctx); e != nil && op.Done() {
				return operationError{e}
			}
			return e
		}
	}
	for _, attempt := range plan {
		if attempt.provisioningModel == "standard" && !s.conf.AllowOnDemand {
			continue
		}
		changeAttempt := s.mutateJob
		if (r.Model == "standard") != (attempt.provisioningModel == "standard") {
			changeAttempt = s.mutate
		}
		err = changeAttempt(ctx, key, token, func(current *lifecycleRecord, f *fleetState) error {
			if current.Terminal {
				return fmt.Errorf("job completed before insert")
			}
			if attempt.provisioningModel == "standard" && current.Model != "standard" {
				if f.Standard >= s.conf.MaxOnDemandRunners {
					return errFleetFull
				}
				f.Standard++
			}
			if attempt.provisioningModel != "standard" && current.Model == "standard" {
				f.Standard--
			}
			current.Zone, current.Model, current.Machine = attempt.zone, attempt.provisioningModel, attempt.machineType
			if current.Template == "" {
				current.AttemptedAt = time.Now()
			}
			current.Template = attempt.template
			r = *current
			return nil
		})
		if err != nil {
			return err
		}
		err = insert(ctx, attempt, r.VMName, s.lifecycleMetadata(r, src))
		if err == nil || IsAlreadyExists(err) {
			log.WithFields(log.Fields{"instance": r.VMName, "zone": r.Zone, "provisioning_model": r.Model, "machine_type": attempt.machineType}).Infof("Created instance %s (%s) as %s", r.VMName, r.Zone, r.Model)
			return s.mutateJob(ctx, key, token, func(current *lifecycleRecord, _ *fleetState) error {
				current.CreatedAt = time.Now()
				current.NextActionAt = time.Now().Add(2 * time.Minute)
				return nil
			})
		}
		var apiErr *apierror.APIError
		var settled operationError
		definite := IsCapacityError(err) || IsRateLimitError(err) || (errors.As(err, &apiErr) && apiErr.HTTPCode() >= 400 && apiErr.HTTPCode() < 500)
		// Only a definite capacity error permits a different zone. An accepted
		// insert stays ambiguous until Compute settles its operation, so a timeout
		// retains the saved attempt and no second zonal VM can appear on a retry.
		if !definite || (r.Operation != "" && !errors.As(err, &settled)) {
			return err
		}
		change := s.mutateJob
		if r.Model == "standard" {
			change = s.mutate
		}
		if e := change(ctx, key, token, func(current *lifecycleRecord, f *fleetState) error {
			clearAttempt(current, f)
			r = *current
			return nil
		}); e != nil {
			return e
		}
		if IsRateLimitError(err) || !IsCapacityError(err) {
			if !IsRateLimitError(err) && apiErr != nil && apiErr.HTTPCode() >= 400 && apiErr.HTTPCode() < 500 {
				return permanentError{fmt.Sprintf("Compute rejected runner configuration: %d", apiErr.HTTPCode())}
			}
			return err
		}
	}
	if err == nil {
		err = fmt.Errorf("no permitted creation attempts")
	}
	return err
}
func rMachine(r lifecycleRecord) string { return r.Machine }

// clearAttempt drops the saved zone, model, operation and template so the next
// attempt can differ. Callers holding a STANDARD reservation must use the
// capacity-aware mutate so the counter decrement is persisted.
func clearAttempt(r *lifecycleRecord, f *fleetState) {
	if r.Model == "standard" {
		f.Standard--
	}
	r.Zone, r.Model, r.Operation, r.Template = "", "", "", ""
}

// attemptsAfter returns the attempts following the first one equal to failed,
// or the whole plan when failed is not part of it (an attempt saved under an
// earlier configuration).
func attemptsAfter(plan []creationAttempt, failed creationAttempt) []creationAttempt {
	for i, a := range plan {
		if a == failed {
			return plan[i+1:]
		}
	}
	return plan
}
func (s *Autoscaler) allowedMachine(machine string) bool {
	for _, allowed := range s.conf.AllowedMachineTypes {
		if strings.EqualFold(machine, allowed) {
			return true
		}
	}
	return false
}
func jitEndpoint(src Source) string {
	switch src.SourceType {
	case TypeRepository:
		return fmt.Sprintf(RUNNER_REPO_JIT_CONFIG_ENDPOINT, src.Name)
	case TypeOrganization:
		return fmt.Sprintf(RUNNER_ORG_JIT_CONFIG_ENDPOINT, src.Name)
	case TypeEnterprise:
		return fmt.Sprintf(RUNNER_ENTERPRISE_JIT_CONFIG_ENDPOINT, src.Name)
	}
	return ""
}
func (s *Autoscaler) currentJobStatus(ctx context.Context, job Job) (string, error) {
	if job.RepositoryFullName == "" {
		return "", fmt.Errorf("missing repository for job %d", job.Id)
	}
	pat, err := s.readPat(ctx)
	if err != nil {
		return "", err
	}
	var result struct {
		Status string `json:"status"`
	}
	err = s.githubGet(ctx, pat, fmt.Sprintf("https://api.github.com/repos/%s/actions/jobs/%d", job.RepositoryFullName, job.Id), &result)
	if err != nil {
		return "", err
	}
	if result.Status == "" {
		return "", fmt.Errorf("empty GitHub job status")
	}
	return result.Status, nil
}
func (s *Autoscaler) githubGet(ctx context.Context, pat, endpoint string, result interface{}) error {
	return s.githubGetWithRunner404(ctx, pat, endpoint, result, false)
}

// Only runner-by-ID lookups may defer classification of a 404 to inventory.
func (s *Autoscaler) githubGetWithRunner404(ctx context.Context, pat, endpoint string, result interface{}, allowRunner404 bool) error {
	if s.store != nil {
		for _, key := range []string{"github", githubEndpointKey(endpoint)} {
			until, err := s.store.Backoff(ctx, key, time.Time{})
			if err != nil {
				return err
			}
			if until.After(time.Now()) {
				return retryAtError{until, "GitHub request deferred by durable backoff"}
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return err
	}
	githubAuthHeaders(req, pat)
	resp, err := s.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if allowRunner404 && resp.StatusCode == http.StatusNotFound {
		return errRunnerLookupMissing
	}
	// GitHub masks missing permissions with 404: never interpret it as completion.
	if resp.StatusCode != 200 {
		return s.githubFailure(ctx, endpoint, resp)
	}
	return json.NewDecoder(resp.Body).Decode(result)
}
func (s *Autoscaler) lifecycleMetadata(r lifecycleRecord, src Source) []*computepb.Items {
	body, _ := json.Marshal(recreateCapability{Job: r.Job, Runner: r.VMName, Purpose: "recreate", Expires: time.Now().Add(time.Duration(s.conf.MachineTimeout+300) * time.Second).Unix()})
	return []*computepb.Items{
		{Key: proto.String("jit_config"), Value: proto.String(r.JIT)},
		{Key: proto.String("startup-script"), Value: proto.String(fmt.Sprintf(runner_script_wrapper, "jit_config", RUNNER_SCRIPT_REGISTER_JIT_RUNNER_ATTR))},
		{Key: proto.String("shutdown-script"), Value: proto.String(shutdownScriptValue)},
		{Key: proto.String(RECREATE_CALLBACK_URL_ATTR), Value: proto.String(s.callbackURL(s.conf.RouteRecreateVm, src.Name))},
		{Key: proto.String(RECREATE_CALLBACK_PAYLOAD_ATTR), Value: proto.String(string(body))},
		{Key: proto.String(RECREATE_CALLBACK_SIG_ATTR), Value: proto.String(CalcSigHex([]byte(src.Secret), append([]byte("recreate\n"), body...)))},
		{Key: proto.String(RECREATE_CALLBACK_JOB_PATTERN_ATTR), Value: proto.String(s.conf.RunnerJobLogPattern)},
	}
}

// Compute accepts a UUID request ID. A hash of immutable attempt inputs survives
// process restarts and ensures a lost Insert response reattaches to that request.
func insertRequestID(name string, a creationAttempt) string {
	sum := sha256.Sum256([]byte(name + ":" + a.zone + ":" + a.provisioningModel + ":" + a.machineType))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// Resolve accepted inserts even if the worker died before recording success.
// Pending operations keep their reservation. Missing operations are considered
// absent only after the old worker's lease/deadline and a propagation margin.
// settled carries the operation's own error when Compute finished it
// unsuccessfully, so the caller can classify the failure.
func (s *Autoscaler) resolveAttempt(ctx context.Context, r lifecycleRecord) (done bool, settled error, err error) {
	op, err := s.lookupOperation(ctx, r)
	if err != nil {
		return false, nil, err
	}
	if op == nil {
		if r.Operation != "" {
			return true, nil, nil // only completed operations expire
		}
		return !r.AttemptedAt.IsZero() && time.Since(r.AttemptedAt) > time.Duration(s.conf.TaskTimeout+120)*time.Second, nil, nil
	}
	done = op.GetStatus() == computepb.Operation_DONE
	if code := op.GetHttpErrorStatusCode(); done && op.HttpErrorStatusCode != nil && (code < 200 || code > 299) {
		settled = operationError{fmt.Errorf("%s: %v", op.GetHttpErrorMessage(), op.GetError())}
	}
	return done, settled, nil
}
func (s *Autoscaler) canResolveAttempt() bool {
	return s.operationLookupFn != nil || s.operationsClient != nil
}

// lookupOperation finds the insert operation for the record's generation by
// operation name when one was recorded, else by the deterministic request id.
// A nil operation means Compute has no record of the insert.
func (s *Autoscaler) lookupOperation(ctx context.Context, r lifecycleRecord) (*computepb.Operation, error) {
	if s.operationLookupFn != nil {
		return s.operationLookupFn(ctx, r)
	}
	if r.Operation != "" {
		op, err := s.operationsClient.Get(ctx, &computepb.GetZoneOperationRequest{Project: s.conf.ProjectId, Zone: r.Zone, Operation: r.Operation})
		if IsNotFound(err) {
			return nil, nil
		}
		return op, err
	}
	a := creationAttempt{zone: r.Zone, provisioningModel: r.Model, machineType: r.Machine}
	it := s.operationsClient.List(ctx, &computepb.ListZoneOperationsRequest{Project: s.conf.ProjectId, Zone: r.Zone, Filter: proto.String(fmt.Sprintf("clientOperationId = %q", insertRequestID(r.VMName, a)))})
	op, err := it.Next()
	if err == iterator.Done {
		return nil, nil
	}
	return op, err
}
