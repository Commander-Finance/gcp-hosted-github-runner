package pkg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The preemption breaker: zones whose Spot VMs are preempted far more often than
// the rest of the fleet move to the back of the Spot creation order. Counts come
// from two log-based metrics read through Cloud Monitoring.

func benchedKeys(m map[string]float64) []string {

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return sortedKeys(toSet(keys))
}

func TestPreemptionWindowBench(t *testing.T) {

	cases := []struct {
		name      string
		preempted map[string]int
		created   map[string]int
		min       int64
		factor    float64
		want      []string
	}{
		{
			// A week of even load: b is preempted ~6x as often as the rest of the
			// fleet; a is below the fleet average.
			name:      "chronically preempted zone among healthy ones",
			preempted: map[string]int{"a": 5, "b": 17, "c": 3},
			created:   map[string]int{"a": 1450, "b": 1450, "c": 1450, "f": 1450},
			min:       5, factor: 2,
			want: []string{"b"},
		},
		{
			name:      "too few preemptions even with nothing elsewhere",
			preempted: map[string]int{"b": 4},
			created:   map[string]int{"a": 100, "b": 100},
			min:       5, factor: 2,
			want: []string{},
		},
		{
			name:      "zero rate elsewhere benches on count alone",
			preempted: map[string]int{"b": 5},
			created:   map[string]int{"a": 1000, "b": 1000},
			min:       5, factor: 2,
			want: []string{"b"},
		},
		{
			name:      "rate exactly factor times the rest benches",
			preempted: map[string]int{"a": 3, "b": 6},
			created:   map[string]int{"a": 1000, "b": 1000},
			min:       5, factor: 2,
			want: []string{"b"},
		},
		{
			name:      "rate below factor times the rest",
			preempted: map[string]int{"a": 3, "b": 5},
			created:   map[string]int{"a": 1000, "b": 1000},
			min:       5, factor: 2,
			want: []string{},
		},
		{
			// Creations that fell before the window opened leave nothing to divide
			// by; the denominator floors at the preemption count instead.
			name:      "preemptions with no creations in the window",
			preempted: map[string]int{"a": 1, "b": 5},
			created:   map[string]int{"a": 1000},
			min:       5, factor: 2,
			want: []string{"b"},
		},
		{
			// Every Spot VM in the window went to b, as when the other zones are
			// stocked out. With no fleet rate to compare against, b is not singled out.
			name:      "no other zone had Spot creations",
			preempted: map[string]int{"b": 5},
			created:   map[string]int{"b": 1000},
			min:       5, factor: 2,
			want: []string{},
		},
		{
			name:      "disabled",
			preempted: map[string]int{"b": 500},
			created:   map[string]int{"a": 1000, "b": 1000},
			min:       0, factor: 2,
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := preemptionWindowBench(tc.preempted, tc.created, tc.min, tc.factor)
			assert.Equal(t, tc.want, benchedKeys(got))
		})
	}
}

// dailyReport builds a report of n days in which every listed zone gets perDay Spot
// creations each day and no preemptions.
func dailyReport(n int, perDay int, zones ...string) preemptionReport {

	r := preemptionReport{preempted: make([]map[string]int, n), created: make([]map[string]int, n)}
	for i := 0; i < n; i++ {
		r.preempted[i] = map[string]int{}
		r.created[i] = map[string]int{}
		for _, z := range zones {
			r.created[i][z] = perDay
		}
	}
	return r
}

var weekWithTwoDayHold = preemptionPolicy{minPreemptions: 5, factor: 2, windowDays: 7, holdDays: 2}

