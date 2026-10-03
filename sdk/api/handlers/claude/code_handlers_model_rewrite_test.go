package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

type modelRewriteRecordingExecutor struct {
	mu       sync.Mutex
	models   []string
	payloads []string
}

func (e *modelRewriteRecordingExecutor) Identifier() string { return "claude" }

func (e *modelRewriteRecordingExecutor) record(req coreexecutor.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.models = append(e.models, req.Model)
	e.payloads = append(e.payloads, string(req.Payload))
}

func (e *modelRewriteRecordingExecutor) last() (string, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.models) == 0 {
		return "", ""
	}
	return e.models[len(e.models)-1], e.payloads[len(e.payloads)-1]
}

func (e *modelRewriteRecordingExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(req)
	return coreexecutor.Response{Payload: []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[]}`)}, nil
}

func (e *modelRewriteRecordingExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.record(req)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *modelRewriteRecordingExecutor) CountTokens(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(req)
	return coreexecutor.Response{Payload: []byte(`{"input_tokens":3}`)}, nil
}

func (e *modelRewriteRecordingExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *modelRewriteRecordingExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "not implemented", HTTPStatus: http.StatusNotImplemented}
}

// Claude /v1/messages (streaming and not) and count_tokens send the rewritten
// model, with its thinking suffix, to the executor and in the upstream body.
func TestClaudeMessagesApplyModelRewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	exec := &modelRewriteRecordingExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	auth := &coreauth.Auth{ID: "claude-model-rewrite-e2e", Provider: "claude", Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-sonnet-5-5"}, {ID: "claude-opus-5-5"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	rules := []internalconfig.ModelRewriteRule{{Match: "*sonnet*", To: "claude-sonnet-5-5"}, {Match: "*opus*", To: "claude-opus-5-5"}}
	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{ModelRewrite: rules}, manager))

	for _, tc := range []struct {
		name, path, body, want string
		count                  bool
	}{
		{"messages", "/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, "claude-sonnet-5-5", false},
		{"messages stream with suffix", "/v1/messages", `{"model":"claude-opus-4-7(16384)","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, "claude-opus-5-5(16384)", false},
		{"messages slash id", "/v1/messages", `{"model":"anthropic/claude-sonnet-4","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, "claude-sonnet-5-5", false},
		{"count_tokens", "/v1/messages/count_tokens", `{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`, "claude-opus-5-5", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			if tc.count {
				handler.ClaudeCountTokens(c)
			} else {
				handler.ClaudeMessages(c)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
			}
			model, payload := exec.last()
			if model != tc.want {
				t.Fatalf("executor model = %q, want %q", model, tc.want)
			}
			if got := gjson.Get(payload, "model").String(); got != tc.want {
				t.Fatalf("upstream body model = %q, want %q", got, tc.want)
			}
		})
	}
}
