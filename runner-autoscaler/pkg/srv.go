package pkg

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/gin-gonic/gin"
	"github.com/googleapis/gax-go/v2/apierror"
	log "github.com/sirupsen/logrus"
	ginlogrus "github.com/toorop/gin-logrus"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const GITHUB_API_VERSION string = "2022-11-28"
const SHA_PREFIX string = "sha256="
const SHA_HEADER string = "x-hub-signature-256"
const EVENT_HEADER string = "x-github-event"

const WEBHOOK_PING_EVENT string = "ping"
const WEBHOOK_JOB_EVENT string = "workflow_job"

const RUNNER_REGISTRATION_TOKEN_ATTR string = "registration_token"
const RUNNER_JIT_CONFIG_ATTR string = "jit_config"

const RUNNER_SCRIPT_REGISTER_RUNNER_ATTR string = "startup_script_register_runner"         // has to match the global custom metadata in compute.tf
const RUNNER_SCRIPT_REGISTER_JIT_RUNNER_ATTR string = "startup_script_register_jit_runner" // has to match the global custom metadata in compute.tf

// RUNNER_SCRIPT_SHUTDOWN_ATTR is the project-metadata key for the shutdown script body (Terraform creates it).
const RUNNER_SCRIPT_SHUTDOWN_ATTR string = "shutdown_script_recreate_runner"

// RECREATE_CALLBACK_* are the instance-metadata keys written by createVmWithJitConfig and
// consumed by shutdown_script_wrapper when the VM shuts down without accepting a job.
const RECREATE_CALLBACK_URL_ATTR string = "recreate_callback_url"
const RECREATE_CALLBACK_PAYLOAD_ATTR string = "recreate_callback_payload"
const RECREATE_CALLBACK_SIG_ATTR string = "recreate_callback_sig"
const RECREATE_CALLBACK_JOB_PATTERN_ATTR string = "recreate_job_accepted_pattern"

// DefaultRunnerJobLogPattern matches the runner_job_log_pattern Terraform default. Both
// the startup script's phase-2 dispatch wait and the shutdown script's job-accepted
// check grep journald for it; createVmWithJitConfig falls back to it so the stored
// pattern is never empty (an empty grep pattern would match everything).
const DefaultRunnerJobLogPattern = "Running job:"

// Cloud Task callback kinds. The task name embeds the kind so the create-vm and
// delete-vm callbacks for the same job never collide on the same task name (a
// collision can make enqueueing the delete callback fail with AlreadyExists
// against the tombstoned create task, leaking the VM until machine_timeout).
const TaskKindCreate string = "create"
const TaskKindDelete string = "delete"

// How often the opportunistic orphan sweep is allowed to run. The sweep reclaims
// stopped/terminated runner VMs (e.g. a runner that shut itself down without ever
// picking up a job, which produces no completed-webhook). It piggybacks on the
// create-vm/delete-vm callbacks and is throttled to keep the hot path cheap.
const sweepInterval = 5 * time.Minute

// Maximum number of orphans deleted per sweep, to bound callback latency.
const maxSweepDeletes = 25

// Highest retry suffix a create/delete callback task name may carry (suffixes
// 0..maxTaskRetryCount). CreateCallbackTaskWithToken bumps the suffix on
// AlreadyExists; DeleteCallbackTask must cancel every candidate.
const maxTaskRetryCount = 2

// recreateVmDelay is how long to wait before the re-create task fires after a
// VM dies without accepting a job. The delay lets the dying VM fully disappear
// before the replacement create runs; decideCreate handles any stopped leftover.
const recreateVmDelay = 45 * time.Second

const RUNNER_REGISTER_TOKEN_ORG_ENDPOINT string = "https://api.github.com/orgs/%s/actions/runners/registration-token"

const RUNNER_ENTERPRISE_JIT_CONFIG_ENDPOINT string = "https://api.github.com/enterprises/%s/actions/runners/generate-jitconfig"
const RUNNER_ORG_JIT_CONFIG_ENDPOINT string = "https://api.github.com/orgs/%s/actions/runners/generate-jitconfig"
const RUNNER_REPO_JIT_CONFIG_ENDPOINT string = "https://api.github.com/repos/%s/actions/runners/generate-jitconfig" // format USER/REPO

type SourceType string

const (
	TypeEnterprise   SourceType = "enterprise"
	TypeOrganization SourceType = "organization"
	TypeRepository   SourceType = "repository"
)

type Source struct {
	Name       string     `json:"name"`
	SourceType SourceType `json:"type"`
	Secret     string     `json:"secret"`
}

type Job struct {
	Id                 int64    `json:"id"`
	Name               string   `json:"name"`
	Status             string   `json:"status"`
	Labels             []string `json:"labels"`
	RunnerName         string   `json:"runner_name"`
	RunnerGroupName    string   `json:"runner_group_name"`
	RunnerGroupId      int64    `json:"runner_group_id"`
	RepositoryFullName string   `json:"repository_full_name,omitempty"`
}

// Repository is the top-level "repository" object in a GitHub webhook payload.
type Repository struct {
	FullName string `json:"full_name"`
}

type Payload struct {
	Action     Action     `json:"action"`
	Job        Job        `json:"workflow_job"`
	Repository Repository `json:"repository"`
}

type VmSettings struct {
	Name        string  `json:"name"`
	MachineType *string `json:"machineType,omitempty"`
}

func (j Job) hasLabel(label string) bool {

	for _, l := range j.Labels {
		if l == label {
			return true
		}
	}
	return false
}

type MagicLabel string

const (
	MagicLabelMachine MagicLabel = "machine"
)

// matchMachineLabel matches GCE machine-type overrides like
// "gce-machine-c2d-standard-16" or "gce-machine-f1-micro". These characters
// are all valid GitHub runner labels, so the same string is both a parseable
// override for us AND a label we can register with GitHub — which is what
// lets the spawned runner match the job's `runs-on`. The 2+ segment shape
// accepts both 3-segment types (family-class-size, e.g. c2d-standard-16) and
// 2-segment shared-core types (family-variety, e.g. f1-micro, e2-medium)
// while still keeping arbitrary user labels like "gce-machine-foo" from being
// misclassified.
var matchMachineLabel = regexp.MustCompile(`^gce-machine-([a-z0-9]+(?:-[a-z0-9]+)+)$`)

// matchLegacyMagicLabel detects the historical "@<key>:" syntax, which
// GitHub's JIT API rejects because labels may not contain "@" or ":". Kept
// only so handleWebhook can emit a migration warning.
var matchLegacyMagicLabel = regexp.MustCompile(`^@` + string(MagicLabelMachine) + `:`)

func IsMagicLabel(label string) bool {
	return matchMachineLabel.MatchString(label)
}

func (j Job) GetMagicLabelValue(key MagicLabel) *string {

	if key != MagicLabelMachine {
		return nil
	}
	for _, l := range j.Labels {
		if matches := matchMachineLabel.FindStringSubmatch(l); len(matches) >= 2 {
			ret := matches[1]
			return &ret
		}
	}
	return nil
}

func (j Job) HasLegacyMagicLabel() bool {

	for _, l := range j.Labels {
		if matchLegacyMagicLabel.MatchString(l) {
			return true
		}
	}
	return false
}

// hasAllLabels returns the non-magic labels from `labels` not present on the job.
func (j Job) hasAllLabels(labels []string) []string {

	missingLabels := []string{}
	for _, label := range labels {
		if !IsMagicLabel(label) {
			if !j.hasLabel(label) {
				missingLabels = append(missingLabels, label)
			}
		}
	}
	return missingLabels
}

