package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"sui-copilot/internal/buildinfo"
)

func TestCheckUpdate(t *testing.T) {
	originalVersion := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = originalVersion })
	for _, tc := range []struct {
		name      string
		current   string
		payload   string
		upstream  int
		err       error
		wantCode  int
		available bool
	}{
		{name: "newer release", current: "v0.2.0", payload: `{"tag_name":"v0.3.0"}`, wantCode: 200, available: true},
		{name: "same version", current: "v0.3.0", payload: `{"tag_name":"v0.3.0"}`, wantCode: 200},
		{name: "local suffix", current: "v0.3.0-5-gabcdef-dirty", payload: `{"tag_name":"v0.3.0"}`, wantCode: 200},
		{name: "ahead of release", current: "v0.4.0", payload: `{"tag_name":"v0.3.0"}`, wantCode: 200},
		{name: "development build", current: "dev", payload: `{"tag_name":"v0.3.0"}`, wantCode: 200, available: true},
		{name: "rate limited", upstream: http.StatusForbidden, wantCode: 502},
		{name: "network failure", err: errors.New("offline"), wantCode: 502},
		{name: "missing tag", payload: `{}`, wantCode: 502},
		{name: "invalid tag", payload: `{"tag_name":"unknown"}`, wantCode: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buildinfo.Version = tc.current
			calls := 0
			server := &Server{logger: slog.Default(), mux: http.NewServeMux()}
			server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != "https://api.github.com/repos/nov12/sui-copilot/releases/latest" {
					t.Fatalf("unexpected release request: %s", r.URL)
				}
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > 15*time.Second {
					t.Fatal("update check must have a short timeout")
				}
				if tc.err != nil {
					return nil, tc.err
				}
				status := tc.upstream
				if status == 0 {
					status = http.StatusOK
				}
				return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(tc.payload)), Header: make(http.Header)}, nil
			})}
			server.routes()
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/updates", nil))
			if response.Code != tc.wantCode || calls != 1 {
				t.Fatalf("status=%d, calls=%d, body=%s", response.Code, calls, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("manual check must not return a browser-cached result")
			}
			if tc.wantCode != http.StatusOK {
				if strings.Contains(response.Body.String(), `"updateAvailable"`) {
					t.Fatal("failed check must not be reported as an up-to-date result")
				}
				return
			}
			var body struct {
				CurrentVersion  string `json:"currentVersion"`
				LatestVersion   string `json:"latestVersion"`
				UpdateAvailable bool   `json:"updateAvailable"`
				ReleaseURL      string `json:"releaseUrl"`
				CheckedAt       string `json:"checkedAt"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if _, err := time.Parse(time.RFC3339Nano, body.CheckedAt); err != nil {
				t.Fatalf("missing successful check time: %q", body.CheckedAt)
			}
			if body.CurrentVersion != tc.current || body.LatestVersion != "v0.3.0" || body.UpdateAvailable != tc.available || body.ReleaseURL != "https://github.com/nov12/sui-copilot/releases/tag/v0.3.0" {
				t.Fatalf("unexpected update result: %+v", body)
			}
		})
	}
}

func cachedUpdateStatus(t *testing.T, server *Server) updateStatus {
	t.Helper()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/updates/status", nil))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cached status: code=%d, headers=%v", response.Code, response.Header())
	}
	var status updateStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestUpdateStatusSharesManualResultsAndPreservesCacheOnFailure(t *testing.T) {
	calls := 0
	fail := false
	server := &Server{logger: slog.Default(), mux: http.NewServeMux()}
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if fail {
			return nil, errors.New("offline")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"tag_name":"v0.3.0"}`)), Header: make(http.Header)}, nil
	})}
	server.routes()
	initial := cachedUpdateStatus(t, server)
	if initial.CheckedAt != "" || initial.UpdateAvailable || initial.LatestVersion != "" || initial.CurrentVersion != buildinfo.Version || calls != 0 {
		t.Fatalf("uncached read must not query GitHub or claim a check succeeded: %+v, calls=%d", initial, calls)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/updates", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("manual check failed: %s", response.Body.String())
	}
	status := cachedUpdateStatus(t, server)
	if status.LatestVersion != "v0.3.0" || status.CheckedAt == "" || calls != 1 {
		t.Fatalf("manual result not shared with cached status: %+v, calls=%d", status, calls)
	}
	fail = true
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/updates", nil))
	if response.Code != http.StatusBadGateway || calls != 2 {
		t.Fatalf("manual recheck must query again and report failure: code=%d, calls=%d", response.Code, calls)
	}
	if got := cachedUpdateStatus(t, server); got != status || calls != 2 {
		t.Fatalf("failed check changed last successful result or cache read queried GitHub: %+v, calls=%d", got, calls)
	}
}

func TestUpdateCheckLoopStartsImmediatelyAndRepeatsEverySixHours(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		server := &Server{logger: slog.Default(), mux: http.NewServeMux()}
		server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 2 {
				return nil, errors.New("temporary network failure")
			}
			tag := "v0.3.0"
			if calls > 2 {
				tag = "v0.4.0"
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"tag_name":"` + tag + `"}`)), Header: make(http.Header)}, nil
		})}
		server.routes()
		done := make(chan struct{})
		go func() {
			defer close(done)
			server.runUpdateCheckLoop(ctx)
		}()
		synctest.Wait()
		initial := cachedUpdateStatus(t, server)
		if calls != 1 || initial.LatestVersion != "v0.3.0" || initial.CheckedAt == "" {
			t.Fatalf("startup check did not immediately populate cache: %+v, calls=%d", initial, calls)
		}
		time.Sleep(6*time.Hour - time.Nanosecond)
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("checked before six hours elapsed: calls=%d", calls)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if got := cachedUpdateStatus(t, server); calls != 2 || got != initial {
			t.Fatalf("failed periodic check must preserve cached result: %+v, calls=%d", got, calls)
		}
		time.Sleep(6 * time.Hour)
		synctest.Wait()
		if got := cachedUpdateStatus(t, server); calls != 3 || got.LatestVersion != "v0.4.0" || got.CheckedAt == initial.CheckedAt {
			t.Fatalf("next periodic check did not recover: %+v, calls=%d", got, calls)
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("update loop did not stop with the service")
		}
		time.Sleep(6 * time.Hour)
		synctest.Wait()
		if calls != 3 {
			t.Fatalf("loop continued after shutdown: calls=%d", calls)
		}
	})
}

func TestUpdateCheckLoopCancelsInflightCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := false
		cancelled := false
		server := &Server{logger: slog.Default()}
		server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			started = true
			<-r.Context().Done()
			cancelled = true
			return nil, r.Context().Err()
		})}
		go server.runUpdateCheckLoop(ctx)
		synctest.Wait()
		if !started {
			t.Fatal("startup check did not begin")
		}
		cancel()
		synctest.Wait()
		if !cancelled || server.lastUpdate.CheckedAt != "" {
			t.Fatal("shutdown must cancel the check without publishing a successful result")
		}
	})
}