func TestPreemptionBenchedZonesHoldsPastTheWindow(t *testing.T) {

	cases := []struct {
		name    string
		age     int // days ago that b's five preemptions happened
		holdDay int
		want    map[string]bool
	}{
		{"inside today's window", 6, 2, map[string]bool{"b": true}},
		{"inside the window that ended two days ago", 8, 2, map[string]bool{"b": true}},
		{"older than every held window", 9, 2, map[string]bool{}},
		{"no hold releases once today's window clears", 7, 0, map[string]bool{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := dailyReport(12, 100, "a", "b")
			r.preempted[tc.age]["b"] = 5
			policy := weekWithTwoDayHold
			policy.holdDays = tc.holdDay
			assert.Equal(t, tc.want, preemptionBenchedZones(r, policy, 2))
		})
	}
}

func TestPreemptionBenchedZonesStaysBenchedThroughTheHold(t *testing.T) {

	// A zone just over the threshold: three preemptions on day 0 and two on day 5.
	// Day 5's window holds all five; from day 7 the window holds two. Without a hold
	// the zone is released the moment the day-0 preemptions age out; with a two-day
	// hold it stays benched two more days.
	history := dailyReport(20, 100, "a", "b")
	history.preempted[0]["b"] = 3
	history.preempted[5]["b"] = 2

	// reportOn returns the report as it would be read at the end of absolute day d:
	// age 0 is day d.
	reportOn := func(d int) preemptionReport {
		r := preemptionReport{}
		for day := d; day >= 0; day-- {
			r.preempted = append(r.preempted, history.preempted[day])
			r.created = append(r.created, history.created[day])
		}
		return r
	}
	benchedOn := func(policy preemptionPolicy) []bool {
		var out []bool
		for d := 5; d <= 9; d++ {
			out = append(out, preemptionBenchedZones(reportOn(d), policy, 2)["b"])
		}
		return out
	}

	noHold := weekWithTwoDayHold
	noHold.holdDays = 0
	assert.Equal(t, []bool{true, true, false, false, false}, benchedOn(noHold), "days 5-9 without a hold")
	assert.Equal(t, []bool{true, true, true, true, false}, benchedOn(weekWithTwoDayHold), "days 5-9 with a two-day hold")
}

func TestPreemptionBenchedZonesCapsAtHalfTheZones(t *testing.T) {

	// Three of four zones qualify against a large, clean fourth zone. Benching all
	// three would leave one zone for every Spot create, so only the worst two go.
	r := dailyReport(1, 0)
	r.preempted[0] = map[string]int{"a": 10, "b": 20, "c": 30}
	r.created[0] = map[string]int{"a": 1000, "b": 1000, "c": 1000, "d": 10000}

	assert.Equal(t, map[string]bool{"b": true, "c": true}, preemptionBenchedZones(r, weekWithTwoDayHold, 4))
	assert.Equal(t, map[string]bool{"c": true}, preemptionBenchedZones(r, weekWithTwoDayHold, 3))
	assert.Equal(t, map[string]bool{}, preemptionBenchedZones(r, weekWithTwoDayHold, 1), "a single zone has nothing to prefer")
}

func TestPreemptionBenchedZonesWithLessHistoryThanTheWindow(t *testing.T) {

	// Log-based metrics do not backfill: right after deployment the report covers
	// fewer days than the window. The rule runs on what exists.
	r := dailyReport(2, 100, "a", "b")
	r.preempted[1]["b"] = 5
	assert.Equal(t, map[string]bool{"b": true}, preemptionBenchedZones(r, weekWithTwoDayHold, 2))
	assert.Equal(t, map[string]bool{}, preemptionBenchedZones(preemptionReport{}, weekWithTwoDayHold, 2))
}

func preemptScaler(min int64) *Autoscaler {
	return &Autoscaler{conf: AutoscalerConfig{
		Zones:                    []string{"a", "b"},
		InstanceTemplate:         "primary",
		FallbackInstanceTemplate: "ondemand",
		PreemptionBenchMin:       min,
		PreemptionBenchFactor:    2,
		PreemptionWindowDays:     7,
		PreemptionHoldDays:       2,
	}}
}

// bWorstReport is one day in which b's Spot VMs were preempted and a's were not.
func bWorstReport() preemptionReport {
	r := dailyReport(1, 0)
	r.preempted[0] = map[string]int{"b": 17}
	r.created[0] = map[string]int{"a": 1450, "b": 1450}
	return r
}