// ParseLabelGroups decodes the RUNNER_LABELS env value into the OR-of-ANDs
// shape: groups separated by ';', labels within a group by ','. Whitespace is
// trimmed per label; empty labels and empty groups are dropped.
func ParseLabelGroups(raw string) [][]string {

	groups := [][]string{}
	for _, rawGroup := range strings.Split(raw, ";") {
		group := []string{}
		for _, label := range strings.Split(rawGroup, ",") {
			if trimmed := strings.TrimSpace(label); trimmed != "" {
				group = append(group, trimmed)
			}
		}
		if len(group) > 0 {
			groups = append(groups, group)
		}
	}
	return groups
}

// FormatLabelGroups renders groups as `[a, b], [c, d]`, or `(none)` if empty.
func FormatLabelGroups(groups [][]string) string {

	if len(groups) == 0 {
		return "(none)"
	}
	rendered := make([]string, len(groups))
	for i, group := range groups {
		rendered[i] = "[" + strings.Join(group, ", ") + "]"
	}
	return strings.Join(rendered, ", ")
}

// gatingLabels returns the non-magic labels from group — the subset that
// actually participates in matching. Magic labels (e.g. gce-machine-*) are
// per-job overrides, not gating labels, so they're excluded from both the
// match check and the human-readable miss reason.
func gatingLabels(group []string) []string {

	filtered := make([]string, 0, len(group))
	for _, label := range group {
		if !IsMagicLabel(label) {
			filtered = append(filtered, label)
		}
	}
	return filtered
}

// HasAnyLabelGroup reports whether the job satisfies at least one label group
// (OR-of-ANDs). Magic labels in a group are ignored for matching, and groups
// containing only magic labels are filtered out entirely. On a miss, the
// second return is a human-readable reason suitable for log output.
func (j Job) HasAnyLabelGroup(groups [][]string) (bool, string) {

	if len(groups) == 0 {
		return false, "no label groups configured — rejecting all jobs"
	}
	matchable := make([][]string, 0, len(groups))
	for _, group := range groups {
		if gating := gatingLabels(group); len(gating) > 0 {
			matchable = append(matchable, gating)
		}
	}
	if len(matchable) == 0 {
		return false, "no label groups contain gating labels — gce-machine-* are per-job overrides, not gating labels"
	}
	if len(matchable) == 1 {
		missing := j.hasAllLabels(matchable[0])
		if len(missing) == 0 {
			return true, ""
		}
		return false, fmt.Sprintf("missing the label(s) %q", strings.Join(missing, ", "))
	}
	for _, group := range matchable {
		if len(j.hasAllLabels(group)) == 0 {
			return true, ""
		}
	}
	return false, "none of the label groups matched (required one of: " + FormatLabelGroups(matchable) + ")"
}

type Action string

const (
	QUEUED      Action = "queued"
	COMPLETED   Action = "completed"
	IN_PROGRESS Action = "in_progress"
	WAITING     Action = "waiting"
)

type State string

const (
	// running
	PROVISIONING State = "PROVISIONING" // resources are allocated for the VM. The VM is not running yet.
	STAGING      State = "STAGING"      // resources are acquired, and the VM is preparing for first boot.
	RUNNING      State = "RUNNING"      // the VM is booting up or running.
	// stopped
	STOPPING   State = "STOPPING"   // the VM is being stopped. You requested a stop, or a failure occurred. This is a temporary status after which the VM enters the TERMINATED status.
	SUSPENDING State = "SUSPENDING" // the VM is in the process of being suspended. You suspended the VM.
	SUSPENDED  State = "SUSPENDED"  // the VM is in a suspended state. You can resume the VM or delete it.
	TERMINATED State = "TERMINATED" // the VM is stopped. You stopped the VM, or the VM encountered a failure. You can restart or delete the VM.
	// should result in running state
	REPAIRING State = "REPAIRING" // the VM is being repaired. Repairing occurs when the VM encounters an internal error or the underlying machine is unavailable due to maintenance. During this time, the VM is unusable. You are not billed when a VM is in repair. VMs are not covered by the Service level agreement (SLA) while they are in repair. If repair succeeds, the VM returns to one of the above states.
	Unknown   State = "unknown"
)

// isStopped reports whether a VM in this state is stopped (and therefore safe for
// the orphan sweep to reclaim) - never a RUNNING/PROVISIONING VM that may be
// executing a job.
func (s State) isStopped() bool {

	return s == STOPPING || s == SUSPENDING || s == SUSPENDED || s == TERMINATED
}

type InstanceClient struct {
	*compute.InstancesClient
}

func createCallbackUrl(ctx *gin.Context, path string, srcQueryName string, srcQueryValue string) string {

	return "https://" + ctx.Request.Host + path + "?" + srcQueryName + "=" + url.QueryEscape(srcQueryValue)
}

func newComputeClient(ctx context.Context) *InstanceClient {

	if client, err := compute.NewInstancesRESTClient(ctx); err != nil {
		panic(err)
	} else {
		return &InstanceClient{client}
	}
}

func newTaskClient(ctx context.Context) *cloudtasks.Client {

	if client, err := cloudtasks.NewClient(ctx); err != nil {
		panic(err)
	} else {
		return client
	}
}

func newSecretAccessClient(ctx context.Context) *secretmanager.Client {

	if client, err := secretmanager.NewClient(ctx); err != nil {
		panic(err)
	} else {
		return client
	}
}

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyz")

func RandStringRunes(n int) string {

	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

func CalcSigHex(secret []byte, data []byte) string {

	sig := hmac.New(sha256.New, secret)
	sig.Write(data)
	return hex.EncodeToString(sig.Sum(nil))
}

// PickRandomZone returns the deterministic hash-picked zone for the seed (the first
// zone OrderedZones would try). Panics if no zones are configured; all production
// paths use OrderedZones, which is empty-safe.
func (s *Autoscaler) PickRandomZone(seed string) string {

	return s.OrderedZones(seed)[0]
}

// OrderedZones returns every configured zone exactly once, rotated so the first
// element is the deterministic hash-picked zone for the seed. Used to try VM
// creation across zones (capacity fallback) and to locate a VM for deletion
// without assuming which zone it actually landed in.
func (s *Autoscaler) OrderedZones(seed string) []string {

	n := len(s.conf.Zones)
	if n == 0 {
		return []string{}
	}
	hash := sha256.Sum256([]byte(seed))
	start := int(binary.BigEndian.Uint64(hash[:8]) % uint64(n))
	ordered := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ordered = append(ordered, s.conf.Zones[(start+i)%n])
	}
	return ordered
}

// InstanceName builds the deterministic runner/instance name for a job. Deriving
// it from the (globally unique) job id makes create-vm callback retries idempotent:
// a retry targets the same instance name, so a duplicate Insert is detected as
// AlreadyExists instead of spawning a second VM.
func InstanceName(prefix string, jobId int64) string {

	return fmt.Sprintf("%s-%d", prefix, jobId)
}

// IsOwnedRunnerName reports whether name has the "<prefix>-..." shape that
// InstanceName produces, i.e. it looks like a runner VM this autoscaler created.
// The delete-vm callback uses it to guard deletion of the webhook-supplied
// runner_name. A prefix match (rather than the exact per-job name) is required
// because GitHub dispatches by label, not job id, so a VM created for one job
// routinely runs another and reports a different job's id in its name; matching
// only the exact per-job name would skip the delete and leak those VMs. It is
// safe because single-use JIT runners have already exited by the completed
// webhook, so deleting by name cannot kill an active job. Empty (never-assigned)
// and foreign names are rejected.
func IsOwnedRunnerName(prefix string, name string) bool {

	return name != "" && strings.HasPrefix(name, prefix+"-")
}

// CallbackTaskName builds the Cloud Tasks task resource name for a callback,
// namespaced by kind so create and delete callbacks for the same job never collide.
func CallbackTaskName(queue string, kind string, jobId int64, retryCount int) string {

	return fmt.Sprintf("%s/tasks/%s-%d-%d", queue, kind, jobId, retryCount)
}

