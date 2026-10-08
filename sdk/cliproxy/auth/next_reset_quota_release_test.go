package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestManager_ReleaseQuotaCooldown_ClearsCredentialQuotaDaysAway covers Fix
// 2's core case: a real usage_limit_reached 429 sets a credential_quota
// cooldown days away on the auth and its model state; ReleaseQuotaCooldown
// clears it and the credential becomes selectable again.
func TestManager_ReleaseQuotaCooldown_ClearsCredentialQuotaDaysAway(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	authID := uuid.NewString() + "-release-quota"
	model := "release-quota-model"
	daysAway := time.Now().Add(6 * 24 * time.Hour)

	if _, err := manager.Register(ctx, &Auth{
		ID:             authID,
		Provider:       "codex",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: daysAway,
		Quota:          QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: daysAway},
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: daysAway,
				Quota:          QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: daysAway},
			},
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	released, errRelease := manager.ReleaseQuotaCooldown(ctx, authID)
	if errRelease != nil {
		t.Fatalf("ReleaseQuotaCooldown() error = %v", errRelease)
	}
	if !released {
		t.Fatal("ReleaseQuotaCooldown() = false, want true")
	}

	updated, ok := manager.GetByID(authID)
	if !ok {
		t.Fatal("auth not found after release")
	}
	if updated.Quota.Exceeded || updated.Quota.Reason != "" || !updated.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("auth quota not cleared: %+v", updated.Quota)
	}
	if updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("auth still unavailable after release: unavailable=%v next=%v", updated.Unavailable, updated.NextRetryAfter)
	}
	state := updated.ModelStates[model]
	if state == nil || state.Quota.Exceeded || state.Unavailable || !state.NextRetryAfter.IsZero() {
		t.Fatalf("model state not released: %+v", state)
	}

	if blocked, _, _ := isAuthBlockedForModel(updated, model, time.Now()); blocked {
		t.Fatal("auth still blocked for model after release")
	}
}

// TestManager_ReleaseQuotaCooldown_NoopWhenNoQuotaCooldown covers an auth
// with nothing to release: ReleaseQuotaCooldown must report no change.
func TestManager_ReleaseQuotaCooldown_NoopWhenNoQuotaCooldown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	authID := uuid.NewString() + "-release-noop"
	if _, err := manager.Register(ctx, &Auth{ID: authID, Provider: "codex"}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	released, errRelease := manager.ReleaseQuotaCooldown(ctx, authID)
	if errRelease != nil || released {
		t.Fatalf("ReleaseQuotaCooldown() = %v, %v, want false, nil", released, errRelease)
	}
}

