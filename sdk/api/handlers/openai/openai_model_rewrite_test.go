package openai

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

type chatModelRewriteExecutor struct {
	mu       sync.Mutex
	models   []string
	payloads []string
}

func (e *chatModelRewriteExecutor) Identifier() string { return "claude" }

func (e *chatModelRewriteExecutor) record(req coreexecutor.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.models = append(e.models, req.Model)
	e.payloads = append(e.payloads, string(req.Payload))
}

func (e *chatModelRewriteExecutor) last() (string, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.models) == 0 {
		return "", ""
	}
	return e.models[len(e.models)-1], e.payloads[len(e.payloads)-1]
}

func (e *chatModelRewriteExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(req)
	return coreexecutor.Response{Payload: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`)}, nil
}

func (e *chatModelRewriteExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.record(req)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[]}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *chatModelRewriteExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *chatModelRewriteExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *chatModelRewriteExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "not implemented", HTTPStatus: http.StatusNotImplemented}
}

// OpenAI /v1/chat/completions sends the rewritten model to the executor and in the upstream body.
func TestChatCompletionsApplyModelRewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	exec := &chatModelRewriteExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	auth := &coreauth.Auth{ID: "openai-chat-model-rewrite-e2e", Provider: "claude", Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-sonnet-5-5"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	rules := []internalconfig.ModelRewriteRule{{Match: "*sonnet*", To: "claude-sonnet-5-5"}}
	handler := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{ModelRewrite: rules}, manager))

	for _, stream := range []bool{false, true} {
		body := `{"model":"claude-sonnet-4-5(4096)","messages":[{"role":"user","content":"hi"}]}`
		if stream {
			body = `{"model":"claude-sonnet-4-5(4096)","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		handler.ChatCompletions(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("stream=%t status = %d, body %s", stream, recorder.Code, recorder.Body.String())
		}
		model, payload := exec.last()
		if model != "claude-sonnet-5-5(4096)" {
			t.Fatalf("stream=%t executor model = %q, want claude-sonnet-5-5(4096)", stream, model)
		}
		if got := gjson.Get(payload, "model").String(); got != "claude-sonnet-5-5(4096)" {
			t.Fatalf("stream=%t upstream body model = %q, want claude-sonnet-5-5(4096)", stream, got)
		}
	}
}