// IsCapacityError reports whether err is a (retryable) capacity/quota error, i.e.
// the request can be retried in another zone or with on-demand provisioning.
func IsCapacityError(err error) bool {

	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	for _, needle := range []string{
		// covers ZONE_RESOURCE_POOL_EXHAUSTED, RESOURCE_POOL_EXHAUSTED,
		// RESOURCE_EXHAUSTED and the gRPC "ResourceExhausted" form
		"EXHAUSTED",
		"QUOTA_EXCEEDED",
		"QUOTA EXCEEDED",
		"DOES NOT HAVE ENOUGH RESOURCES",
		"STOCKOUT",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// IsAlreadyExists reports whether err indicates the resource already exists (HTTP
// 409 / gRPC AlreadyExists), which create-vm treats as idempotent success.
func IsAlreadyExists(err error) bool {

	if err == nil {
		return false
	}
	if apiErr, ok := err.(*apierror.APIError); ok && apiErr.HTTPCode() == 409 {
		return true
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "ALREADYEXISTS") || strings.Contains(msg, "ALREADY EXISTS")
}

// IsNotFound reports whether err indicates the resource was not found (HTTP 404),
// which delete treats as already-gone.
func IsNotFound(err error) bool {

	if err == nil {
		return false
	}
	if apiErr, ok := err.(*apierror.APIError); ok && apiErr.HTTPCode() == 404 {
		return true
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "NOTFOUND") || strings.Contains(msg, "NOT FOUND")
}

// opContext returns a context for long-running GCP operations (VM create/delete)
// that is decoupled from the inbound HTTP request context. This prevents a Cloud
// Run request-deadline or client cancellation from aborting an in-flight Insert
// (which previously caused a retry to spawn a duplicate VM).
func (s *Autoscaler) opContext() (context.Context, context.CancelFunc) {

	timeout := s.conf.TaskTimeout
	if timeout <= 0 {
		timeout = 180
	}
	return context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
}

// returns http body, "src" query, error
func (s *Autoscaler) verifySignature(ctx *gin.Context) ([]byte, Source, error) {

	if signature := ctx.GetHeader(SHA_HEADER); len(signature) == 71 {
		if body, err := io.ReadAll(ctx.Request.Body); err != nil {
			log.Errorf("Error receiving http body: %s", err.Error())
			return nil, Source{}, ctx.AbortWithError(http.StatusBadRequest, err)
		} else {
			if src, ok := ctx.GetQuery(s.conf.SourceQueryParam); ok {
				if source, ok := s.conf.RegisteredSources[src]; ok {
					calcSignature := CalcSigHex([]byte(source.Secret), body)
					if hmac.Equal([]byte(calcSignature), []byte(signature[7:])) {
						return body, source, nil
					} else {
						log.Warnf("%s signature did not match", ctx.RemoteIP())
						return nil, Source{}, ctx.AbortWithError(http.StatusUnauthorized, fmt.Errorf("unauthorized"))
					}
				} else {
					// Return the same 401 as a bad signature so an unauthenticated
					// caller cannot distinguish "unknown source" from "wrong signature"
					// and enumerate which org/repo names are registered.
					log.Warnf("%s used an unregistered source", ctx.RemoteIP())
					return nil, Source{}, ctx.AbortWithError(http.StatusUnauthorized, fmt.Errorf("unauthorized"))
				}
			} else {
				log.Errorf("Missing %s query parameter", s.conf.SourceQueryParam)
				return nil, Source{}, ctx.AbortWithError(http.StatusBadRequest, fmt.Errorf("missing %s query parameter", s.conf.SourceQueryParam))
			}
		}
	} else {
		log.Warnf("%s did not provide a signature", ctx.RemoteIP())
		return nil, Source{}, ctx.AbortWithError(http.StatusUnauthorized, fmt.Errorf("unauthorized"))
	}
}

/*
func (s *Autoscaler) GetInstanceState(ctx context.Context, instanceName string) (State, error) {

	client := newComputeClient(ctx)
	defer client.Close()
	if res, err := client.Get(ctx, &computepb.GetInstanceRequest{
		Project:  s.conf.ProjectId,
		Zone:     s.conf.Zone,
		Instance: instanceName,
	}); err != nil {
		log.Errorf("Could not get status for instance: %s - %s", instanceName, err.Error())
		return Unknown, err
	} else if res.Status == nil {
		log.Errorf("Could not read status for instance: %s", instanceName)
		return Unknown, fmt.Errorf("instance status is unknown")
	} else {
		return (State)(*res.Status), nil
	}
}

// blocking until instance started or failed to start
func (s *Autoscaler) StartInstance(ctx context.Context, instanceName string) error {

	if s.conf.Simulate {
		log.Infof("(SIMULATE) About to start instance: %s", instanceName)
		time.Sleep(1 * time.Minute)
		log.Infof("(SIMULATE) Started instance: %s", instanceName)
	} else {
		log.Infof("About to start instance: %s", instanceName)
		client := newComputeClient(ctx)
		defer client.Close()
		if res, err := client.Start(ctx, &computepb.StartInstanceRequest{
			Project:  s.conf.ProjectId,
			Zone:     s.conf.Zone,
			Instance: instanceName,
		}); err != nil {
			log.Errorf("Could not start instance: %s - %s", instanceName, err.Error())
			return err
		} else {
			if err := res.Wait(ctx); err != nil {
				log.Errorf("Failed to wait for instance to start: %s", err.Error())
				return err
			} else {
				log.Infof("Started instance: %s", instanceName)
			}
		}
	}
	return nil
}

// blocking until instance stopped or failed to stop
func (s *Autoscaler) StopInstance(ctx context.Context, instanceName string) error {

	log.Debugf("About to stop instance: %s", instanceName)
	client := newComputeClient(ctx)
	defer client.Close()
	if res, err := client.Stop(ctx, &computepb.StopInstanceRequest{
		Project:  s.conf.ProjectId,
		Zone:     s.conf.Zone,
		Instance: instanceName,
	}); err != nil {
		log.Errorf("Could not stop instance: %s - %s", instanceName, err.Error())
		return err
	} else {
		if err := res.Wait(ctx); err != nil {
			log.Errorf("Failed to wait for instance to stop: %s", err.Error())
			return err
		} else {
			log.Infof("Stopped instance: %s", instanceName)
		}
	}
	return nil
}
*/

// realDeleteInZone deletes the instance in one specific zone using the given
// client. It returns whether the instance was found in this zone (the delete was
// accepted) and any error. A 404 means "not in this zone" (found=false, err=nil);
// a delete that is accepted but whose wait fails returns found=true with the error,
// so callers know the instance WAS here and must not fall through to a false
// success (which would leak the VM).
func (s *Autoscaler) realDeleteInZone(ctx context.Context, client *InstanceClient, instanceName string, zone string) (bool, error) {

	res, err := client.Delete(ctx, &computepb.DeleteInstanceRequest{
		Project:  s.conf.ProjectId,
		Zone:     zone,
		Instance: instanceName,
	})
	if err != nil {
		if IsNotFound(err) {
			return false, nil // not in this zone
		}
		return false, err // unknown - let the caller try other zones
	}
	if err := res.Wait(ctx); err != nil {
		return true, err // was here, but deletion/confirmation failed
	}
	return true, nil
}

// blocking until the instance is deleted or the deletion fails.
//
// The instance may live in any configured zone (VM creation falls back across
// zones on capacity errors), so we don't assume the hash-picked zone. We try the
// zones in order, treating a 404 as "not in this zone" and moving on; a successful
// delete in any zone wins. If the instance is found in no zone we treat it as
// already gone (idempotent).
func (s *Autoscaler) DeleteInstance(ctx context.Context, instanceName string) error {

	if s.conf.Simulate {
		log.Debugf("(SIMULATE) About to delete instance %s", instanceName)
		time.Sleep(30 * time.Second)
		log.Infof("(SIMULATE) Deleted instance %s", instanceName)
		return nil
	}

	deleteInZone := s.deleteInZoneFn
	if deleteInZone == nil {
		client := newComputeClient(ctx)
		defer client.Close()
		deleteInZone = func(ctx context.Context, name string, zone string) (bool, error) {
			return s.realDeleteInZone(ctx, client, name, zone)
		}
	}

	var lastErr error
	for _, zone := range s.OrderedZones(instanceName) {
		log.Debugf("About to delete instance %s (%s)", instanceName, zone)
		found, err := deleteInZone(ctx, instanceName, zone)
		if err != nil {
			log.Errorf("Could not delete instance %s (%s): %s", instanceName, zone, err.Error())
			if found {
				// The instance was in this zone but deletion failed - authoritative,
				// surface the error instead of acking the task and leaking the VM.
				return err
			}
			lastErr = err
			continue
		}
		if found {
			log.Infof("Deleted instance %s (%s)", instanceName, zone)
			return nil
		}
		// not in this zone - try the next one
	}

	if lastErr != nil {
		return lastErr
	}
	// We ignore the "not found in any zone" case because the instance may no longer
	// exist, as it may have been terminated prematurely.
	log.Infof("Instance %s already gone (not found in any configured zone)", instanceName)
	return nil
}

// instanceState returns whether an instance with the given name exists in any
// configured zone and, if so, its current state. Used by create-vm to decide
// whether an existing same-named VM is a live runner (idempotent skip) or a
// stopped leftover that must be replaced (see decideCreate).
func (s *Autoscaler) instanceState(ctx context.Context, instanceName string) (bool, State, error) {

	if s.conf.Simulate {
		return false, Unknown, nil
	}
	client := newComputeClient(ctx)
	defer client.Close()
	for _, zone := range s.OrderedZones(instanceName) {
		if inst, err := client.Get(ctx, &computepb.GetInstanceRequest{
			Project:  s.conf.ProjectId,
			Zone:     zone,
			Instance: instanceName,
		}); err == nil {
			return true, State(inst.GetStatus()), nil
		} else if !IsNotFound(err) {
			return false, Unknown, err
		}
	}
	return false, Unknown, nil
}

type createDecision int

const (
	createProceed createDecision = iota // no existing VM - create it
	createSkip                          // a live VM already exists - idempotent success
	createReplace                       // a stopped leftover exists - delete it, then create
)

// decideCreate decides what create-vm should do given whether a same-named instance
// exists and its state. A stopped/terminated leftover (e.g. a runner that shut
// itself down without ever taking a job) must be replaced rather than treated as
// success, otherwise the queued job is stranded. A live VM (running or coming up)
// is left alone so we never duplicate or disturb a runner that may take the job.
func decideCreate(found bool, state State) createDecision {

	if !found {
		return createProceed
	}
	if state.isStopped() {
		return createReplace
	}
	return createSkip
}

// creationAttempt is a single (template, zone, provisioning model) tuple to try.
type creationAttempt struct {
	template          string
	zone              string
	provisioningModel string // "spot" or "standard"
}

// creationPlan returns the ordered list of creation attempts. The primary template
// is tried across EVERY configured zone first; only when the primary is SPOT
// (FallbackInstanceTemplate is set) is the on-demand (STANDARD) template appended,
// again across every zone. This guarantees SPOT is attempted everywhere it is
// feasible before any on-demand fallback. Pure (no I/O) so the ordering is testable.
func (s *Autoscaler) creationPlan(instanceName string) []creationAttempt {

	type tmpl struct{ template, model string }
	primaryModel := "standard"
	if s.conf.FallbackInstanceTemplate != "" {
		// A fallback template only exists when the primary is preemptible/SPOT.
		primaryModel = "spot"
	}
	templates := []tmpl{{s.conf.InstanceTemplate, primaryModel}}
	if s.conf.FallbackInstanceTemplate != "" {
		templates = append(templates, tmpl{s.conf.FallbackInstanceTemplate, "standard"})
	}

	zones := s.OrderedZones(instanceName)
	plan := make([]creationAttempt, 0, len(templates)*len(zones))
	for _, t := range templates {
		for _, zone := range zones {
			plan = append(plan, creationAttempt{template: t.template, zone: zone, provisioningModel: t.model})
		}
	}
	return plan
}

// tryInsertInstance attempts a single Insert+Wait for one creation attempt.
func (s *Autoscaler) tryInsertInstance(ctx context.Context, client *InstanceClient, attempt creationAttempt, instanceName string, machineType *string, metadata []*computepb.Items) error {

	var machine *string
	if machineType != nil {
		machine = proto.String(fmt.Sprintf("zones/%s/machineTypes/%s", attempt.zone, *machineType))
	}
	res, err := client.Insert(ctx, &computepb.InsertInstanceRequest{
		Project: s.conf.ProjectId,
		Zone:    attempt.zone,
		InstanceResource: &computepb.Instance{
			Name:        proto.String(instanceName),
			MachineType: machine,
			Metadata: &computepb.Metadata{
				Items: metadata,
			},
		},
		SourceInstanceTemplate: proto.String(attempt.template),
	})
	if err != nil {
		return err
	}
	return res.Wait(ctx)
}

// blocking until instance started or failed to start.
//
// Creation is hardened against capacity stockouts: we try every configured zone
// (capacity errors fall through to the next zone) and, if the primary template is
// SPOT and every zone is exhausted, we retry the whole sweep with the on-demand
// (STANDARD) template (see creationPlan for the ordering guarantee). A duplicate
// Insert (same deterministic name) is treated as idempotent success. The
// provisioning model that actually succeeded is logged so a log-based metric can
// track the spot-vs-standard split.
func (s *Autoscaler) CreateInstanceFromTemplate(ctx context.Context, instanceName string, machineType *string, metadata ...*computepb.Items) error {

	if s.conf.Simulate {
		log.Debugf("(SIMULATE) About to create instance %s from template", instanceName)
		time.Sleep(1 * time.Minute)
		log.Infof("(SIMULATE) Created instance from template: %s", instanceName)
		return nil
	}

	plan := s.creationPlan(instanceName)
	if len(plan) == 0 {
		// No attempt would run (no zones configured) - surface it instead of
		// silently reporting success.
		return fmt.Errorf("no zones configured - could not create instance %s", instanceName)
	}

	// tryInsert is a seam: tests inject a fake; in production we build one compute
	// client and reuse it across every attempt.
	tryInsert := s.tryInsertFn
	if tryInsert == nil {
		client := newComputeClient(ctx)
		defer client.Close()
		tryInsert = func(ctx context.Context, attempt creationAttempt, name string, mt *string, md []*computepb.Items) error {
			return s.tryInsertInstance(ctx, client, attempt, name, mt, md)
		}
	}

	var lastErr error
	for _, attempt := range plan {
		log.Debugf("About to create instance %s (%s) from %s template", instanceName, attempt.zone, attempt.provisioningModel)
		err := tryInsert(ctx, attempt, instanceName, machineType, metadata)
		if err == nil {
			log.WithFields(log.Fields{
				"instance":           instanceName,
				"zone":               attempt.zone,
				"provisioning_model": attempt.provisioningModel,
			}).Infof("Created instance %s (%s) as %s", instanceName, attempt.zone, attempt.provisioningModel)
			return nil
		}
		if IsAlreadyExists(err) {
			// A previous (possibly cancelled) attempt already created this VM.
			log.Infof("Instance %s already exists - treating create as idempotent success", instanceName)
			return nil
		}
		if IsCapacityError(err) {
			log.Warnf("Capacity error creating instance %s (%s, %s): %s - trying next zone/model", instanceName, attempt.zone, attempt.provisioningModel, err.Error())
			lastErr = err
			continue
		}
		// Non-retryable error (bad template, permission, invalid machine type, ...).
		log.Errorf("Could not create instance %s (%s) from %s template: %s", instanceName, attempt.zone, attempt.provisioningModel, err.Error())
		return err
	}

	log.Errorf("Exhausted all zones/provisioning models creating instance %s: %v", instanceName, lastErr)
	return lastErr
}

// sweepOrphans deletes runner VMs that are stopped/terminated (e.g. a runner that
// shut itself down without ever picking up a job, which produces no completed
// webhook and would otherwise linger). Best-effort: errors are logged, not returned.
func (s *Autoscaler) sweepOrphans(ctx context.Context) {

	if s.conf.Simulate {
		return
	}
	client := newComputeClient(ctx)
	defer client.Close()

	deleted := 0
	for _, zone := range s.conf.Zones {
		it := client.List(ctx, &computepb.ListInstancesRequest{
			Project: s.conf.ProjectId,
			Zone:    zone,
			Filter:  proto.String(fmt.Sprintf("name eq ^%s-.*", s.conf.RunnerPrefix)),
		})
		for {
			if deleted >= maxSweepDeletes {
				return
			}
			instance, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				log.Warnf("Orphan sweep: could not list instances in %s: %s", zone, err.Error())
				break
			}
			status := instance.GetStatus()
			// Only reclaim instances that are stopped - never a RUNNING/PROVISIONING
			// VM that may be executing (or about to execute) a job. max_run_duration
			// remains the backstop for runaway RUNNING VMs.
			if State(status).isStopped() {
				name := instance.GetName()
				log.Infof("Orphan sweep: reclaiming stopped runner VM %s (%s, status %s)", name, zone, status)
				// Delete in the zone we just listed it from, reusing this client.
				if _, err := s.realDeleteInZone(ctx, client, name, zone); err != nil {
					log.Warnf("Orphan sweep: failed to delete %s: %s", name, err.Error())
				} else {
					deleted++
				}
			}
		}
	}
}

// maybeSweepOrphans runs sweepOrphans at most once per sweepInterval. It is called
// opportunistically from the create/delete callbacks so cleanup happens without any
// always-on scheduler, while keeping the hot path cheap.
func (s *Autoscaler) maybeSweepOrphans() {

	s.sweepMu.Lock()
	if !s.lastSweep.IsZero() && time.Since(s.lastSweep) < sweepInterval {
		s.sweepMu.Unlock()
		return
	}
	s.lastSweep = time.Now()
	s.sweepMu.Unlock()

	ctx, cancel := s.opContext()
	defer cancel()
	s.sweepOrphans(ctx)
}

func (s *Autoscaler) readPat(ctx context.Context) (string, error) {

	log.Debugf("About to read PAT from secret version: %s", s.conf.SecretVersion)
	secretAccessClient := newSecretAccessClient(ctx)
	defer secretAccessClient.Close()
	if secretResult, err := secretAccessClient.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: s.conf.SecretVersion,
	}); err != nil {
		log.Errorf("Could not access GitHub PAT secret version %s: %s", s.conf.SecretVersion, err.Error())
		return "", fmt.Errorf("missing GitHub PAT")
	} else {
		if pat := string(secretResult.Payload.Data); len(pat) == 0 {
			log.Errorf("The GitHub PAT secret is empty")
			return "", fmt.Errorf("empty GitHub PAT")
		} else {
			return pat, nil
		}
	}
}

