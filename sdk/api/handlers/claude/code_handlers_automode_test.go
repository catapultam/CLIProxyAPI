package claude

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/automode"
)

func TestClaudeBetaHasAFKMode(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"empty", "", false},
		{"exact match", "afk-mode-2026-01-31", true},
		{"case insensitive", "AFK-Mode-2026-01-31", true},
		{"among others", "interleaved-thinking-2025-05-14, afk-mode-2026-01-31", true},
		{"padded whitespace", " afk-mode-2026-01-31 ,other-beta", true},
		{"absent", "interleaved-thinking-2025-05-14,other-beta", false},
		{"partial token not a match", "afk-mode-2026-01-31-extra", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeBetaHasAFKMode(tc.header); got != tc.want {
				t.Fatalf("claudeBetaHasAFKMode(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

// newAutoModeTestContext builds a gin.Context for a POST /v1/messages-style
// request with the given headers and raw body.
func newAutoModeTestContext(headers map[string]string, body string) *gin.Context {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	return c
}

func TestObserveAutoModeFromRequestGating(t *testing.T) {
	const toolsBody = `{"model":"claude-x","tools":[{"name":"Bash"}]}`
	const noToolsBody = `{"model":"claude-x","tools":[]}`
	const missingToolsBody = `{"model":"claude-x"}`

	cases := []struct {
		name         string
		headers      map[string]string
		body         string
		wantObserved bool
		wantMode     string
	}{
		{
			name:         "no session header",
			headers:      map[string]string{},
			body:         toolsBody,
			wantObserved: false,
		},
		{
			name: "subagent request skipped",
			headers: map[string]string{
				"x-claude-code-session-id": "sess-subagent",
				"x-claude-code-agent-id":   "agent-1",
			},
			body:         toolsBody,
			wantObserved: false,
		},
		{
			name: "empty tools array skipped",
			headers: map[string]string{
				"x-claude-code-session-id": "sess-empty-tools",
			},
			body:         noToolsBody,
			wantObserved: false,
		},
		{
			name: "missing tools field skipped",
			headers: map[string]string{
				"x-claude-code-session-id": "sess-missing-tools",
			},
			body:         missingToolsBody,
			wantObserved: false,
		},
		{
			name: "afk with safeguards is server mode",
			headers: map[string]string{
				"x-claude-code-session-id": "sess-server",
				"anthropic-beta":           "afk-mode-2026-01-31",
			},
			body:         `{"model":"claude-x","tools":[{"name":"Bash"}],"safeguards":{}}`,
			wantObserved: true,
			wantMode:     automode.ModeServer,
		},
		{
			name: "afk without safeguards is local mode",
			headers: map[string]string{
				"x-claude-code-session-id": "sess-local",
				"anthropic-beta":           "afk-mode-2026-01-31",
			},
			body:         toolsBody,
			wantObserved: true,
			wantMode:     automode.ModeLocal,
		},
		{
			name: "no afk beta is off mode",
			headers: map[string]string{
				"x-claude-code-session-id": "sess-off",
			},
			body:         toolsBody,
			wantObserved: true,
			wantMode:     automode.ModeOff,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := tc.headers["x-claude-code-session-id"]
			c := newAutoModeTestContext(tc.headers, tc.body)
			observeAutoModeFromRequest(c, []byte(tc.body))

			state, ok := automode.Lookup(sessionID)
			if ok != tc.wantObserved {
				t.Fatalf("observed = %v, want %v (state=%+v)", ok, tc.wantObserved, state)
			}
			if tc.wantObserved && state.Mode != tc.wantMode {
				t.Fatalf("Mode = %q, want %q", state.Mode, tc.wantMode)
			}
		})
	}
}
