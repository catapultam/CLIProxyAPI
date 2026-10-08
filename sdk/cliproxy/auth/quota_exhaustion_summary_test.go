package auth

import (
	"strconv"
	"testing"
	"time"
)

func newQuotaSummaryTestManager(auths ...*Auth) *Manager {
	m := NewManager(nil, nil, nil)
	for _, a := range auths {
		if a == nil || a.ID == "" {
			continue
		}
		m.auths[a.ID] = a
	}
	return m
}

func TestSummarizeModelQuotaUnavailability_AllQuotaCooldown(t *testing.T) {
	now := time.Now()
	authA := &Auth{ID: "a", Provider: "claude", Quota: QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(2 * time.Hour),
	}}
	authB := &Auth{ID: "b", Provider: "claude", Quota: QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour),
		Signals: map[string]string{"Anthropic-Ratelimit-Unified-Representative-Claim": "seven_day"},
	}}
	m := newQuotaSummaryTestManager(authA, authB)

	summary := m.SummarizeModelQuotaUnavailability("claude", "claude-opus-5-5", now)
	if !summary.Applicable || !summary.AllQuotaCooldown {
		t.Fatalf("summary = %+v, want Applicable and AllQuotaCooldown", summary)
	}
	if !summary.EarliestReset.Equal(authB.Quota.NextRecoverAt) {
		t.Fatalf("EarliestReset = %v, want %v (the earlier of the two)", summary.EarliestReset, authB.Quota.NextRecoverAt)
	}
	if summary.RepresentativeClaim != "seven_day" {
		t.Fatalf("RepresentativeClaim = %q, want seven_day", summary.RepresentativeClaim)
	}
}

func TestSummarizeModelQuotaUnavailability_MixedReasonIsNotAllQuota(t *testing.T) {
	now := time.Now()
	quotaAuth := &Auth{ID: "a", Provider: "claude", Quota: QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour),
	}}
	// Simulates a 5xx/transport-driven cooldown: Unavailable with a retry timer,
	// but no quota signal. This must not count as a usage-limit cooldown.
	transientAuth := &Auth{ID: "b", Provider: "claude", Unavailable: true, NextRetryAfter: now.Add(30 * time.Second)}
	m := newQuotaSummaryTestManager(quotaAuth, transientAuth)

	summary := m.SummarizeModelQuotaUnavailability("claude", "claude-opus-5-5", now)
	if summary.AllQuotaCooldown {
		t.Fatalf("summary = %+v, want AllQuotaCooldown=false for a mixed set", summary)
	}
}

func TestSummarizeModelQuotaUnavailability_DisabledAuthExcluded(t *testing.T) {
	now := time.Now()
	quotaAuth := &Auth{ID: "a", Provider: "claude", Quota: QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour),
	}}
	disabledAuth := &Auth{ID: "b", Provider: "claude", Disabled: true}
	m := newQuotaSummaryTestManager(quotaAuth, disabledAuth)

	summary := m.SummarizeModelQuotaUnavailability("claude", "claude-opus-5-5", now)
	if !summary.AllQuotaCooldown {
		t.Fatalf("summary = %+v, want disabled auth excluded from the candidate set", summary)
	}
}

// A quota cooldown must not be masked by an unrelated, lower-priority block
// reason (such as an expired access token) that can also be true for the same
// credential at the same time.
func TestSummarizeModelQuotaUnavailability_ExpiredTokenDoesNotMaskQuota(t *testing.T) {
	now := time.Now()
	quotaAuth := &Auth{
		ID:       "a",
		Provider: "claude",
		Metadata: map[string]any{
			"access_token": "expired-token",
			"expire":       now.Add(-time.Hour).Format(time.RFC3339),
		},
		Quota: QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(3 * time.Hour)},
	}
	m := newQuotaSummaryTestManager(quotaAuth)

	summary := m.SummarizeModelQuotaUnavailability("claude", "claude-opus-5-5", now)
	if !summary.AllQuotaCooldown {
		t.Fatalf("summary = %+v, want the quota cooldown to take priority over the expired token", summary)
	}
	if !summary.EarliestReset.Equal(quotaAuth.Quota.NextRecoverAt) {
		t.Fatalf("EarliestReset = %v, want %v", summary.EarliestReset, quotaAuth.Quota.NextRecoverAt)
	}
}

func TestSummarizeModelQuotaUnavailability_AvailableAuthReturnsFalse(t *testing.T) {
	now := time.Now()
	availableAuth := &Auth{ID: "a", Provider: "claude"}
	m := newQuotaSummaryTestManager(availableAuth)

	summary := m.SummarizeModelQuotaUnavailability("claude", "claude-opus-5-5", now)
	if !summary.Applicable || summary.AllQuotaCooldown {
		t.Fatalf("summary = %+v, want Applicable=true AllQuotaCooldown=false when a candidate is usable", summary)
	}
}

