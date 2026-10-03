package auth

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// useFreshClaudeThreadOwners swaps the process-wide owner store for an empty one
// with a controllable clock for the duration of the test.
func useFreshClaudeThreadOwners(t *testing.T, now *time.Time) *claudeThreadOwnerStore {
	t.Helper()
	previous := claudeThreadOwners
	previousPath := claudeThreadOwnerStatePath.Load()
	store := newClaudeThreadOwnerStore()
	if now != nil {
		store.now = func() time.Time { return *now }
	}
	claudeThreadOwners = store
	t.Cleanup(func() {
		claudeThreadOwners = previous
		if previousPath != nil {
			claudeThreadOwnerStatePath.Store(previousPath)
		} else {
			claudeThreadOwnerStatePath.Store("")
		}
	})
	return store
}

const threadOwnerTestModel = "claude-thread-owner-test"

// newThreadOwnerTestManager registers two Claude OAuth credentials behind a
// session-affinity selector and an executor that records which credential served
// each request.
func newThreadOwnerTestManager(t *testing.T, prefix string) (*Manager, *[]string, string, string) {
	t.Helper()
	ctx := context.Background()
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	t.Cleanup(selector.Stop)
	manager := NewManager(nil, selector, nil)
	manager.SetRetryConfig(0, 0, 0)
	ids := []string{prefix + "-a", prefix + "-b"}
	for _, id := range ids {
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: threadOwnerTestModel}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := manager.Register(ctx, &Auth{ID: id, Provider: "claude", Status: StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}}); errRegister != nil {
			t.Fatalf("register %s: %v", id, errRegister)
		}
	}
	var served []string
	execute := func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		served = append(served, auth.ID)
		return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
	}
	manager.RegisterExecutor(&customStreamMockExecutor{
		identifier:              "claude",
		mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: execute},
		streamFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
			response, _ := execute(ctx, auth, req, opts)
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			chunks <- cliproxyexecutor.StreamChunk{Payload: response.Payload}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
		},
	})
	return manager, &served, ids[0], ids[1]
}

func threadOwnerTestOptions(session string, body string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		Headers:         http.Header{"X-Claude-Code-Session-Id": []string{session}},
		OriginalRequest: []byte(body),
		Metadata:        map[string]any{},
	}
}

func executeThreadOwnerRequest(t *testing.T, manager *Manager, stream bool, opts cliproxyexecutor.Options) string {
	t.Helper()
	req := cliproxyexecutor.Request{Model: threadOwnerTestModel, Payload: opts.OriginalRequest}
	if !stream {
		resp, errExecute := manager.Execute(context.Background(), []string{"claude"}, req, opts)
		if errExecute != nil {
			t.Fatalf("execute: %v", errExecute)
		}
		return string(resp.Payload)
	}
	opts.Stream = true
	result, errStream := manager.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
	if errStream != nil {
		t.Fatalf("execute stream: %v", errStream)
	}
	var payload []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	return string(payload)
}

// A thread continuation must go to the credential that produced
// previous_message_id even when session affinity points at another one, and the
// session must then stay bound to the owner.
func TestClaudeThreadContinueRoutesToOwnerOverSessionAffinity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := map[bool]string{false: "execute", true: "stream"}[stream]
		t.Run(name, func(t *testing.T) {
			useFreshClaudeThreadOwners(t, nil)
			manager, served, authA, authB := newThreadOwnerTestManager(t, "thread-owner-route-"+name)
			session := "thread-owner-session-" + name

			// Bind the session to whichever credential serves first, then make the
			// other one the thread owner.
			first := executeThreadOwnerRequest(t, manager, stream, threadOwnerTestOptions(session, `{"messages":[]}`))
			owner := authA
			if first == authA {
				owner = authB
			}
			RecordClaudeThreadOwner("msg_owned", owner)

			continueBody := `{"thread":{"type":"continue","previous_message_id":"msg_owned"},"messages":[]}`
			if got := executeThreadOwnerRequest(t, manager, stream, threadOwnerTestOptions(session, continueBody)); got != owner {
				t.Fatalf("continuation served by %s, want owner %s (affinity was %s); served %v", got, owner, first, *served)
			}
			// The owner pin is request-local: a later non-thread request in the
			// same session follows the rebound affinity, i.e. the owner.
			if got := executeThreadOwnerRequest(t, manager, stream, threadOwnerTestOptions(session, `{"messages":[]}`)); got != owner {
				t.Fatalf("follow-up served by %s, want rebound owner %s", got, owner)
			}
		})
	}
}

