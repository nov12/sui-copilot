package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"sui-copilot/internal/buildinfo"
	"sui-copilot/internal/store"
)

func TestHealthReportsBuildVersion(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("health status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Version == "" || body.Version != buildinfo.Version {
		t.Fatalf("health must report the running build version: %+v (want %q)", body, buildinfo.Version)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("health response must not cache an old running version")
	}
}
