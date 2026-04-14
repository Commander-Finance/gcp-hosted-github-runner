package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Tereius/gcp-hosted-github-runner/pkg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var PORT = 9999

var scaler *pkg.Autoscaler

const PROJECT_ID = "my-gcp-project-id"
const REGION = "us-east1"
const ZONE = "us-east1-c"
const GIT_HUB_ORG = "Privatehive"
const TEST_REPO = "Privatehive/runner-test"
const TEST_REPO_KEY = "repository-" + TEST_REPO
const SOURCE_QUERY_PARAM_NAME = "src"
const PUBLIC_SECRET = "It's a Secret to Everybody"

func init() {

	scaler = pkg.NewAutoscaler(pkg.AutoscalerConfig{
		RouteWebhook:     "/webhook",
		RouteCreateVm:    "/create",
		RouteDeleteVm:    "/delete",
		ProjectId:        PROJECT_ID,
		Zones:            []string{ZONE},
		TaskQueue:        "projects/" + PROJECT_ID + "/locations/" + REGION + "/queues/autoscaler-callback-queue",
		InstanceTemplate: "projects/" + PROJECT_ID + "/global/instanceTemplates/ephemeral-github-runner",
		SecretVersion:    "projects/" + PROJECT_ID + "/secrets/github-pat-token/versions/latest",
		RunnerPrefix:     "runner",
		RunnerGroupId:    1,
		RunnerLabels:     []string{"self-hosted"},
		SourceQueryParam: SOURCE_QUERY_PARAM_NAME,
		RegisteredSources: map[string]pkg.Source{
			TEST_REPO_KEY: {
				Name:       TEST_REPO,
				SourceType: pkg.TypeRepository,
				Secret:     PUBLIC_SECRET,
			},
		},
	})
	go scaler.Srv(PORT)
	time.Sleep(1 * time.Second)
}

func TestWebhookSignature(t *testing.T) {

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://127.0.0.1:%d/webhook?%s=%s", PORT, SOURCE_QUERY_PARAM_NAME, url.QueryEscape(TEST_REPO_KEY)), strings.NewReader("Hello, World!"))
	req.Header.Add("x-hub-signature-256", "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17")
	resp, err := http.DefaultClient.Do(req)
	assert.Nil(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestGenerateRunnerJitConfig(t *testing.T) {

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	jitConfig, err := scaler.GenerateRunnerJitConfig(ctx, fmt.Sprintf(pkg.RUNNER_REPO_JIT_CONFIG_ENDPOINT, TEST_REPO), "unit_test_runner_"+pkg.RandStringRunes(10), 1, []string{"self-hosted"})
	assert.Nil(t, err)
	assert.NotEmpty(t, jitConfig)
}

func TestGetMagicLabelValue(t *testing.T) {

	job := pkg.Job{
		Labels: []string{"self-hosted", "gce-machine-c2d-standard-16", "linux"},
	}
	result := job.GetMagicLabelValue(pkg.MagicLabelMachine)
	require.NotNil(t, result)
	assert.Equal(t, "c2d-standard-16", *result)
}

func TestGetMagicLabelValueRejectsLegacy(t *testing.T) {

	job := pkg.Job{Labels: []string{"self-hosted", "@machine:c2d-standard-16"}}
	assert.Nil(t, job.GetMagicLabelValue(pkg.MagicLabelMachine))
}

func TestHasLegacyMagicLabel(t *testing.T) {

	assert.True(t, pkg.Job{Labels: []string{"self-hosted", "@machine:c2d-standard-16"}}.HasLegacyMagicLabel())
	assert.True(t, pkg.Job{Labels: []string{"@machine:"}}.HasLegacyMagicLabel())

	assert.False(t, pkg.Job{Labels: []string{"self-hosted", "gce-machine-c2d-standard-16"}}.HasLegacyMagicLabel())
	assert.False(t, pkg.Job{Labels: []string{"self-hosted", "linux"}}.HasLegacyMagicLabel())
	assert.False(t, pkg.Job{Labels: []string{}}.HasLegacyMagicLabel())
}

func TestIsMagicLabel(t *testing.T) {

	positive := []string{
		"gce-machine-c2d-standard-16",
		"gce-machine-e2-highmem-8",
		"gce-machine-t2d-standard-1",
		"gce-machine-n2d-highcpu-96",
	}
	for _, label := range positive {
		assert.True(t, pkg.IsMagicLabel(label), "expected %q to be recognised as a magic label", label)
	}

	negative := []string{
		"gce-machine-foo",                // only 1 segment after prefix, not shape-valid
		"gce-machine-learning",           // same: 1 segment
		"self-hosted",                    // not a magic label at all
		"@machine:c2d-standard-16",       // legacy syntax — deliberately rejected
		"GCE-MACHINE-c2d-standard-16",    // wrong case
		"foogce-machine-c2d-standard-16", // full-label-anchored: prefix not at start
	}
	for _, label := range negative {
		assert.False(t, pkg.IsMagicLabel(label), "expected %q NOT to be recognised as a magic label", label)
	}
}

func TestCreateCallbackTask(t *testing.T) {

	job := pkg.Job{
		Id:     rand.Int63n(math.MaxInt64),
		Labels: []string{"test", "gce-machine-c2d-standard-16"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := scaler.DeleteCallbackTask(ctx, job)
	assert.Nil(t, err)
}

func TestHasAllLabels(t *testing.T) {

	job := pkg.Job{
		Labels: []string{"test", "gce-machine-c2d-standard-16"},
	}
	result, missing := job.HasAllLabels([]string{"test"})
	assert.True(t, result)
	assert.Empty(t, missing)
	result, missing = job.HasAllLabels([]string{"test", "foo"})
	assert.False(t, result)
	assert.NotEmpty(t, missing)
	assert.Len(t, missing, 1)
}

func TestDeleteNotExistingVM(t *testing.T) {

	job := pkg.Job{
		RunnerName: "non-existing-unit-test-runner",
	}
	jobData, _ := json.Marshal(job)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://127.0.0.1:%d/delete?%s=%s", PORT, SOURCE_QUERY_PARAM_NAME, url.QueryEscape(TEST_REPO_KEY)), bytes.NewReader(jobData))
	req.Header.Add("x-hub-signature-256", "sha256="+pkg.CalcSigHex([]byte(PUBLIC_SECRET), jobData))
	resp, err := http.DefaultClient.Do(req)
	assert.Nil(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
}
