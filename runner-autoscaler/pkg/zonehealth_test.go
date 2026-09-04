package pkg

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/logging"
	logtest "github.com/sirupsen/logrus/hooks/test"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The zone circuit breaker: zones whose runner VMs are logging outbound dial
// timeouts move to the back of the creation order. The sensor is a Cloud Logging
// query behind an in-process cache; every path must fail open.

func TestBenchedZonesThresholds(t *testing.T) {

	cases := []struct {
		name     string
		report   zoneReport
		minVMs   int64
		minRatio float64
		want     []string
	}{
		{
			name:   "both thresholds met",
			report: zoneReport{failing: map[string]int{"b": 5}, created: map[string]int{"b": 10, "a": 10}},
			minVMs: 3, minRatio: 0.2,
			want: []string{"b"},
		},
		{
			name:   "too few failing VMs even at 100%",
			report: zoneReport{failing: map[string]int{"b": 2}, created: map[string]int{"b": 2}},
			minVMs: 3, minRatio: 0.2,
			want: nil,
		},
		{
			name:   "ratio below threshold",
			report: zoneReport{failing: map[string]int{"b": 3}, created: map[string]int{"b": 100}},
			minVMs: 3, minRatio: 0.2,
			want: nil,
		},
		{
			name:   "ratio exactly at threshold benches",
			report: zoneReport{failing: map[string]int{"b": 4}, created: map[string]int{"b": 20}},
			minVMs: 3, minRatio: 0.2,
			want: []string{"b"},
		},
		{
			// A VM that fails after the creation window closed has no create to be
			// counted against; the denominator floors at the failing count so the
			// zone is still benched rather than divided by zero or skipped.
			name:   "failing VMs with no creates in window",
			report: zoneReport{failing: map[string]int{"a": 3}, created: map[string]int{}},
			minVMs: 3, minRatio: 0.2,
			want: []string{"a"},
		},
		{
			name:   "several zones, only the bad one benched",
			report: zoneReport{failing: map[string]int{"a": 1, "b": 30, "c": 0}, created: map[string]int{"a": 60, "b": 60, "c": 60, "f": 60}},
			minVMs: 3, minRatio: 0.2,
			want: []string{"b"},
		},
		{
			name:   "minVMs of zero disables the breaker",
			report: zoneReport{failing: map[string]int{"b": 50}, created: map[string]int{"b": 50}},
			minVMs: 0, minRatio: 0.2,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := benchedZones(tc.report, tc.minVMs, tc.minRatio)
			var gotList []string
			for z := range got {
				gotList = append(gotList, z)
			}
			assert.ElementsMatch(t, tc.want, gotList)
		})
	}
}

func TestOrderedZonesBenchedMovesBenchedZonesLast(t *testing.T) {

	s := &Autoscaler{conf: AutoscalerConfig{Zones: []string{"a", "b", "c", "f"}}}
	plain := s.OrderedZones("runner-42")

	got := s.orderedZonesBenched("runner-42", map[string]bool{"b": true})
	require.Len(t, got, 4)
	assert.Equal(t, "b", got[3], "benched zone must be tried last")
	// The healthy zones keep their rotation order relative to each other.
	var healthy []string
	for _, z := range plain {
		if z != "b" {
			healthy = append(healthy, z)
		}
	}
	assert.Equal(t, healthy, got[:3])
	assert.ElementsMatch(t, plain, got, "every zone still appears exactly once")
}

func TestOrderedZonesBenchedNoBenchIsPlainRotation(t *testing.T) {

	s := &Autoscaler{conf: AutoscalerConfig{Zones: []string{"a", "b", "c"}}}
	assert.Equal(t, s.OrderedZones("x"), s.orderedZonesBenched("x", nil))
	assert.Equal(t, s.OrderedZones("x"), s.orderedZonesBenched("x", map[string]bool{}))
}

func TestOrderedZonesBenchedAllBenchedIsPlainRotation(t *testing.T) {

	// If every zone is benched there is nothing to prefer; keep the deterministic
	// rotation rather than an arbitrary reshuffle.
	s := &Autoscaler{conf: AutoscalerConfig{Zones: []string{"a", "b", "c"}}}
	all := map[string]bool{"a": true, "b": true, "c": true}
	assert.Equal(t, s.OrderedZones("x"), s.orderedZonesBenched("x", all))
}

