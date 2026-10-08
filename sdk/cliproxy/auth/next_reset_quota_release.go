package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// isQuotaCooldownReason reports whether reason names a quota/usage-limit
// cooldown -- "quota" for a per-model rate limit, "credential_quota" for a
// credential-wide usage limit -- as opposed to a non-quota block reason such
// as an unauthorized/forbidden/not-found response, a 5xx, a Cloudflare
// challenge, or a model-support cooldown.
func isQuotaCooldownReason(reason string) bool {
	return reason == "quota" || reason == "credential_quota"
}

// quotaOnlyRetryBlock reports whether a credential's (or one of its model
// states') current NextRetryAfter/LastError are wholly explained by the
// quota cooldown being released (whose recovery time was quotaNext before
// it was cleared), so Unavailable and NextRetryAfter are also safe to clear.
// MarkResult always writes NextRetryAfter equal to the cooldown's own
// NextRecoverAt at the moment a quota cooldown is set, but a later failure
// can extend (never shorten) a still-live NextRetryAfter past that point
// (see MarkResult's "a later failure only extends a still-live cooldown"
// rule) without updating Quota.NextRecoverAt to match; NextRetryAfter
// strictly after quotaNext therefore signals a separate, still-active
// concern. LastError is checked the same way hasUnauthorizedAuthFailure
// does: a non-429 LastError (401, 403, 404, 5xx, ...) means the credential
// is also blocked for a non-quota reason that must not be cleared here.
func quotaOnlyRetryBlock(nextRetryAfter time.Time, lastError *Error, quotaNext time.Time) bool {
	if !nextRetryAfter.IsZero() && nextRetryAfter.After(quotaNext) {
		return false
	}
	return lastError == nil || lastError.StatusCode() == http.StatusTooManyRequests
}

// clearQuotaCooldownOnAuthLocked clears a quota-exhaustion cooldown on auth
// -- auth-level, and any model state whose own cooldown is quota-driven --
// leaving a concurrent non-quota cooldown (401/403/404, 5xx, Cloudflare
// challenge, model-support) untouched. It mirrors the field resets
// clearCooldownStateForAuth applies for the management
// POST /v8/management/routing/cooldown/reset endpoint (via ResetQuota), but
// gated to QuotaState.Reason being "quota" or "credential_quota", and uses
// quotaOnlyRetryBlock to decide whether Unavailable, NextRetryAfter, Status,
// and LastError are also safe to clear alongside the Quota fields
// themselves. Must be called with m.mu held for writing. Reports whether
// anything changed.
func clearQuotaCooldownOnAuthLocked(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	changed := false

	if isQuotaCooldownReason(auth.Quota.Reason) && (auth.Quota.Exceeded || !auth.Quota.NextRecoverAt.IsZero()) {
		quotaNext := auth.Quota.NextRecoverAt
		quotaOnly := quotaOnlyRetryBlock(auth.NextRetryAfter, auth.LastError, quotaNext)
		applyCooldownFields(&auth.Quota, QuotaState{})
		if quotaOnly {
			auth.Unavailable = false
			auth.NextRetryAfter = time.Time{}
		}
		auth.UpdatedAt = now
		changed = true
	}

	for _, state := range auth.ModelStates {
		if state == nil || !isQuotaCooldownReason(state.Quota.Reason) || (!state.Quota.Exceeded && state.Quota.NextRecoverAt.IsZero()) {
			continue
		}
		quotaNext := state.Quota.NextRecoverAt
		quotaOnly := quotaOnlyRetryBlock(state.NextRetryAfter, state.LastError, quotaNext)
		applyCooldownFields(&state.Quota, QuotaState{})
		if quotaOnly {
			state.Unavailable = false
			state.NextRetryAfter = time.Time{}
			state.LastError = nil
			state.StatusMessage = ""
			if state.Status == StatusError {
				state.Status = StatusActive
			}
		}
		state.UpdatedAt = now
		changed = true
	}

	if !changed {
		return false
	}

	if len(auth.ModelStates) > 0 {
		updateAggregatedAvailability(auth, now)
	}
	// Only declare the credential itself healthy again when nothing
	// non-quota is still holding it back: no lingering model error, no
	// aggregate Unavailable, and no auth-level LastError other than the
	// quota 429 itself (the same signal hasUnauthorizedAuthFailure and
	// friends read to keep a 401/403/... block alive).
	if !auth.Disabled && auth.Status != StatusDisabled && !auth.Unavailable && !hasModelError(auth, now) &&
		(auth.LastError == nil || auth.LastError.StatusCode() == http.StatusTooManyRequests) {
		auth.LastError = nil
		auth.StatusMessage = ""
		auth.Status = StatusActive
	}
	auth.Generation++
	return true
}

