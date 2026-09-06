package pkg

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Autoscaler) readBody(c *gin.Context) ([]byte, error) {
	limit := s.conf.MaxRequestBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, limit))
	if err != nil {
		status := http.StatusBadRequest
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			status = http.StatusRequestEntityTooLarge
		}
		c.AbortWithStatus(status)
	}
	return body, err
}
func (s *Autoscaler) privateRequest(c *gin.Context) bool {
	token := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	if token == "" || s.tokenValidator == nil {
		c.AbortWithStatus(401)
		return false
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	payload, err := s.tokenValidator.Validate(ctx, token, s.conf.CallbackBaseURL)
	if err != nil || payload.Claims["email"] != s.conf.CallbackServiceAccount || payload.Claims["email_verified"] != true || (payload.Issuer != "https://accounts.google.com" && payload.Issuer != "accounts.google.com") {
		c.AbortWithStatus(401)
		return false
	}
	return true
}
func (s *Autoscaler) callbackURL(path, source string) string {
	return strings.TrimRight(s.conf.CallbackBaseURL, "/") + path + "?" + url.QueryEscape(s.conf.SourceQueryParam) + "=" + url.QueryEscape(source)
}
func (s *Autoscaler) queue(ctx context.Context, route, source string, payload interface{}, delay time.Duration) error {
	if s.queueFn != nil {
		return s.queueFn(ctx, route, source, payload, delay)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	queue := s.conf.TaskQueue
	if route == s.conf.RouteDeleteVm {
		queue = s.conf.DeleteTaskQueue
	}
	if route == "/reconcile" || route == "/sweep" || route == "/discover" {
		queue = s.conf.MaintenanceTaskQueue
	}
	// Deduplicate bursts, not the lifetime of a job. Durable demand is re-enqueued
	// each reconciliation interval even after a previous task exhausted retries.
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s:%d", route, source, data, time.Now().Unix()/60))))
	client, closeClient := s.tasks(ctx)
	defer closeClient()
	rpc, cancel := boundTaskRPCContext(ctx)
	defer cancel()
	_, err = client.CreateTask(rpc, &taskspb.CreateTaskRequest{Parent: queue, Task: &taskspb.Task{
		Name: queue + "/tasks/" + key, ScheduleTime: timestamppb.New(time.Now().Add(delay)),
		DispatchDeadline: durationpb.New(time.Duration(s.conf.TaskTimeout+5) * time.Second),
		MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{
			HttpMethod: taskspb.HttpMethod_POST, Url: s.callbackURL(route, source), Body: data,
			Headers:             map[string]string{"Content-Type": "application/json"},
			AuthorizationHeader: &taskspb.HttpRequest_OidcToken{OidcToken: &taskspb.OidcToken{ServiceAccountEmail: s.conf.CallbackServiceAccount, Audience: s.conf.CallbackBaseURL}},
		}},
	}})
	if IsAlreadyExists(err) {
		return nil
	}
	return err
}
func (s *Autoscaler) durableWebhook(c *gin.Context) {
	data, src, err := s.verifySignature(c)
	if err != nil {
		return
	}
	if c.GetHeader(EVENT_HEADER) == WEBHOOK_PING_EVENT {
		c.Status(200)
		return
	}
	if c.GetHeader(EVENT_HEADER) != WEBHOOK_JOB_EVENT {
		c.Status(200)
		return
	}
	var p Payload
	if json.Unmarshal(data, &p) != nil || p.Job.Id <= 0 || p.Repository.FullName == "" {
		c.AbortWithStatus(400)
		return
	}
	p.Job.RepositoryFullName = p.Repository.FullName
	if ok, _ := p.Job.HasAnyLabelGroup(s.conf.RunnerLabelGroups); !ok {
		c.Status(200)
		return
	}
	if p.Job.HasLegacyMagicLabel() {
		c.AbortWithStatus(422)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	if err = s.observe(ctx, src, p.Job, p.Action == COMPLETED); err != nil {
		c.AbortWithError(503, err)
		return
	}
	// The state write is the durable acknowledgement; enqueue is a latency
	// optimization. A failed enqueue remains visible to the scheduled reconciler.
	switch p.Action {
	case QUEUED:
		err = s.queue(ctx, s.conf.RouteCreateVm, src.Name, p.Job, time.Duration(s.conf.CreateVmDelay)*time.Second)
	case COMPLETED:
		if IsOwnedRunnerName(s.conf.RunnerPrefix, p.Job.RunnerName) {
			err = s.queue(ctx, s.conf.RouteDeleteVm, src.Name, p.Job, 0)
		}
	}
	if err != nil {
		log.WithField("job_id", p.Job.Id).Errorf("Lifecycle enqueue deferred to reconciliation: %v", err)
	}
	log.WithFields(log.Fields{"job_id": p.Job.Id, "action": p.Action}).Info("Recorded workflow job")
	c.Status(200)
}
func (s *Autoscaler) workerJob(c *gin.Context) (Job, Source, bool) {
	if !s.privateRequest(c) {
		return Job{}, Source{}, false
	}
	data, err := s.readBody(c)
	if err != nil {
		return Job{}, Source{}, false
	}
	var job Job
	src, ok := s.conf.RegisteredSources[c.Query(s.conf.SourceQueryParam)]
	if !ok || json.Unmarshal(data, &job) != nil || job.Id <= 0 {
		c.AbortWithStatus(400)
		return job, src, false
	}
	return job, src, true
}
func (s *Autoscaler) durableCreate(c *gin.Context) {
	job, src, ok := s.workerJob(c)
	if !ok {
		return
	}
	ctx, cancel := s.opContext()
	defer cancel()
	if err := s.processJob(ctx, src, job); err != nil {
		log.WithField("job_id", job.Id).Errorf("Lifecycle create failed: %v", err)
		c.AbortWithError(503, err)
		return
	}
	c.Status(200)
}
func (s *Autoscaler) durableDelete(c *gin.Context) {
	job, src, ok := s.workerJob(c)
	if !ok {
		return
	}
	if !IsOwnedRunnerName(s.conf.RunnerPrefix, job.RunnerName) {
		c.Status(200)
		return
	}
	ctx, cancel := s.opContext()
	defer cancel()
	if err := s.DeleteInstance(ctx, job.RunnerName); err != nil {
		log.Errorf("Lifecycle delete failed: %v", err)
		c.AbortWithError(503, err)
		return
	}
	if err := s.store.Update(ctx, jobKey(src.Name, job), func(r *lifecycleRecord, _ *fleetState) error {
		if r.PendingDelete == job.RunnerName {
			r.PendingDelete = ""
		}
		if r.Terminal && r.VMName == "" && r.PendingDelete == "" {
			r.ExpiresAt = time.Now().Add(7 * 24 * time.Hour)
		}
		return nil
	}); err != nil {
		c.AbortWithError(503, err)
		return
	}
	// The originating job remains in Firestore. No metadata read or top-up enqueue
	// is needed here; a crash after deletion cannot lose replacement intent.
	c.Status(200)
}

type recreateCapability struct {
	Job     Job    `json:"job"`
	Runner  string `json:"runner"`
	Purpose string `json:"purpose"`
	Expires int64  `json:"expires"`
}

func (s *Autoscaler) durableRecreate(c *gin.Context) {
	src, ok := s.conf.RegisteredSources[c.Query(s.conf.SourceQueryParam)]
	sig := c.GetHeader(SHA_HEADER)
	if !ok || !strings.HasPrefix(sig, SHA_PREFIX) || len(sig) != 71 {
		c.AbortWithStatus(401)
		return
	}
	data, err := s.readBody(c)
	if err != nil {
		return
	}
	expected := CalcSigHex([]byte(src.Secret), append([]byte("recreate\n"), data...))
	var cap recreateCapability
	if !hmac.Equal([]byte(expected), []byte(sig[7:])) || json.Unmarshal(data, &cap) != nil || cap.Purpose != "recreate" || cap.Expires <= time.Now().Unix() || cap.Job.Id <= 0 {
		c.AbortWithStatus(401)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	valid := false
	err = s.store.Update(ctx, jobKey(src.Name, cap.Job), func(r *lifecycleRecord, _ *fleetState) error {
		valid = !r.Terminal && r.VMName == cap.Runner && r.VMName != ""
		return nil
	})
	if err != nil {
		c.AbortWithError(503, err)
		return
	}
	if !valid {
		c.AbortWithStatus(409)
		return
	}
	if err = s.queue(ctx, s.conf.RouteCreateVm, src.Name, cap.Job, recreateVmDelay); err != nil {
		c.AbortWithError(503, err)
		return
	}
	c.Status(200)
}

type pageRequest struct {
	After string `json:"after"`
}

func (s *Autoscaler) reconcile(c *gin.Context) {
	if !s.privateRequest(c) {
		return
	}
	data, err := s.readBody(c)
	if err != nil {
		return
	}
	var p pageRequest
	if len(data) > 0 && json.Unmarshal(data, &p) != nil {
		c.AbortWithStatus(400)
		return
	}
	ctx, cancel := s.opContext()
	defer cancel()
	rows, next, err := s.store.Page(ctx, p.After, 50)
	if err != nil {
		c.AbortWithError(503, err)
		return
	}
	for _, row := range rows {
		r := row.Record
		if r.PendingDelete != "" {
			deletion := r.Job
			deletion.RunnerName = r.PendingDelete
			if err = s.queue(ctx, s.conf.RouteDeleteVm, r.Source, deletion, 0); err != nil {
				c.AbortWithError(503, err)
				return
			}
		}
		if r.Terminal && r.VMName == "" {
			continue
		}
		if r.Lease != "" && time.Now().Before(r.LeaseUntil) {
			continue
		}
		if err = s.queue(ctx, s.conf.RouteCreateVm, r.Source, r.Job, 0); err != nil {
			c.AbortWithError(503, err)
			return
		}
		if !r.Terminal && r.Job.Status == "queued" {
			log.WithFields(log.Fields{"job_id": r.Job.Id, "age_seconds": time.Since(r.UpdatedAt).Seconds()}).Info("Reconcile pending job")
		}
	}
	if next != "" {
		err = s.queue(ctx, "/reconcile", "", pageRequest{next}, 0)
	}
	if err != nil {
		c.AbortWithError(503, err)
		return
	}
	c.Status(200)
}
func (s *Autoscaler) durableSweep(c *gin.Context) {
	if !s.privateRequest(c) {
		return
	}
	ctx, cancel := s.opContext()
	defer cancel()
	if err := s.sweepOrphans(ctx); err != nil {
		log.Errorf("Lifecycle sweep failed: %v", err)
		c.AbortWithError(503, err)
		return
	}
	c.Status(200)
}
