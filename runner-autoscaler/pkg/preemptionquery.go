package pkg

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	monitoring "google.golang.org/api/monitoring/v3"
)

const countBucket = 24 * time.Hour

// dailyCountsFromSeries turns timeSeries.list pages, aligned to one-day buckets and
// grouped by the metric's zone label, into per-zone counts indexed by age (0 is the
// day ending at end). Every age below days gets a map, empty when nothing was
// counted. Bucket end times are rounded to the nearest day so a bucket boundary a
// few seconds off the requested end still lands on its day; buckets outside
// [0, days) are dropped. A partial result or an unreadable bucket fails the read,
// because a dropped series could hide the one zone being preempted.
func dailyCountsFromSeries(pages []*monitoring.ListTimeSeriesResponse, end time.Time, days int) ([]map[string]int, error) {

	counts := make([]map[string]int, days)
	for i := range counts {
		counts[i] = map[string]int{}
	}
	for _, page := range pages {
		if len(page.ExecutionErrors) > 0 {
			return nil, fmt.Errorf("partial time series result: %s", page.ExecutionErrors[0].Message)
		}
		for _, ts := range page.TimeSeries {
			if ts.Metric == nil || ts.Metric.Labels["zone"] == "" {
				continue
			}
			zone := ts.Metric.Labels["zone"]
			for _, p := range ts.Points {
				if p.Interval == nil || p.Value == nil {
					return nil, fmt.Errorf("time series point for zone %s has no interval or value", zone)
				}
				bucketEnd, err := time.Parse(time.RFC3339, p.Interval.EndTime)
				if err != nil {
					return nil, fmt.Errorf("time series point for zone %s: %w", zone, err)
				}
				age := int(math.Floor((float64(end.Sub(bucketEnd)) + float64(countBucket)/2) / float64(countBucket)))
				if age < 0 || age >= days {
					continue
				}
				switch {
				case p.Value.Int64Value != nil:
					counts[age][zone] += int(*p.Value.Int64Value)
				case p.Value.DoubleValue != nil:
					counts[age][zone] += int(math.Round(*p.Value.DoubleValue))
				}
			}
		}
	}
	return counts, nil
}

// queryPreemptionReport reads daily per-zone Spot preemptions and Spot creations
// for the window plus the hold, ending at end. Both are log-based counters, so one
// timeSeries.list per metric (summed per day, grouped by zone) returns at most a
// few points per zone however busy the fleet is.
func (s *Autoscaler) queryPreemptionReport(ctx context.Context, end time.Time) (preemptionReport, error) {

	svc, err := monitoring.NewService(ctx, s.monitoringOpts...)
	if err != nil {
		return preemptionReport{}, fmt.Errorf("monitoring client: %w", err)
	}
	days := int(s.conf.PreemptionWindowDays + s.conf.PreemptionHoldDays)

	read := func(metric string) ([]map[string]int, error) {
		call := svc.Projects.TimeSeries.List("projects/" + s.conf.ProjectId).
			Filter(fmt.Sprintf(`metric.type="logging.googleapis.com/user/%s"`, metric)).
			IntervalStartTime(end.Add(-time.Duration(days) * countBucket).Format(time.RFC3339)).
			IntervalEndTime(end.Format(time.RFC3339)).
			AggregationAlignmentPeriod(fmt.Sprintf("%ds", int64(countBucket/time.Second))).
			AggregationPerSeriesAligner("ALIGN_SUM").
			AggregationCrossSeriesReducer("REDUCE_SUM").
			AggregationGroupByFields("metric.label.zone")
		var pages []*monitoring.ListTimeSeriesResponse
		if err := call.Pages(ctx, func(page *monitoring.ListTimeSeriesResponse) error {
			pages = append(pages, page)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("%s: %w", metric, err)
		}
		return dailyCountsFromSeries(pages, end, days)
	}

	// The two reads are independent and share one deadline, so run them together
	// rather than letting the first starve the second of the budget. A
	// monitoring.Service is safe for concurrent use.
	var (
		wg                 sync.WaitGroup
		preempted, created []map[string]int
		preErr, crtErr     error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		preempted, preErr = read(s.conf.PreemptedMetric)
	}()
	go func() {
		defer wg.Done()
		created, crtErr = read(s.conf.SpotCreatedMetric)
	}()
	wg.Wait()
	if preErr != nil {
		return preemptionReport{}, preErr
	}
	if crtErr != nil {
		return preemptionReport{}, crtErr
	}
	return preemptionReport{preempted: preempted, created: created}, nil
}