func TestPreemptionBenchedCachedReusesAnswerWithinTTL(t *testing.T) {

	s := preemptScaler(5)
	calls := 0
	s.preemptionReportFn = func(ctx context.Context, end time.Time) (preemptionReport, error) {
		calls++
		assert.WithinDuration(t, time.Now(), end, 5*time.Second)
		return bWorstReport(), nil
	}

	first := s.preemptionBenchedCached(context.Background())
	second := s.preemptionBenchedCached(context.Background())
	assert.Equal(t, map[string]bool{"b": true}, first)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, calls, "second call inside the TTL must be served from cache")

	s.preemptCheckedAt = time.Now().Add(-2 * preemptionCacheTTL)
	s.preemptionBenchedCached(context.Background())
	assert.Equal(t, 2, calls)
}

func TestPreemptionBenchedCachedFailsOpenAndBacksOff(t *testing.T) {

	s := preemptScaler(5)
	calls := 0
	s.preemptionReportFn = func(ctx context.Context, end time.Time) (preemptionReport, error) {
		calls++
		return preemptionReport{}, errors.New("monitoring unavailable")
	}

	assert.Empty(t, s.preemptionBenchedCached(context.Background()), "with no verdict yet, a sensor error deprioritizes nothing")
	s.preemptionBenchedCached(context.Background())
	assert.Equal(t, 1, calls, "a failed query waits out the TTL instead of running on every create")
}

func TestPreemptionBenchedCachedSkipsWhenItCannotApply(t *testing.T) {

	never := func(ctx context.Context, end time.Time) (preemptionReport, error) {
		t.Fatal("the sensor must not be queried")
		return preemptionReport{}, nil
	}

	disabled := preemptScaler(0)
	disabled.preemptionReportFn = never
	assert.Empty(t, disabled.preemptionBenchedCached(context.Background()))

	// With no Spot template nothing can be preempted.
	standardOnly := preemptScaler(5)
	standardOnly.conf.FallbackInstanceTemplate = ""
	standardOnly.preemptionReportFn = never
	assert.Empty(t, standardOnly.preemptionBenchedCached(context.Background()))
}

func TestPreemptionBenchedCachedBoundsQueryTime(t *testing.T) {

	s := preemptScaler(5)
	s.preemptionReportFn = func(ctx context.Context, end time.Time) (preemptionReport, error) {
		dl, ok := ctx.Deadline()
		require.True(t, ok, "the sensor query must carry a deadline")
		assert.WithinDuration(t, time.Now().Add(preemptionQueryTimeout), dl, 2*time.Second)
		return preemptionReport{}, nil
	}
	s.preemptionBenchedCached(context.Background())
}

func TestPreemptionDeprioritizeAndRestoreAreLoggedOncePerTransition(t *testing.T) {

	hook := logtest.NewGlobal()
	defer hook.Reset()

	s := preemptScaler(5)
	report := bWorstReport()
	s.preemptionReportFn = func(ctx context.Context, end time.Time) (preemptionReport, error) { return report, nil }

	for i := 0; i < 3; i++ {
		s.preemptCheckedAt = time.Time{}
		s.preemptionBenchedCached(context.Background())
	}
	benched := messagesWithPrefix(hook, "Deprioritized zone")
	require.Len(t, benched, 1, "one line per bench, not one per refresh")
	assert.True(t, strings.HasPrefix(benched[0], "Deprioritized zone b for Spot: 17 of 1450 Spot VMs preempted"), benched[0])
	assert.Empty(t, messagesWithPrefix(hook, "Benched zone"), "the connectivity alert matches ^Benched zone and must not fire")

	refreshes := messagesWithPrefix(hook, "Preemption health:")
	require.Len(t, refreshes, 3, "one report per refresh, none from the cache")
	assert.Contains(t, refreshes[0], "a=0/1450 b=17/1450")

	report = dailyReport(1, 1450, "a", "b")
	s.preemptCheckedAt = time.Time{}
	s.preemptionBenchedCached(context.Background())
	restored := messagesWithPrefix(hook, "Restored zone")
	require.Len(t, restored, 1)
	assert.True(t, strings.HasPrefix(restored[0], "Restored zone b for Spot"), restored[0])
}

