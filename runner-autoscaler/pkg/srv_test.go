package pkg

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// deleteHandlerScaler builds an Autoscaler whose delete-vm callback can be driven
// without touching GCP: deletes are intercepted by the deleteInZoneFn seam, and
// the detached orphan sweep is neutered by pre-seeding lastSweep so it short-
// circuits on the throttle instead of constructing a real compute client.
func deleteHandlerScaler(secret string, deleted *[]string) *Autoscaler {
	gin.SetMode(gin.TestMode)
	s := &Autoscaler{conf: AutoscalerConfig{
		RunnerPrefix:      "runner",
		Zones:             []string{"z1"},
		SourceQueryParam:  "src",
		RegisteredSources: map[string]Source{"repo": {Name: "repo", Secret: secret}},
	}}
	s.deleteInZoneFn = func(ctx context.Context, name string, zone string) (bool, error) {
		*deleted = append(*deleted, name)
		return true, nil // found + deleted in this zone
	}
	s.lastSweep = time.Now() // suppress the detached maybeSweepOrphans goroutine
	return s
}

// postDelete drives handleDeleteVm with a correctly signed delete-vm payload and
// returns the recorded response.
func postDelete(s *Autoscaler, secret string, job Job) *httptest.ResponseRecorder {
	body, _ := json.Marshal(job)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/delete?src=repo", bytes.NewReader(body))
	c.Request.Header.Set(SHA_HEADER, SHA_PREFIX+CalcSigHex([]byte(secret), body))
	s.handleDeleteVm(c)
	return w
}

// A completed job whose VM carries a *different* job's id (cross-assignment) must
// still be deleted, by the webhook-supplied runner_name. This pins the actual
// fix at the handler boundary: the guard routes an owned name into DeleteInstance
// rather than skipping it (which is the leak the change addresses).
func TestHandleDeleteVmDeletesOwnedRunner(t *testing.T) {

	secret := "topsecret"
	var deleted []string
	s := deleteHandlerScaler(secret, &deleted)

	w := postDelete(s, secret, Job{Id: 79130065907, RunnerName: "runner-79130066573"})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"runner-79130066573"}, deleted, "the webhook-named VM must be deleted")
}

// An empty runner_name (job cancelled while queued) or a foreign name must be
// acknowledged (200, so the task isn't retried) without issuing any delete.
func TestHandleDeleteVmSkipsEmptyAndForeignNames(t *testing.T) {

	secret := "topsecret"
	for _, name := range []string{"", "other-42"} {
		var deleted []string
		s := deleteHandlerScaler(secret, &deleted)

		w := postDelete(s, secret, Job{Id: 42, RunnerName: name})

		assert.Equal(t, http.StatusOK, w.Code, "empty/foreign name must be acked, not retried")
		assert.Empty(t, deleted, "must not issue a delete for runner_name %q", name)
	}
}