// Requests that are not thread continuations keep their session affinity even
// when the owner store has entries, and so do continuations whose owner is
// unknown.
func TestClaudeThreadOwnerLeavesOtherRequestsOnAffinity(t *testing.T) {
	useFreshClaudeThreadOwners(t, nil)
	manager, _, authA, authB := newThreadOwnerTestManager(t, "thread-owner-nonthread")
	session := "thread-owner-nonthread-session"
	bound := executeThreadOwnerRequest(t, manager, false, threadOwnerTestOptions(session, `{"messages":[]}`))
	other := authA
	if bound == authA {
		other = authB
	}
	RecordClaudeThreadOwner("msg_other", other)

	for _, body := range []string{
		`{"messages":[]}`,
		`{"thread":{"type":"create"},"messages":[]}`,
		`{"thread":{"type":"continue","previous_message_id":"msg_unknown"},"messages":[]}`,
		`{"metadata":{"previous_message_id":"msg_other"},"messages":[]}`,
	} {
		for i := 0; i < 3; i++ {
			if got := executeThreadOwnerRequest(t, manager, false, threadOwnerTestOptions(session, body)); got != bound {
				t.Fatalf("request %s served by %s, want affinity-bound %s", body, got, bound)
			}
		}
	}
}

// When the owner cannot serve the model (here it is cooling down), the
// continuation falls back to normal selection instead of failing.
func TestClaudeThreadContinueFallsBackWhenOwnerUnavailable(t *testing.T) {
	withQuotaCooldownEnabled(t)
	useFreshClaudeThreadOwners(t, nil)
	manager, _, authA, authB := newThreadOwnerTestManager(t, "thread-owner-cooling")
	retryAfter := time.Hour
	manager.MarkResult(context.Background(), Result{
		AuthID: authA, Provider: "claude", Model: threadOwnerTestModel, RetryAfter: &retryAfter,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
	})
	RecordClaudeThreadOwner("msg_cooling_owner", authA)

	body := `{"thread":{"type":"continue","previous_message_id":"msg_cooling_owner"},"messages":[]}`
	if got := executeThreadOwnerRequest(t, manager, false, threadOwnerTestOptions("thread-owner-cooling-session", body)); got != authB {
		t.Fatalf("continuation served by %s, want fallback %s", got, authB)
	}
}

// Owners survive a save/load round trip, so continuations still reach their
// owner after a restart; expired owners are dropped on load.
func TestClaudeThreadOwnersSurviveSaveLoad(t *testing.T) {
	now := time.Date(2026, 10, 2, 23, 0, 0, 0, time.UTC)
	store := useFreshClaudeThreadOwners(t, &now)
	path := filepath.Join(t.TempDir(), "claude-thread-owners.json")

	store.record("msg_old", "auth-old")
	now = now.Add(claudeThreadOwnerTTL - time.Minute)
	store.record("msg_new", "auth-new")
	if errSave := store.save(path); errSave != nil {
		t.Fatalf("save: %v", errSave)
	}

	// A fresh store stands in for the restarted process, two minutes later: the
	// older owner is now past its TTL.
	now = now.Add(2 * time.Minute)
	restored := useFreshClaudeThreadOwners(t, &now)
	restored.load(path)
	if got := restored.owner("msg_new"); got != "auth-new" {
		t.Fatalf("owner(msg_new) = %q after reload, want auth-new", got)
	}
	if got := restored.owner("msg_old"); got != "" {
		t.Fatalf("owner(msg_old) = %q after reload, want expired", got)
	}

	// Loading a missing file is fine and keeps what is in memory.
	restored.load(filepath.Join(t.TempDir(), "missing.json"))
	if got := restored.owner("msg_new"); got != "auth-new" {
		t.Fatalf("owner(msg_new) = %q after loading a missing file", got)
	}
}

// SaveClaudeThreadOwners writes to the path persistence was started with, so a
// shutdown save lands where the next start loads from.
func TestStartClaudeThreadOwnerPersistenceLoadsAndSaves(t *testing.T) {
	useFreshClaudeThreadOwners(t, nil)
	path := filepath.Join(t.TempDir(), "claude-thread-owners.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartClaudeThreadOwnerPersistence(ctx, path)
	RecordClaudeThreadOwner("msg_shutdown", "auth-shutdown")
	SaveClaudeThreadOwners()

	restored := useFreshClaudeThreadOwners(t, nil)
	restored.load(path)
	if got := restored.owner("msg_shutdown"); got != "auth-shutdown" {
		t.Fatalf("owner(msg_shutdown) = %q after shutdown save, want auth-shutdown", got)
	}
}

// The store stays bounded: past its cap, the oldest owners are evicted first.
func TestClaudeThreadOwnerStoreIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	store := useFreshClaudeThreadOwners(t, &now)
	for i := 0; i <= claudeThreadOwnerMaxEntries; i++ {
		now = now.Add(time.Millisecond)
		store.record("msg_"+time.Duration(i).String(), "auth")
	}
	if n := len(store.entries); n > claudeThreadOwnerMaxEntries {
		t.Fatalf("store holds %d entries, cap is %d", n, claudeThreadOwnerMaxEntries)
	}
	if got := store.owner("msg_" + time.Duration(0).String()); got != "" {
		t.Fatal("oldest owner survived eviction")
	}
	if got := store.owner("msg_" + time.Duration(claudeThreadOwnerMaxEntries).String()); got != "auth" {
		t.Fatal("newest owner was evicted")
	}
}