// seedStartingAt finds an instance name whose plain zone rotation starts at zone,
// so a test can tell a reordering apart from the rotation it started with.
func seedStartingAt(t *testing.T, s *Autoscaler, zone string) string {

	for i := 0; i < 1000; i++ {
		seed := fmt.Sprintf("runner-%d", i)
		if s.OrderedZones(seed)[0] == zone {
			return seed
		}
	}
	t.Fatalf("no seed starts the rotation at %s", zone)
	return ""
}

func zonesOf(plan []creationAttempt, model string) []string {

	var zones []string
	for _, a := range plan {
		if a.provisioningModel == model {
			zones = append(zones, a.zone)
		}
	}
	return zones
}

func TestCreationPlanPreemptionBenchAppliesToSpotOnly(t *testing.T) {

	s := spotFallbackScaler([]string{"a", "b", "c"})
	seed := seedStartingAt(t, s, "b")
	plan := s.creationPlan(seed, nil, nil, map[string]bool{"b": true})

	spot := zonesOf(plan, "spot")
	require.Len(t, spot, 3)
	assert.Equal(t, "b", spot[2], "the preempted zone is the last Spot attempt")
	// On-demand VMs are never preempted, so the on-demand sweep keeps the plain
	// rotation, which here starts at b.
	assert.Equal(t, s.OrderedZones(seed), zonesOf(plan, "standard"))
}

func TestCreationPlanFamilyFallbackPreemptionBenchAppliesToSpotOnly(t *testing.T) {

	s := familyScaler([]string{"a", "b"}, []string{"c4d-standard-2", "n4-standard-2", "c3-standard-4"})
	plan := s.creationPlan("runner-7", nil, nil, map[string]bool{"a": true})
	unbenched := s.creationPlan("runner-7", nil, nil, nil)

	for i, zone := range zonesOf(plan, "spot") {
		assert.Equal(t, "b", zone, "spot attempt %d", i)
	}
	assert.Equal(t, zonesOf(unbenched, "standard"), zonesOf(plan, "standard"))
}

func TestCreationPlanPrefersPreemptedZoneOverDisconnectedOne(t *testing.T) {

	// A zone that black-holes outbound traffic fails every job; a preempted zone
	// fails some. When both breakers trip, Spot goes healthy, then preempted, then
	// disconnected, and on-demand only avoids the disconnected zone.
	s := spotFallbackScaler([]string{"a", "b", "c"})
	connectivity := map[string]bool{"a": true}
	preemption := map[string]bool{"b": true}
	plan := s.creationPlan("runner-7", nil, connectivity, preemption)
	assert.Equal(t, []string{"c", "b", "a"}, zonesOf(plan, "spot"))
	assert.Equal(t, "a", zonesOf(plan, "standard")[2])

	// Every zone tripped one breaker or the other: Spot still prefers the
	// preempted zone to the disconnected one.
	two := spotFallbackScaler([]string{"a", "b"})
	assert.Equal(t, []string{"b", "a"}, zonesOf(two.creationPlan("runner-7", nil, connectivity, preemption), "spot"))

	family := familyScaler([]string{"a", "b"}, []string{"c4d-standard-2", "n4-standard-2", "c3-standard-4"})
	for i, zone := range zonesOf(family.creationPlan("runner-7", nil, connectivity, preemption), "spot") {
		assert.Equal(t, "b", zone, "family spot attempt %d", i)
	}
}

