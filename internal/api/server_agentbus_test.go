package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentbusRoutesRequireClientKey(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(s.stopAgentbus)

	req := httptest.NewRequest(http.MethodGet, "/v1/agentbus/peers", nil)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("without key = %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/agentbus/peers", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	w = httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"peers"`) {
		t.Fatalf("with key = %d %s", w.Code, w.Body)
	}
}

func TestAgentbusStateSavedNextToConfig(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	s := newTestServer(t)
	t.Cleanup(s.stopAgentbus)
	want := filepath.Join(filepath.Dir(s.configFilePath), "agentbus-state.json")
	if got := s.runtimeStatePath("agentbus-state.json"); got != want {
		t.Fatalf("state path = %q, want %q", got, want)
	}
}

func TestSlackConfigReadsCommandsNextToState(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	s := newTestServer(t)
	t.Cleanup(s.stopAgentbus)
	cfg := s.slackConfig()
	want := filepath.Join(filepath.Dir(s.configFilePath), "agent-commands")
	if cfg.CommandsDir != want {
		t.Fatalf("CommandsDir = %q, want %q", cfg.CommandsDir, want)
	}
	if want = s.runtimeStatePath("slack-state.json"); cfg.StatePath != want {
		t.Fatalf("StatePath = %q, want %q", cfg.StatePath, want)
	}
}
