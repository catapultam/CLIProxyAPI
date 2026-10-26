package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const fastModeCreditRefusalBody = `{"error":{"type":"invalid_request_error","message":"Usage credits are required for fast mode."}}`

func TestUsageRestrictionSemanticStatus(t *testing.T) {
	for _, test := range []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"monthly cap", monthlyUsageRefusalBody, http.StatusPaymentRequired},
		{"low credit", `{"error":{"type":"invalid_request_error","message":"Credit balance is too low"}}`, http.StatusPaymentRequired},
		{"structured quota", quotaRefusalBody, http.StatusTooManyRequests},
	} {
		for _, status := range []int{0, 400, 401, 403, 404, 502} {
			upstream := customStatusError{code: status, msg: test.body}
			for form, err := range []error{upstream, fmt.Errorf("wrapped: %w", upstream), &Error{Code: "upstream_error", HTTPStatus: status, Message: test.body}} {
				t.Run(fmt.Sprintf("%s/%d/%d", test.name, status, form), func(t *testing.T) {
					if !isUsageRestrictionError(err) || isRequestInvalidError(err) {
						t.Fatalf("expected account restriction despite transport status %d: %v", status, err)
					}
					resultErr := resultErrorFromError(err)
					if resultErr.HTTPStatus != status || resultErr.Message != test.body {
						t.Fatalf("upstream response changed: %#v", resultErr)
					}
					if got := cooldownStatusCodeFromResult(resultErr); got != test.wantStatus {
						t.Fatalf("cooldown status = %d, want %d", got, test.wantStatus)
					}
					selector := NewSessionAffinitySelector(&RoundRobinSelector{})
					defer selector.Stop()
					opts := usageAffinityTestOptions("explicit")
					if _, errPick := selector.Pick(context.Background(), "claude", "model", opts, []*Auth{{ID: "auth-a", Provider: "claude"}}); errPick != nil {
						t.Fatal(errPick)
					}
					assertUsageAffinityBindings(t, selector, true)
					selector.OnResult(Result{AuthID: "auth-a", Provider: "claude", Model: "model", Error: resultErr, Options: opts})
					assertUsageAffinityBindings(t, selector, false)
				})
			}
		}
	}
}

func TestUsageRestrictionStructuredIdentifiers(t *testing.T) {
	for _, field := range []string{"type", "code", "status"} {
		for _, identifier := range []string{"usage_limit_reached", "insufficient_quota", "quota_exceeded", "rate_limit_exceeded", "rate_limit_error", "RESOURCE_EXHAUSTED", "billing_error"} {
			t.Run(field+"/"+identifier, func(t *testing.T) {
				body := fmt.Sprintf(`{"error":{"%s":%q,"message":"account restricted"}}`, field, strings.ToUpper(identifier))
				upstream := customStatusError{code: 400, msg: body}
				for _, err := range []error{upstream, fmt.Errorf("wrapped: %w", upstream), &Error{Code: "upstream_error", HTTPStatus: 400, Message: body}} {
					if !isUsageRestrictionError(err) || isRequestInvalidError(err) {
						t.Fatalf("expected account restriction, not request fault: %v", err)
					}
					resultErr := resultErrorFromError(err)
					if resultErr.HTTPStatus != 400 || resultErr.Message != body {
						t.Fatalf("upstream response changed: %#v", resultErr)
					}
					wantStatus := http.StatusTooManyRequests
					if identifier == "billing_error" {
						wantStatus = http.StatusPaymentRequired
					}
					if got := cooldownStatusCodeFromResult(resultErr); got != wantStatus {
						t.Fatalf("cooldown status = %d, want %d", got, wantStatus)
					}
				}
			})
		}
	}
}

func TestUsageRestrictionExcludesRequestLimits(t *testing.T) {
	for _, body := range []string{
		`{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"prompt is too long"}}`,
		`{"error":{"type":"invalid_request_error","message":"max_tokens exceeds the model limit"}}`,
		`{"error":{"type":"invalid_request_error","message":"quota_exceeded"}}`,
		`{"error":{"type":"invalid_request_error","message":"invalid input"},"code":"insufficient_quota"}`,
		`{"error":{"type":"invalid_request_error","message":"invalid input","details":{"code":"rate_limit_exceeded"}}}`,
		`{"error":{"type":"quota_exceeded_extra","message":"invalid input"}}`,
		`{"error":{"type":"invalid_request_error","message":"fast mode is not supported for this model"}}`,
		`{"error":{"type":"invalid_request_error","message":"invalid input"},"message":"Usage credits are required for fast mode."}`,
		`{"error":{"type":"api_error","message":"Usage credits are required for fast mode."}}`,
		`{"error":{"code":"insufficient_quota"}`,
		"usage limit exceeded",
	} {
		err := customStatusError{code: 400, msg: body}
		if isUsageRestrictionError(err) {
			t.Fatalf("request fault classified as account restriction: %v", err)
		}
		if !isRequestInvalidError(err) {
			t.Fatalf("expected request fault: %v", err)
		}
	}
	if isUsageRestrictionError(nil) || isUsageRestrictionError(context.Canceled) {
		t.Fatal("nil/cancellation must not be an account restriction")
	}
}

