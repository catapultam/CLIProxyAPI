package handlers

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

var testModelRewriteRules = []config.ModelRewriteRule{
	{Match: "*sonnet*", To: "claude-sonnet-5-5"},
	{Match: "*opus*", To: "claude-opus-5-5"},
}

// countModelRewrites installs the test observer and returns a counter of applied rewrites.
func countModelRewrites(t *testing.T) func() int {
	t.Helper()
	var mu sync.Mutex
	count := 0
	previous := modelRewriteObserver
	modelRewriteObserver = func(string, string) {
		mu.Lock()
		count++
		mu.Unlock()
	}
	t.Cleanup(func() { modelRewriteObserver = previous })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return count
	}
}

func TestRewriteModelNameSinglePass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules []config.ModelRewriteRule
		in    string
		want  string
	}{
		{"sonnet", testModelRewriteRules, "claude-sonnet-4-5", "claude-sonnet-5-5"},
		{"opus case-insensitive", testModelRewriteRules, "Claude-OPUS-4-7", "claude-opus-5-5"},
		{"slash in id", testModelRewriteRules, "anthropic/claude-sonnet-4", "claude-sonnet-5-5"},
		{"suffix preserved", testModelRewriteRules, "claude-sonnet-4-5(8192)", "claude-sonnet-5-5(8192)"},
		{"level suffix preserved", testModelRewriteRules, "claude-opus-4-7(high)", "claude-opus-5-5(high)"},
		{"identity unchanged", testModelRewriteRules, "claude-sonnet-5-5", "claude-sonnet-5-5"},
		{"identity with suffix unchanged", testModelRewriteRules, "claude-sonnet-5-5(8192)", "claude-sonnet-5-5(8192)"},
		{"no match passes through", testModelRewriteRules, "gpt-5.5", "gpt-5.5"},
		{"target matching its own pattern resolves once", []config.ModelRewriteRule{{Match: "*sonnet*", To: "sonnet-next"}}, "claude-sonnet-4", "sonnet-next"},
		{"chain yields first hop", []config.ModelRewriteRule{{Match: "model-a", To: "model-b"}, {Match: "model-b", To: "model-c"}}, "model-a", "model-b"},
		{"no rules", nil, "claude-sonnet-4-5", "claude-sonnet-4-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := rewriteModelName(tc.rules, tc.in)
			if got != tc.want {
				t.Fatalf("rewriteModelName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{ModelRewrite: tc.rules}, nil)
			if pure := h.RewriteModelName(tc.in); pure != tc.want {
				t.Fatalf("RewriteModelName(%q) = %q, want %q", tc.in, pure, tc.want)
			}
		})
	}
}

func TestApplyModelRewriteRunsOncePerRequest(t *testing.T) {
	count := countModelRewrites(t)
	// The second rule matches the first rule's target; it must never fire.
	rules := []config.ModelRewriteRule{{Match: "*sonnet*", To: "claude-sonnet-5-5"}, {Match: "claude-sonnet-5-5", To: "wrong"}}
	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{ModelRewrite: rules}, nil)

	ctx, model, body := h.applyModelRewrite(context.Background(), "claude-sonnet-4-5", []byte(`{"model":"claude-sonnet-4-5"}`), modelExecutionOptions{})
	if model != "claude-sonnet-5-5" || gjson.GetBytes(body, "model").String() != "claude-sonnet-5-5" {
		t.Fatalf("first pass = %q body %s, want claude-sonnet-5-5", model, body)
	}
	// Re-entering the seam for the same request leaves both model and body alone.
	_, again, againBody := h.applyModelRewrite(ctx, model, body, modelExecutionOptions{})
	if again != "claude-sonnet-5-5" || string(againBody) != string(body) {
		t.Fatalf("re-entry = %q body %s, want unchanged", again, againBody)
	}
	_, other, _ := h.applyModelRewrite(ctx, "claude-opus-4-7", nil, modelExecutionOptions{})
	if other != "claude-opus-4-7" {
		t.Fatalf("re-entry with another model = %q, want unchanged", other)
	}
	// Internal host-model callbacks are never rewritten.
	_, internal, _ := h.applyModelRewrite(context.Background(), "claude-sonnet-4-5", nil, modelExecutionOptions{InternalSource: true})
	if internal != "claude-sonnet-4-5" {
		t.Fatalf("internal source = %q, want unchanged", internal)
	}
	if got := count(); got != 1 {
		t.Fatalf("rewrites applied = %d, want 1", got)
	}
}

