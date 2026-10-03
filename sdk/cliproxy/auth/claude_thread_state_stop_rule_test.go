package auth

import (
	"context"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const claudeThreadNotFoundAnnotatedBody = `{"type":"error","error":{"type":"not_found_error","message":"No thread state was found for the requested ` + "`previous_message_id`" + `.","details":{"error_code":"thread_not_found"}}}`

// Production config carries oauth-request-scoped-errors: claude: [{status: 404,
// match: ["No thread state"], action: stop}]. Through that stop rule the
// annotated 404 must still reach the caller with error.details.error_code
// "thread_not_found", from a single attempt and without cooling the credential.
func TestClaudeMissingThreadStateStopRuleKeepsThreadNotFoundBody(t *testing.T) {
	withQuotaCooldownEnabled(t)
	const model = "claude-thread-stop-rule"
	for _, stream := range []bool{false, true} {
		name := map[bool]string{false: "execute", true: "stream"}[stream]
		t.Run(name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(&internalconfig.Config{
				OAuthRequestScopedErrors: map[string][]internalconfig.RequestScopedErrorRule{
					"claude": {{Status: http.StatusNotFound, Match: []string{"No thread state"}, Action: "stop"}},
				},
			})
			ids := []string{"claude-thread-stop-rule-" + name + "-a", "claude-thread-stop-rule-" + name + "-b"}
			for _, id := range ids {
				registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				if _, errRegister := manager.Register(context.Background(), &Auth{ID: id, Provider: "claude", Status: StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}}); errRegister != nil {
					t.Fatalf("register %s: %v", id, errRegister)
				}
			}
			attempts := 0
			fail := func() error {
				attempts++
				return &claudeThreadStateTestError{status: http.StatusNotFound, msg: claudeThreadNotFoundAnnotatedBody}
			}
			manager.RegisterExecutor(&customStreamMockExecutor{
				identifier: "claude",
				mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
					return cliproxyexecutor.Response{}, fail()
				}},
				streamFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					return nil, fail()
				},
			})

			req := cliproxyexecutor.Request{Model: model}
			var err error
			if stream {
				_, err = manager.ExecuteStream(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{Stream: true})
			} else {
				_, err = manager.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
			}
			if err == nil {
				t.Fatal("expected the 404")
			}
			if code := gjson.Get(err.Error(), "error.details.error_code").String(); code != "thread_not_found" {
				t.Fatalf("error.details.error_code = %q, want thread_not_found; error %s", code, err.Error())
			}
			if statusCodeFromError(err) != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", statusCodeFromError(err))
			}
			first, _ := manager.GetByID(ids[0])
			if action, ok := matchRequestScopedErrorAction(first, err, manager.runtimeConfigSnapshot()); !ok || action != RequestScopedActionStop {
				t.Fatalf("stop rule did not match the 404 (action=%q ok=%v)", action, ok)
			}
			if attempts != 1 {
				t.Fatalf("attempts = %d, want 1 (stop rule, no rotation)", attempts)
			}
			for _, id := range ids {
				auth, ok := manager.GetByID(id)
				if !ok || auth.Unavailable || !auth.NextRetryAfter.IsZero() {
					t.Fatalf("auth %s was cooled down: %#v", id, auth)
				}
				if state := auth.ModelStates[model]; state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
					t.Fatalf("auth %s model state was cooled down: %#v", id, state)
				}
			}
		})
	}
}
