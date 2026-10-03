package auth

import (
	"net/http"
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
		total               int
		quotaBlocked        int
		otherBlocked        int
		earliest            time.Time
		representativeClaim string
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

		if quotaExceeded, nextRecoverAt, claim := directQuotaCooldown(candidate, checkModel, now); quotaExceeded {
			quotaBlocked++
			if earliest.IsZero() || nextRecoverAt.Before(earliest) {
				earliest = nextRecoverAt
				representativeClaim = claim
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
			if earliest.IsZero() || next.Before(earliest) {
				earliest = next
				representativeClaim = representativeClaimFromSignals(candidate.Quota.Signals)
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
		EarliestReset:       earliest,
		RepresentativeClaim: representativeClaim,
	}
}

// directQuotaCooldown reports an active quota cooldown for auth/model, checking
// quota fields directly and ahead of any other block reason (such as an
// expired access token or an unauthorized failure) so that a genuine
// usage-limit cooldown is never masked by an unrelated, lower-priority
// condition that may also currently be true for the same credential. The
// ordering mirrors the credential-wide gate in isAuthBlockedForModel, but the
// per-model fallback here only consults quota fields rather than the full
// availability check.
func directQuotaCooldown(auth *Auth, model string, now time.Time) (bool, time.Time, string) {
	if auth == nil {
		return false, time.Time{}, ""
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
		return true, auth.Quota.NextRecoverAt, representativeClaimFromSignals(auth.Quota.Signals)
	}
	modelKey := canonicalModelKey(model)
	if modelKey != "" && len(auth.ModelStates) > 0 {
		for stateModel, state := range auth.ModelStates {
			if state == nil || canonicalModelKey(stateModel) != modelKey {
				continue
			}
			if state.Quota.Exceeded && state.Quota.NextRecoverAt.After(now) {
				return true, state.Quota.NextRecoverAt, representativeClaimFromSignals(state.Quota.Signals)
			}
			return false, time.Time{}, ""
		}
		return false, time.Time{}, ""
	}
	if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) {
		return true, auth.Quota.NextRecoverAt, representativeClaimFromSignals(auth.Quota.Signals)
	}
	return false, time.Time{}, ""
}

// representativeClaimFromSignals extracts the upstream Anthropic unified
// rate-limit representative claim from a quota signal snapshot, if present.
func representativeClaimFromSignals(signals map[string]string) string {
	if len(signals) == 0 {
		return ""
	}
	return strings.TrimSpace(signals[http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-Representative-Claim")])
}
