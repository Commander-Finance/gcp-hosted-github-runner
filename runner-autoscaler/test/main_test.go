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
		RouteWebhook:      "/webhook",
		RouteCreateVm:     "/create",
		RouteDeleteVm:     "/delete",
		ProjectId:         PROJECT_ID,
		Zones:             []string{ZONE},
		TaskQueue:         "projects/" + PROJECT_ID + "/locations/" + REGION + "/queues/autoscaler-callback-queue",
		InstanceTemplate:  "projects/" + PROJECT_ID + "/global/instanceTemplates/ephemeral-github-runner",
		SecretVersion:     "projects/" + PROJECT_ID + "/secrets/github-pat-token/versions/latest",
		RunnerPrefix:      "runner",
		RunnerGroupId:     1,
		RunnerLabelGroups: [][]string{{"self-hosted"}},
		SourceQueryParam:  SOURCE_QUERY_PARAM_NAME,
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
		// shared-core types are only 2 segments (series-variety) — must still match.
		"gce-machine-f1-micro",
		"gce-machine-g1-small",
		"gce-machine-e2-micro",
		"gce-machine-e2-medium",
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

func TestParseLabelGroups(t *testing.T) {

	cases := []struct {
		name string
		raw  string
		want [][]string
	}{
		{"single label single group", "self-hosted", [][]string{{"self-hosted"}}},
		{"single group two labels", "self-hosted,linux", [][]string{{"self-hosted", "linux"}}},
		{"two single-label groups", "spock;spock-prime", [][]string{{"spock"}, {"spock-prime"}}},
		{"two two-label groups", "spock,linux;spock-prime,linux", [][]string{{"spock", "linux"}, {"spock-prime", "linux"}}},
		{"whitespace trimmed", "spock, linux ; spock-prime", [][]string{{"spock", "linux"}, {"spock-prime"}}},
		{"empty labels dropped", "spock,;,spock-prime", [][]string{{"spock"}, {"spock-prime"}}},
		{"empty groups dropped", "spock;;spock-prime", [][]string{{"spock"}, {"spock-prime"}}},
		{"trailing separators", "spock;", [][]string{{"spock"}}},
		{"empty string", "", [][]string{}},
		{"only separators", " ; , ; ", [][]string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, pkg.ParseLabelGroups(tc.raw))
		})
	}
}

func TestHasAnyLabelGroup(t *testing.T) {

	cases := []struct {
		name    string
		labels  []string
		groups  [][]string
		wantOk  bool
		wantMsg string
	}{
		{
			name:    "single group fully satisfied",
			labels:  []string{"test", "gce-machine-c2d-standard-16"},
			groups:  [][]string{{"test"}},
			wantOk:  true,
			wantMsg: "",
		},
		{
			name:    "single group one missing preserves legacy text",
			labels:  []string{"test"},
			groups:  [][]string{{"test", "foo"}},
			wantOk:  false,
			wantMsg: `missing the label(s) "foo"`,
		},
		{
			name:    "single group multiple missing comma-joined",
			labels:  []string{"test"},
			groups:  [][]string{{"test", "foo", "bar"}},
			wantOk:  false,
			wantMsg: `missing the label(s) "foo, bar"`,
		},
		{
			name:    "single group magic label in config is skipped",
			labels:  []string{"spock"},
			groups:  [][]string{{"spock", "gce-machine-c2d-standard-16"}},
			wantOk:  true,
			wantMsg: "",
		},
		{
			name:    "multi group first matches",
			labels:  []string{"spock"},
			groups:  [][]string{{"spock"}, {"spock-prime"}},
			wantOk:  true,
			wantMsg: "",
		},
		{
			name:    "multi group second matches",
			labels:  []string{"spock-prime", "gce-machine-t2d-standard-4"},
			groups:  [][]string{{"spock"}, {"spock-prime"}},
			wantOk:  true,
			wantMsg: "",
		},
		{
			name:    "multi group no match describes required groups",
			labels:  []string{"ghost-pool"},
			groups:  [][]string{{"spock"}, {"spock-prime"}},
			wantOk:  false,
			wantMsg: "none of the label groups matched (required one of: [spock], [spock-prime])",
		},
		{
			name:    "multi group magic labels skipped per group",
			labels:  []string{"spock-prime", "gce-machine-t2d-standard-4"},
			groups:  [][]string{{"spock", "gce-machine-c2d-standard-16"}, {"spock-prime", "gce-machine-t2d-standard-4"}},
			wantOk:  true,
			wantMsg: "",
		},
		{
			name:    "zero groups rejects all",
			labels:  []string{"spock"},
			groups:  [][]string{},
			wantOk:  false,
			wantMsg: "no label groups configured — rejecting all jobs",
		},
		{
			name:    "single magic-only group does not match any job",
			labels:  []string{"spock", "anything"},
			groups:  [][]string{{"gce-machine-c2d-standard-16"}},
			wantOk:  false,
			wantMsg: "no label groups contain gating labels — gce-machine-* are per-job overrides, not gating labels",
		},
		{
			name:    "all magic-only groups do not match any job",
			labels:  []string{"spock"},
			groups:  [][]string{{"gce-machine-c2d-standard-16"}, {"gce-machine-t2d-standard-4"}},
			wantOk:  false,
			wantMsg: "no label groups contain gating labels — gce-machine-* are per-job overrides, not gating labels",
		},
		{
			name:    "mixed magic-only and valid group matches on valid group",
			labels:  []string{"spock-prime"},
			groups:  [][]string{{"gce-machine-c2d-standard-16"}, {"spock-prime"}},
			wantOk:  true,
			wantMsg: "",
		},
		{
			name:    "mixed magic-only and valid group miss lists only matchable groups",
			labels:  []string{"ghost"},
			groups:  [][]string{{"gce-machine-c2d-standard-16"}, {"spock"}},
			wantOk:  false,
			wantMsg: `missing the label(s) "spock"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := pkg.Job{Labels: tc.labels}
			ok, msg := job.HasAnyLabelGroup(tc.groups)
			assert.Equal(t, tc.wantOk, ok, "match outcome")
			assert.Equal(t, tc.wantMsg, msg, "message text")
		})
	}
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
