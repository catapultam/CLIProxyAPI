package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

const threadNotFoundUpstreamBody = `{"type":"error","error":{"type":"not_found_error","message":"No thread state was found for the requested ` + "`previous_message_id`" + `. Replay the full conversation with ` + "`thread: {\\\"type\\\": \\\"create\\\"}`" + ` to start a new Thread."}}`

// Anthropic's missing-thread 404 must reach Claude Code as a 404 that carries
// error.details.error_code "thread_not_found": that is the signal Claude Code
// checks before it replays the conversation with thread: {"type":"create"}.
// Without it the client reports "There's an issue with the selected model".
// This drives the real Claude executor, auth manager and handler against a fake
// upstream, for both the streaming and non-streaming request paths.
func TestClaudeMessagesMissingThreadStateReachesClientWithThreadNotFoundCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(threadNotFoundUpstreamBody))
	}))
	t.Cleanup(upstream.Close)

	const model = "claude-thread-not-found-e2e"
	for _, stream := range []bool{false, true} {
		name := "non-streaming"
		if stream {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			upstreamCalls = 0
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(executor.NewClaudeExecutor(&internalconfig.Config{}))
			var authIDs []string
			for _, suffix := range []string{"a", "b"} {
				auth := &coreauth.Auth{
					ID:         "claude-thread-not-found-e2e-" + name + "-" + suffix,
					Provider:   "claude",
					Status:     coreauth.StatusActive,
					Attributes: map[string]string{"api_key": "test-key-" + suffix, "base_url": upstream.URL},
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
					t.Fatalf("register auth: %v", errRegister)
				}
				authIDs = append(authIDs, auth.ID)
			}
			handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))

			body := `{"model":"` + model + `","max_tokens":64,"stream":` + map[bool]string{false: "false", true: "true"}[stream] +
				`,"thread":{"type":"continue","previous_message_id":"msg_previous"},"messages":[{"role":"user","content":"hi"}]}`
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")

			handler.ClaudeMessages(c)

			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body %s", recorder.Code, recorder.Body.String())
			}
			got := recorder.Body.String()
			if code := gjson.Get(got, "error.details.error_code").String(); code != "thread_not_found" {
				t.Fatalf("error.details.error_code = %q, want thread_not_found; body %s", code, got)
			}
			if typ := gjson.Get(got, "error.type").String(); typ != "not_found_error" {
				t.Fatalf("error.type = %q, want not_found_error; body %s", typ, got)
			}
			if msg, want := gjson.Get(got, "error.message").String(), gjson.Get(threadNotFoundUpstreamBody, "error.message").String(); msg != want {
				t.Fatalf("error.message = %q, want %q", msg, want)
			}
			// The client replays on its own; the proxy must neither rotate to
			// another credential nor cool any of them down.
			if upstreamCalls != 1 {
				t.Fatalf("upstream calls = %d, want 1 (no rotation)", upstreamCalls)
			}
			for _, id := range authIDs {
				auth, ok := manager.GetByID(id)
				if !ok || auth.Unavailable || !auth.NextRetryAfter.IsZero() {
					t.Fatalf("auth %s was cooled down or removed: %#v", id, auth)
				}
				if state := auth.ModelStates[model]; state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
					t.Fatalf("auth %s model state was cooled down: %#v", id, state)
				}
			}
		})
	}
}
