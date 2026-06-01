package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/Tereius/gcp-hosted-github-runner/pkg"
	"github.com/stretchr/testify/assert"
)

// simScaler runs in Simulate mode so delete/sweep make no real GCP calls - used by
// hermetic tests that exercise request handling without credentials.
var simScaler *pkg.Autoscaler
var simPort = 9998

func init() {

	simScaler = pkg.NewAutoscaler(pkg.AutoscalerConfig{
		RouteWebhook:      "/webhook",
		RouteCreateVm:     "/create",
		RouteDeleteVm:     "/delete",
		ProjectId:         PROJECT_ID,
		Zones:             []string{ZONE},
		TaskQueue:         "projects/" + PROJECT_ID + "/locations/" + REGION + "/queues/autoscaler-callback-queue",
		RunnerPrefix:      "runner",
		RunnerGroupId:     1,
		RunnerLabelGroups: [][]string{{"self-hosted"}},
		SourceQueryParam:  SOURCE_QUERY_PARAM_NAME,
		RegisteredSources: map[string]pkg.Source{
			TEST_REPO_KEY: {Name: TEST_REPO, SourceType: pkg.TypeRepository, Secret: PUBLIC_SECRET},
		},
		Simulate: true,
	})
	go simScaler.Srv(simPort)
	time.Sleep(500 * time.Millisecond)
}

func TestIsCapacityError(t *testing.T) {

	capacity := []string{
		"Operation failed: ZONE_RESOURCE_POOL_EXHAUSTED",
		"the zone does not have enough resources available to fulfill the request",
		"Quota 'CPUS' exceeded. QUOTA_EXCEEDED",
		"ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS: try another zone",
		"RESOURCE_POOL_EXHAUSTED",
		"rpc error: code = ResourceExhausted desc = quota",
	}
	for _, msg := range capacity {
		assert.True(t, pkg.IsCapacityError(fmt.Errorf(msg)), "expected %q to be a capacity error", msg)
	}

	other := []string{
		"the instance template was not found",
		"permission denied",
		"invalid machine type",
		"",
	}
	for _, msg := range other {
		assert.False(t, pkg.IsCapacityError(fmt.Errorf(msg)), "expected %q NOT to be a capacity error", msg)
	}
	assert.False(t, pkg.IsCapacityError(nil))
}

func TestIsAlreadyExists(t *testing.T) {

	assert.True(t, pkg.IsAlreadyExists(fmt.Errorf("The resource 'runner-42' already exists")))
	assert.True(t, pkg.IsAlreadyExists(fmt.Errorf("rpc error: code = AlreadyExists")))
	assert.False(t, pkg.IsAlreadyExists(fmt.Errorf("not found")))
	assert.False(t, pkg.IsAlreadyExists(nil))
}

func TestIsNotFound(t *testing.T) {

	assert.True(t, pkg.IsNotFound(fmt.Errorf("The resource 'runner-42' was not found")))
	assert.False(t, pkg.IsNotFound(fmt.Errorf("already exists")))
	assert.False(t, pkg.IsNotFound(nil))
}

func TestInstanceName(t *testing.T) {

	// Deterministic: same job id yields the same name (this is what makes
	// create-vm retries idempotent).
	assert.Equal(t, "runner-12345", pkg.InstanceName("runner", 12345))
	assert.Equal(t, pkg.InstanceName("runner", 999), pkg.InstanceName("runner", 999))
	assert.NotEqual(t, pkg.InstanceName("runner", 1), pkg.InstanceName("runner", 2))
}

func TestCallbackTaskNameDistinguishesCreateAndDelete(t *testing.T) {

	queue := "projects/p/locations/r/queues/q"
	// The create and delete callbacks for the SAME job must not collide on the
	// Cloud Tasks task name (otherwise enqueueing the delete can hit
	// AlreadyExists against the tombstoned create task and never run).
	create := pkg.CallbackTaskName(queue, pkg.TaskKindCreate, 42, 0)
	del := pkg.CallbackTaskName(queue, pkg.TaskKindDelete, 42, 0)
	assert.NotEqual(t, create, del)
	// Retry count varies the name within a kind.
	assert.NotEqual(t, create, pkg.CallbackTaskName(queue, pkg.TaskKindCreate, 42, 1))
}

func TestEmptyRunnerNameDeleteReturnsOk(t *testing.T) {

	// A completed job that was never picked up has no runner_name. The delete
	// callback must short-circuit to 200 instead of attempting (and retrying for an
	// hour) a delete of an empty instance name.
	job := pkg.Job{RunnerName: ""}
	jobData, _ := json.Marshal(job)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://127.0.0.1:%d/delete?%s=%s", simPort, SOURCE_QUERY_PARAM_NAME, url.QueryEscape(TEST_REPO_KEY)), bytes.NewReader(jobData))
	req.Header.Add("x-hub-signature-256", "sha256="+pkg.CalcSigHex([]byte(PUBLIC_SECRET), jobData))
	resp, err := http.DefaultClient.Do(req)
	assert.Nil(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestOrderedZones(t *testing.T) {

	zones := []string{"a", "b", "c", "d"}
	scaler := pkg.NewAutoscaler(pkg.AutoscalerConfig{
		Zones:         zones,
		RouteWebhook:  "/webhook",
		RouteCreateVm: "/create",
		RouteDeleteVm: "/delete",
	})

	ordered := scaler.OrderedZones("runner-7")

	// Returns every configured zone exactly once (a rotation/permutation).
	assert.Len(t, ordered, len(zones))
	sorted := append([]string{}, ordered...)
	sort.Strings(sorted)
	assert.Equal(t, []string{"a", "b", "c", "d"}, sorted)

	// Deterministic and starts at the hashed zone.
	assert.Equal(t, ordered, scaler.OrderedZones("runner-7"))
	assert.Equal(t, scaler.PickRandomZone("runner-7"), ordered[0])
}