func TestCreationPlanLegacyTriesBenchedZoneLastPerTemplate(t *testing.T) {

	s := spotFallbackScaler([]string{"a", "b", "c"})
	plan := s.creationPlan("runner-7", nil, map[string]bool{"b": true})

	require.Len(t, plan, 6)
	// Within the SPOT sweep and again within the STANDARD sweep, "b" is last.
	assert.Equal(t, "b", plan[2].zone)
	assert.Equal(t, "spot", plan[2].provisioningModel)
	assert.Equal(t, "b", plan[5].zone)
	assert.Equal(t, "standard", plan[5].provisioningModel)
	assert.NotEqual(t, "b", plan[0].zone)
	assert.NotEqual(t, "b", plan[3].zone)
}

func TestCreationPlanFamilyFallbackAvoidsBenchedZoneFirst(t *testing.T) {

	s := familyScaler([]string{"a", "b"}, []string{"c4d-standard-2", "n4-standard-2", "c3-standard-4"})
	plan := s.creationPlan("runner-7", nil, map[string]bool{"a": true})

	require.Len(t, plan, 5) // 3 spot families + 2 standard
	// Zones rotate by attempt index over the benched-last order, so the first family
	// (and the first on-demand retry) land on the healthy zone.
	assert.Equal(t, "b", plan[0].zone)
	assert.Equal(t, "b", plan[3].zone)
}

func TestCreationPlanNilBenchedMatchesPlainOrder(t *testing.T) {

	s := spotFallbackScaler([]string{"a", "b", "c"})
	assert.Equal(t, s.creationPlan("runner-7", nil, nil), s.creationPlan("runner-7", nil, map[string]bool{}))
	assert.Equal(t, s.OrderedZones("runner-7")[0], s.creationPlan("runner-7", nil, nil)[0].zone)
}

func breakerScaler(minVMs int64) *Autoscaler {
	return &Autoscaler{conf: AutoscalerConfig{
		Zones:             []string{"a", "b"},
		ZoneBenchMinVMs:   minVMs,
		ZoneBenchMinRatio: 0.2,
		ZoneHealthWindow:  600,
	}}
}

func TestBenchedZonesCachedReusesAnswerWithinTTL(t *testing.T) {

	s := breakerScaler(3)
	calls := 0
	s.zoneReportFn = func(ctx context.Context, since time.Time) (zoneReport, error) {
		calls++
		assert.WithinDuration(t, time.Now().Add(-600*time.Second), since, 5*time.Second)
		return zoneReport{failing: map[string]int{"b": 5}, created: map[string]int{"b": 10}}, nil
	}

	first := s.benchedZonesCached(context.Background())
	second := s.benchedZonesCached(context.Background())
	assert.Equal(t, map[string]bool{"b": true}, first)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, calls, "second call inside the TTL must be served from cache")

	// Age the cache past the TTL: the next call must query again.
	s.zoneCheckedAt = time.Now().Add(-2 * zoneHealthCacheTTL)
	s.benchedZonesCached(context.Background())
	assert.Equal(t, 2, calls)
}

func TestBenchedZonesCachedFailsOpenAndBacksOff(t *testing.T) {

	s := breakerScaler(3)
	calls := 0
	s.zoneReportFn = func(ctx context.Context, since time.Time) (zoneReport, error) {
		calls++
		return zoneReport{}, errors.New("logging unavailable")
	}

	got := s.benchedZonesCached(context.Background())
	assert.Empty(t, got, "a sensor error must never bench a zone")
	s.benchedZonesCached(context.Background())
	assert.Equal(t, 1, calls, "a failed query is not retried on every create; it waits out the TTL")
}

func TestBenchedZonesCachedDisabledNeverQueries(t *testing.T) {

	s := breakerScaler(0)
	s.zoneReportFn = func(ctx context.Context, since time.Time) (zoneReport, error) {
		t.Fatal("breaker disabled: the sensor must not be queried")
		return zoneReport{}, nil
	}
	assert.Empty(t, s.benchedZonesCached(context.Background()))
}

func TestBenchedZonesCachedBoundsQueryTime(t *testing.T) {

	s := breakerScaler(3)
	s.zoneReportFn = func(ctx context.Context, since time.Time) (zoneReport, error) {
		dl, ok := ctx.Deadline()
		require.True(t, ok, "the sensor query must carry a deadline")
		assert.WithinDuration(t, time.Now().Add(zoneHealthQueryTimeout), dl, 2*time.Second)
		return zoneReport{}, nil
	}
	s.benchedZonesCached(context.Background())
}