// modelRewriteExecutor records every request it receives.
type modelRewriteExecutor struct {
	mu       sync.Mutex
	requests []coreexecutor.Request
	options  []coreexecutor.Options
	onCall   func(ctx context.Context, call int)
}

func (e *modelRewriteExecutor) Identifier() string { return "claude" }

func (e *modelRewriteExecutor) record(ctx context.Context, req coreexecutor.Request, opts coreexecutor.Options) {
	e.mu.Lock()
	e.requests = append(e.requests, coreexecutor.Request{Model: req.Model, Payload: cloneBytes(req.Payload)})
	e.options = append(e.options, opts)
	call := len(e.requests)
	onCall := e.onCall
	e.mu.Unlock()
	if onCall != nil {
		onCall(ctx, call)
	}
}

func (e *modelRewriteExecutor) Execute(ctx context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(ctx, req, opts)
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *modelRewriteExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.record(ctx, req, opts)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte("data: {}\n\n")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *modelRewriteExecutor) CountTokens(ctx context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(ctx, req, opts)
	return coreexecutor.Response{Payload: []byte(`{"input_tokens":1}`)}, nil
}

func (e *modelRewriteExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *modelRewriteExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "not implemented", HTTPStatus: http.StatusNotImplemented}
}

func (e *modelRewriteExecutor) recorded() ([]coreexecutor.Request, []coreexecutor.Options) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]coreexecutor.Request(nil), e.requests...), append([]coreexecutor.Options(nil), e.options...)
}

func newModelRewriteHandler(t *testing.T, exec *modelRewriteExecutor, rules []config.ModelRewriteRule, models ...string) *BaseAPIHandler {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	auth := &coreauth.Auth{ID: "model-rewrite-" + t.Name(), Provider: "claude", Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model})
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", infos)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	return NewBaseAPIHandlers(&sdkconfig.SDKConfig{ModelRewrite: rules}, manager)
}

func TestModelRewriteReachesExecutorOnEveryExecutionPath(t *testing.T) {
	count := countModelRewrites(t)
	exec := &modelRewriteExecutor{}
	h := newModelRewriteHandler(t, exec, testModelRewriteRules, "claude-sonnet-5-5")
	body := []byte(`{"model":"claude-sonnet-4-5(8192)","messages":[{"role":"user","content":"hi"}]}`)

	if _, _, errMsg := h.ExecuteWithAuthManager(context.Background(), "claude", "claude-sonnet-4-5(8192)", body, ""); errMsg != nil {
		t.Fatalf("execute: %v", errMsg.Error)
	}
	if _, _, errMsg := h.ExecuteCountWithAuthManager(context.Background(), "claude", "claude-sonnet-4-5(8192)", body, ""); errMsg != nil {
		t.Fatalf("count: %v", errMsg.Error)
	}
	data, _, errs := h.ExecuteStreamWithAuthManager(context.Background(), "claude", "claude-sonnet-4-5(8192)", body, "")
	for range data {
	}
	for errMsg := range errs {
		if errMsg != nil {
			t.Fatalf("stream: %v", errMsg.Error)
		}
	}

	requests, options := exec.recorded()
	if len(requests) != 3 {
		t.Fatalf("executor calls = %d, want 3", len(requests))
	}
	for i, req := range requests {
		if req.Model != "claude-sonnet-5-5(8192)" {
			t.Errorf("call %d model = %q, want claude-sonnet-5-5(8192)", i, req.Model)
		}
		if got := gjson.GetBytes(req.Payload, "model").String(); got != "claude-sonnet-5-5(8192)" {
			t.Errorf("call %d payload model = %q, want claude-sonnet-5-5(8192)", i, got)
		}
		if got := gjson.GetBytes(options[i].OriginalRequest, "model").String(); got != "claude-sonnet-5-5(8192)" {
			t.Errorf("call %d original request model = %q, want claude-sonnet-5-5(8192)", i, got)
		}
		if got, _ := options[i].Metadata[coreexecutor.RequestedModelMetadataKey].(string); got != "claude-sonnet-5-5(8192)" {
			t.Errorf("call %d requested-model metadata = %q, want claude-sonnet-5-5(8192)", i, got)
		}
	}
	if got := count(); got != 3 {
		t.Fatalf("rewrites applied = %d, want one per request (3)", got)
	}
}

