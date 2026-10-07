package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/automode"
)

func TestAutoModeHandlerMissingSession(t *testing.T) {
	server := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/auto-mode", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["error"] != "session is required" {
		t.Fatalf("error = %q, want %q", resp["error"], "session is required")
	}
}

func TestAutoModeHandlerUnknownSession(t *testing.T) {
	server := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/auto-mode?session=never-seen-session", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp autoModeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	want := autoModeResponse{Session: "never-seen-session", State: "unknown"}
	if resp != want {
		t.Fatalf("response = %+v, want %+v", resp, want)
	}
}

func TestAutoModeHandlerKnownSession(t *testing.T) {
	server := newTestServer(t)

	sessionID := "auto-mode-handler-known-session"
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updated := since.Add(10 * time.Minute)
	automode.Observe(sessionID, true, true, since)
	automode.Observe(sessionID, true, true, updated)

	req := httptest.NewRequest(http.MethodGet, "/v1/auto-mode?session="+sessionID, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp autoModeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	want := autoModeResponse{
		Session: sessionID,
		State:   automode.ModeServer,
		Since:   since.Format(time.RFC3339),
		Updated: updated.Format(time.RFC3339),
	}
	if resp != want {
		t.Fatalf("response = %+v, want %+v", resp, want)
	}
}

func TestAutoModeHandlerRequiresAuth(t *testing.T) {
	server := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/auto-mode?session=any", nil)
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}