// TestManager_ReleaseQuotaCooldown_NeverClearsNonQuotaCooldown covers an
// auth blocked purely by a non-quota cooldown (a 401 here): ReleaseQuotaCooldown
// must not touch it.
func TestManager_ReleaseQuotaCooldown_NeverClearsNonQuotaCooldown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	authID := uuid.NewString() + "-release-401"
	next := time.Now().Add(30 * time.Minute)
	if _, err := manager.Register(ctx, &Auth{
		ID:             authID,
		Provider:       "claude",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: next,
		LastError:      &Error{HTTPStatus: http.StatusUnauthorized, Code: "unauthorized", Message: "unauthorized"},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	released, errRelease := manager.ReleaseQuotaCooldown(ctx, authID)
	if errRelease != nil {
		t.Fatalf("ReleaseQuotaCooldown() error = %v", errRelease)
	}
	if released {
		t.Fatal("ReleaseQuotaCooldown() = true, want false: there is no quota cooldown to release")
	}

	updated, ok := manager.GetByID(authID)
	if !ok {
		t.Fatal("auth not found")
	}
	if !updated.Unavailable || updated.NextRetryAfter.IsZero() || updated.LastError == nil {
		t.Fatalf("non-quota cooldown was cleared: unavailable=%v next=%v lastError=%v", updated.Unavailable, updated.NextRetryAfter, updated.LastError)
	}
}

// TestManager_ReleaseQuotaCooldown_KeepsConcurrentUnauthorizedBlock is the
// mixed case the advisor flagged: a real usage_limit_reached 429 sets a
// days-away credential_quota cooldown, then a later, unrelated 401 on the
// same auth leaves its LastError/Unavailable/NextRetryAfter looking
// quota-shaped (MarkResult's "a later failure only extends a still-live
// cooldown" rule keeps NextRetryAfter at the quota's own deadline). Releasing
// the quota cooldown must clear the Quota fields themselves (the usage
// limit really did clear), but must NOT clear Unavailable, NextRetryAfter,
// or LastError, since those are what keep the credential correctly blocked
// for the still-unresolved 401.
func TestManager_ReleaseQuotaCooldown_KeepsConcurrentUnauthorizedBlock(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	authID := uuid.NewString() + "-release-mixed"
	daysAway := time.Now().Add(6 * 24 * time.Hour)
	if _, err := manager.Register(ctx, &Auth{
		ID:             authID,
		Provider:       "codex",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: daysAway,
		LastError:      &Error{HTTPStatus: http.StatusUnauthorized, Code: "unauthorized"},
		Quota:          QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: daysAway},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	released, errRelease := manager.ReleaseQuotaCooldown(ctx, authID)
	if errRelease != nil {
		t.Fatalf("ReleaseQuotaCooldown() error = %v", errRelease)
	}
	if !released {
		t.Fatal("ReleaseQuotaCooldown() = false, want true: the quota fields themselves should still be cleared")
	}

	updated, ok := manager.GetByID(authID)
	if !ok {
		t.Fatal("auth not found")
	}
	if updated.Quota.Exceeded || updated.Quota.Reason != "" {
		t.Fatalf("quota fields not cleared: %+v", updated.Quota)
	}
	if !updated.Unavailable || updated.NextRetryAfter.IsZero() || updated.LastError == nil || updated.LastError.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("a concurrent 401 block was cleared alongside the quota cooldown: unavailable=%v next=%v lastError=%v",
			updated.Unavailable, updated.NextRetryAfter, updated.LastError)
	}
	if blocked, _, _ := isAuthBlockedForModel(updated, "", time.Now()); !blocked {
		t.Fatal("auth became selectable despite the concurrent 401 block")
	}
}

// TestNextResetSnapshotShowsRoom covers the window logic the poller's
// release gate relies on: a full or rejected known window blocks release, an
// entirely unknown snapshot never claims room, and a full Fable window
// (model-specific) never blocks the general release.
func TestNextResetSnapshotShowsRoom(t *testing.T) {
	full := nextResetSnapshot{Weekly: nextResetWindow{Known: true, UsedPct: 100}}
	if nextResetSnapshotShowsRoom(full) {
		t.Fatal("a full known weekly window reported room")
	}
	rejected := nextResetSnapshot{Short: nextResetWindow{Known: true, UsedPct: 5}, Weekly: nextResetWindow{Known: true, Rejected: true}}
	if nextResetSnapshotShowsRoom(rejected) {
		t.Fatal("a rejected window reported room")
	}
	room := nextResetSnapshot{Short: nextResetWindow{Known: true, UsedPct: 5}, Weekly: nextResetWindow{Known: true, UsedPct: 10}}
	if !nextResetSnapshotShowsRoom(room) {
		t.Fatal("two known, non-full windows did not report room")
	}
	unknown := nextResetSnapshot{}
	if nextResetSnapshotShowsRoom(unknown) {
		t.Fatal("an entirely unknown snapshot reported room")
	}
	fableFull := nextResetSnapshot{
		Short:  nextResetWindow{Known: true, UsedPct: 5},
		Weekly: nextResetWindow{Known: true, UsedPct: 10},
		Fable:  nextResetWindow{Known: true, UsedPct: 100},
	}
	if !nextResetSnapshotShowsRoom(fableFull) {
		t.Fatal("a full Fable window blocked the general release")
	}
}

// TestNextResetPollerReleasesQuotaCooldownWhenUsageConfirmsRoom is the
// end-to-end Fix 2 case: an auth with a credential_quota cooldown days away,
// polled by the usage endpoint showing room in both windows, is released
// and becomes selectable.
func TestNextResetPollerReleasesQuotaCooldownWhenUsageConfirmsRoom(t *testing.T) {
	withNextReset(t)
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	authID := uuid.NewString() + "-poller-release"
	daysAway := nrNow.Add(6 * 24 * time.Hour)
	auth := tokenAuth(&Auth{
		ID:             authID,
		Provider:       "codex",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: daysAway,
		Quota:          QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: daysAway},
	})
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	d := &fakeUsageDoer{status: 200, body: `{"rate_limit":{` +
		`"primary_window":{"used_percent":5,"limit_window_seconds":18000},` +
		`"secondary_window":{"used_percent":10,"limit_window_seconds":604800}}}`}
	p := &nextResetPoller{
		list: manager.List, do: d.do, store: nextResetPolled,
		releaseQuota: manager.ReleaseQuotaCooldown,
		backoff:      map[string]time.Time{}, lastPoll: map[string]time.Time{},
	}

	registered, _ := manager.GetByID(authID)
	p.fetch(ctx, registered, nrNow)

	updated, ok := manager.GetByID(authID)
	if !ok {
		t.Fatal("auth not found")
	}
	if updated.Quota.Exceeded || updated.Quota.Reason != "" {
		t.Fatalf("quota cooldown not released by a room-confirming poll: %+v", updated.Quota)
	}
	if updated.Unavailable {
		t.Fatal("auth still unavailable after a room-confirming poll")
	}
	if blocked, _, _ := isAuthBlockedForModel(updated, "", nrNow); blocked {
		t.Fatal("auth still blocked for selection after release")
	}
}

// TestNextResetPollerKeepsQuotaCooldownWhenUsageStillFull covers the other
// half: the same setup, but the weekly window is still full, so nothing is
// released.
func TestNextResetPollerKeepsQuotaCooldownWhenUsageStillFull(t *testing.T) {
	withNextReset(t)
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	authID := uuid.NewString() + "-poller-keep"
	daysAway := nrNow.Add(6 * 24 * time.Hour)
	auth := tokenAuth(&Auth{
		ID:             authID,
		Provider:       "codex",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: daysAway,
		Quota:          QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: daysAway},
	})
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	d := &fakeUsageDoer{status: 200, body: `{"rate_limit":{` +
		`"primary_window":{"used_percent":5,"limit_window_seconds":18000},` +
		`"secondary_window":{"used_percent":100,"limit_window_seconds":604800}}}`}
	p := &nextResetPoller{
		list: manager.List, do: d.do, store: nextResetPolled,
		releaseQuota: manager.ReleaseQuotaCooldown,
		backoff:      map[string]time.Time{}, lastPoll: map[string]time.Time{},
	}

	registered, _ := manager.GetByID(authID)
	p.fetch(ctx, registered, nrNow)

	updated, ok := manager.GetByID(authID)
	if !ok {
		t.Fatal("auth not found")
	}
	if !updated.Quota.Exceeded || updated.Quota.Reason != "credential_quota" {
		t.Fatalf("quota cooldown released despite the weekly window still being full: %+v", updated.Quota)
	}
}
