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

func TestBillingErrorClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{"billing type", billingRefusalBody, true},
		{"Anthropic balance refusal", `{"error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`, true},
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
	for _, mode := range []string{"execute", "count", "stream", "bootstrap"} {
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
					return customStatusError{code: 400, msg: billingRefusalBody}
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
			case "execute":
				_, err = m.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
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
				if state == nil || !state.Unavailable || state.NextRetryAfter.Sub(state.UpdatedAt) != 30*time.Minute {
					t.Fatalf("expected payment cooldown for %s: %#v", id, state)
				}
				if state.LastError.HTTPStatus != 400 || state.LastError.Message != billingRefusalBody {
					t.Fatalf("upstream error changed: %#v", state.LastError)
				}
			}
			if got := registry.GetGlobalRegistry().GetModelProviders(model); !reflect.DeepEqual(got, []string{"claude"}) {
				t.Fatalf("billing failures removed model route: %v", got)
			}
		})
	}
}