func entry(zone, instanceID, service, message string, fields map[string]interface{}) *logging.Entry {
	payload := map[string]interface{}{"message": message}
	for k, v := range fields {
		payload[k] = v
	}
	st, err := structpb.NewStruct(payload)
	if err != nil {
		panic(err)
	}
	labels := map[string]string{"zone": zone}
	if instanceID != "" {
		labels["instance_id"] = instanceID
	}
	if service != "" {
		labels["service_name"] = service
	}
	return &logging.Entry{Payload: st, Resource: &mrpb.MonitoredResource{Labels: labels}}
}

func TestZoneReportFromEntriesCountsDistinctVMs(t *testing.T) {

	// A stuck VM logs the timeout every minute; it must count once.
	failing := []*logging.Entry{
		entry("us-central1-b", "111", "", "Exporting failed ... dial tcp 1.2.3.4:443: i/o timeout", nil),
		entry("us-central1-b", "111", "", "Exporting failed ... dial tcp 1.2.3.4:443: i/o timeout", nil),
		entry("us-central1-b", "222", "", "Exporting failed ... i/o timeout", nil),
		entry("us-central1-a", "333", "", "Exporting failed ... i/o timeout", nil),
		entry("us-central1-a", "", "", "no instance id - skipped", nil),
		{Resource: nil, Payload: "no resource - skipped, not a panic"},
	}
	created := []*logging.Entry{
		entry("", "", "autoscaler", "Created instance runner-1 (us-central1-b) as spot", map[string]interface{}{"zone": "us-central1-b", "instance": "runner-1"}),
		entry("", "", "autoscaler", "Created instance runner-2 (us-central1-b) as spot", map[string]interface{}{"zone": "us-central1-b", "instance": "runner-2"}),
		entry("", "", "autoscaler", "Created instance runner-2 (us-central1-b) as spot", map[string]interface{}{"zone": "us-central1-b", "instance": "runner-2"}),
		entry("", "", "autoscaler", "Created instance runner-3 (us-central1-f) as spot", map[string]interface{}{"zone": "us-central1-f", "instance": "runner-3"}),
		entry("", "", "autoscaler", "no zone field - skipped", map[string]interface{}{"instance": "runner-4"}),
		{Payload: "text payload - skipped, not a panic"},
	}

	r := zoneReportFromEntries(failing, created)
	assert.Equal(t, map[string]int{"us-central1-b": 2, "us-central1-a": 1}, r.failing)
	assert.Equal(t, map[string]int{"us-central1-b": 2, "us-central1-f": 1}, r.created)
}

func TestZoneHealthFiltersScopeByTimeProjectAndService(t *testing.T) {

	since := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	failF, createdF := zoneHealthFilters("spock-runner", "github-runner-autoscaler", since)

	assert.Contains(t, failF, `resource.type="gce_instance"`)
	assert.Contains(t, failF, `logName="projects/spock-runner/logs/syslog"`)
	assert.Contains(t, failF, `timestamp>="2026-09-04T00:00:00Z"`)
	assert.Contains(t, failF, `jsonPayload.message:"Exporting failed"`)
	assert.Contains(t, failF, `jsonPayload.message:"i/o timeout"`)

	assert.Contains(t, createdF, `resource.type="cloud_run_revision"`)
	assert.Contains(t, createdF, `resource.labels.service_name="github-runner-autoscaler"`)
	assert.Contains(t, createdF, `timestamp>="2026-09-04T00:00:00Z"`)
	assert.Contains(t, createdF, `jsonPayload.message:"Created instance"`)

	// Without a service name (running outside Cloud Run) the create filter must
	// still be valid and simply not scope by service.
	_, unscoped := zoneHealthFilters("spock-runner", "", since)
	assert.NotContains(t, unscoped, "service_name")
}

