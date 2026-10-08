package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// registerCodexOnlyTestModel registers modelName as served solely by the
// codex provider in the global model registry (the same registry
// codexIsSoleProviderForModel consults via util.GetProviderName), and
// unregisters it when the test ends so the global singleton is not left
// polluted for other tests in this package or binary.
func registerCodexOnlyTestModel(t *testing.T, modelName string) {
	t.Helper()
	clientID := "codex-quota-exhausted-test-" + modelName
	registry.GetGlobalRegistry().RegisterClient(clientID, "codex", []*registry.ModelInfo{{ID: modelName}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(clientID)
	})
}

func newQuotaExhaustedTestManager(t *testing.T, auths ...*coreauth.Auth) *coreauth.Manager {
	t.Helper()
	m := coreauth.NewManager(nil, nil, nil)
	ctx := context.Background()
	for _, a := range auths {
		if a == nil {
			continue
		}
		if _, errRegister := m.Register(ctx, a); errRegister != nil {
			t.Fatalf("Register(%q) failed: %v", a.ID, errRegister)
		}
	}
	return m
}

func TestRewriteQuotaExhaustedError_AllCodexQuotaCooldownReturns429(t *testing.T) {
	const model = "gpt-5-codex-quota-test-all"
	registerCodexOnlyTestModel(t, model)

	now := time.Now()
	authA := &coreauth.Auth{ID: "auth-a", Provider: "codex", Quota: coreauth.QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(2 * time.Hour),
	}}
	resetAt := now.Add(time.Hour)
	authB := &coreauth.Auth{ID: "auth-b", Provider: "codex", Quota: coreauth.QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: resetAt,
		ObservedAt: now,
		Signals: map[string]string{
			"X-Codex-Primary-Window-Minutes": "300",
			"X-Codex-Primary-Reset-At":       strconv.FormatInt(resetAt.Unix(), 10),
		},
	}}
	manager := newQuotaExhaustedTestManager(t, authA, authB)
	handler := &OpenAIResponsesAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{AuthManager: manager}}

	errMsg := &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable},
	}

	got := handler.rewriteQuotaExhaustedError(model, errMsg)
	if got == errMsg {
		t.Fatal("rewriteQuotaExhaustedError() did not rewrite an all-quota-cooldown failure")
	}
	if got.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("StatusCode = %d, want %d", got.StatusCode, http.StatusTooManyRequests)
	}
	if !got.DirectResponse {
		t.Fatal("DirectResponse = false, want true")
	}
	if got := got.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	retryAfter, errParse := strconv.Atoi(got.Headers.Get("Retry-After"))
	if errParse != nil || retryAfter <= 0 || retryAfter > int(time.Hour.Seconds())+5 {
		t.Fatalf("Retry-After = %q, want a small positive number of seconds", got.Headers.Get("Retry-After"))
	}

	if gotType := gjson.GetBytes(got.Body, "error.type").String(); gotType != "usage_limit_reached" {
		t.Fatalf("body error.type = %q, want usage_limit_reached", gotType)
	}
	if gjson.GetBytes(got.Body, "error.message").String() == "" {
		t.Fatal("body error.message is empty")
	}
	if gotResetsAt := gjson.GetBytes(got.Body, "error.resets_at").Int(); gotResetsAt != resetAt.Unix() {
		t.Fatalf("body error.resets_at = %d, want %d (earliest reset)", gotResetsAt, resetAt.Unix())
	}
	if !gjson.GetBytes(got.Body, "error.resets_in_seconds").Exists() {
		t.Fatal("body error.resets_in_seconds is missing")
	}
	if gotWindow := gjson.GetBytes(got.Body, "error.limit_window_minutes").Int(); gotWindow != 300 {
		t.Fatalf("body error.limit_window_minutes = %d, want 300", gotWindow)
	}
	if gjson.GetBytes(got.Body, "error.plan_type").Exists() {
		t.Fatal("body error.plan_type must be omitted for a pooled proxy")
	}
}

