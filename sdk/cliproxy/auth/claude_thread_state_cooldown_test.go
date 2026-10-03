package auth

import (
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// claudeThreadStateTestError stands in for the executor-level error Claude's
// thread-continuation 404 is classified into (internal/runtime/executor
// cannot be imported here without an import cycle). It mirrors that error's
// shape: a 404 status that is request-scoped and explicitly not credential-
// scoped, so the plumbing this test exercises is the same regardless of
// which package produced the error.
type claudeThreadStateTestError struct {
	status int
	msg    string
}

func (e *claudeThreadStateTestError) Error() string         { return e.msg }
func (e *claudeThreadStateTestError) StatusCode() int       { return e.status }
func (e *claudeThreadStateTestError) IsRequestScoped() bool { return true }

const claudeMissingThreadStateBody = `{"type":"error","error":{"type":"not_found_error","message":"No thread state was found for the requested ` + "`previous_message_id`" + `. Replay the full conversation with thread: {\"type\": \"create\"} instead."}}`

// TestClaudeMissingThreadState_ResultErrorIsRequestScopedNoCooldown proves the
// SDK side of the fix: once the executor classifies the 404 as request-scoped,
// resultErrorFromError must preserve that classification (the model_not_found
// check that runs first in that switch must not steal it, since the body
// mentions neither "model" nor an exact model name) and MarkResult must leave
// the credential and its model state untouched.
func TestClaudeMissingThreadState_ResultErrorIsRequestScopedNoCooldown(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	rawErr := &claudeThreadStateTestError{status: http.StatusNotFound, msg: claudeMissingThreadStateBody}

	resultErr := resultErrorFromError(rawErr)
	if resultErr == nil {
		t.Fatal("resultErrorFromError returned nil")
	}
	if resultErr.Code != requestScopedErrorCode {
		t.Fatalf("resultErr.Code = %q, want %q", resultErr.Code, requestScopedErrorCode)
	}
	if !shouldSkipCredentialCooldown(resultErr) {
		t.Fatalf("shouldSkipCredentialCooldown(%#v) = false, want true", resultErr)
	}

	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-claude-thread-state", Provider: "claude"}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "claude-opus-5.5"
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error:    resultErr,
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("expected auth to remain registered")
	}
	if updated.Unavailable {
		t.Fatal("expected missing thread state 404 to keep auth available")
	}
	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf("expected missing thread state 404 to keep auth cooldown unset, got %v", updated.NextRetryAfter)
	}
	if state := updated.ModelStates[model]; state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
		t.Fatalf("expected missing thread state 404 to avoid model cooldown state, got %#v", state)
	}
}

// TestClaudeMissingThreadState_SessionAffinityPreserved proves the selector
// keeps session affinity bound to the same credential after this failure,
// unlike a genuine credential-scoped failure which would release it.
func TestClaudeMissingThreadState_SessionAffinityPreserved(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()

	auth1 := &Auth{ID: "auth-thread-state-1", Provider: "claude"}
	auth2 := &Auth{ID: "auth-thread-state-2", Provider: "claude"}
	candidates := []*Auth{auth1, auth2}

	opts := cliproxyexecutor.Options{
		Headers: http.Header{"X-Session-Id": []string{"session-claude-thread-state-test"}},
	}

	picked, errPick := selector.Pick(context.Background(), "claude", "claude-opus-5.5", opts, candidates)
	if errPick != nil || picked == nil || picked.ID != "auth-thread-state-1" {
		t.Fatalf("initial Pick failed: %v", errPick)
	}

	selector.OnResult(Result{
		AuthID:   picked.ID,
		Provider: "claude",
		Model:    "claude-opus-5.5",
		Success:  true,
		Options:  opts,
	})

	rawErr := &claudeThreadStateTestError{status: http.StatusNotFound, msg: claudeMissingThreadStateBody}
	resultErr := resultErrorFromError(rawErr)

	selector.OnResult(Result{
		AuthID:   picked.ID,
		Provider: "claude",
		Model:    "claude-opus-5.5",
		Success:  false,
		Error:    resultErr,
		Options:  opts,
	})

	// Reverse candidate order: if affinity were dropped, round-robin would
	// hand back auth2 first. Affinity must keep returning auth1.
	reverseCandidates := []*Auth{auth2, auth1}
	nextPicked, errNextPick := selector.Pick(context.Background(), "claude", "claude-opus-5.5", opts, reverseCandidates)
	if errNextPick != nil || nextPicked == nil {
		t.Fatalf("next Pick failed: %v", errNextPick)
	}
	if nextPicked.ID != "auth-thread-state-1" {
		t.Fatalf("affinity was dropped: got %s, want auth-thread-state-1", nextPicked.ID)
	}
}
