package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeDualDoer is fakeUsageDoer (next_reset_latch_test.go) extended to serve
// two different endpoints from one NextResetHTTPDoer, since fetch now makes
// both a usage and (for Claude) a plan-size profile request per cycle.
type fakeDualDoer struct {
	calls         []string
	profileHeader http.Header

	usageStatus int
	usageBody   string

	profileStatus int
	profileBody   string
}

func (f *fakeDualDoer) do(_ context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	url := req.URL.String()
	f.calls = append(f.calls, auth.ID+" "+url)
	if url == claudeProfileURL {
		f.profileHeader = req.Header.Clone()
		return &http.Response{StatusCode: f.profileStatus, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(f.profileBody))}, nil
	}
	return &http.Response{StatusCode: f.usageStatus, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(f.usageBody))}, nil
}

// newPlanSizeTestPoller is newTestPoller (next_reset_latch_test.go) built
// from a *fakeDualDoer instead of a *fakeUsageDoer, since these tests need
// two different canned responses (usage and profile) from one doer.
func newPlanSizeTestPoller(auths []*Auth, d *fakeDualDoer) *nextResetPoller {
	return &nextResetPoller{
		list: func() []*Auth { return auths }, do: d.do, store: nextResetPolled,
		backoff: map[string]time.Time{}, lastPoll: map[string]time.Time{},
	}
}

func profileCalls(calls []string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, claudeProfileURL) {
			n++
		}
	}
	return n
}

// TestNextResetPollerFetchesClaudePlanSizeOnFirstCycleThenAt24hCadence covers
// the plan-size schedule end to end: fetched on this poller's first cycle
// for the account (planDue starts empty), not fetched again inside 24h, and
// fetched again once 24h has passed. It calls fetch directly (rather than
// runOnce) to control exactly when the account is due, since the usage poll's
// own due() schedule is unrelated to the 24h plan-size cadence being tested
// here.
func TestNextResetPollerFetchesClaudePlanSizeOnFirstCycleThenAt24hCadence(t *testing.T) {
	withNextReset(t)
	withPlanSizeStore(t)
	a := tokenAuth(claudeAuth("plan-a", 10, 100*time.Hour))
	d := &fakeDualDoer{
		usageStatus: 200, usageBody: `{"seven_day":{"utilization":4,"resets_at":"2026-10-09T00:00:00Z"}}`,
		profileStatus: 200, profileBody: claudeProfileFixture(false, "claude_team", "default"),
	}
	p := newPlanSizeTestPoller([]*Auth{a}, d)

	p.fetch(context.Background(), a, nrNow)
	if n := profileCalls(d.calls); n != 1 {
		t.Fatalf("first cycle profile calls = %d, want 1: %v", n, d.calls)
	}
	entry, ok := nextResetPlanSizes.get("plan-a")
	if !ok || !entry.Known || entry.Size != 1 {
		t.Fatalf("plan size not derived: %+v %v", entry, ok)
	}

	p.fetch(context.Background(), a, nrNow.Add(23*time.Hour))
	if n := profileCalls(d.calls); n != 1 {
		t.Fatalf("re-fetched profile within 24h: %v", d.calls)
	}

	p.fetch(context.Background(), a, nrNow.Add(24*time.Hour+time.Minute))
	if n := profileCalls(d.calls); n != 2 {
		t.Fatalf("did not re-fetch profile after 24h: %v", d.calls)
	}
}

// TestNextResetPollerSkipsPlanSizeForCodexAndAPIKeyAccounts covers both
// exclusions: Codex derives its size from plan_type with nothing to fetch
// (fetch's own "only Claude" guard), and an API-key Claude credential never
// reaches fetch at all because runOnce filters it via nextResetTracked, the
// same gate the usage poll itself relies on.
func TestNextResetPollerSkipsPlanSizeForCodexAndAPIKeyAccounts(t *testing.T) {
	withNextReset(t)
	withPlanSizeStore(t)
	codex := tokenAuth(codexAuth("codex-a", 10, 100*time.Hour))
	apiKeyClaude := tokenAuth(claudeAuth("apikey-a", 10, 100*time.Hour))
	apiKeyClaude.Attributes = map[string]string{"auth_kind": "apikey"}
	d := &fakeDualDoer{
		usageStatus: 200, usageBody: `{"rate_limit":{"secondary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_at":1}}}`,
		profileStatus: 200, profileBody: claudeProfileFixture(false, "claude_team", "default"),
	}
	p := newPlanSizeTestPoller([]*Auth{codex, apiKeyClaude}, d)
	p.runOnce(context.Background(), nrNow)
	if n := profileCalls(d.calls); n != 0 {
		t.Fatalf("codex/apikey accounts triggered a profile fetch: %v", d.calls)
	}
	if _, ok := nextResetPlanSizes.get("codex-a"); ok {
		t.Fatal("codex account got a Claude plan-size entry")
	}
	if _, ok := nextResetPlanSizes.get("apikey-a"); ok {
		t.Fatal("API-key account got a Claude plan-size entry")
	}
}

// TestNextResetPollerFailedPlanSizeFetchKeepsLastKnownValue covers the
// "a failed fetch keeps the last known derived size" requirement: a 500 from
// the profile endpoint must not overwrite or clear a previously derived size.
func TestNextResetPollerFailedPlanSizeFetchKeepsLastKnownValue(t *testing.T) {
	withNextReset(t)
	withPlanSizeStore(t)
	a := tokenAuth(claudeAuth("plan-fail", 10, 100*time.Hour))
	nextResetPlanSizes.set("plan-fail", nextResetPlanSizeEntry{Size: 5, Known: true})
	d := &fakeDualDoer{
		usageStatus: 200, usageBody: `{"seven_day":{"utilization":4,"resets_at":"2026-10-09T00:00:00Z"}}`,
		profileStatus: 500,
	}
	p := newPlanSizeTestPoller([]*Auth{a}, d)
	p.fetch(context.Background(), a, nrNow)
	if n := profileCalls(d.calls); n != 1 {
		t.Fatalf("profile fetch did not happen: %v", d.calls)
	}
	entry, ok := nextResetPlanSizes.get("plan-fail")
	if !ok || !entry.Known || entry.Size != 5 {
		t.Fatalf("failed fetch changed the stored size: %+v %v", entry, ok)
	}
}

// TestNextResetPollerPlanSizeRequestUsesClaudeHeaders covers "the same
// Claude headers the usage poll uses": Bearer token, anthropic-beta, and the
// claude-cli User-Agent, on the profile request specifically.
func TestNextResetPollerPlanSizeRequestUsesClaudeHeaders(t *testing.T) {
	withNextReset(t)
	withPlanSizeStore(t)
	a := tokenAuth(claudeAuth("plan-headers", 10, 100*time.Hour))
	d := &fakeDualDoer{
		usageStatus: 200, usageBody: `{"seven_day":{"utilization":4,"resets_at":"2026-10-09T00:00:00Z"}}`,
		profileStatus: 200, profileBody: claudeProfileFixture(false, "claude_team", "default"),
	}
	p := newPlanSizeTestPoller([]*Auth{a}, d)
	p.fetch(context.Background(), a, nrNow)
	if got := d.profileHeader.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization header = %q", got)
	}
	if got := d.profileHeader.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Fatalf("anthropic-beta header = %q", got)
	}
	if got := d.profileHeader.Get("User-Agent"); got != claudeUsageUserAgent {
		t.Fatalf("User-Agent header = %q", got)
	}
}
