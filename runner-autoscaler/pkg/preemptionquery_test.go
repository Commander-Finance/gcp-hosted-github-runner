package pkg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	monitoring "google.golang.org/api/monitoring/v3"
	"google.golang.org/api/option"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var queryEnd = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func int64Point(end time.Time, n int64) *monitoring.Point {
	return &monitoring.Point{
		Interval: &monitoring.TimeInterval{EndTime: end.Format(time.RFC3339)},
		Value:    &monitoring.TypedValue{Int64Value: &n},
	}
}

func zoneSeries(zone string, points ...*monitoring.Point) *monitoring.TimeSeries {
	return &monitoring.TimeSeries{Metric: &monitoring.Metric{Labels: map[string]string{"zone": zone}}, Points: points}
}

func TestDailyCountsFromSeries(t *testing.T) {

	day := 24 * time.Hour
	double := 4.0
	pages := []*monitoring.ListTimeSeriesResponse{
		{TimeSeries: []*monitoring.TimeSeries{
			zoneSeries("a", int64Point(queryEnd, 3), int64Point(queryEnd.Add(-day), 7)),
			zoneSeries("b", int64Point(queryEnd.Add(-2*day), 2)),
		}},
		{TimeSeries: []*monitoring.TimeSeries{
			// The same zone in a second page adds to the first.
			zoneSeries("a", int64Point(queryEnd.Add(-day), 1)),
			// A bucket end a minute off the query end still lands on its day.
			zoneSeries("b", int64Point(queryEnd.Add(-2*day+time.Minute), 1)),
			// Older than the requested days, and in the future: dropped.
			zoneSeries("b", int64Point(queryEnd.Add(-3*day), 100), int64Point(queryEnd.Add(13*time.Hour), 100)),
			// Aggregation can return a double; it counts the same.
			{Metric: &monitoring.Metric{Labels: map[string]string{"zone": "c"}}, Points: []*monitoring.Point{{
				Interval: &monitoring.TimeInterval{EndTime: queryEnd.Format(time.RFC3339)},
				Value:    &monitoring.TypedValue{DoubleValue: &double},
			}}},
			// A series with no zone cannot be attributed.
			{Metric: &monitoring.Metric{}, Points: []*monitoring.Point{int64Point(queryEnd, 50)}},
		}},
	}

	got, err := dailyCountsFromSeries(pages, queryEnd, 3)
	require.NoError(t, err)
	assert.Equal(t, []map[string]int{
		{"a": 3, "c": 4},
		{"a": 8},
		{"b": 3},
	}, got)
}

func TestDailyCountsFromSeriesRejectsBadInput(t *testing.T) {

	bad := []*monitoring.ListTimeSeriesResponse{{TimeSeries: []*monitoring.TimeSeries{
		zoneSeries("a", &monitoring.Point{Interval: &monitoring.TimeInterval{EndTime: "yesterday"}, Value: &monitoring.TypedValue{}}),
	}}}
	_, err := dailyCountsFromSeries(bad, queryEnd, 3)
	assert.Error(t, err, "an unreadable bucket must fail the read, not silently count as zero")

	partial := []*monitoring.ListTimeSeriesResponse{{ExecutionErrors: []*monitoring.Status{{Message: "partial"}}}}
	_, err = dailyCountsFromSeries(partial, queryEnd, 3)
	assert.Error(t, err, "a partial result could hide a zone's preemptions")
}

func TestQueryPreemptionReportReadsBothMetricsByDay(t *testing.T) {

	day := 24 * time.Hour
	var (
		mu       sync.Mutex
		requests []url.Values
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v3/projects/proj/timeSeries", r.URL.Path)
		q := r.URL.Query()
		mu.Lock()
		requests = append(requests, q)
		mu.Unlock()

		var resp monitoring.ListTimeSeriesResponse
		switch {
		case strings.Contains(q.Get("filter"), `"logging.googleapis.com/user/github_runner/vm_preempted"`):
			// Two pages: the second must be fetched.
			if q.Get("pageToken") == "" {
				resp = monitoring.ListTimeSeriesResponse{NextPageToken: "p2", TimeSeries: []*monitoring.TimeSeries{zoneSeries("b", int64Point(queryEnd, 2))}}
			} else {
				resp = monitoring.ListTimeSeriesResponse{TimeSeries: []*monitoring.TimeSeries{zoneSeries("b", int64Point(queryEnd.Add(-day), 3))}}
			}
		case strings.Contains(q.Get("filter"), `"logging.googleapis.com/user/github_runner/spot_vm_created"`):
			resp = monitoring.ListTimeSeriesResponse{TimeSeries: []*monitoring.TimeSeries{
				zoneSeries("a", int64Point(queryEnd, 100)),
				zoneSeries("b", int64Point(queryEnd, 90)),
			}}
		default:
			t.Errorf("unexpected filter %q", q.Get("filter"))
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer srv.Close()

	s := monitoringScaler(srv)

	got, err := s.queryPreemptionReport(context.Background(), queryEnd)
	require.NoError(t, err)

	// Window plus hold: nine days, newest first.
	require.Len(t, got.preempted, 9)
	require.Len(t, got.created, 9)
	assert.Equal(t, map[string]int{"b": 2}, got.preempted[0])
	assert.Equal(t, map[string]int{"b": 3}, got.preempted[1])
	assert.Equal(t, map[string]int{"a": 100, "b": 90}, got.created[0])
	assert.Empty(t, got.created[1])

	require.Len(t, requests, 3, "one read per metric plus the preemption metric's second page")
	for _, q := range requests {
		assert.Equal(t, "86400s", q.Get("aggregation.alignmentPeriod"))
		assert.Equal(t, "ALIGN_SUM", q.Get("aggregation.perSeriesAligner"))
		assert.Equal(t, "REDUCE_SUM", q.Get("aggregation.crossSeriesReducer"))
		assert.Equal(t, []string{"metric.label.zone"}, q["aggregation.groupByFields"])
		assert.Equal(t, queryEnd.Format(time.RFC3339), q.Get("interval.endTime"))
		assert.Equal(t, queryEnd.Add(-9*day).Format(time.RFC3339), q.Get("interval.startTime"))
	}
}

// monitoringScaler points the real Monitoring client at srv.
func monitoringScaler(srv *httptest.Server) *Autoscaler {

	s := &Autoscaler{conf: AutoscalerConfig{
		ProjectId:            "proj",
		PreemptionWindowDays: 7,
		PreemptionHoldDays:   2,
		PreemptedMetric:      "github_runner/vm_preempted",
		SpotCreatedMetric:    "github_runner/spot_vm_created",
	}}
	s.monitoringOpts = []option.ClientOption{option.WithEndpoint(srv.URL + "/"), option.WithHTTPClient(srv.Client())}
	return s
}

func TestQueryPreemptionReportFailsWhenEitherReadFails(t *testing.T) {

	// A rate needs both sides; a report with only preemptions would read as a
	// zone with no creations and bench on the count alone.
	for _, failing := range []string{"vm_preempted", "spot_vm_created"} {
		t.Run(failing, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Query().Get("filter"), `/`+failing+`"`) {
					http.Error(w, `{"error":{"code":500,"message":"backend error"}}`, http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.NewEncoder(w).Encode(monitoring.ListTimeSeriesResponse{TimeSeries: []*monitoring.TimeSeries{zoneSeries("b", int64Point(queryEnd, 5))}}))
			}))
			defer srv.Close()

			_, err := monitoringScaler(srv).queryPreemptionReport(context.Background(), queryEnd)
			assert.ErrorContains(t, err, failing)
		})
	}
}
