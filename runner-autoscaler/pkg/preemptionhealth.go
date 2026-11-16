package pkg

import (
	"context"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// preemptionWindowBench applies the bench rule to one window of per-zone counts and
// returns the preemption rate of every zone that meets it. A zone qualifies when it
// had at least minPreemptions preemptions and its rate is at least factor times the
// combined rate of every other zone. A zone's denominator floors at its preemption
// count, so creations that fell before the window opened cannot divide by zero.
// When no other zone saw a Spot VM in the window there is no fleet rate to compare
// against, so nothing qualifies. minPreemptions <= 0 disables the rule.
func preemptionWindowBench(preempted map[string]int, created map[string]int, minPreemptions int64, factor float64) map[string]float64 {

	benched := map[string]float64{}
	if minPreemptions <= 0 {
		return benched
	}

	denom := func(zone string) int {
		if created[zone] < preempted[zone] {
			return preempted[zone]
		}
		return created[zone]
	}
	zones := map[string]bool{}
	for z := range preempted {
		zones[z] = true
	}
	for z := range created {
		zones[z] = true
	}
	totalPreempted, totalDenom := 0, 0
	for z := range zones {
		totalPreempted += preempted[z]
		totalDenom += denom(z)
	}

	for zone, p := range preempted {
		if int64(p) < minPreemptions {
			continue
		}
		d := denom(zone)
		restPreempted, restDenom := totalPreempted-p, totalDenom-d
		if restDenom == 0 {
			continue
		}
		// p/d >= factor * restPreempted/restDenom, cross-multiplied so an exact
		// multiple is not lost to float division; a rest rate of zero benches on
		// the count alone.
		if float64(p)*float64(restDenom) >= factor*float64(restPreempted)*float64(d) {
			benched[zone] = float64(p) / float64(d)
		}
	}
	return benched
}

// preemptionReport holds daily per-zone Spot counts indexed by age: index 0 is the
// 24 h ending at query time, index 1 the day before, and so on.
type preemptionReport struct {
	preempted []map[string]int
	created   []map[string]int
}

type preemptionPolicy struct {
	minPreemptions int64
	factor         float64
	windowDays     int
	holdDays       int
}

// window sums the days aged [from, from+days) into per-zone totals, clipped to the
// history the report holds.
func (r preemptionReport) window(from int, days int) (map[string]int, map[string]int) {

	preempted, created := map[string]int{}, map[string]int{}
	for age := from; age < from+days; age++ {
		if age < len(r.preempted) {
			for z, n := range r.preempted[age] {
				preempted[z] += n
			}
		}
		if age < len(r.created) {
			for z, n := range r.created[age] {
				created[z] += n
			}
		}
	}
	return preempted, created
}

// onlyZones returns the report restricted to the given zones. The metrics keep a
// zone's history after it leaves the configuration; that zone gets no VMs, so it
// must neither take a bench slot nor shift the fleet rate the others are held to.
func (r preemptionReport) onlyZones(zones []string) preemptionReport {

	keep := toSet(zones)
	filter := func(days []map[string]int) []map[string]int {
		out := make([]map[string]int, len(days))
		for i, counts := range days {
			out[i] = map[string]int{}
			for z, n := range counts {
				if keep[z] {
					out[i][z] = n
				}
			}
		}
		return out
	}
	return preemptionReport{preempted: filter(r.preempted), created: filter(r.created)}
}

// preemptionBenchedZones returns the zones to try last for Spot. The rule runs on
// the window ending today and on the windows ending each of the previous holdDays
// days, and a zone that met it in any of them is benched. A benched zone gets almost
// no new Spot VMs, so its own recent counts say little; the earlier windows still
// cover the traffic that got it benched, and keep it benched for holdDays after
// today's window clears instead of releasing it the moment one old preemption ages
// out. Recovery is only ever tested by releasing the zone.
//
// At most zoneCount/2 zones are benched, worst rate first, so most Spot capacity
// stays in rotation: in family-fallback mode a benched zone receives no Spot
// attempt at all.
func preemptionBenchedZones(r preemptionReport, p preemptionPolicy, zoneCount int) map[string]bool {

	worst := map[string]float64{}
	for end := 0; end <= p.holdDays; end++ {
		preempted, created := r.window(end, p.windowDays)
		for zone, rate := range preemptionWindowBench(preempted, created, p.minPreemptions, p.factor) {
			if rate > worst[zone] {
				worst[zone] = rate
			}
		}
	}

	zones := make([]string, 0, len(worst))
	for z := range worst {
		zones = append(zones, z)
	}
	sort.Slice(zones, func(i, j int) bool {
		if worst[zones[i]] != worst[zones[j]] {
			return worst[zones[i]] > worst[zones[j]]
		}
		return zones[i] < zones[j]
	})
	if limit := zoneCount / 2; len(zones) > limit {
		zones = zones[:limit]
	}
	benched := map[string]bool{}
	for _, z := range zones {
		benched[z] = true
	}
	return benched
}

const (
	// preemptionCacheTTL bounds Monitoring reads to one per 10 minutes per instance.
	// A decision turns on days of counts, so a verdict up to 10 minutes old costs
	// little.
	preemptionCacheTTL = 10 * time.Minute

	// preemptionQueryTimeout is this sensor's slice of the create callback's budget,
	// on top of the connectivity breaker's; past it the last verdict stands.
	preemptionQueryTimeout = 10 * time.Second
)

// preemptionBenchedCached returns the zones to try last for Spot, querying
// Monitoring at most once per preemptionCacheTTL per process. It never queries
// when the breaker is disabled or the fleet has no Spot template. A failed or slow
// query keeps the last verdict, which rests on days of counts a failed read does
// not change; with no verdict yet that is no zone. The query is not retried until
// the TTL passes. As with the connectivity breaker, the lock is held across the
// query so a burst of creates waits on one read.
func (s *Autoscaler) preemptionBenchedCached(ctx context.Context) map[string]bool {

	if s.conf.PreemptionBenchMin <= 0 || s.conf.FallbackInstanceTemplate == "" {
		return map[string]bool{}
	}

	s.preemptMu.Lock()
	defer s.preemptMu.Unlock()

	if !s.preemptCheckedAt.IsZero() && time.Since(s.preemptCheckedAt) < preemptionCacheTTL {
		return s.preemptBenched
	}

	query := s.preemptionReportFn
	if query == nil {
		query = s.queryPreemptionReport
	}
	qctx, cancel := context.WithTimeout(ctx, preemptionQueryTimeout)
	defer cancel()
	started := time.Now()
	report, err := query(qctx, started)
	s.preemptCheckedAt = time.Now()
	if err != nil {
		if s.preemptBenched == nil {
			s.preemptBenched = map[string]bool{}
		}
		log.Warnf("Preemption health query failed after %s - keeping the last verdict (%s deprioritized) until the next check in %s: %s", time.Since(started).Round(time.Millisecond), zoneList(s.preemptBenched), preemptionCacheTTL, err.Error())
		return s.preemptBenched
	}

	report = report.onlyZones(s.conf.Zones)
	window := int(s.conf.PreemptionWindowDays)
	preempted, created := report.window(0, window)
	log.Infof("Preemption health: %s Spot VMs preempted/created in the last %d days, query took %s", countsSummary(preempted, created), window, time.Since(started).Round(time.Millisecond))

	benched := preemptionBenchedZones(report, preemptionPolicy{
		minPreemptions: s.conf.PreemptionBenchMin,
		factor:         s.conf.PreemptionBenchFactor,
		windowDays:     window,
		holdDays:       int(s.conf.PreemptionHoldDays),
	}, len(s.conf.Zones))

	// Transitions only, so the log carries one line per change rather than one per
	// refresh. The wording avoids "Benched zone", which the connectivity alert matches.
	for _, zone := range sortedKeys(benched) {
		if !s.preemptBenched[zone] {
			log.Warnf("Deprioritized zone %s for Spot: %d of %d Spot VMs preempted in the last %d days - trying it last for Spot", zone, preempted[zone], created[zone], window)
		}
	}
	for _, zone := range sortedKeys(s.preemptBenched) {
		if !benched[zone] {
			log.Infof("Restored zone %s for Spot: back in the normal rotation", zone)
		}
	}
	s.preemptBenched = benched
	return benched
}

// zoneList renders a zone set as "a, b" in zone order, or "no zone".
func zoneList(zones map[string]bool) string {

	if len(zones) == 0 {
		return "no zone"
	}
	return strings.Join(sortedKeys(zones), ", ")
}
