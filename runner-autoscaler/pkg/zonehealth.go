package pkg

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/logadmin"
	log "github.com/sirupsen/logrus"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/structpb"
)

// Zone circuit breaker.
//
// GCP zonal host networking degrades for minutes to hours at a time: every outbound
// TCP connect from the runner VMs in one zone black-holes while the VMs themselves
// report nothing. The one signal that catches it is the runner's own Ops Agent,
// which retries its metrics export every minute and logs a dial timeout when it
// cannot connect out. The autoscaler reads that stream from Cloud Logging and moves
// the affected zone to the back of the creation order.
//
// The service scales to zero and is restarted under memory pressure, so nothing
// here survives in memory across requests by design: Cloud Logging is the state
// and the lookback window is the memory. The query runs on the create path behind
// a short cache, and every failure falls open to the plain zone rotation.
//
// The sensor assumes the runner image ships the Ops Agent with its default syslog
// receiver, so the line lands in projects/<project>/logs/syslog. This module does
// not install the agent; the consumer's image does (spock-runner bakes it in with
// Packer). Without it the failing side is always empty and the breaker never
// trips, silently. Disable it with ZoneBenchMinVMs=0 on images without the agent.

const (
	// zoneHealthCacheTTL bounds Logging reads to about one per minute per instance,
	// well inside the entries.list quota, and bounds how stale a bench decision can be.
	zoneHealthCacheTTL = 60 * time.Second

	// zoneHealthQueryTimeout caps what the sensor may cost a create. The whole create
	// callback shares a 180 s budget with the JIT-config round trip and every zone and
	// family attempt, so the sensor gets a fixed slice and fails open past it. The two
	// reads finish in under a second from a workstation but can take several seconds
	// from Cloud Run; the refresh log line records the real duration so this can be
	// tuned from evidence.
	zoneHealthQueryTimeout = 15 * time.Second

	// Caps on entries read per query. A stuck VM logs one or two timeout lines a
	// minute, so 3000 covers hundreds of VM-minutes; creates run at most tens a minute.
	zoneHealthMaxFailingEntries = 3000
	zoneHealthMaxCreatedEntries = 2000

	defaultZoneHealthWindow = 10 * time.Minute
)

// zoneReport is what the sensor returns for one lookback window: distinct VMs per
// zone that logged an outbound dial timeout, and VMs created per zone. Both are in
// VMs, not log lines, so the two sides of the ratio share a unit.
type zoneReport struct {
	failing map[string]int
	created map[string]int
}

// summary renders the report as "zone=failing/created ..." in zone order, or
// "no VMs" when the window was empty.
func (r zoneReport) summary() string {

	zones := map[string]bool{}
	for z := range r.failing {
		zones[z] = true
	}
	for z := range r.created {
		zones[z] = true
	}
	if len(zones) == 0 {
		return "no VMs"
	}
	parts := make([]string, 0, len(zones))
	for _, z := range sortedKeys(zones) {
		parts = append(parts, fmt.Sprintf("%s=%d/%d", z, r.failing[z], r.created[z]))
	}
	return strings.Join(parts, " ")
}

// benchedZones applies the thresholds: a zone is benched when at least minVMs of its
// VMs failed and they are at least minRatio of the VMs created there in the window.
// The denominator floors at the failing count so a zone whose VMs were created
// before the window opened still benches instead of dividing by zero. minVMs <= 0
// disables the breaker.
func benchedZones(r zoneReport, minVMs int64, minRatio float64) map[string]bool {

	benched := map[string]bool{}
	if minVMs <= 0 {
		return benched
	}
	for zone, n := range r.failing {
		if int64(n) < minVMs {
			continue
		}
		denom := r.created[zone]
		if denom < n {
			denom = n
		}
		if float64(n)/float64(denom) >= minRatio {
			benched[zone] = true
		}
	}
	return benched
}

// orderedZonesBenched is OrderedZones with the benched zones moved to the end, each
// group keeping its rotation order. Benched zones stay in the list: they are the
// last resort when every healthy zone is out of capacity, not excluded. When every
// zone is benched there is nothing to prefer, so the plain rotation stands.
func (s *Autoscaler) orderedZonesBenched(seed string, benched map[string]bool) []string {

	ordered := s.OrderedZones(seed)
	if len(benched) == 0 {
		return ordered
	}
	healthy := make([]string, 0, len(ordered))
	bad := make([]string, 0, len(benched))
	for _, z := range ordered {
		if benched[z] {
			bad = append(bad, z)
		} else {
			healthy = append(healthy, z)
		}
	}
	if len(healthy) == 0 {
		return ordered
	}
	return append(healthy, bad...)
}

