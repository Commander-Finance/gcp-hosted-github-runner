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
	return s.store.Update(ctx, jobKey(src.Name, job), func(r *lifecycleRecord, _ *fleetState) error {
		if !r.Terminal || terminal {
			r.Job, r.Source = job, src.Name
		}
		if terminal && IsOwnedRunnerName(s.conf.RunnerPrefix, job.RunnerName) {
			r.PendingDelete = job.RunnerName
			r.ExpiresAt = time.Time{}
		}
		r.Terminal = r.Terminal || terminal
		if r.UpdatedAt.IsZero() {
			r.UpdatedAt = time.Now()
		}
		if r.Terminal && r.VMName == "" && r.PendingDelete == "" {
			r.ExpiresAt = time.Now().Add(7 * 24 * time.Hour)
		}
		return nil
	})
}

// claim does NOT reserve a VM. Demand must be checked before admission. The lease
// outlasts the worker's operation deadline, including time spent waiting on APIs.
func (s *Autoscaler) claim(ctx context.Context, key, token string) (lifecycleRecord, error) {
	var record lifecycleRecord
	err := s.store.Update(ctx, key, func(r *lifecycleRecord, _ *fleetState) error {
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
	r.AttemptedAt = time.Time{}
	if r.Terminal && r.PendingDelete == "" {
		r.ExpiresAt = time.Now().Add(7 * 24 * time.Hour)
	}
}

// processJob retains a durable record through deletion, API outages and enqueue
// failures. There is intentionally no boolean idle-runner gate: each queued job
// has at most one reservation and is rechecked until GitHub serves it, regardless
// of which job its original runner actually accepted.
func (s *Autoscaler) processJob(ctx context.Context, src Source, job Job) error {
	if s.conf.Simulate {
		return nil
	}
	key, token := jobKey(src.Name, job), nonce()
	r, err := s.claim(ctx, key, token)
	if err != nil {
		return err
	}
	defer func() {
		finish, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := s.mutate(finish, key, token, func(r *lifecycleRecord, _ *fleetState) error { r.Lease = ""; return nil }); e != nil {
			log.Warnf("Lifecycle lease release failed for job %d: %v", job.Id, e)
		}
	}()
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
			if r.CreatedAt.IsZero() {
				return s.mutate(ctx, key, token, func(current *lifecycleRecord, _ *fleetState) error { current.CreatedAt = time.Now(); return nil })
			}
			return nil
		}
		if found {
			if e = s.DeleteInstance(ctx, r.VMName); e != nil {
				return e
			}
		}
		if !found && r.CreatedAt.IsZero() && r.Zone != "" && s.operationsClient != nil {
			done, e := s.resolveAttempt(ctx, r)
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
				// A completed operation and no VM: it either failed or the VM was
				// already removed by a delete/sweep before creation could be recorded.
				if e = s.mutate(ctx, key, token, func(current *lifecycleRecord, f *fleetState) error {
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
	if r.Terminal {
		if r.VMName != "" && r.Zone == "" {
			return s.mutate(ctx, key, token, func(current *lifecycleRecord, f *fleetState) error { releaseReservation(current, f); return nil })
		}
		return nil
	}
	statusFn := s.jobStatusFn
	if statusFn == nil {
		statusFn = s.currentJobStatus
	}
	status, err := statusFn(ctx, job)
	if err != nil {
		return err
	}
	if err = s.mutate(ctx, key, token, func(current *lifecycleRecord, _ *fleetState) error {
		if !current.Terminal {
			current.Job.Status = status
		}
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
		return fmt.Errorf("machine type %q is not allowed", *override)
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
		if e = s.mutate(ctx, key, token, func(current *lifecycleRecord, _ *fleetState) error {
			current.JIT = jit
			current.JITIssuedAt = time.Now()
			r = *current
			return nil
		}); e != nil {
			return e
		}
	}
	plan := s.creationPlan(r.VMName, override, s.benchedZonesCached(ctx))
	if r.Zone != "" {
		template := r.Template
		// Persist the selected machine alongside the attempt, not the job override.
		plan = []creationAttempt{{template: template, zone: r.Zone, provisioningModel: r.Model, machineType: rMachine(r)}}
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
			if e = s.mutate(ctx, key, token, func(current *lifecycleRecord, _ *fleetState) error {
				current.Operation = op.Name()
				r = *current
				return nil
			}); e != nil {
				return e
			}
			return op.Wait(ctx)
		}
	}
	for _, attempt := range plan {
		if attempt.provisioningModel == "standard" && !s.conf.AllowOnDemand {
			continue
		}
		err = s.mutate(ctx, key, token, func(current *lifecycleRecord, f *fleetState) error {
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
			return s.mutate(ctx, key, token, func(current *lifecycleRecord, _ *fleetState) error { current.CreatedAt = time.Now(); return nil })
		}
		var apiErr *apierror.APIError
		definite := IsCapacityError(err) || IsRateLimitError(err) || (errors.As(err, &apiErr) && apiErr.HTTPCode() >= 400 && apiErr.HTTPCode() < 500)
		if !definite || r.Operation != "" {
			return err
		}
		// Only a definite capacity error permits a different zone; timeouts retain
		// the saved attempt so no second zonal VM can be created on a retry.
		if e := s.mutate(ctx, key, token, func(current *lifecycleRecord, f *fleetState) error {
			if current.Model == "standard" {
				f.Standard--
			}
			current.Zone, current.Model, current.Operation, current.Template = "", "", "", ""
			return nil
		}); e != nil {
			return e
		}
		if IsRateLimitError(err) || !IsCapacityError(err) {
			return err
		}
	}
	if err == nil {
		err = fmt.Errorf("no permitted creation attempts")
	}
	return err
}
func rMachine(r lifecycleRecord) string { return r.Machine }
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
	// GitHub masks missing permissions with 404: never interpret it as completion.
	if resp.StatusCode != 200 {
		return fmt.Errorf("GitHub read failed: %s (check Actions-read permission)", resp.Status)
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
func (s *Autoscaler) resolveAttempt(ctx context.Context, r lifecycleRecord) (bool, error) {
	var op *computepb.Operation
	var err error
	if r.Operation != "" {
		op, err = s.operationsClient.Get(ctx, &computepb.GetZoneOperationRequest{Project: s.conf.ProjectId, Zone: r.Zone, Operation: r.Operation})
		if IsNotFound(err) {
			return true, nil
		} // only completed operations expire
	} else {
		a := creationAttempt{zone: r.Zone, provisioningModel: r.Model, machineType: r.Machine}
		it := s.operationsClient.List(ctx, &computepb.ListZoneOperationsRequest{Project: s.conf.ProjectId, Zone: r.Zone, Filter: proto.String(fmt.Sprintf("clientOperationId = %q", insertRequestID(r.VMName, a)))})
		op, err = it.Next()
		if err == iterator.Done {
			return !r.AttemptedAt.IsZero() && time.Since(r.AttemptedAt) > time.Duration(s.conf.TaskTimeout+120)*time.Second, nil
		}
	}
	if err != nil {
		return false, err
	}
	return op.GetStatus() == computepb.Operation_DONE, nil
}
