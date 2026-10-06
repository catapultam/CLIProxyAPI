package claude

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

// fakeStreamExecutor stands in for a non-Claude provider served through the
// Claude endpoint (e.g. a model translated into Claude's response shape). It
// emits the given SSE chunks verbatim and never fills safeguard_results
// itself, unlike the real Claude executor.
type fakeStreamExecutor struct {
	provider string
	chunks   [][]byte
}

func (f *fakeStreamExecutor) Identifier() string { return f.provider }

func (f *fakeStreamExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, fmt.Errorf("fakeStreamExecutor: non-streaming not implemented")
}

func (f *fakeStreamExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	ch := make(chan cliproxyexecutor.StreamChunk, len(f.chunks))
	for _, chunk := range f.chunks {
		ch <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (f *fakeStreamExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (f *fakeStreamExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, fmt.Errorf("fakeStreamExecutor: count tokens not implemented")
}

func (f *fakeStreamExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("fakeStreamExecutor: http request not implemented")
}

// fakeSSEEvent builds one complete SSE "data:" event, matching the chunk
// granularity real executors emit (a whole event per chunk).
func fakeSSEEvent(jsonPayload string) []byte {
	return []byte("data: " + jsonPayload + "\n\n")
}

const fakeSafeguardStreamModel = "claude-safeguard-handler-fill-e2e"

func newFakeSafeguardHandler(t *testing.T, chunks [][]byte) *ClaudeCodeAPIHandler {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&fakeStreamExecutor{provider: "claude", chunks: chunks})
	auth := &coreauth.Auth{
		ID:         "fake-safeguard-" + t.Name(),
		Provider:   "claude",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "test-key"},
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: fakeSafeguardStreamModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	return NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
}

// TestClaudeHandlerFillsSafeguardResultsForProviderThatDidNot drives a
// streaming response, through a fake executor that never fills
// safeguard_results (standing in for a non-Claude provider served on the
// Claude endpoint), and checks that the handler-level filler still answers
// Claude Code's `safeguards` request with a usable (if unavailable/retryable)
// result rather than leaving the client to find none and latch its own
// billed fallback classifier.
func TestClaudeHandlerFillsSafeguardResultsForProviderThatDidNot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	chunks := [][]byte{
		fakeSSEEvent(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"` + fakeSafeguardStreamModel + `","usage":{"input_tokens":1,"output_tokens":1}}}`),
		fakeSSEEvent(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_FAKE","name":"Bash","input":{}}}`),
		fakeSSEEvent(`{"type":"content_block_stop","index":0}`),
		fakeSSEEvent(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`),
		fakeSSEEvent(`{"type":"message_stop"}`),
	}
	handler := newFakeSafeguardHandler(t, chunks)

	body := `{"model":"` + fakeSafeguardStreamModel + `","max_tokens":64,"stream":true,"safeguards":[{"type":"dangerous_tool_use"}],"messages":[{"role":"user","content":"hi"}]}`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ClaudeMessages(c)

	got := recorder.Body.String()
	var delta string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "data:") && strings.Contains(line, `"message_delta"`) {
			delta = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if delta == "" {
		t.Fatalf("no message_delta line in client output:\n%s", got)
	}
	call := gjson.Get(delta, "delta.safeguard_results.0.status.tool_uses.toolu_FAKE")
	if call.Get("type").String() != "unavailable" || call.Get("reason").String() != "error" {
		t.Fatalf("want an unavailable/error safeguard result for toolu_FAKE, got %s", delta)
	}
	if !strings.Contains(got, "toolu_FAKE") || !strings.Contains(got, "message_stop") {
		t.Fatalf("stream content changed unexpectedly:\n%s", got)
	}
}

// TestClaudeHandlerLeavesStreamUntouchedWithoutSafeguardsRequest checks that
// a request that never asked for safeguards (no `safeguards` field) is not
// touched by the handler-level filler.
func TestClaudeHandlerLeavesStreamUntouchedWithoutSafeguardsRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	chunks := [][]byte{
		fakeSSEEvent(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"` + fakeSafeguardStreamModel + `","usage":{"input_tokens":1,"output_tokens":1}}}`),
		fakeSSEEvent(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_FAKE","name":"Bash","input":{}}}`),
		fakeSSEEvent(`{"type":"content_block_stop","index":0}`),
		fakeSSEEvent(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`),
		fakeSSEEvent(`{"type":"message_stop"}`),
	}
	handler := newFakeSafeguardHandler(t, chunks)

	body := `{"model":"` + fakeSafeguardStreamModel + `","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ClaudeMessages(c)

	got := recorder.Body.String()
	if strings.Contains(got, "safeguard_results") {
		t.Fatalf("no fill expected without a safeguards request:\n%s", got)
	}
}