func TestCreateInstanceFromTemplateAvoidsPreemptedZoneForSpot(t *testing.T) {

	s := preemptScaler(5)
	s.preemptionReportFn = func(ctx context.Context, end time.Time) (preemptionReport, error) { return bWorstReport(), nil }
	var attempted []creationAttempt
	s.tryInsertFn = func(ctx context.Context, attempt creationAttempt, name string, md []*computepb.Items) error {
		attempted = append(attempted, attempt)
		return nil
	}

	for _, seed := range []string{"runner-1", "runner-2", "runner-3", "runner-4"} {
		attempted = nil
		require.NoError(t, s.CreateInstanceFromTemplate(context.Background(), seed, nil))
		require.Len(t, attempted, 1)
		assert.Equal(t, "a", attempted[0].zone, "seed %s: the first Spot attempt must avoid the preempted zone", seed)
		assert.Equal(t, "spot", attempted[0].provisioningModel)
	}
}

func TestValidateRejectsAPreemptionPolicyThatCannotWork(t *testing.T) {

	ok := AutoscalerConfig{
		RunnerPrefix: "runner", PreemptionBenchMin: 5, PreemptionBenchFactor: 2, PreemptionWindowDays: 7, PreemptionHoldDays: 2,
		PreemptedMetric: "github_runner/vm_preempted", SpotCreatedMetric: "github_runner/spot_vm_created",
	}

	bad := map[string]func(c *AutoscalerConfig){
		// A factor of 1 benches any zone merely average; below 1, a zone better than
		// the rest.
		"factor of one":               func(c *AutoscalerConfig) { c.PreemptionBenchFactor = 1 },
		"factor below one":            func(c *AutoscalerConfig) { c.PreemptionBenchFactor = 0.5 },
		"no window":                   func(c *AutoscalerConfig) { c.PreemptionWindowDays = 0 },
		"negative hold":               func(c *AutoscalerConfig) { c.PreemptionHoldDays = -1 },
		"missing preempted metric":    func(c *AutoscalerConfig) { c.PreemptedMetric = "" },
		"missing spot-created metric": func(c *AutoscalerConfig) { c.SpotCreatedMetric = "" },
	}
	require.NoError(t, ok.Validate())
	for name, mutate := range bad {
		c := ok
		mutate(&c)
		assert.Error(t, c.Validate(), name)
	}

	// Disabled, the other settings are unused and must not block startup.
	require.NoError(t, AutoscalerConfig{RunnerPrefix: "runner"}.Validate())
}

func TestPreemptionBenchedCachedIgnoresZonesNoLongerConfigured(t *testing.T) {

	// Zone x was removed from the configuration, but the metrics still hold its
	// history. It has the worst rate; counting it would spend one of the two bench
	// slots on a zone that receives no VMs anyway.
	s := preemptScaler(5)
	s.conf.Zones = []string{"a", "b", "c", "d"}
	r := dailyReport(1, 0)
	r.preempted[0] = map[string]int{"b": 20, "c": 30, "x": 90}
	r.created[0] = map[string]int{"a": 1000, "b": 1000, "c": 1000, "d": 10000, "x": 1000}
	s.preemptionReportFn = func(ctx context.Context, end time.Time) (preemptionReport, error) { return r, nil }

	assert.Equal(t, map[string]bool{"b": true, "c": true}, s.preemptionBenchedCached(context.Background()))
}

func TestPreemptionBenchedCachedKeepsLastVerdictThroughAFailedRead(t *testing.T) {

	// The verdict rests on days of counts; one failed read says nothing about them.
	hook := logtest.NewGlobal()
	defer hook.Reset()

	s := preemptScaler(5)
	fail := false
	s.preemptionReportFn = func(ctx context.Context, end time.Time) (preemptionReport, error) {
		if fail {
			return preemptionReport{}, errors.New("monitoring unavailable")
		}
		return bWorstReport(), nil
	}

	require.Equal(t, map[string]bool{"b": true}, s.preemptionBenchedCached(context.Background()))

	fail = true
	s.preemptCheckedAt = time.Time{}
	assert.Equal(t, map[string]bool{"b": true}, s.preemptionBenchedCached(context.Background()), "a failed read keeps the last verdict")

	fail = false
	s.preemptCheckedAt = time.Time{}
	s.preemptionBenchedCached(context.Background())
	assert.Len(t, messagesWithPrefix(hook, "Deprioritized zone"), 1, "the zone never left, so it is not deprioritized again")
	assert.Empty(t, messagesWithPrefix(hook, "Restored zone"))
}