// ReleaseQuotaCooldown clears authID's quota-exhaustion cooldown (see
// clearQuotaCooldownOnAuthLocked). It is meant to be called by the
// next-reset usage poller once a fresh snapshot confirms room in every
// window that matters for that credential (see
// nextResetPoller.releaseQuotaCooldownIfRoom); the caller is responsible for
// deciding "confirmed," not this method. It never touches a non-quota
// cooldown, and is a no-op when the auth has no active quota cooldown to
// clear. It mirrors ResetQuota's persistence, registry projection, and
// scheduler sync, narrowed to the quota-only clear above. Reports whether
// anything changed.
func (m *Manager) ReleaseQuotaCooldown(ctx context.Context, authID string) (bool, error) {
	if m == nil {
		return false, nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	var snapshot *Auth
	cooldownStateChanged := false

	m.mu.Lock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.Unlock()
		return false, nil
	}

	var cooldownRecordsBefore []CooldownStateRecord
	trackCooldownState := m.cooldownStore != nil
	if trackCooldownState {
		cooldownRecordsBefore = m.cooldownStateRecordsForAuthLocked(auth, now)
	}

	if !clearQuotaCooldownOnAuthLocked(auth, now) {
		m.mu.Unlock()
		return false, nil
	}

	snapshot = auth.Clone()
	if trackCooldownState {
		cooldownRecordsAfter := m.cooldownStateRecordsForAuthLocked(auth, now)
		cooldownStateChanged = !cooldownStateRecordsEqual(cooldownRecordsBefore, cooldownRecordsAfter)
	}
	errPersist := m.persist(ctx, auth)
	m.mu.Unlock()

	defer func() {
		if cooldownStateChanged {
			m.persistCooldownStates(context.Background())
		}
	}()

	supportedModels, regEpoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(authID)
	projections := make([]registry.ClientModelProjection, 0, len(supportedModels))
	for _, sm := range supportedModels {
		if sm == nil || strings.TrimSpace(sm.ID) == "" {
			continue
		}
		projections = append(projections, m.clientModelProjectionForAuth(snapshot, sm.ID, now))
	}
	if snapshot != nil {
		registry.GetGlobalRegistry().ApplyClientModelProjections(authID, regEpoch, snapshot.Generation, projections)
	}
	if m.scheduler != nil && snapshot != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	if errPersist != nil {
		return true, errPersist
	}
	return true, nil
}

// nextResetSnapshotShowsRoom reports whether snap shows room in every window
// that gates releasing a general quota cooldown: the short and weekly
// windows, when known. Claude's Fable sub-limit (snap.Fable) is a
// model-specific weekly cap (see claudeFableWindow) that only concerns
// Fable-family models, so a saturated Fable window never blocks releasing
// the general credential/model cooldown here; it is tracked separately as
// that model's own cooldown. Reports false when neither window is known, so
// an empty snapshot is never mistaken for confirmed room.
func nextResetSnapshotShowsRoom(snap nextResetSnapshot) bool {
	knownAny := false
	for _, w := range []nextResetWindow{snap.Short, snap.Weekly} {
		if !w.Known {
			continue
		}
		knownAny = true
		if nextResetWindowFull(w) {
			return false
		}
	}
	return knownAny
}