func TestUsageRestrictionPreservesPlainTextAndFastModePolicy(t *testing.T) {
	for _, status := range []int{http.StatusPaymentRequired, http.StatusTooManyRequests} {
		body := "account restricted"
		err := customStatusError{code: status, msg: body}
		if !isUsageRestrictionError(err) || isRequestInvalidError(err) {
			t.Fatalf("expected raw HTTP %d usage restriction", status)
		}
		resultErr := resultErrorFromError(err)
		if resultErr.HTTPStatus != status || resultErr.Message != body || cooldownStatusCodeFromResult(resultErr) != status {
			t.Fatalf("plain-text upstream response changed: %#v", resultErr)
		}
	}
	err := customStatusError{code: 400, msg: fastModeCreditRefusalBody}
	if !isUsageRestrictionError(err) || isBillingError(err) || !isRequestInvalidError(err) {
		t.Fatal("fast-mode credit refusal must release affinity but remain a non-billing request fault")
	}
	resultErr := resultErrorFromError(err)
	if !shouldSkipCredentialCooldown(resultErr) || cooldownStatusCodeFromResult(resultErr) != 400 {
		t.Fatalf("fast-mode retry/cooldown policy changed: %#v", resultErr)
	}
}

func TestUsageRestrictionSessionAffinity(t *testing.T) {
	for _, test := range []struct {
		name          string
		err           error
		requestScoped bool
		wantKeep      bool
	}{
		{name: "rate limit", err: customStatusError{code: 429, msg: "rate limited"}},
		{name: "payment", err: customStatusError{code: 402, msg: "payment required"}},
		{name: "monthly cap", err: customStatusError{code: 400, msg: monthlyUsageRefusalBody}},
		{name: "low credit", err: customStatusError{code: 400, msg: `{"error":{"type":"invalid_request_error","message":"Credit balance is too low"}}`}},
		{name: "structured quota", err: customStatusError{code: 400, msg: quotaRefusalBody}},
		{name: "forbidden", err: customStatusError{code: 403, msg: "access denied"}},
		{name: "fast mode credits", err: customStatusError{code: 400, msg: fastModeCreditRefusalBody}},
		{name: "request scoped monthly cap", err: customStatusError{code: 400, msg: monthlyUsageRefusalBody}, requestScoped: true},
		{name: "request scoped quota", err: customStatusError{code: 400, msg: quotaRefusalBody}, requestScoped: true},
		{name: "request scoped rate limit", err: customStatusError{code: 429, msg: "rate limited"}, requestScoped: true},
		{name: "context limit", err: customStatusError{code: 400, msg: `{"error":{"code":"context_length_exceeded","message":"prompt is too long"}}`}, wantKeep: true},
		{name: "malformed request", err: customStatusError{code: 400, msg: "max_tokens is required"}, wantKeep: true},
		{name: "unsupported fast mode", err: customStatusError{code: 400, msg: `{"error":{"type":"invalid_request_error","message":"fast mode is not supported for this model"}}`}, wantKeep: true},
		{name: "cancellation", err: context.Canceled, wantKeep: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, mode := range []string{"explicit", "lcp"} {
				t.Run(mode, func(t *testing.T) {
					selector := NewSessionAffinitySelector(&RoundRobinSelector{})
					defer selector.Stop()
					opts := usageAffinityTestOptions(mode)
					auths := []*Auth{{ID: "auth-a", Provider: "claude"}, {ID: "auth-b", Provider: "claude"}}
					picked, errPick := selector.Pick(context.Background(), "claude", "model", opts, auths)
					if errPick != nil || picked.ID != "auth-a" {
						t.Fatalf("initial Pick = %#v, %v", picked, errPick)
					}
					if mode == "explicit" {
						selector.OnResult(Result{AuthID: picked.ID, Provider: "claude", Model: "model", Success: true, Options: opts})
						assertUsageAffinityBindings(t, selector, true)
					}
					result := Result{AuthID: picked.ID, Provider: "claude", Model: "model", Error: resultErrorFromError(test.err), Options: opts}
					if test.requestScoped {
						applyRequestScopedActionToResult(RequestScopedActionStop, true, &result)
						if !shouldSkipCredentialCooldown(result.Error) {
							t.Fatal("explicit request-scoped cooling policy not retained")
						}
					}
					selector.OnResult(result)
					if mode == "explicit" {
						assertUsageAffinityBindings(t, selector, test.wantKeep)
					}
					next, errNext := selector.Pick(context.Background(), "claude", "model", opts, auths)
					wantID := "auth-b"
					if test.wantKeep {
						wantID = "auth-a"
					}
					if errNext != nil || next == nil {
						t.Fatalf("next Pick failed: %v", errNext)
					}
					if next.ID != wantID {
						t.Fatalf("next Pick = %s, want %s", next.ID, wantID)
					}
				})
			}
		})
	}
}