func TestPreemptionWindowBenchRateFloorsAtPreemptions(t *testing.T) {

	// The rate ranks benched zones for the cap, so it must stay a fraction.
	noCreations := preemptionWindowBench(map[string]int{"a": 1, "b": 5}, map[string]int{"a": 1000}, 5, 2)
	assert.Equal(t, map[string]float64{"b": 1}, noCreations)

	fewerCreations := preemptionWindowBench(map[string]int{"a": 1, "b": 8}, map[string]int{"a": 1000, "b": 3}, 5, 2)
	assert.Equal(t, map[string]float64{"b": 1}, fewerCreations)
}

func TestPreemptionBenchedZonesHoldsAZoneThatGetsNoVMsWhileBenched(t *testing.T) {

	// In family-fallback mode a benched zone gets no Spot VMs, so after the bench
	// its own counts are all zero. Five preemptions on day 5 bench b; from day 6
	// all Spot traffic goes to a. The day-5 preemptions sit in the window ending on
	// days 5-11 and the two-day hold carries b through day 13.
	history := dailyReport(20, 0)
	for day := 0; day < 20; day++ {
		if day <= 5 {
			history.created[day] = map[string]int{"a": 100, "b": 100}
		} else {
			history.created[day] = map[string]int{"a": 200}
		}
	}
	history.preempted[5]["b"] = 5

	reportOn := func(d int) preemptionReport {
		r := preemptionReport{}
		for day := d; day >= 0; day-- {
			r.preempted = append(r.preempted, history.preempted[day])
			r.created = append(r.created, history.created[day])
		}
		return r
	}
	var benched []int
	for d := 5; d < 20; d++ {
		if preemptionBenchedZones(reportOn(d), weekWithTwoDayHold, 2)["b"] {
			benched = append(benched, d)
		}
	}
	assert.Equal(t, []int{5, 6, 7, 8, 9, 10, 11, 12, 13}, benched)
}

func TestProcessJobAvoidsPreemptedZoneForSpot(t *testing.T) {

	// The durable create path, across enough jobs that some VM names' plain
	// rotation starts at the preempted zone.
	startedAtPreempted := 0
	for id := int64(1); id <= 20; id++ {
		s, _, src, j := lifecycleTestScaler()
		s.conf.PreemptionBenchMin, s.conf.PreemptionBenchFactor = 5, 2
		s.conf.PreemptionWindowDays, s.conf.PreemptionHoldDays = 7, 2
		r := dailyReport(1, 0)
		r.preempted[0] = map[string]int{"z2": 17}
		r.created[0] = map[string]int{"z1": 1450, "z2": 1450}
		s.preemptionReportFn = func(ctx context.Context, end time.Time) (preemptionReport, error) { return r, nil }
		var attempted []creationAttempt
		var name string
		s.tryInsertFn = func(_ context.Context, a creationAttempt, n string, _ []*computepb.Items) error {
			attempted = append(attempted, a)
			name = n
			return nil
		}

		j.Id = id
		ctx := context.Background()
		require.NoError(t, s.observe(ctx, src, j, false))
		require.NoError(t, s.processJob(ctx, src, j))
		require.NotEmpty(t, attempted)
		assert.Equal(t, "z1", attempted[0].zone, "job %d", id)
		assert.Equal(t, "spot", attempted[0].provisioningModel, "job %d", id)
		if s.OrderedZones(name)[0] == "z2" {
			startedAtPreempted++
		}
	}
	require.NotZero(t, startedAtPreempted, "no job's plain rotation started at the preempted zone, so the test proved nothing")
}