func TestSummarizeModelQuotaUnavailability_PerModelQuota(t *testing.T) {
	now := time.Now()
	quotaAuth := &Auth{
		ID:       "a",
		Provider: "claude",
		ModelStates: map[string]*ModelState{
			"claude-opus-5-5": {Quota: QuotaState{Exceeded: true, NextRecoverAt: now.Add(45 * time.Minute)}},
		},
	}
	m := newQuotaSummaryTestManager(quotaAuth)

	summary := m.SummarizeModelQuotaUnavailability("claude", "claude-opus-5-5", now)
	if !summary.AllQuotaCooldown {
		t.Fatalf("summary = %+v, want per-model quota cooldown detected", summary)
	}
}

func TestSummarizeModelQuotaUnavailability_NoCandidatesNotApplicable(t *testing.T) {
	m := NewManager(nil, nil, nil)
	summary := m.SummarizeModelQuotaUnavailability("claude", "claude-opus-5-5", time.Now())
	if summary.Applicable {
		t.Fatalf("summary = %+v, want Applicable=false with no candidates", summary)
	}
}

func TestSummarizeModelQuotaUnavailability_CodexWindowMinutesFromPrimarySignal(t *testing.T) {
	now := time.Now()
	resetAt := now.Add(90 * time.Minute)
	codexAuth := &Auth{ID: "a", Provider: "codex", Quota: QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: resetAt,
		ObservedAt: now,
		Signals: map[string]string{
			"X-Codex-Primary-Window-Minutes": "300",
			"X-Codex-Primary-Reset-At":       strconv.FormatInt(resetAt.Unix(), 10),
		},
	}}
	m := newQuotaSummaryTestManager(codexAuth)

	summary := m.SummarizeModelQuotaUnavailability("codex", "gpt-5-codex", now)
	if !summary.Applicable || !summary.AllQuotaCooldown {
		t.Fatalf("summary = %+v, want Applicable and AllQuotaCooldown", summary)
	}
	if summary.WindowMinutes != 300 {
		t.Fatalf("WindowMinutes = %d, want 300 (matched via Primary-Reset-At)", summary.WindowMinutes)
	}
}

func TestSummarizeModelQuotaUnavailability_CodexWindowMinutesFromSecondaryResetAfterSeconds(t *testing.T) {
	now := time.Now()
	resetAt := now.Add(6 * 24 * time.Hour)
	codexAuth := &Auth{ID: "a", Provider: "codex", Quota: QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: resetAt,
		ObservedAt: now,
		Signals: map[string]string{
			"X-Codex-Primary-Window-Minutes":        "300",
			"X-Codex-Primary-Reset-After-Seconds":   "60",
			"X-Codex-Secondary-Window-Minutes":      "10080",
			"X-Codex-Secondary-Reset-After-Seconds": strconv.FormatInt(int64(resetAt.Sub(now).Seconds()), 10),
		},
	}}
	m := newQuotaSummaryTestManager(codexAuth)

	summary := m.SummarizeModelQuotaUnavailability("codex", "gpt-5-codex", now)
	if !summary.AllQuotaCooldown {
		t.Fatalf("summary = %+v, want AllQuotaCooldown", summary)
	}
	if summary.WindowMinutes != 10080 {
		t.Fatalf("WindowMinutes = %d, want 10080 (matched via Secondary-Reset-After-Seconds)", summary.WindowMinutes)
	}
}

func TestSummarizeModelQuotaUnavailability_CodexWindowMinutesUnknownWhenSignalsDoNotMatch(t *testing.T) {
	now := time.Now()
	codexAuth := &Auth{ID: "a", Provider: "codex", Quota: QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(2 * time.Hour),
		ObservedAt: now,
		Signals: map[string]string{
			// Stale signal: window-minutes is known but its own reset time is far
			// from NextRecoverAt, so it must not be attributed to this cooldown.
			"X-Codex-Primary-Window-Minutes": "300",
			"X-Codex-Primary-Reset-At":       strconv.FormatInt(now.Add(5*time.Minute).Unix(), 10),
		},
	}}
	m := newQuotaSummaryTestManager(codexAuth)

	summary := m.SummarizeModelQuotaUnavailability("codex", "gpt-5-codex", now)
	if !summary.AllQuotaCooldown {
		t.Fatalf("summary = %+v, want AllQuotaCooldown", summary)
	}
	if summary.WindowMinutes != 0 {
		t.Fatalf("WindowMinutes = %d, want 0 (no signal window matches the cooldown reset)", summary.WindowMinutes)
	}
}