func TestRewriteQuotaExhaustedError_MixedReasonKeepsOriginal(t *testing.T) {
	const model = "gpt-5-codex-quota-test-mixed"
	registerCodexOnlyTestModel(t, model)

	now := time.Now()
	quotaAuth := &coreauth.Auth{ID: "auth-a", Provider: "codex", Quota: coreauth.QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour),
	}}
	transientAuth := &coreauth.Auth{ID: "auth-b", Provider: "codex", Unavailable: true, NextRetryAfter: now.Add(30 * time.Second)}
	manager := newQuotaExhaustedTestManager(t, quotaAuth, transientAuth)
	handler := &OpenAIResponsesAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{AuthManager: manager}}

	errMsg := &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable},
	}

	got := handler.rewriteQuotaExhaustedError(model, errMsg)
	if got != errMsg {
		t.Fatalf("rewriteQuotaExhaustedError() = %+v, want the original response unchanged for a mixed reason set", got)
	}
}

func TestRewriteQuotaExhaustedError_OneAuthAvailableLeavesErrorUnchanged(t *testing.T) {
	const model = "gpt-5-codex-quota-test-available"
	registerCodexOnlyTestModel(t, model)

	availableAuth := &coreauth.Auth{ID: "auth-a", Provider: "codex"}
	manager := newQuotaExhaustedTestManager(t, availableAuth)
	handler := &OpenAIResponsesAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{AuthManager: manager}}

	errMsg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadGateway,
		Error:      errors.New("upstream exploded"),
	}

	got := handler.rewriteQuotaExhaustedError(model, errMsg)
	if got != errMsg {
		t.Fatalf("rewriteQuotaExhaustedError() = %+v, want the unrelated error unchanged", got)
	}
}

func TestRewriteQuotaExhaustedError_MultiProviderModelLeftUnchanged(t *testing.T) {
	// Register the same model name under two providers so
	// codexIsSoleProviderForModel reports false; "all Codex accounts are
	// quota-blocked" is not a safe claim when the model could also have
	// routed to a different provider.
	const model = "gpt-5-codex-quota-test-multi-provider"
	registry.GetGlobalRegistry().RegisterClient("codex-quota-exhausted-test-multi-codex", "codex", []*registry.ModelInfo{{ID: model}})
	registry.GetGlobalRegistry().RegisterClient("codex-quota-exhausted-test-multi-openai", "openai", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient("codex-quota-exhausted-test-multi-codex")
		registry.GetGlobalRegistry().UnregisterClient("codex-quota-exhausted-test-multi-openai")
	})

	now := time.Now()
	codexAuth := &coreauth.Auth{ID: "auth-a", Provider: "codex", Quota: coreauth.QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour),
	}}
	manager := newQuotaExhaustedTestManager(t, codexAuth)
	handler := &OpenAIResponsesAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{AuthManager: manager}}

	errMsg := &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable},
	}

	got := handler.rewriteQuotaExhaustedError(model, errMsg)
	if got != errMsg {
		t.Fatalf("rewriteQuotaExhaustedError() = %+v, want unchanged when the model resolves to more than one provider", got)
	}
}

func TestWriteErrorResponse_AllCodexQuotaCooldownProducesUsageLimitBody(t *testing.T) {
	const model = "gpt-5-codex-quota-test-write"
	registerCodexOnlyTestModel(t, model)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	now := time.Now()
	authA := &coreauth.Auth{ID: "auth-a", Provider: "codex", Quota: coreauth.QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour),
	}}
	manager := newQuotaExhaustedTestManager(t, authA)
	handler := &OpenAIResponsesAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{AuthManager: manager}}

	errMsg := &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable},
	}
	rewritten := handler.rewriteQuotaExhaustedError(model, errMsg)
	handler.WriteErrorResponse(c, rewritten)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type header = %q, want application/json", got)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After header is empty")
	}
	body := recorder.Body.Bytes()
	if got := gjson.GetBytes(body, "error.type").String(); got != "usage_limit_reached" {
		t.Fatalf("error.type = %q, want usage_limit_reached; body=%s", got, body)
	}
}
