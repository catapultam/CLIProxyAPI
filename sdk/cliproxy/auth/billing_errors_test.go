package auth

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const billingRefusalBody = `{"error":{"type":"billing_error","message":"Credit balance is too low"}}`
const monthlyUsageRefusalBody = `{"error":{"type":"invalid_request_error","message":"You have reached your specified API usage limits. You will regain access on 2026-11-01 at 00:00 UTC."}}`
const quotaRefusalBody = `{"error":{"type":"invalid_request_error","code":"insufficient_quota","message":"Account quota exceeded"}}`

func TestBillingErrorClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{"billing type", billingRefusalBody, true},
		{"Anthropic balance refusal", `{"error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`, true},
		{"Anthropic monthly cap refusal", monthlyUsageRefusalBody, true},
		{"monthly cap case insensitive", `{"error":{"type":"invalid_request_error","message":"You have REACHED YOUR SPECIFIED API USAGE LIMITS."}}`, true},
		{"monthly cap outside error message", `{"error":{"type":"invalid_request_error","message":"invalid messages"},"message":"You have reached your specified API usage limits."}`, false},
		{"monthly cap in error metadata", `{"error":{"type":"invalid_request_error","message":"invalid messages","details":"You have reached your specified API usage limits."}}`, false},
		{"monthly cap incorrect type", `{"error":{"type":"api_error","message":"You have reached your specified API usage limits."}}`, false},
		{"monthly cap missing type", `{"error":{"message":"You have reached your specified API usage limits."}}`, false},
		{"malformed request", `{"error":{"type":"invalid_request_error","message":"max_tokens is required"}}`, false},
		{"fast mode entitlement", `{"error":{"type":"invalid_request_error","message":"Usage credits are required for fast mode."}}`, false},
		{"user text", `{"error":{"type":"invalid_request_error","message":"invalid messages"},"messages":"credit balance is too low"}`, false},
		{"plain text", "Credit balance is too low", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := customStatusError{code: http.StatusBadRequest, msg: test.body}
			for _, err := range []error{upstream, fmt.Errorf("upstream: %w", upstream), &Error{Code: "upstream_error", HTTPStatus: 400, Message: test.body}} {
				if got := isBillingError(err); got != test.want {
					t.Fatalf("isBillingError(%v) = %v, want %v", err, got, test.want)
				}
				if got := isRequestInvalidError(err); got == test.want {
					t.Fatalf("isRequestInvalidError(%v) = %v", err, got)
				}
			}
		})
	}
}

func TestBillingErrorRotatesCredentials(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	for _, test := range []struct {
		name     string
		body     string
		status   int
		cooldown time.Duration
	}{
		{"billing", billingRefusalBody, 400, 30 * time.Minute},
		{"monthly cap", monthlyUsageRefusalBody, 400, 30 * time.Minute},
		{"structured quota", quotaRefusalBody, 400, quotaBackoffBase},
		{"monthly cap SSE", monthlyUsageRefusalBody, 502, 30 * time.Minute},
		{"low credit SSE", `{"error":{"type":"invalid_request_error","message":"Credit balance is too low"}}`, 502, 30 * time.Minute},
		{"structured quota SSE", quotaRefusalBody, 502, quotaBackoffBase},
	} {
		t.Run(test.name, func(t *testing.T) {
			testBillingErrorRotatesCredentials(t, test.body, test.status, test.cooldown)
		})
	}
}

