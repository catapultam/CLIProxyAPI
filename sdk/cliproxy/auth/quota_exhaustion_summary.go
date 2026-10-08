package auth

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// QuotaUnavailabilitySummary reports, for client-facing error presentation only,
// whether every usable candidate auth for a provider/model is currently blocked
// exclusively by a quota/rate-limit cooldown. It never participates in auth
// selection or cooldown scheduling and never mutates auth state.
type QuotaUnavailabilitySummary struct {
	// Applicable is true when at least one non-disabled candidate exists for the
	// provider/model, so AllQuotaCooldown reflects a real verdict.
	Applicable bool
	// AllQuotaCooldown is true when every non-disabled candidate is blocked only
	// by a quota/rate-limit cooldown (none disabled, none blocked by a different
	// reason such as a transient 5xx/transport cooldown, and none currently
	// available).
	AllQuotaCooldown bool
	// EarliestReset is the earliest cooldown recovery time among the quota-cooled
	// candidates. Zero when AllQuotaCooldown is false.
	EarliestReset time.Time
	// RepresentativeClaim is the upstream "anthropic-ratelimit-unified-representative-claim"
	// value observed on the candidate with the earliest reset, if the proxy has
	// seen it. Empty when unknown.
	RepresentativeClaim string
	// WindowMinutes is the length, in minutes, of the Codex rate-limit window
	// (e.g. 300 or 10080) responsible for EarliestReset on the candidate with
	// that reset, recovered from that candidate's last observed "X-Codex-*"
	// quota signals. Zero when unknown or not applicable, e.g. for Claude,
	// whose signals never carry Codex window-minutes keys.
	WindowMinutes int
	// AllUsageExhausted is a stricter version of AllQuotaCooldown, for the
	// Codex usage_limit_reached rewrite only: true when every quota-cooled
	// candidate's cooldown is not merely Quota.Exceeded, but one of (a)
	// credential-scoped usage exhaustion (QuotaState.Reason ==
	// "credential_quota", at the auth or the matching per-model level --
	// what a real upstream usage_limit_reached 429 produces), (b) a
	// next-reset latch, or (c) a cooldown reset time matched to a real
	// rate-limit window via signals (WindowMinutes > 0). This excludes a
	// transient quota/rate-limit backoff (e.g. the 1s/2s/4s backoff
	// MarkResult applies for "Model is at capacity") that happens to also
	// set Quota.Exceeded with Reason "quota" but is not actually a
	// credential-wide usage limit. AllQuotaCooldown itself is left
	// unchanged -- and still consumed as-is by the Claude /v1/messages
	// rewrite -- so this field is purely additive.
	AllUsageExhausted bool
}