func TestValidateRejectsBenchRatioOutsideUnitInterval(t *testing.T) {

	// A ratio <= 0 benches on the VM count alone; a ratio > 1 can never be met
	// (the denominator floors at the failing count). Both silently change what
	// the breaker does, so refuse to start, as with an empty RunnerPrefix.
	for _, bad := range []float64{0, -0.1, 1.5} {
		err := AutoscalerConfig{RunnerPrefix: "runner", ZoneBenchMinVMs: 3, ZoneBenchMinRatio: bad}.Validate()
		require.Error(t, err, "ratio %v", bad)
		assert.Contains(t, err.Error(), "ZoneBenchMinRatio")
	}
	for _, ok := range []float64{0.01, 0.2, 1} {
		require.NoError(t, AutoscalerConfig{RunnerPrefix: "runner", ZoneBenchMinVMs: 3, ZoneBenchMinRatio: ok}.Validate(), "ratio %v", ok)
	}
	// With the breaker disabled the ratio is unused and must not block startup.
	require.NoError(t, AutoscalerConfig{RunnerPrefix: "runner", ZoneBenchMinVMs: 0, ZoneBenchMinRatio: 0}.Validate())
}

func TestCreateInstanceFromTemplateTriesHealthyZoneFirstWhenBreakerTrips(t *testing.T) {

	// The one seam where the breaker meets the create path: a tripped zone must
	// not receive the first Insert.
	s := &Autoscaler{conf: AutoscalerConfig{
		Zones:             []string{"a", "b"},
		InstanceTemplate:  "primary",
		ZoneBenchMinVMs:   3,
		ZoneBenchMinRatio: 0.2,
		ZoneHealthWindow:  600,
	}}
	s.zoneReportFn = func(ctx context.Context, since time.Time) (zoneReport, error) {
		return zoneReport{failing: map[string]int{"a": 5}, created: map[string]int{"a": 5, "b": 5}}, nil
	}
	var attempted []string
	s.tryInsertFn = func(ctx context.Context, attempt creationAttempt, name string, md []*computepb.Items) error {
		attempted = append(attempted, attempt.zone)
		return nil
	}

	for _, seed := range []string{"runner-1", "runner-2", "runner-3", "runner-4"} {
		attempted = nil
		require.NoError(t, s.CreateInstanceFromTemplate(context.Background(), seed, nil))
		require.Equal(t, []string{"b"}, attempted, "seed %s: the benched zone must not get the first attempt", seed)
	}
}

func TestBenchAndReleaseAreLoggedOncePerTransition(t *testing.T) {

	hook := logtest.NewGlobal()
	defer hook.Reset()

	s := breakerScaler(3)
	report := zoneReport{failing: map[string]int{"b": 5}, created: map[string]int{"b": 10}}
	s.zoneReportFn = func(ctx context.Context, since time.Time) (zoneReport, error) { return report, nil }

	// Three refresh cycles with the zone still bad: one "Benched zone" line, not three.
	for i := 0; i < 3; i++ {
		s.zoneCheckedAt = time.Time{}
		s.benchedZonesCached(context.Background())
	}
	var benched, released []string
	for _, e := range hook.AllEntries() {
		if strings.HasPrefix(e.Message, "Benched zone") {
			benched = append(benched, e.Message)
		}
		if strings.HasPrefix(e.Message, "Released zone") {
			released = append(released, e.Message)
		}
	}
	require.Len(t, benched, 1, "the alert metric counts this line; it must fire once per bench")
	// monitoring.tf matches ^Benched zone on the message; pin the literal prefix.
	assert.True(t, strings.HasPrefix(benched[0], "Benched zone b: 5 of 10 VMs"), benched[0])
	assert.Empty(t, released)

	// Zone recovers: exactly one release line, and still one bench line.
	report = zoneReport{}
	s.zoneCheckedAt = time.Time{}
	s.benchedZonesCached(context.Background())
	released = nil
	for _, e := range hook.AllEntries() {
		if strings.HasPrefix(e.Message, "Released zone") {
			released = append(released, e.Message)
		}
	}
	require.Len(t, released, 1)
	assert.True(t, strings.HasPrefix(released[0], "Released zone b"), released[0])
}

func TestBenchedZonesCachedDefaultsWindowWhenUnset(t *testing.T) {

	s := breakerScaler(3)
	s.conf.ZoneHealthWindow = 0
	s.zoneReportFn = func(ctx context.Context, since time.Time) (zoneReport, error) {
		assert.WithinDuration(t, time.Now().Add(-defaultZoneHealthWindow), since, 5*time.Second)
		return zoneReport{}, nil
	}
	s.benchedZonesCached(context.Background())
}