func TestUsageRestrictionMarkResultEvictsWithCoolingDisabled(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	for _, mode := range []string{"global", "per auth", "request scoped"} {
		t.Run(mode, func(t *testing.T) {
			quotaCooldownDisabled.Store(mode == "global")
			for _, upstream := range []customStatusError{
				{code: 429, msg: "rate limited"},
				{code: 402, msg: "payment required"},
				{code: 400, msg: monthlyUsageRefusalBody},
				{code: 400, msg: billingRefusalBody},
				{code: 400, msg: quotaRefusalBody},
				{code: 400, msg: fastModeCreditRefusalBody},
			} {
				t.Run(fmt.Sprintf("%d/%s", upstream.code, upstream.msg), func(t *testing.T) {
					selector := NewSessionAffinitySelector(&RoundRobinSelector{})
					defer selector.Stop()
					m := NewManager(nil, selector, nil)
					auth := &Auth{ID: "auth-a", Provider: "claude", Status: StatusActive}
					if mode == "per auth" {
						auth.Metadata = map[string]any{"disable_cooling": true}
					}
					if _, err := m.Register(context.Background(), auth); err != nil {
						t.Fatal(err)
					}
					opts := usageAffinityTestOptions("explicit")
					if _, err := selector.Pick(context.Background(), "claude", "model", opts, []*Auth{auth}); err != nil {
						t.Fatal(err)
					}
					selector.OnResult(Result{AuthID: auth.ID, Provider: "claude", Model: "model", Success: true, Options: opts})
					assertUsageAffinityBindings(t, selector, true)
					result := Result{AuthID: auth.ID, Provider: "claude", Model: "model", Error: resultErrorFromError(upstream), Options: opts}
					if mode == "request scoped" {
						applyRequestScopedActionToResult(RequestScopedActionStop, true, &result)
					}
					m.MarkResult(context.Background(), result)
					assertUsageAffinityBindings(t, selector, false)
					updated, _ := m.GetByID(auth.ID)
					if updated.Unavailable || !updated.NextRetryAfter.IsZero() {
						t.Fatalf("cooling disabled but auth unavailable: %#v", updated)
					}
					if state := updated.ModelStates["model"]; state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
						t.Fatalf("cooling disabled but model unavailable: %#v", state)
					}
				})
			}
		})
	}
}

func TestUsageRestrictionCompactIsNotAvailabilityNeutral(t *testing.T) {
	opts := cliproxyexecutor.Options{Alt: "responses/compact"}
	for _, body := range []string{monthlyUsageRefusalBody, quotaRefusalBody, fastModeCreditRefusalBody} {
		upstream := customStatusError{code: 400, msg: body}
		resultErr := resultErrorFromError(upstream)
		if isResponsesCompactAvailabilityNeutralError(opts, upstream, resultErr) ||
			isResponsesCompactAvailabilityNeutralError(opts, nil, resultErr) {
			t.Fatalf("usage restriction bypasses result/affinity handling: %s", body)
		}
	}
}

func usageAffinityTestOptions(mode string) cliproxyexecutor.Options {
	if mode == "lcp" {
		return cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAI,
			OriginalRequest: []byte(`{"messages":[{"role":"user","content":"stable prompt"}]}`),
			Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller-a"},
		}
	}
	return cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"conversation":{"id":"usage-conversation"},"prompt_cache_key":"usage-prompt"}`),
	}
}

func assertUsageAffinityBindings(t *testing.T, selector *SessionAffinitySelector, wantBound bool) {
	t.Helper()
	for _, id := range []string{"pck:usage-prompt", "conv:usage-conversation"} {
		bound, ok := selector.cache.Get("claude::" + id + "::model")
		if ok != wantBound || (ok && bound != "auth-a") {
			t.Fatalf("affinity %s = (%q, %v), want bound=%v", id, bound, ok, wantBound)
		}
	}
}