// SummarizeModelQuotaUnavailability inspects the manager's current auths for
// provider/model and reports whether every non-disabled candidate is blocked
// exclusively by quota/rate-limit cooldown, as opposed to disablement or a
// different kind of cooldown (e.g. a transient upstream 5xx). It is read-only
// reporting used to decide how a route presents an already-failed "no auth
// available" response to a client; it does not change cooldown or selection
// behavior.
func (m *Manager) SummarizeModelQuotaUnavailability(provider, routeModel string, now time.Time) QuotaUnavailabilitySummary {
	if m == nil {
		return QuotaUnavailabilitySummary{}
	}
	targetProvider := strings.ToLower(strings.TrimSpace(provider))
	if targetProvider == "" {
		return QuotaUnavailabilitySummary{}
	}

	var (
		total                 int
		quotaBlocked          int
		usageExhaustedBlocked int
		otherBlocked          int
		earliest              time.Time
		representativeClaim   string
		windowMinutes         int
	)
	for _, candidate := range m.List() {
		if candidate == nil || strings.ToLower(strings.TrimSpace(candidate.Provider)) != targetProvider {
			continue
		}
		if candidate.Disabled || candidate.Status == StatusDisabled {
			continue
		}
		total++

		checkModel := m.selectionModelForAuth(candidate, routeModel)

		if quotaExceeded, nextRecoverAt, claim, minutes := directQuotaCooldown(candidate, checkModel, now); quotaExceeded {
			quotaBlocked++
			if isUsageExhaustedCooldown(candidate, checkModel, minutes, now) {
				usageExhaustedBlocked++
			}
			if earliest.IsZero() || nextRecoverAt.Before(earliest) {
				earliest = nextRecoverAt
				representativeClaim = claim
				windowMinutes = minutes
			}
			continue
		}

		blocked, reason, next := isAuthBlockedForModel(candidate, checkModel, now)
		if !blocked {
			// At least one candidate is actually usable, so the "no auth
			// available" premise does not hold for this provider/model.
			return QuotaUnavailabilitySummary{Applicable: true, AllQuotaCooldown: false}
		}
		if reason == blockReasonCooldown && next.After(now) {
			quotaBlocked++
			minutes, _ := codexWindowMinutesFromSignals(candidate.Quota, next)
			if isUsageExhaustedCooldown(candidate, checkModel, minutes, now) {
				usageExhaustedBlocked++
			}
			if earliest.IsZero() || next.Before(earliest) {
				earliest = next
				representativeClaim = representativeClaimFromSignals(candidate.Quota.Signals)
				windowMinutes = minutes
			}
			continue
		}
		otherBlocked++
	}

	if total == 0 || otherBlocked > 0 {
		return QuotaUnavailabilitySummary{Applicable: total > 0, AllQuotaCooldown: false}
	}
	return QuotaUnavailabilitySummary{
		Applicable:          true,
		AllQuotaCooldown:    quotaBlocked == total,
		AllUsageExhausted:   usageExhaustedBlocked == total,
		EarliestReset:       earliest,
		RepresentativeClaim: representativeClaim,
		WindowMinutes:       windowMinutes,
	}
}

// isUsageExhaustedCooldown reports whether candidate's current quota cooldown
// for checkModel represents genuine credential-scoped usage-limit exhaustion
// -- what the Codex usage_limit_reached rewrite requires -- rather than a
// transient quota/rate-limit backoff that also sets Quota.Exceeded (see
// AllUsageExhausted). windowMinutes is whatever this candidate's cooldown
// already resolved for WindowMinutes, so the signal-matching check is not
// redone here.
func isUsageExhaustedCooldown(candidate *Auth, checkModel string, windowMinutes int, now time.Time) bool {
	if candidate == nil {
		return false
	}
	if candidate.Quota.Exceeded && candidate.Quota.Reason == "credential_quota" && candidate.Quota.NextRecoverAt.After(now) {
		return true
	}
	if modelKey := canonicalModelKey(checkModel); modelKey != "" {
		for stateModel, state := range candidate.ModelStates {
			if state == nil || canonicalModelKey(stateModel) != modelKey {
				continue
			}
			if state.Quota.Exceeded && state.Quota.Reason == "credential_quota" && state.Quota.NextRecoverAt.After(now) {
				return true
			}
			break
		}
	}
	if nextResetIsLatched(candidate.ID) {
		return true
	}
	return windowMinutes > 0
}