// A jit-config needs: RunnerName, RunnerGroupId, Labels, WorkFolder
func (s *Autoscaler) GenerateRunnerJitConfig(ctx context.Context, url string, runnerName string, runnerGroupId int64, labels []string) (string, error) {

	log.Debugf("About to request GitHub runner %s jit config from %s (runner group %d) using PAT from secret version: %s", runnerName, url, runnerGroupId, s.conf.SecretVersion)
	secretAccessClient := newSecretAccessClient(ctx)
	defer secretAccessClient.Close()
	if pat, err := s.readPat(ctx); err != nil {
		return "", err
	} else {
		reqPayload := map[string]any{}
		reqPayload["name"] = runnerName
		reqPayload["runner_group_id"] = runnerGroupId
		reqPayload["labels"] = labels
		reqPayload["work_folder"] = "_work"
		data, _ := json.Marshal(reqPayload)
		if req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data)); err != nil {
			log.Errorf("Could not create GitHub runner jit-config request")
			return "", fmt.Errorf("failed jit-config request")
		} else {
			req.Header.Add("Accept", "application/vnd.github+json")
			req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", pat))
			req.Header.Add("X-GitHub-Api-Version", GITHUB_API_VERSION)
			req.Header.Add("User-Agent", "github-runner-autoscaler")
			if resp, err := http.DefaultClient.Do(req); err != nil {
				log.Errorf("GitHub runner jit-config request failed: %s", err.Error())
				return "", fmt.Errorf("failed jit-config response")
			} else if resp.StatusCode != 201 {
				log.Errorf("GitHub runner jit-config request unsuccessful: %s", resp.Status)
				defer resp.Body.Close()
				return "", fmt.Errorf("failed jit-config response")
			} else {
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				payload := map[string]any{}
				if err := json.Unmarshal(body, &payload); err != nil {
					log.Errorf("GitHub runner jit-config response missing: %s", err.Error())
					return "", fmt.Errorf("failed jit-config response")
				} else if jitConfig, ok := payload["encoded_jit_config"].(string); ok && len(jitConfig) > 0 {
					return jitConfig, nil
				} else {
					log.Errorf("GitHub runner jit-config is empty")
					return "", fmt.Errorf("failed jit-config response")
				}
			}
		}
	}
}

