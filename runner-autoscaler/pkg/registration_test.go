package pkg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunner404InventoryFindsRegistrationOnLaterPage(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, j, false))
	require.NoError(t, s.processJob(ctx, src, j))
	name := m.get(jobKey(src.Name, j)).VMName
	require.NoError(t, m.RememberRunnerID(ctx, name, 42))
	requests := 0
	s.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		code, body := 200, ""
		switch {
		case strings.HasSuffix(req.URL.Path, "/42"):
			code = 404
		case req.URL.Query().Get("page") == "1":
			body = `{"runners":[` + strings.TrimSuffix(strings.Repeat(`{"id":1,"name":"other"},`, 100), ",") + `]}`
		default:
			body = fmt.Sprintf(`{"runners":[{"id":43,"name":%q,"busy":true,"status":"online"}]}`, name)
		}
		return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	state, err := s.runnerStateWithPAT(ctx, src, name, "test")
	require.NoError(t, err)
	require.Equal(t, runnerBusy, state)
	require.Equal(t, 3, requests)
	require.Empty(t, m.backoffs)
	registration, err := m.Runner(ctx, name)
	require.NoError(t, err)
	require.EqualValues(t, 43, registration.Record.RunnerID)
}

func TestRegistrationCleanupRetainsCapacityWhenDeletionFails(t *testing.T) {
	s, m, src, j := lifecycleTestScaler()
	ctx := context.Background()
	require.NoError(t, s.observe(ctx, src, j, false))
	require.NoError(t, s.processJob(ctx, src, j))
	old := m.get(jobKey(src.Name, j)).VMName
	s.instanceStateFn = func(context.Context, string) (bool, State, error) { return true, RUNNING, nil }
	s.runnerStateFn = func(context.Context, Source, string) (runnerRegistration, error) { return runnerGone, nil }
	s.deleteInZoneFn = func(context.Context, string, string) (bool, error) { return true, errors.New("delete unavailable") }
	require.Error(t, s.processJob(ctx, src, j))
	require.Equal(t, 1, m.fleet.Runners)
	require.Equal(t, old, m.get(jobKey(src.Name, j)).VMName)
}

