package claude

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
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

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

func TestRewriteQuotaExhaustedError_AllQuotaCooldownReturns429(t *testing.T) {
	now := time.Now()
	authA := &coreauth.Auth{ID: "auth-a", Provider: "claude", Quota: coreauth.QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(2 * time.Hour),
	}}
	authB := &coreauth.Auth{ID: "auth-b", Provider: "claude", Quota: coreauth.QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour),
		Signals: map[string]string{"Anthropic-Ratelimit-Unified-Representative-Claim": "seven_day"},
	}}
	manager := newQuotaExhaustedTestManager(t, authA, authB)
	handler := &ClaudeCodeAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{AuthManager: manager}}

	errMsg := &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable},
	}

	got := handler.rewriteQuotaExhaustedError("claude-opus-5-5", errMsg)
	if got == errMsg {
		t.Fatal("rewriteQuotaExhaustedError() did not rewrite an all-quota-cooldown failure")
	}
	if got.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("StatusCode = %d, want %d", got.StatusCode, http.StatusTooManyRequests)
	}
	if !got.DirectResponse {
		t.Fatal("DirectResponse = false, want true")
	}
	if got := got.Headers.Get("Anthropic-Ratelimit-Unified-Status"); got != "rejected" {
		t.Fatalf("Anthropic-Ratelimit-Unified-Status = %q, want rejected", got)
	}
	if got := got.Headers.Get("Anthropic-Ratelimit-Unified-Representative-Claim"); got != "seven_day" {
		t.Fatalf("Anthropic-Ratelimit-Unified-Representative-Claim = %q, want seven_day", got)
	}
	if got := got.Headers.Get("X-Should-Retry"); got != "false" {
		t.Fatalf("X-Should-Retry = %q, want false", got)
	}
	wantReset := strconv.FormatInt(authB.Quota.NextRecoverAt.Unix(), 10)
	if got := got.Headers.Get("Anthropic-Ratelimit-Unified-Reset"); got != wantReset {
		t.Fatalf("Anthropic-Ratelimit-Unified-Reset = %q, want %q (earliest reset)", got, wantReset)
	}
	retryAfter, errParse := strconv.Atoi(got.Headers.Get("Retry-After"))
	if errParse != nil || retryAfter <= 0 || retryAfter > int(time.Hour.Seconds())+5 {
		t.Fatalf("Retry-After = %q, want a small positive number of seconds", got.Headers.Get("Retry-After"))
	}

	if got := gjson.GetBytes(got.Body, "type").String(); got != "error" {
		t.Fatalf("body type = %q, want error", got)
	}
	if got := gjson.GetBytes(got.Body, "error.type").String(); got != "rate_limit_error" {
		t.Fatalf("body error.type = %q, want rate_limit_error", got)
	}
	message := gjson.GetBytes(got.Body, "error.message").String()
	if message == "" {
		t.Fatal("body error.message is empty")
	}
}

func TestRewriteQuotaExhaustedError_ModelCooldownErrorIsAlsoRewritten(t *testing.T) {
	now := time.Now()
	authA := &coreauth.Auth{ID: "auth-a", Provider: "claude", Quota: coreauth.QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(90 * time.Minute),
	}}
	manager := newQuotaExhaustedTestManager(t, authA)
	handler := &ClaudeCodeAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{AuthManager: manager}}

	errMsg := &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      coreauth.NewModelCooldownError("claude-opus-5-5", "claude", 90*time.Minute),
	}

	got := handler.rewriteQuotaExhaustedError("claude-opus-5-5", errMsg)
	if got == errMsg {
		t.Fatal("rewriteQuotaExhaustedError() did not rewrite a model_cooldown all-quota failure")
	}
	if !got.DirectResponse || got.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("got = %+v, want a rewritten direct 429 response", got)
	}
}

func TestRewriteQuotaExhaustedError_MixedReasonKeepsOriginal503(t *testing.T) {
	now := time.Now()
	quotaAuth := &coreauth.Auth{ID: "auth-a", Provider: "claude", Quota: coreauth.QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour),
	}}
	transientAuth := &coreauth.Auth{ID: "auth-b", Provider: "claude", Unavailable: true, NextRetryAfter: now.Add(30 * time.Second)}
	manager := newQuotaExhaustedTestManager(t, quotaAuth, transientAuth)
	handler := &ClaudeCodeAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{AuthManager: manager}}

	errMsg := &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable},
	}

	got := handler.rewriteQuotaExhaustedError("claude-opus-5-5", errMsg)
	if got != errMsg {
		t.Fatalf("rewriteQuotaExhaustedError() = %+v, want the original 503 unchanged for a mixed reason set", got)
	}
}

func TestRewriteQuotaExhaustedError_OneAuthAvailableLeavesUnrelatedErrorUnchanged(t *testing.T) {
	availableAuth := &coreauth.Auth{ID: "auth-a", Provider: "claude"}
	manager := newQuotaExhaustedTestManager(t, availableAuth)
	handler := &ClaudeCodeAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{AuthManager: manager}}

	errMsg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadGateway,
		Error:      errors.New("upstream exploded"),
	}

	got := handler.rewriteQuotaExhaustedError("claude-opus-5-5", errMsg)
	if got != errMsg {
		t.Fatalf("rewriteQuotaExhaustedError() = %+v, want the unrelated error unchanged", got)
	}
}

func TestWriteErrorResponse_AllQuotaCooldownProducesAnthropicShapedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	now := time.Now()
	authA := &coreauth.Auth{ID: "auth-a", Provider: "claude", Quota: coreauth.QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour),
	}}
	manager := newQuotaExhaustedTestManager(t, authA)
	handler := &ClaudeCodeAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{AuthManager: manager}}

	errMsg := &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable},
	}
	rewritten := handler.rewriteQuotaExhaustedError("claude-opus-5-5", errMsg)
	handler.WriteErrorResponse(c, rewritten)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("X-Should-Retry"); got != "false" {
		t.Fatalf("X-Should-Retry header = %q, want false", got)
	}
	if got := recorder.Header().Get("Anthropic-Ratelimit-Unified-Status"); got != "rejected" {
		t.Fatalf("Anthropic-Ratelimit-Unified-Status header = %q, want rejected", got)
	}
	body := recorder.Body.Bytes()
	if got := gjson.GetBytes(body, "error.type").String(); got != "rate_limit_error" {
		t.Fatalf("error.type = %q, want rate_limit_error; body=%s", got, body)
	}
}