func (s *Autoscaler) CreateCallbackTaskWithToken(ctx context.Context, kind string, url string, secret string, job Job, delay time.Duration) error {

	data, _ := json.Marshal(job)
	now := timestamppb.Now()
	now.Seconds += int64(delay.Seconds())
	req := &taskspb.CreateTaskRequest{
		Parent: s.conf.TaskQueue,
		Task: &taskspb.Task{
			// the timeout of the cloud task callback - must be greater the time it takes to start/delete the VM
			DispatchDeadline: &durationpb.Duration{
				Seconds: s.conf.TaskTimeout + 5, // short buffer so cloud run timeout ends before cloud task timeout
				Nanos:   0,
			},
			ScheduleTime: now,
			MessageType: &taskspb.Task_HttpRequest{
				HttpRequest: &taskspb.HttpRequest{
					HttpMethod: taskspb.HttpMethod_POST,
					Url:        url,
					Headers: map[string]string{
						SHA_HEADER: SHA_PREFIX + CalcSigHex([]byte(secret), []byte(data)),
					},
				},
			},
		},
	}
	req.Task.GetHttpRequest().Body = []byte(data)

	client := newTaskClient(ctx)
	defer client.Close()

	var sendAndRetry func(int) error
	sendAndRetry = func(retryCount int) error {
		name := CallbackTaskName(s.conf.TaskQueue, kind, job.Id, retryCount)
		req.Task.Name = name
		if _, err := client.CreateTask(ctx, req); err != nil {
			if IsAlreadyExists(err) {
				// Cloud Tasks returns ALREADY_EXISTS both for a still-active task and
				// for a recently deleted/executed (tombstoned) name. Only bump to a
				// fresh suffix for the tombstone case: if the task is still active the
				// callback is already queued, and minting another would duplicate the
				// VM. GetTask distinguishes the two (active -> found, tombstoned -> 404).
				if _, getErr := client.GetTask(ctx, &taskspb.GetTaskRequest{Name: name}); getErr == nil {
					log.Infof("Cloud task callback for job Id %d already queued (%s) - not duplicating", job.Id, name)
					return nil
				} else if IsNotFound(getErr) && retryCount < maxTaskRetryCount {
					return sendAndRetry(retryCount + 1)
				}
			}
			return fmt.Errorf("cloudtasks.CreateTask failed for job Id %d: %v", job.Id, err)
		} else {
			log.Infof("Created cloud task callback for workflow job Id %d with url \"%s\" and payload \"%s\"", job.Id, url, data)
			return nil
		}
	}

	return sendAndRetry(0)
}