// oauth.excluded-models keeps an excluded id out of the auth's registered models.
// A request for that id that matches a rule is resolved to the target before
// provider lookup, so it is served by the target instead of failing.
func TestModelRewriteRunsBeforeExcludedModelLookup(t *testing.T) {
	exec := &modelRewriteExecutor{}
	// claude-opus-4-7 is excluded: only the target is registered for the auth.
	h := newModelRewriteHandler(t, exec, nil, "claude-opus-5-5")
	body := []byte(`{"model":"claude-opus-4-7"}`)
	if _, _, errMsg := h.ExecuteWithAuthManager(context.Background(), "claude", "claude-opus-4-7", body, ""); errMsg == nil || errMsg.StatusCode != http.StatusBadRequest {
		t.Fatalf("without rules the excluded model must be unroutable, got %+v", errMsg)
	}
	h.UpdateClients(&sdkconfig.SDKConfig{ModelRewrite: testModelRewriteRules})
	if _, _, errMsg := h.ExecuteWithAuthManager(context.Background(), "claude", "claude-opus-4-7", body, ""); errMsg != nil {
		t.Fatalf("with rules: %v", errMsg.Error)
	}
	requests, _ := exec.recorded()
	if len(requests) != 1 || requests[0].Model != "claude-opus-5-5" {
		t.Fatalf("executor requests = %+v, want one for claude-opus-5-5", requests)
	}
}

// A request that re-enters execution with the same context (e.g. a nested call
// made while serving it) is not rewritten again.
func TestModelRewriteNotReappliedOnReentry(t *testing.T) {
	count := countModelRewrites(t)
	exec := &modelRewriteExecutor{}
	h := newModelRewriteHandler(t, exec, testModelRewriteRules, "claude-sonnet-4-5", "claude-sonnet-5-5")
	var nestedErr error
	exec.onCall = func(ctx context.Context, call int) {
		if call != 1 {
			return
		}
		if _, _, errMsg := h.ExecuteWithAuthManager(ctx, "claude", "claude-sonnet-4-5", []byte(`{"model":"claude-sonnet-4-5"}`), ""); errMsg != nil {
			nestedErr = errMsg.Error
		}
		if _, errMsg := h.ExecuteModel(context.Background(), ModelExecutionRequest{EntryProtocol: "claude", ExitProtocol: "claude", Model: "claude-sonnet-4-5", Body: []byte(`{"model":"claude-sonnet-4-5"}`)}); errMsg != nil {
			nestedErr = errMsg.Error
		}
	}
	if _, _, errMsg := h.ExecuteWithAuthManager(context.Background(), "claude", "claude-sonnet-4-5", []byte(`{"model":"claude-sonnet-4-5"}`), ""); errMsg != nil {
		t.Fatalf("execute: %v", errMsg.Error)
	}
	if nestedErr != nil {
		t.Fatalf("nested execute: %v", nestedErr)
	}
	requests, _ := exec.recorded()
	want := []string{"claude-sonnet-5-5", "claude-sonnet-4-5", "claude-sonnet-4-5"}
	if len(requests) != len(want) {
		t.Fatalf("executor calls = %d, want %d", len(requests), len(want))
	}
	for i, model := range want {
		if requests[i].Model != model {
			t.Errorf("call %d model = %q, want %q", i, requests[i].Model, model)
		}
	}
	if got := count(); got != 1 {
		t.Fatalf("rewrites applied = %d, want 1", got)
	}
}