// directQuotaCooldown reports an active quota cooldown for auth/model, checking
// quota fields directly and ahead of any other block reason (such as an
// expired access token or an unauthorized failure) so that a genuine
// usage-limit cooldown is never masked by an unrelated, lower-priority
// condition that may also currently be true for the same credential. The
// ordering mirrors the credential-wide gate in isAuthBlockedForModel, but the
// per-model fallback here only consults quota fields rather than the full
// availability check. The fourth return value is the Codex rate-limit window
// length in minutes that produced the reported reset time, when known.
func directQuotaCooldown(auth *Auth, model string, now time.Time) (bool, time.Time, string, int) {
	if auth == nil {
		return false, time.Time{}, "", 0
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
		minutes, _ := codexWindowMinutesFromSignals(auth.Quota, auth.Quota.NextRecoverAt)
		return true, auth.Quota.NextRecoverAt, representativeClaimFromSignals(auth.Quota.Signals), minutes
	}
	modelKey := canonicalModelKey(model)
	if modelKey != "" && len(auth.ModelStates) > 0 {
		for stateModel, state := range auth.ModelStates {
			if state == nil || canonicalModelKey(stateModel) != modelKey {
				continue
			}
			if state.Quota.Exceeded && state.Quota.NextRecoverAt.After(now) {
				minutes, _ := codexWindowMinutesFromSignals(state.Quota, state.Quota.NextRecoverAt)
				return true, state.Quota.NextRecoverAt, representativeClaimFromSignals(state.Quota.Signals), minutes
			}
			return false, time.Time{}, "", 0
		}
		return false, time.Time{}, "", 0
	}
	if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) {
		minutes, _ := codexWindowMinutesFromSignals(auth.Quota, auth.Quota.NextRecoverAt)
		return true, auth.Quota.NextRecoverAt, representativeClaimFromSignals(auth.Quota.Signals), minutes
	}
	return false, time.Time{}, "", 0
}

// representativeClaimFromSignals extracts the upstream Anthropic unified
// rate-limit representative claim from a quota signal snapshot, if present.
func representativeClaimFromSignals(signals map[string]string) string {
	if len(signals) == 0 {
		return ""
	}
	return strings.TrimSpace(signals[http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-Representative-Claim")])
}

// codexWindowMinutesTolerance bounds how far a signal window's resolved reset
// time may drift from the cooldown reset time it is being matched against and
// still be treated as the window responsible for that cooldown. The two
// timestamps come from independent observations (a "codex.rate_limits"
// snapshot vs. the cooldown scheduler's own now-based math), so they are
// expected to agree closely but not necessarily to the second.
const codexWindowMinutesTolerance = time.Minute

// codexWindowMinutesFromSignals recovers the Codex rate-limit window length,
// in minutes, of whichever of the credential's primary/secondary windows last
// observed in q.Signals resolves to a reset time within
// codexWindowMinutesTolerance of resetAt. It reports false when neither
// window matches closely enough, or when q carries no Codex window signals at
// all (e.g. a Claude credential, or a Codex credential with no quota
// observation yet).
func codexWindowMinutesFromSignals(q QuotaState, resetAt time.Time) (int, bool) {
	if len(q.Signals) == 0 || resetAt.IsZero() {
		return 0, false
	}
	for _, window := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + window + "-"
		minutesRaw, hasMinutes := q.Signals[http.CanonicalHeaderKey(prefix+"Window-Minutes")]
		if !hasMinutes {
			continue
		}
		minutes, errParse := strconv.ParseFloat(strings.TrimSpace(minutesRaw), 64)
		if errParse != nil || minutes <= 0 {
			continue
		}
		windowReset, ok := codexWindowResetAt(q, prefix)
		if !ok {
			continue
		}
		drift := windowReset.Sub(resetAt)
		if drift < 0 {
			drift = -drift
		}
		if drift <= codexWindowMinutesTolerance {
			return int(minutes), true
		}
	}
	return 0, false
}

// codexWindowResetAt resolves one Codex rate-limit window's reset time from
// q.Signals, preferring an absolute reset-at timestamp and falling back to a
// reset-after-seconds offset from when the signals were observed.
func codexWindowResetAt(q QuotaState, prefix string) (time.Time, bool) {
	if raw, ok := q.Signals[http.CanonicalHeaderKey(prefix+"Reset-At")]; ok {
		if secs, errParse := strconv.ParseFloat(strings.TrimSpace(raw), 64); errParse == nil && secs > 0 {
			return time.Unix(int64(secs), 0), true
		}
	}
	if raw, ok := q.Signals[http.CanonicalHeaderKey(prefix+"Reset-After-Seconds")]; ok && !q.ObservedAt.IsZero() {
		if secs, errParse := strconv.ParseFloat(strings.TrimSpace(raw), 64); errParse == nil && secs >= 0 {
			return q.ObservedAt.Add(time.Duration(secs * float64(time.Second))), true
		}
	}
	return time.Time{}, false
}