func TestBillingErrorSSEPostBootstrap(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	for _, test := range []struct {
		name     string
		body     string
		cooldown time.Duration
	}{
		{"monthly", monthlyUsageRefusalBody, 30 * time.Minute},
		{"quota", quotaRefusalBody, quotaBackoffBase},
	} {
		t.Run(test.name, func(t *testing.T) {
			selector := NewSessionAffinitySelector(&RoundRobinSelector{})
			defer selector.Stop()
			m := NewManager(nil, selector, nil)
			model := "claude-sse-tail-" + test.name
			auth := &Auth{ID: model, Provider: "claude", Status: StatusActive}
			if _, err := m.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			m.RegisterExecutor(&customStreamMockExecutor{identifier: "claude", streamFn: func(_ context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				chunks := make(chan cliproxyexecutor.StreamChunk, 2)
				chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("first")}
				chunks <- cliproxyexecutor.StreamChunk{Err: customStatusError{code: 502, msg: test.body}}
				close(chunks)
				return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
			}})
			result, err := m.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, usageAffinityTestOptions("explicit"))
			if err != nil {
				t.Fatalf("expected stream to start: %v", err)
			}
			var tailErr error
			var payload string
			for chunk := range result.Chunks {
				payload += string(chunk.Payload)
				if chunk.Err != nil {
					tailErr = chunk.Err
				}
			}
			if payload != "first" || statusCodeFromError(tailErr) != 502 || extractErrorBody(tailErr) != test.body {
				t.Fatalf("stream response changed: payload=%q error=%v", payload, tailErr)
			}
			updated, _ := m.GetByID(auth.ID)
			state := updated.ModelStates[model]
			if state == nil || !state.Unavailable || state.NextRetryAfter.Sub(state.UpdatedAt) != test.cooldown {
				t.Fatalf("incorrect SSE restriction cooldown: %#v", state)
			}
			if state.LastError.HTTPStatus != 502 || state.LastError.Message != test.body {
				t.Fatalf("SSE upstream error changed: %#v", state.LastError)
			}
			for _, sessionID := range []string{"pck:usage-prompt", "conv:usage-conversation"} {
				if _, status := selector.LookupAffinity("claude", model, sessionID); status != "unbound" {
					t.Fatalf("SSE restriction retained %s affinity: %s", sessionID, status)
				}
			}
		})
	}
}

func testBillingErrorRotatesCredentials(t *testing.T, body string, status int, cooldown time.Duration) {
	t.Helper()
	for _, mode := range []string{"execute", "count", "stream", "bootstrap", "compact"} {
		t.Run(mode, func(t *testing.T) {
			m := NewManager(nil, quotaAttemptIsolationSelector{}, nil)
			m.SetRetryConfig(0, 0, 3)
			model := "claude-billing-" + mode
			ids := []string{model + "-a", model + "-b", model + "-c"}
			for _, id := range ids {
				if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "claude", Status: StatusActive}); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			var attempts []string
			attempt := func(auth *Auth) error {
				attempts = append(attempts, auth.ID)
				if auth.ID != ids[2] {
					return customStatusError{code: status, msg: body}
				}
				return nil
			}
			run := func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, attempt(auth)
			}
			if mode == "stream" || mode == "bootstrap" {
				m.RegisterExecutor(&customStreamMockExecutor{identifier: "claude", streamFn: func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					err := attempt(auth)
					if err == nil {
						return successStreamResult(), nil
					}
					if mode == "stream" {
						return nil, err
					}
					chunks := make(chan cliproxyexecutor.StreamChunk, 1)
					chunks <- cliproxyexecutor.StreamChunk{Err: err}
					close(chunks)
					return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
				}})
			} else {
				m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "claude", executeFn: run, countFn: run})
			}
			req := cliproxyexecutor.Request{Model: model}
			var err error
			switch mode {
			case "execute", "compact":
				opts := cliproxyexecutor.Options{}
				if mode == "compact" {
					opts.Alt = "responses/compact"
				}
				_, err = m.Execute(context.Background(), []string{"claude"}, req, opts)
			case "count":
				_, err = m.ExecuteCount(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
			default:
				var result *cliproxyexecutor.StreamResult
				result, err = m.ExecuteStream(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
				if err == nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				}
			}
			if err != nil {
				t.Fatalf("expected healthy third key to serve request: %v", err)
			}
			if !reflect.DeepEqual(attempts, ids) {
				t.Fatalf("attempts = %v, want %v", attempts, ids)
			}
			for _, id := range ids[:2] {
				auth, _ := m.GetByID(id)
				state := auth.ModelStates[model]
				if state == nil || !state.Unavailable || state.NextRetryAfter.Sub(state.UpdatedAt) != cooldown {
					t.Fatalf("expected cooldown %v for %s: %#v", cooldown, id, state)
				}
				if got := state.Quota.Exceeded; got != (body == quotaRefusalBody) {
					t.Fatalf("quota exceeded = %v for %s", got, id)
				}
				if state.LastError.HTTPStatus != status || state.LastError.Message != body {
					t.Fatalf("upstream error changed: %#v", state.LastError)
				}
			}
			if got := registry.GetGlobalRegistry().GetModelProviders(model); !reflect.DeepEqual(got, []string{"claude"}) {
				t.Fatalf("billing failures removed model route: %v", got)
			}
		})
	}
}