// healthyZones returns the ordered zones with the benched ones removed, for walks
// that assign zones by index rather than trying every zone. All zones are returned
// when none or all are benched, matching orderedZonesBenched.
func healthyZones(ordered []string, benched map[string]bool) []string {

	if len(benched) == 0 {
		return ordered
	}
	healthy := make([]string, 0, len(ordered))
	for _, z := range ordered {
		if !benched[z] {
			healthy = append(healthy, z)
		}
	}
	if len(healthy) == 0 {
		return ordered
	}
	return healthy
}

// benchedZonesCached returns the zones to try last, querying the sensor at most
// once per zoneHealthCacheTTL per process. A failed query benches nothing and is
// not retried until the TTL passes, so a Logging outage costs one bounded query a
// minute rather than one per create. The lock is held across the query on purpose:
// a burst of creates waits on one query instead of each issuing its own.
func (s *Autoscaler) benchedZonesCached(ctx context.Context) map[string]bool {

	if s.conf.ZoneBenchMinVMs <= 0 {
		return map[string]bool{}
	}

	s.zoneMu.Lock()
	defer s.zoneMu.Unlock()

	if !s.zoneCheckedAt.IsZero() && time.Since(s.zoneCheckedAt) < zoneHealthCacheTTL {
		return s.zoneBenched
	}

	window := time.Duration(s.conf.ZoneHealthWindow) * time.Second
	if window <= 0 {
		window = defaultZoneHealthWindow
	}
	query := s.zoneReportFn
	if query == nil {
		query = s.queryZoneReport
	}

	qctx, cancel := context.WithTimeout(ctx, zoneHealthQueryTimeout)
	defer cancel()
	started := time.Now()
	report, err := query(qctx, started.Add(-window))
	s.zoneCheckedAt = time.Now()
	if err != nil {
		log.Warnf("Zone health query failed after %s - benching no zone until the next check in %s: %s", time.Since(started).Round(time.Millisecond), zoneHealthCacheTTL, err.Error())
		s.zoneBenched = map[string]bool{}
		return s.zoneBenched
	}
	// One line per refresh (at most one a minute): what the sensor saw and what it
	// cost, so the query budget and thresholds can be tuned from the log.
	log.Infof("Zone health: %s in the last %s, query took %s", report.summary(), window, time.Since(started).Round(time.Millisecond))

	benched := benchedZones(report, s.conf.ZoneBenchMinVMs, s.conf.ZoneBenchMinRatio)
	s.logBenchChanges(benched, report, window)
	s.zoneBenched = benched
	return benched
}

// logBenchChanges announces transitions only, so the log carries one line per
// bench or release rather than one per minute. "Benched zone" is the substring the
// monitoring alert matches on.
func (s *Autoscaler) logBenchChanges(benched map[string]bool, r zoneReport, window time.Duration) {

	for _, zone := range sortedKeys(benched) {
		if !s.zoneBenched[zone] {
			log.Warnf("Benched zone %s: %d of %d VMs created in the last %s lost outbound connectivity - trying it last", zone, r.failing[zone], r.created[zone], window)
		}
	}
	for _, zone := range sortedKeys(s.zoneBenched) {
		if !benched[zone] {
			log.Infof("Released zone %s: back in the normal rotation", zone)
		}
	}
}

