package sidecar

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestOrchestratorHTTPClientHasTimeout locks the requirement that the best-effort report
// client has a timeout. Without one (the previous code used http.Post / http.DefaultClient),
// a hung orchestrator blocks the report loop indefinitely.
func TestOrchestratorHTTPClientHasTimeout(t *testing.T) {
	if orchestratorHTTPClient.Timeout <= 0 {
		t.Fatal("orchestratorHTTPClient must have a positive timeout")
	}
}

// TestPostReportRespectsTimeout verifies the report POST returns promptly with an error
// when the orchestrator is unresponsive, instead of hanging.
func TestPostReportRespectsTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()

	client := &http.Client{Timeout: 50 * time.Millisecond}
	start := time.Now()
	err := postReport(client, slow.URL, []byte(`{}`))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error from the unresponsive server, got nil")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("expected postReport to return near the 50ms timeout, took %v (client timeout not honored)", elapsed)
	}
}

// TestPostReportSuccess verifies the happy path returns no error on 2xx.
func TestPostReportSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := postReport(srv.Client(), srv.URL, []byte(`{}`)); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}