// DeleteCallbackTask cancels the pending create-vm callback for a job (used when a
// job transitions to 'waiting' or is cancelled before the VM is created). The live
// create task may carry any retry suffix (0..maxTaskRetryCount) because
// CreateCallbackTaskWithToken bumps the suffix on AlreadyExists, so we attempt to
// delete every candidate. Best-effort: a not-found candidate is not an error.
func (s *Autoscaler) DeleteCallbackTask(ctx context.Context, job Job) error {

	client := newTaskClient(ctx)
	defer client.Close()

	var lastErr error
	deletedAny := false
	for retryCount := 0; retryCount <= maxTaskRetryCount; retryCount++ {
		err := client.DeleteTask(ctx, &taskspb.DeleteTaskRequest{
			Name: CallbackTaskName(s.conf.TaskQueue, TaskKindCreate, job.Id, retryCount),
		})
		if err == nil {
			deletedAny = true
		} else if !IsNotFound(err) {
			lastErr = err
		}
	}
	if deletedAny {
		log.Infof("Deleted cloud task callback for workflow job Id %d", job.Id)
	}
	if lastErr != nil {
		return fmt.Errorf("cloudtasks.DeleteTask failed for job Id %d: %v", job.Id, lastErr)
	}
	return nil
}

const runner_script_wrapper = `
#!/bin/bash
val=$(curl "http://metadata.google.internal/computeMetadata/v1/instance/attributes/%s" -H "Metadata-Flavor: Google")
curl "http://metadata.google.internal/computeMetadata/v1/project/attributes/%s" -H "Metadata-Flavor: Google" > runner_startup.sh
sed -i 's/\r$//' ./runner_startup.sh
chmod +x ./runner_startup.sh
./runner_startup.sh $val
rm runner_startup.sh
`

// shutdown_script_wrapper runs on GCE shutdown (preemption, `shutdown now`, instance delete).
// All five metadata fetches run in parallel via temp files (a `VAR=$(...) &` background
// assignment would be lost in its subshell), bounding the fetch phase at ~3s of the ~30s
// preemption budget instead of 15s sequentially. Every step is fail-open - missing files
// yield empty args, which the shutdown script treats as "skip the callback" - so a
// metadata hiccup never blocks shutdown.
// Five %s placeholders: RECREATE_CALLBACK_URL_ATTR, RECREATE_CALLBACK_PAYLOAD_ATTR,
// RECREATE_CALLBACK_SIG_ATTR, RECREATE_CALLBACK_JOB_PATTERN_ATTR, RUNNER_SCRIPT_SHUTDOWN_ATTR.
const shutdown_script_wrapper = `#!/bin/bash
md() { curl -sf --max-time 3 "http://metadata.google.internal/computeMetadata/v1/$1" -H "Metadata-Flavor: Google" -o "$2"; }
md "instance/attributes/%s" /tmp/recreate_cb_url &
md "instance/attributes/%s" /tmp/recreate_cb_payload &
md "instance/attributes/%s" /tmp/recreate_cb_sig &
md "instance/attributes/%s" /tmp/recreate_cb_pattern &
md "project/attributes/%s" /tmp/runner_shutdown.sh &
wait
[ -s /tmp/runner_shutdown.sh ] || exit 0
sed -i 's/\r$//' /tmp/runner_shutdown.sh
chmod +x /tmp/runner_shutdown.sh
/tmp/runner_shutdown.sh "$(cat /tmp/recreate_cb_url 2>/dev/null)" "$(cat /tmp/recreate_cb_payload 2>/dev/null)" "$(cat /tmp/recreate_cb_sig 2>/dev/null)" "$(cat /tmp/recreate_cb_pattern 2>/dev/null)"
rm -f /tmp/runner_shutdown.sh /tmp/recreate_cb_url /tmp/recreate_cb_payload /tmp/recreate_cb_sig /tmp/recreate_cb_pattern
`

// shutdownScriptValue is constant across all VM creations - render it once.
var shutdownScriptValue = fmt.Sprintf(shutdown_script_wrapper,
	RECREATE_CALLBACK_URL_ATTR,
	RECREATE_CALLBACK_PAYLOAD_ATTR,
	RECREATE_CALLBACK_SIG_ATTR,
	RECREATE_CALLBACK_JOB_PATTERN_ATTR,
	RUNNER_SCRIPT_SHUTDOWN_ATTR,
)