func TestRunner404RequiresSuccessfulInventory(t *testing.T) {
	for _, code := range []int{200, 403, 404, 429, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s, m, src, j := lifecycleTestScaler()
			ctx := context.Background()
			require.NoError(t, s.observe(ctx, src, j, false))
			require.NoError(t, s.processJob(ctx, src, j))
			name := m.get(jobKey(src.Name, j)).VMName
			require.NoError(t, m.RememberRunnerID(ctx, name, 42))
			requests := 0
			s.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				status := code
				if strings.HasSuffix(req.URL.Path, "/42") {
					status = 404
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"runners":[]}`))}, nil
			})}
			state, err := s.runnerStateWithPAT(ctx, src, name, "test")
			require.Equal(t, 2, requests)
			if code == 200 {
				require.NoError(t, err)
				require.Equal(t, runnerGone, state)
				require.Empty(t, m.backoffs)
			} else {
				require.Error(t, err)
				if code != 503 {
					require.NotEmpty(t, m.backoffs)
				}
			}
		})
	}
}

func TestRegistrationLifecycleCleanup(t *testing.T) {
	for _, tc := range []struct {
		status         string
		registration   runnerRegistration
		remove, detach bool
	}{
		{"completed", runnerGone, true, false},
		{"completed", runnerIdle, true, false},
		{"queued", runnerGone, true, false},
		{"queued", runnerIdle, false, false},
		{"queued", runnerBusy, false, true},
		{"completed", runnerBusy, false, true},
		{"completed", runnerOffline, true, false},
		{"queued", runnerOffline, false, false},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.status, tc.registration), func(t *testing.T) {
			s, m, src, j := lifecycleTestScaler()
			ctx := context.Background()
			require.NoError(t, s.observe(ctx, src, j, false))
			require.NoError(t, s.processJob(ctx, src, j))
			old := m.get(jobKey(src.Name, j)).VMName
			s.jobStatusFn = func(context.Context, Job) (string, error) { return tc.status, nil }
			s.instanceStateFn = func(context.Context, string) (bool, State, error) { return true, RUNNING, nil }
			s.runnerStateFn = func(context.Context, Source, string) (runnerRegistration, error) { return tc.registration, nil }
			deleted := 0
			s.deleteInZoneFn = func(_ context.Context, name, _ string) (bool, error) {
				require.Equal(t, old, name)
				deleted++
				return true, nil
			}
			require.NoError(t, s.processJob(ctx, src, j))
			if tc.remove {
				require.Equal(t, 1, deleted)
				require.Zero(t, m.fleet.Runners)
				if tc.status == "queued" {
					require.NoError(t, s.processJob(ctx, src, j))
					require.NotEmpty(t, m.get(jobKey(src.Name, j)).VMName)
					require.NotEqual(t, old, m.get(jobKey(src.Name, j)).VMName)
				}
			} else {
				require.Zero(t, deleted)
				require.Equal(t, 1, m.fleet.Runners)
				if tc.detach {
					require.Empty(t, m.get(jobKey(src.Name, j)).VMName)
					// A detached runner that later loses registration must also be reclaimed.
					require.NoError(t, m.DeferRunner(ctx, old, time.Now().Add(-time.Minute), false))
					s.runnerStateFn = func(context.Context, Source, string) (runnerRegistration, error) { return runnerGone, nil }
					require.NoError(t, s.reconcileRunners(ctx))
					require.Equal(t, 1, deleted)
					require.Zero(t, m.fleet.Runners)
				} else {
					require.Equal(t, old, m.get(jobKey(src.Name, j)).VMName)
				}
			}
		})
	}
}

func TestOfflineRunnerGraceAndDetachedAvailability(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(fmt.Sprint(detached), func(t *testing.T) {
			s, m, src, j := lifecycleTestScaler()
			s.conf.RunnerRegisterTimeout = 300
			ctx := context.Background()
			require.NoError(t, s.observe(ctx, src, j, false))
			require.NoError(t, s.processJob(ctx, src, j))
			key := jobKey(src.Name, j)
			name := m.get(key).VMName
			s.instanceStateFn = func(context.Context, string) (bool, State, error) { return true, RUNNING, nil }
			s.runnerStateFn = func(context.Context, Source, string) (runnerRegistration, error) { return runnerOffline, nil }
			deleted := 0
			s.deleteInZoneFn = func(context.Context, string, string) (bool, error) { deleted++; return true, nil }
			if detached {
				// Previously idle spare disconnects after it was advertised.
				_, err := s.claim(ctx, key, "detach")
				require.NoError(t, err)
				require.NoError(t, m.Detach(ctx, key, "detach", true, time.Now()))
				require.NoError(t, s.reconcileRunners(ctx))
				r, err := m.Runner(ctx, name)
				require.NoError(t, err)
				require.False(t, r.Available)
				m.mu.Lock()
				r.Record.CreatedAt = time.Now().Add(-301 * time.Second)
				r.NextActionAt = time.Now()
				m.runners[name] = r
				m.mu.Unlock()
			} else {
				require.NoError(t, s.processJob(ctx, src, j))
				require.Equal(t, name, m.get(key).VMName)
				require.NoError(t, m.UpdateJob(ctx, key, func(r *lifecycleRecord, _ *fleetState) error {
					r.CreatedAt = time.Now().Add(-301 * time.Second)
					return nil
				}))
			}
			require.Zero(t, deleted)
			if detached {
				require.NoError(t, s.reconcileRunners(ctx))
			} else {
				require.NoError(t, s.processJob(ctx, src, j))
			}
			require.Equal(t, 1, deleted)
			require.Zero(t, m.fleet.Runners)
			if !detached {
				require.NoError(t, s.processJob(ctx, src, j))
				require.NotEmpty(t, m.get(key).VMName)
				require.NotEqual(t, name, m.get(key).VMName)
			}
		})
	}
}

func TestRegistrationStatusFromDirectAndInventoryResponses(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for _, tc := range []struct {
			status string
			busy   bool
			want   runnerRegistration
		}{
			{"online", false, runnerIdle}, {"online", true, runnerBusy},
			{"offline", false, runnerOffline}, {"offline", true, runnerOffline},
			{"", false, runnerOffline},
		} {
			t.Run(fmt.Sprintf("%t/%s/%t", direct, tc.status, tc.busy), func(t *testing.T) {
				s, m, src, j := lifecycleTestScaler()
				ctx := context.Background()
				require.NoError(t, s.observe(ctx, src, j, false))
				require.NoError(t, s.processJob(ctx, src, j))
				name := m.get(jobKey(src.Name, j)).VMName
				if direct {
					require.NoError(t, m.RememberRunnerID(ctx, name, 42))
				}
				s.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					body := fmt.Sprintf(`{"id":42,"name":%q,"status":%q,"busy":%t}`, name, tc.status, tc.busy)
					if !direct {
						body = `{"runners":[` + body + `]}`
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				state, err := s.runnerStateWithPAT(ctx, src, name, "test")
				require.NoError(t, err)
				require.Equal(t, tc.want, state)
			})
		}
	}
}