func sortedKeys(m map[string]bool) []string {

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// zoneHealthFilters builds the two Cloud Logging filters for a window. The first
// matches the Ops Agent's dial-timeout line on runner VMs; the second matches the
// autoscaler's own "Created instance" line, scoped to this service when Cloud Run
// provides its name via K_SERVICE. Substring (":") matches are used instead of
// regex because they are cheaper for the Logging backend.
//
// The syslog line starts "<Mon> <d> <time> <hostname> <process>[<pid>]: ...", so
// matching " <prefix>-" keeps other workloads' VMs in the same project from being
// read at all; syslogHostname then checks the hostname exactly.
func zoneHealthFilters(project string, runnerPrefix string, service string, since time.Time) (string, string) {

	ts := since.UTC().Format(time.RFC3339)
	failing := fmt.Sprintf(`resource.type="gce_instance" AND logName="projects/%s/logs/syslog" AND timestamp>="%s" AND jsonPayload.message:"Exporting failed" AND jsonPayload.message:"i/o timeout" AND jsonPayload.message:" %s-"`, project, ts, runnerPrefix)
	created := fmt.Sprintf(`resource.type="cloud_run_revision" AND timestamp>="%s" AND jsonPayload.message:"Created instance"`, ts)
	if service != "" {
		created += fmt.Sprintf(` AND resource.labels.service_name="%s"`, service)
	}
	return failing, created
}

// zoneReportFromEntries reduces raw entries to VMs per zone. A stuck VM emits the
// timeout line every minute, so failing VMs are deduplicated by instance id; created
// VMs are deduplicated by instance name so a retried create counts once. Only VMs
// whose syslog hostname carries this autoscaler's runner prefix count on the
// failing side, so another workload sharing the project cannot bench a zone.
func zoneReportFromEntries(runnerPrefix string, failing []*logging.Entry, created []*logging.Entry) zoneReport {

	r := zoneReport{failing: map[string]int{}, created: map[string]int{}}

	seen := map[string]bool{}
	for _, e := range failing {
		if e.Resource == nil {
			continue
		}
		zone, id := e.Resource.Labels["zone"], e.Resource.Labels["instance_id"]
		if zone == "" || id == "" || seen[id] {
			continue
		}
		if !IsOwnedRunnerName(runnerPrefix, syslogHostname(e.Payload)) {
			continue
		}
		seen[id] = true
		r.failing[zone]++
	}

	seenCreated := map[string]bool{}
	for i, e := range created {
		zone := payloadString(e.Payload, "zone")
		if zone == "" {
			continue
		}
		key := payloadString(e.Payload, "instance")
		if key == "" {
			key = fmt.Sprintf("#%d", i)
		}
		if seenCreated[key] {
			continue
		}
		seenCreated[key] = true
		r.created[zone]++
	}
	return r
}

// syslogHostname returns the hostname field of an Ops Agent syslog line
// ("<Mon> <d> <time> <hostname> <process>[<pid>]: ..."), or "" when the payload
// is not that shape. The Ops Agent does not label the entry with the instance
// name, so the line itself is the only place the runner's name appears.
func syslogHostname(payload interface{}) string {

	fields := strings.Fields(payloadString(payload, "message"))
	if len(fields) < 4 {
		return ""
	}
	return fields[3]
}

// payloadString reads one string field from a jsonPayload. logadmin currently
// delivers jsonPayload as *structpb.Struct and carries a TODO to switch to
// map[string]interface{}; accepting both keeps a library bump from silently
// emptying the sensor (which fails open, so nothing would announce it).
func payloadString(payload interface{}, key string) string {

	switch p := payload.(type) {
	case *structpb.Struct:
		return p.GetFields()[key].GetStringValue()
	case map[string]interface{}:
		s, _ := p[key].(string)
		return s
	}
	return ""
}

// queryZoneReport is the production sensor: two bounded Cloud Logging reads. The
// client is built per query, matching how the other GCP clients are used here; at
// one query a minute the construction cost is noise.
func (s *Autoscaler) queryZoneReport(ctx context.Context, since time.Time) (zoneReport, error) {

	client, err := logadmin.NewClient(ctx, s.conf.ProjectId)
	if err != nil {
		return zoneReport{}, fmt.Errorf("logging client: %w", err)
	}
	defer client.Close()

	failF, createdF := zoneHealthFilters(s.conf.ProjectId, s.conf.RunnerPrefix, strings.TrimSpace(os.Getenv("K_SERVICE")), since)

	// The two reads are independent and share one deadline, so run them together:
	// during a real incident the dial-timeout read is at its largest, and serially
	// it would starve the create read of the budget. logadmin.Client is safe for
	// concurrent use.
	var (
		wg              sync.WaitGroup
		failing         []*logging.Entry
		created         []*logging.Entry
		failErr, crtErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		failing, failErr = listEntries(ctx, client, failF, zoneHealthMaxFailingEntries)
	}()
	go func() {
		defer wg.Done()
		created, crtErr = listEntries(ctx, client, createdF, zoneHealthMaxCreatedEntries)
	}()
	wg.Wait()
	if failErr != nil {
		return zoneReport{}, fmt.Errorf("dial-timeout entries: %w", failErr)
	}
	if crtErr != nil {
		return zoneReport{}, fmt.Errorf("created-instance entries: %w", crtErr)
	}
	return zoneReportFromEntries(s.conf.RunnerPrefix, failing, created), nil
}

func listEntries(ctx context.Context, client *logadmin.Client, filter string, max int) ([]*logging.Entry, error) {

	it := client.Entries(ctx, logadmin.Filter(filter), logadmin.NewestFirst())
	entries := make([]*logging.Entry, 0, 64)
	for len(entries) < max {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}