// createVmWithJitConfig generates a JIT runner config and creates the VM. ginCtx is
// used only to write the HTTP response; all GCP operations run on opCtx, which is
// decoupled from the request so a Cloud Run request-deadline cancellation can't
// abort an in-flight create (and cause a duplicate VM on retry).
func (s *Autoscaler) createVmWithJitConfig(ginCtx *gin.Context, opCtx context.Context, url string, runnerGroupId int64, settings VmSettings, labels []string, job Job, src Source) {

	// Idempotency: inspect any existing VM with this job's deterministic name. A live
	// runner means this callback already did its job (don't regenerate a JIT config /
	// duplicate it); a stopped leftover (e.g. a runner that shut down without taking
	// the job) must be deleted and recreated so the queued job isn't stranded.
	instanceState := s.instanceStateFn
	if instanceState == nil {
		instanceState = s.instanceState
	}
	found, state, err := instanceState(opCtx, settings.Name)
	if err != nil {
		ginCtx.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	switch decideCreate(found, state) {
	case createSkip:
		log.Infof("Instance %s already exists and is %s - create-vm callback is idempotent, nothing to do", settings.Name, state)
		ginCtx.Status(http.StatusOK)
		return
	case createReplace:
		log.Infof("Instance %s exists but is stopped (%s) - deleting and recreating so the job isn't stranded", settings.Name, state)
		if err := s.DeleteInstance(opCtx, settings.Name); err != nil {
			ginCtx.AbortWithError(http.StatusInternalServerError, err)
			return
		}
	}

	generateJitConfig := s.jitConfigFn
	if generateJitConfig == nil {
		generateJitConfig = s.GenerateRunnerJitConfig
	}
	if jitConfig, err := generateJitConfig(opCtx, url, settings.Name, runnerGroupId, labels); err != nil {
		ginCtx.AbortWithError(http.StatusInternalServerError, err)
	} else {
		jit_config_attr := fmt.Sprintf("%s_%s", RUNNER_JIT_CONFIG_ATTR, RandStringRunes(16))
		metadata := []*computepb.Items{
			{
				Key:   proto.String(jit_config_attr),
				Value: proto.String(jitConfig),
			},
			{
				Key:   proto.String("startup-script"),
				Value: proto.String(fmt.Sprintf(runner_script_wrapper, jit_config_attr, RUNNER_SCRIPT_REGISTER_JIT_RUNNER_ATTR)),
			},
		}
		if s.conf.RouteRecreateVm != "" {
			metadata = append(metadata, s.recreateMetadata(ginCtx, job, src)...)
		} else {
			// Without a recreate route the shutdown callback can't exist: a VM that
			// dies before accepting a job will not be replaced. Loud on purpose -
			// this is almost always a misconfiguration, not a choice.
			log.Warnf("RouteRecreateVm is not configured - creating %s without the recreate shutdown callback", settings.Name)
		}
		if err := s.CreateInstanceFromTemplate(opCtx, settings.Name, settings.MachineType, metadata...); err != nil {
			ginCtx.AbortWithError(http.StatusInternalServerError, err)
		} else {
			ginCtx.Status(http.StatusOK)
		}
	}
}

// recreateMetadata builds the shutdown-script wrapper and the per-instance attributes
// it consumes, so a VM that shuts down without ever accepting a job can post a signed
// recreate callback. jobJSON is both the stored payload and the signed data so the
// recreate handler can verify origin with the regular webhook signature check.
func (s *Autoscaler) recreateMetadata(ginCtx *gin.Context, job Job, src Source) []*computepb.Items {

	jobJSON, _ := json.Marshal(job)
	recreateUrl := createCallbackUrl(ginCtx, s.conf.RouteRecreateVm, s.conf.SourceQueryParam, src.Name)
	recreateSig := CalcSigHex([]byte(src.Secret), jobJSON)
	// An empty grep pattern would match everything, so never store empty.
	// Coupled to the Terraform variable runner_job_log_pattern.
	jobPattern := s.conf.RunnerJobLogPattern
	if jobPattern == "" {
		jobPattern = DefaultRunnerJobLogPattern
	}
	return []*computepb.Items{
		{
			Key:   proto.String("shutdown-script"),
			Value: proto.String(shutdownScriptValue),
		},
		{
			Key:   proto.String(RECREATE_CALLBACK_URL_ATTR),
			Value: proto.String(recreateUrl),
		},
		{
			Key:   proto.String(RECREATE_CALLBACK_PAYLOAD_ATTR),
			Value: proto.String(string(jobJSON)),
		},
		{
			Key:   proto.String(RECREATE_CALLBACK_SIG_ATTR),
			Value: proto.String(recreateSig),
		},
		{
			Key:   proto.String(RECREATE_CALLBACK_JOB_PATTERN_ATTR),
			Value: proto.String(jobPattern),
		},
	}
}

func (s *Autoscaler) handleCreateVm(ctx *gin.Context) {

	log.Info("Received create-vm cloud task callback")
	if data, src, err := s.verifySignature(ctx); err == nil {
		job := Job{}
		if err := json.Unmarshal(data, &job); err != nil || job.Id == 0 {
			// Corrupt or missing payload: retrying can't help, so ack with 200.
			log.Warnf("Create-vm callback has invalid or zero-id job payload - skipping")
			ctx.Status(http.StatusOK)
			return
		}
		opCtx, cancel := s.opContext()
		defer cancel()
		// Deterministic name derived from the job id makes create-vm retries idempotent.
		settings := VmSettings{
			Name:        InstanceName(s.conf.RunnerPrefix, job.Id),
			MachineType: job.GetMagicLabelValue(MagicLabelMachine),
		}
		switch src.SourceType {
		case TypeEnterprise:
			log.Infof("Using jit config for runner registration for enterprise: %s", src.Name)
			s.createVmWithJitConfig(ctx, opCtx, fmt.Sprintf(RUNNER_ENTERPRISE_JIT_CONFIG_ENDPOINT, src.Name), s.conf.RunnerGroupId, settings, job.Labels, job, src)
		case TypeOrganization:
			log.Infof("Using jit config for runner registration for organization: %s", src.Name)
			s.createVmWithJitConfig(ctx, opCtx, fmt.Sprintf(RUNNER_ORG_JIT_CONFIG_ENDPOINT, src.Name), s.conf.RunnerGroupId, settings, job.Labels, job, src)
		case TypeRepository:
			log.Infof("Using jit config for runner registration for repository: %s", src.Name)
			// for repositories there is an implicit runner group with id 1
			s.createVmWithJitConfig(ctx, opCtx, fmt.Sprintf(RUNNER_REPO_JIT_CONFIG_ENDPOINT, src.Name), 1, settings, job.Labels, job, src)
		default:
			log.Errorf("Missing source type for %s", src.Name)
			ctx.Status(http.StatusBadRequest)
		}
		// The orphan sweep runs from the delete-vm callback instead (completed jobs
		// are the common case), to keep this create hot path free of extra latency.
	}
}

func (s *Autoscaler) handleDeleteVm(ctx *gin.Context) {

	log.Info("Received delete-vm cloud task callback")
	if data, _, err := s.verifySignature(ctx); err == nil {
		job := Job{}
		if err := json.Unmarshal(data, &job); err != nil || job.Id == 0 {
			// Corrupt or missing payload: retrying can't help, so ack with 200.
			log.Warnf("Delete-vm callback has invalid or zero-id job payload - skipping")
			ctx.Status(http.StatusOK)
			return
		}
		if !IsOwnedRunnerName(s.conf.RunnerPrefix, job.RunnerName) {
			// Either empty (the job was never picked up, e.g. cancelled while queued)
			// or a name that isn't one of our runners. Don't issue a delete for an
			// empty/foreign name (which could target an unrelated instance);
			// acknowledge so the task isn't retried, and let the sweep handle cleanup.
			log.Infof("Delete-vm callback runner name %q is empty or not one of our runners (job %d) - skipping delete", job.RunnerName, job.Id)
			ctx.Status(http.StatusOK)
			go s.maybeSweepOrphans()
			return
		}
		opCtx, cancel := s.opContext()
		defer cancel()
		if err := s.DeleteInstance(opCtx, job.RunnerName); err != nil {
			ctx.AbortWithError(http.StatusInternalServerError, err)
		} else {
			ctx.Status(http.StatusOK)
		}
		// Run the orphan sweep detached so it can't hold the callback open past the
		// Cloud Tasks dispatch deadline (which would trigger a retry). It uses its own
		// background context and is throttled + mutex-guarded.
		go s.maybeSweepOrphans()
	}
}

func (s *Autoscaler) handleRecreateVm(ctx *gin.Context) {

	log.Info("Received recreate-vm callback from a shutting-down runner VM")
	if data, src, err := s.verifySignature(ctx); err == nil {
		job := Job{}
		if err := json.Unmarshal(data, &job); err != nil || job.Id == 0 {
			// Corrupt or missing payload: retrying can't help, so ack with 200.
			log.Warnf("Recreate-vm callback has invalid or zero-id job payload - skipping")
			ctx.Status(http.StatusOK)
			return
		}
		log.Infof("Recreate-vm callback for job %d - VM died before accepting a job, re-enqueueing create task", job.Id)
		createUrl := createCallbackUrl(ctx, s.conf.RouteCreateVm, s.conf.SourceQueryParam, src.Name)

		createTask := s.createTaskFn
		if createTask == nil {
			createTask = s.CreateCallbackTaskWithToken
		}
		// The enqueue runs on opCtx, not the request context: the dying VM's curl has
		// a short timeout and never retries, so a request-deadline cancellation
		// mid-enqueue would silently lose the only recreate signal for this job.
		opCtx, cancel := s.opContext()
		defer cancel()
		// CreateCallbackTaskWithToken's tombstone suffix bumping caps total creates
		// per job id - this is the recreate loop protection.
		if err := createTask(opCtx, TaskKindCreate, createUrl, src.Secret, job, recreateVmDelay); err != nil {
			log.Errorf("Can not enqueue create-vm cloud task callback for recreate: %s", err.Error())
			ctx.AbortWithError(http.StatusInternalServerError, err)
			return
		}
		ctx.Status(http.StatusOK)
	}
}

func (s *Autoscaler) handleWebhook(ctx *gin.Context) {

	log.Info("Received webhook")
	if data, src, err := s.verifySignature(ctx); err == nil {
		event := ctx.GetHeader(EVENT_HEADER)
		log.Info(string(data))
		if event == WEBHOOK_PING_EVENT {
			log.Info("Webhook ping acknowledged")
			ctx.Status(http.StatusOK)
		} else if event == WEBHOOK_JOB_EVENT {
			payload := Payload{}
			if err := json.Unmarshal(data, &payload); err != nil {
				log.Errorf("Can not unmarshal payload - is the webhook content type set to \"application/json\"? %s", err.Error())
				ctx.AbortWithError(http.StatusBadRequest, err)
			} else {
				// Copy the top-level repository into the job so it survives the Cloud
				// Tasks round-trip (the task body is the marshaled Job). Used for
				// diagnostics and future job-status lookup.
				payload.Job.RepositoryFullName = payload.Repository.FullName
				if payload.Action == QUEUED {
					if payload.Job.HasLegacyMagicLabel() {
						// Skip the create-vm callback: a runner we spawn for this job can
						// never match its `runs-on` (see matchLegacyMagicLabel).
						log.Warnf("Job %d uses deprecated magic-label syntax (@machine:<type>). Use \"gce-machine-<type>\" (e.g. \"gce-machine-c2d-standard-16\") instead. See README \u2192 Magic Labels. Skipping VM creation; the job will time out.", payload.Job.Id)
					} else if ok, reason := payload.Job.HasAnyLabelGroup(s.conf.RunnerLabelGroups); ok {
						createUrl := createCallbackUrl(ctx, s.conf.RouteCreateVm, s.conf.SourceQueryParam, src.Name)
						// delay the create vm callback so we have a chance to delete it if the workflow job is changing its state to 'waiting'
						if err := s.CreateCallbackTaskWithToken(ctx, TaskKindCreate, createUrl, src.Secret, payload.Job, time.Duration(s.conf.CreateVmDelay)*time.Second); err != nil {
							log.Errorf("Can not enqueue create-vm cloud task callback: %s", err.Error())
							ctx.AbortWithError(http.StatusInternalServerError, err)
							return
						}
					} else {
						log.Warnf("Webhook requested to start a runner: %s - ignoring", reason)
					}
				} else if payload.Action == WAITING {
					// the waiting action happens if a deployment environment is configured in the workflow that requires a review. We have to cancel the cloud task callback
					if ok, reason := payload.Job.HasAnyLabelGroup(s.conf.RunnerLabelGroups); ok {
						if err := s.DeleteCallbackTask(ctx, payload.Job); err != nil {
							// best effort - this is not considered an error
							log.Warnf("Can not delete create-vm cloud task callback: %s", err.Error())
						}
					} else {
						log.Warnf("Webhook signals 'wait': %s - ignoring", reason)
					}
				} else if payload.Action == COMPLETED {
					runnerGroupId := s.conf.RunnerGroupId
					if src.SourceType == TypeRepository {
						runnerGroupId = 1
					}
					if payload.Job.RunnerGroupId == runnerGroupId {
						if ok, reason := payload.Job.HasAnyLabelGroup(s.conf.RunnerLabelGroups); ok {

							// if the user immediately cancels a workflow we have the chance to delete the callback if not older than 10 seconds - best effort, ignore all errors
							if err := s.DeleteCallbackTask(ctx, payload.Job); err != nil {
								log.Warnf("Can not delete create-vm cloud task callback: %s", err.Error())
							}

							deleteUrl := createCallbackUrl(ctx, s.conf.RouteDeleteVm, s.conf.SourceQueryParam, src.Name)
							if err := s.CreateCallbackTaskWithToken(ctx, TaskKindDelete, deleteUrl, src.Secret, payload.Job, 1*time.Second); err != nil {
								log.Errorf("Can not enqueue delete-vm cloud task callback: %s", err.Error())
								ctx.AbortWithError(http.StatusInternalServerError, err)
								return
							}
						} else {
							log.Warnf("Webhook signaled to delete a runner: %s - ignoring", reason)
						}
					} else {
						log.Warnf("Webhook signaled to delete a runner that does not belong to the expected runner group (expected \"%d\" got \"%d\") - ignoring", runnerGroupId, payload.Job.RunnerGroupId)
					}
				}
				ctx.Status(http.StatusOK)
			}
		} else {
			log.Infof("Unknown GitHub webhook event \"%s\" received - ignoring", event)
			ctx.Status(http.StatusOK)
		}
	}
}

type Pair struct {
	Name   string
	Secret string
}

func (p Pair) IsIValid() bool {
	return len(p.Name) > 0 && len(p.Secret) > 0
}

type AutoscalerConfig struct {
	RouteWebhook     string
	RouteCreateVm    string
	RouteDeleteVm    string
	RouteRecreateVm  string
	ProjectId        string
	Zones            []string
	TaskQueue        string
	TaskTimeout      int64
	InstanceTemplate string
	// FallbackInstanceTemplate is the on-demand (STANDARD) template tried when the
	// primary (SPOT) template is capacity-exhausted in every zone. Empty when the
	// primary is already on-demand (no fallback needed).
	FallbackInstanceTemplate string
	SecretVersion            string
	RunnerPrefix             string
	RunnerGroupId            int64
	RunnerLabelGroups        [][]string
	RegisteredSources        map[string]Source
	SourceQueryParam         string
	CreateVmDelay            int64
	RunnerJobLogPattern      string
	Simulate                 bool
}

// Validate checks startup invariants that, if violated, would leave the
// autoscaler running in a silently broken state. Call it during config load and
// fail fast on error.
func (c AutoscalerConfig) Validate() error {

	// A non-empty prefix is load-bearing: InstanceName builds "<prefix>-<jobId>"
	// (an empty prefix yields "-<jobId>", an invalid GCE instance name that the
	// create API rejects) and IsOwnedRunnerName uses it to recognise the VMs we
	// own. An empty prefix breaks both create and delete, so refuse to start.
	if c.RunnerPrefix == "" {
		return fmt.Errorf("RunnerPrefix must not be empty")
	}
	return nil
}

type Autoscaler struct {
	engine *gin.Engine
	conf   AutoscalerConfig

	sweepMu   sync.Mutex
	lastSweep time.Time

	// tryInsertFn / deleteInZoneFn are test seams for intercepting individual
	// VM-creation / per-zone-delete attempts. nil in production, where the real
	// compute-client-backed implementations are used.
	tryInsertFn    func(ctx context.Context, attempt creationAttempt, instanceName string, machineType *string, metadata []*computepb.Items) error
	deleteInZoneFn func(ctx context.Context, instanceName string, zone string) (bool, error)

	// createTaskFn / jitConfigFn / instanceStateFn are test seams for Cloud Tasks
	// enqueue, JIT-config generation, and instance-state lookup. nil in production.
	createTaskFn    func(ctx context.Context, kind string, url string, secret string, job Job, delay time.Duration) error
	jitConfigFn     func(ctx context.Context, url string, runnerName string, runnerGroupId int64, labels []string) (string, error)
	instanceStateFn func(ctx context.Context, instanceName string) (bool, State, error)
}

func NewAutoscaler(config AutoscalerConfig) *Autoscaler {

	engine := gin.New()

	scaler := Autoscaler{
		engine: engine,
		conf:   config,
	}
	engine.Use(ginlogrus.Logger(log.WithFields(log.Fields{})))
	engine.POST(config.RouteCreateVm, scaler.handleCreateVm)
	engine.POST(config.RouteDeleteVm, scaler.handleDeleteVm)
	engine.POST(config.RouteWebhook, scaler.handleWebhook)
	// Register the recreate-vm route only when configured, for backward
	// compatibility with callers that don't set RouteRecreateVm.
	if config.RouteRecreateVm != "" {
		engine.POST(config.RouteRecreateVm, scaler.handleRecreateVm)
	}
	engine.GET("/healthcheck", func(ctx *gin.Context) { ctx.Status(http.StatusOK) })
	return &scaler
}

func (s *Autoscaler) Srv(port int) {

	s.engine.Run(fmt.Sprintf("0.0.0.0:%d", port))
}
