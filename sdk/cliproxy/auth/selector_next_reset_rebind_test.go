package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const rebindModel = "claude-sonnet-5-5"

// rebindFixture wires a next-reset fallback with its own move tracker behind
// session affinity, all driven by one controllable clock.
type rebindFixture struct {
	t        *testing.T
	now      time.Time
	tracker  *nextResetRebindTracker
	nr       *NextResetSelector
	affinity *SessionAffinitySelector
	session  string
	canon    string
}

func newRebindFixture(t *testing.T) *rebindFixture {
	t.Helper()
	f := &rebindFixture{t: t, now: nrNow, session: "sess-rebind"}
	clock := func() time.Time { return f.now }
	f.tracker = newNextResetRebindTracker(clock)
	f.nr = &NextResetSelector{polled: newNextResetPolledStore(), now: clock, rebind: f.tracker}
	f.affinity = NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: f.nr, TTL: 24 * time.Hour})
	t.Cleanup(f.affinity.Stop)
	return f
}

func (f *rebindFixture) opts(body string, headers ...string) cliproxyexecutor.Options {
	h := http.Header{"X-Claude-Code-Session-Id": []string{f.session}}
	for i := 0; i+1 < len(headers); i += 2 {
		h.Set(headers[i], headers[i+1])
	}
	if body == "" {
		body = `{"model":"` + rebindModel + `","messages":[{"role":"user","content":"hi"}]}`
	}
	return cliproxyexecutor.Options{Headers: h, OriginalRequest: []byte(body), Metadata: make(map[string]any)}
}

func (f *rebindFixture) pick(opts cliproxyexecutor.Options, auths ...*Auth) *Auth {
	f.t.Helper()
	got, err := f.affinity.Pick(context.Background(), "claude", rebindModel, opts, auths)
	if err != nil {
		f.t.Fatalf("Pick: %v", err)
	}
	if canon, ok := opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string); ok && canon != "" {
		f.canon = canon
	}
	return got
}

// bind binds the session to b by making b the only candidate. The binding
// request happens 90 minutes ago, before any request the tests observe, so
// its pick-time activity never decides whether the cache is warm.
func (f *rebindFixture) bind(b *Auth) {
	f.t.Helper()
	now := f.now
	f.now = now.Add(-90 * time.Minute)
	defer func() { f.now = now }()
	if got := f.pick(f.opts(""), b); got.ID != b.ID {
		f.t.Fatalf("bind got %s, want %s", got.ID, b.ID)
	}
}

// observe records a response for the bound session that happened ago before now.
func (f *rebindFixture) observe(authID string, contextTokens int64, ago time.Duration) {
	f.t.Helper()
	f.tracker.observeUsage(nextResetUsageObservation{
		sessionID: f.canon,
		model:     rebindModel,
		authID:    authID,
		input:     contextTokens,
		output:    10,
		at:        f.now.Add(-ago),
	})
}

func (f *rebindFixture) setTokensPerPct(authID string, v float64) {
	f.tracker.mu.Lock()
	defer f.tracker.mu.Unlock()
	f.tracker.creds[authID] = &nextResetTokenLearner{estimate: v, samples: nextResetLearnerMinSamples, seen: f.now}
}

func approxEqual(a, b float64) bool { return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b)) }

func TestNextResetRebindNoMoveWhenTargetResetsLater(t *testing.T) {
	f := newRebindFixture(t)
	// b resets first but is near full, so next-reset's pick (a) resets later.
	b := claudeAuth("b", 99, 10*time.Hour)
	a := claudeAuth("a", 20, 100*time.Hour)
	f.bind(b)
	f.observe("b", 1000, time.Hour) // cold cache
	if got := f.pick(f.opts(""), a, b); got.ID != "b" {
		t.Fatalf("moved to %s toward a later reset", got.ID)
	}
}

func TestNextResetRebindNoMoveWhenTargetNearFullWeekly(t *testing.T) {
	f := newRebindFixture(t)
	b := claudeAuth("b", 20, 100*time.Hour)
	a := claudeAuth("a", 98, 10*time.Hour)
	f.bind(b)
	f.observe("b", 1000, time.Hour)
	if got := f.pick(f.opts(""), a, b); got.ID != "b" {
		t.Fatalf("moved to weekly near-full %s", got.ID)
	}
}

func TestNextResetRebindNoMoveWhenTargetShortWindowNearFull(t *testing.T) {
	f := newRebindFixture(t)
	b := claudeAuth("b", 20, 100*time.Hour)
	a := claudeAuthWithShortUsed("a", 20, 10*time.Hour, 90)
	f.bind(b)
	f.observe("b", 1000, time.Hour)
	if got := f.pick(f.opts(""), a, b); got.ID != "b" {
		t.Fatalf("moved to %s whose open 5h window is near full", got.ID)
	}
	// The same 90% on a 5h window that has already closed does not block.
	// The request above counts as activity, so let the cache go cold again.
	f.now = f.now.Add(6 * time.Minute)
	a2 := claudeAuthWithShortWindow("a", 20, 10*time.Hour, 90, nrNow.Add(-time.Minute))
	if got := f.pick(f.opts(""), a2, b); got.ID != "a" {
		t.Fatalf("closed 5h window blocked the move: got %s", got.ID)
	}
}

func TestNextResetRebindNoMoveWithinHourOfLastMove(t *testing.T) {
	f := newRebindFixture(t)
	b := claudeAuth("b", 20, 100*time.Hour)
	a := claudeAuth("a", 20, 50*time.Hour)
	c := claudeAuth("c", 20, 10*time.Hour)
	f.bind(b)
	f.observe("b", 1000, time.Hour)
	if got := f.pick(f.opts(""), a, b); got.ID != "a" {
		t.Fatalf("first move got %s, want a", got.ID)
	}
	f.now = f.now.Add(59 * time.Minute)
	if got := f.pick(f.opts(""), a, b, c); got.ID != "a" {
		t.Fatalf("moved again within the hour: got %s", got.ID)
	}
	// The 59-minute request keeps the cache warm, so make the warm move to c
	// affordable: 1150 tokens cost 1.15% of c's quota, under its allowance.
	f.setTokensPerPct("c", 1000)
	f.now = f.now.Add(2 * time.Minute)
	if got := f.pick(f.opts(""), a, b, c); got.ID != "c" {
		t.Fatalf("after the hour got %s, want c", got.ID)
	}
}

func TestNextResetRebindNoMoveWithThread(t *testing.T) {
	f := newRebindFixture(t)
	b := claudeAuth("b", 20, 100*time.Hour)
	a := claudeAuth("a", 20, 10*time.Hour)
	f.bind(b)
	f.observe("b", 1000, time.Hour)
	body := `{"model":"m","thread":{"type":"continue","previous_message_id":"msg_01"},"messages":[{"role":"user","content":"hi"}]}`
	if got := f.pick(f.opts(body), a, b); got.ID != "b" {
		t.Fatalf("thread continuation moved to %s", got.ID)
	}
}

func TestNextResetRebindNoMoveWithAdvisorResults(t *testing.T) {
	bodies := map[string]string{
		"advisor_tool_result": `{"messages":[{"role":"assistant","content":[{"type":"advisor_tool_result","tool_use_id":"x","content":{}}]}]}`,
		"redacted top-level":  `{"messages":[{"role":"assistant","content":[{"type":"advisor_redacted_result","data":"x"}]}]}`,
		"redacted nested":     `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":[{"type":"advisor_redacted_result","data":"x"}]}]}]}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			f := newRebindFixture(t)
			b := claudeAuth("b", 20, 100*time.Hour)
			a := claudeAuth("a", 20, 10*time.Hour)
			f.bind(b)
			f.observe("b", 1000, time.Hour)
			if got := f.pick(f.opts(body), a, b); got.ID != "b" {
				t.Fatalf("advisor conversation moved to %s", got.ID)
			}
		})
	}
}

func TestNextResetRebindColdCacheFromIdleMovesRegardlessOfCost(t *testing.T) {
	ttl1h := `{"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name string
		body string
		idle time.Duration
		want string
	}{
		{"5m ttl idle 5m is still warm", "", 5 * time.Minute, "b"},
		{"5m ttl idle past 5m is cold", "", 5*time.Minute + time.Second, "a"},
		{"1h ttl idle 30m is warm", ttl1h, 30 * time.Minute, "b"},
		{"1h ttl idle 1h is still warm", ttl1h, time.Hour, "b"},
		{"1h ttl idle past 1h is cold", ttl1h, time.Hour + time.Second, "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRebindFixture(t)
			b := claudeAuth("b", 20, 100*time.Hour)
			a := claudeAuth("a", 20, 10*time.Hour)
			f.bind(b)
			// Huge context and no tokens_per_pct for a: a warm move is never allowed.
			f.observe("b", 900_000, tc.idle)
			if got := f.pick(f.opts(tc.body), a, b); got.ID != tc.want {
				t.Fatalf("got %s, want %s", got.ID, tc.want)
			}
		})
	}
}

func TestNextResetRebindCompactionIsColdRegardlessOfCost(t *testing.T) {
	f := newRebindFixture(t)
	b := claudeAuth("b", 20, 100*time.Hour)
	a := claudeAuth("a", 20, 10*time.Hour)
	f.bind(b)
	f.observe("b", 900_000, time.Minute) // warm by idle time
	if got := f.pick(f.opts(""), a, b); got.ID != "b" {
		t.Fatalf("warm cache without an estimate moved to %s", got.ID)
	}
	if got := f.pick(f.opts("", "X-Claude-Code-Context-Compacted", "true"), a, b); got.ID != "a" {
		t.Fatalf("first request after compaction got %s, want a", got.ID)
	}
}

func TestNextResetRebindCompactionHeaderOnlyCountsOnFirstRequest(t *testing.T) {
	tr := newNextResetRebindTracker(func() time.Time { return nrNow })
	key := nextResetRebindKey("s", rebindModel)
	note := func(flag bool) bool { first, _ := tr.noteRequest(key, flag, nrNow); return first }
	if note(false) {
		t.Fatal("unflagged request reported as first after compaction")
	}
	if !note(true) {
		t.Fatal("first flagged request not reported")
	}
	if note(true) {
		t.Fatal("sticky flag reported twice")
	}
	note(false)
	if !note(true) {
		t.Fatal("a later compaction was not reported")
	}
}

func TestNextResetRebindCompactionMetadataIsCold(t *testing.T) {
	if !nextResetMetadataCompacted(map[string]any{cliproxyexecutor.IsCompactionMetadataKey: true}) {
		t.Fatal("IsCompactionMetadataKey not honored")
	}
	if nextResetMetadataCompacted(nil) {
		t.Fatal("nil metadata treated as compacted")
	}
	if nextResetHeaderCompacted(http.Header{"X-Claude-Code-Context-Compacted": []string{"false"}}) {
		t.Fatal("header value false treated as compacted")
	}
	if !nextResetHeaderCompacted(http.Header{"X-Claude-Code-Context-Compacted": []string{"true"}}) {
		t.Fatal("header value true not treated as compacted")
	}
}

// warmFixture binds to b with a warm 5m cache of ctxTokens and returns a with
// weekly 50% used resetting in resetIn.
func warmFixture(t *testing.T, ctxTokens int64, resetIn time.Duration) (*rebindFixture, *Auth, *Auth) {
	f := newRebindFixture(t)
	b := claudeAuth("b", 20, 120*time.Hour)
	a := claudeAuth("a", 50, resetIn)
	f.bind(b)
	f.observe("b", ctxTokens, time.Minute)
	return f, a, b
}

func TestNextResetRebindWarmCostEqualToAllowanceMoves(t *testing.T) {
	const ctxTokens = 100_000
	// a: 50% left, 50h out => allowance exactly 1%.
	f, a, b := warmFixture(t, ctxTokens, 50*time.Hour)
	f.setTokensPerPct("a", nextResetMoveCostTokens(ctxTokens, false)) // cost exactly 1%
	if got := f.pick(f.opts(""), a, b); got.ID != "a" {
		t.Fatalf("cost == allowance got %s, want a", got.ID)
	}
}

func TestNextResetRebindWarmCostAboveAllowanceStays(t *testing.T) {
	const ctxTokens = 100_000
	f, a, b := warmFixture(t, ctxTokens, 50*time.Hour)
	f.setTokensPerPct("a", nextResetMoveCostTokens(ctxTokens, false)*0.999)
	if got := f.pick(f.opts(""), a, b); got.ID != "b" {
		t.Fatalf("cost > allowance moved to %s", got.ID)
	}
}

func TestNextResetRebindAllowanceGrowsAsResetNears(t *testing.T) {
	a := nextResetAssessment{weeklyUsedPct: 50}
	for _, tc := range []struct {
		in   time.Duration
		want float64
	}{
		{48 * time.Hour, 50.0 / 48},
		{3 * time.Hour, 50.0 / 3},
		{30 * time.Minute, 50},
		{0, 50},
	} {
		a.weeklyResetsAt = nrNow.Add(tc.in)
		if got := nextResetMoveAllowance(a, nrNow); !approxEqual(got, tc.want) {
			t.Fatalf("allowance %v out = %v, want %v", tc.in, got, tc.want)
		}
	}

	// Same 5% warm cost: too expensive 48h out, affordable 3h out and in the last hour.
	const ctxTokens = 100_000
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{48 * time.Hour, "b"},
		{3 * time.Hour, "a"},
		{30 * time.Minute, "a"},
	} {
		f, a, b := warmFixture(t, ctxTokens, tc.in)
		f.setTokensPerPct("a", nextResetMoveCostTokens(ctxTokens, false)/5)
		if got := f.pick(f.opts(""), a, b); got.ID != tc.want {
			t.Fatalf("%v out: got %s, want %s", tc.in, got.ID, tc.want)
		}
	}
}

func TestNextResetRebindUnknownTokensPerPctBlocksWarmAllowsCold(t *testing.T) {
	f, a, b := warmFixture(t, 10, 2*time.Hour) // tiny context, generous allowance
	if got := f.pick(f.opts(""), a, b); got.ID != "b" {
		t.Fatalf("warm move without an estimate went to %s", got.ID)
	}
	f.now = f.now.Add(10 * time.Minute) // now cold
	if got := f.pick(f.opts(""), a, b); got.ID != "a" {
		t.Fatalf("cold move without an estimate got %s, want a", got.ID)
	}
}

func TestNextResetRebindNoMoveWithoutObservedResponse(t *testing.T) {
	f := newRebindFixture(t)
	b := claudeAuth("b", 20, 100*time.Hour)
	a := claudeAuth("a", 20, 10*time.Hour)
	f.bind(b)
	if got := f.pick(f.opts(""), a, b); got.ID != "b" {
		t.Fatalf("session with unknown context moved to %s", got.ID)
	}
}

func TestNextResetRebindStaysOnTargetByAffinityAfterMove(t *testing.T) {
	f := newRebindFixture(t)
	b := claudeAuth("b", 20, 100*time.Hour)
	a := claudeAuth("a", 20, 10*time.Hour)
	f.bind(b)
	f.observe("b", 1000, time.Hour)
	if got := f.pick(f.opts(""), a, b); got.ID != "a" {
		t.Fatalf("move got %s, want a", got.ID)
	}
	for i := 0; i < 3; i++ {
		f.now = f.now.Add(2 * time.Hour)
		if got := f.pick(f.opts(""), a, b); got.ID != "a" {
			t.Fatalf("request %d after move got %s, want a", i, got.ID)
		}
	}
}

func TestNextResetRebindMoveLogCarriesHashesOnly(t *testing.T) {
	hook := setupNextResetLogHook(t)
	f := newRebindFixture(t)
	const idB = "claude-11111111-bob@example.com.json"
	const idA = "claude-22222222-alice@example.com.json"
	b := claudeAuth(idB, 20, 100*time.Hour)
	b.Label, b.FileName = "bob@example.com", idB
	a := claudeAuth(idA, 20, 10*time.Hour)
	a.Label, a.FileName = "alice@example.com", idA
	f.bind(b)
	f.observe(idB, 1000, time.Hour)
	if got := f.pick(f.opts(""), a, b); got.ID != idA {
		t.Fatalf("move got %s", got.ID)
	}
	var line string
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "next-reset: moved session") {
			line = e.Message
		}
	}
	if line == "" {
		t.Fatal("no move log line")
	}
	for _, leak := range []string{"@", "example.com", "claude-11111111", "claude-22222222", "alice", "bob"} {
		if strings.Contains(line, leak) {
			t.Fatalf("log line leaked %q: %q", leak, line)
		}
	}
	hash := func(id string) string { s := sha256.Sum256([]byte(id)); return hex.EncodeToString(s[:6]) }
	for _, want := range []string{
		"from=" + hash(idB), "to=" + hash(idA), "cold=true",
		"a_reset=" + nrNow.Add(10*time.Hour).Local().Format(time.RFC3339),
		"b_reset=" + nrNow.Add(100*time.Hour).Local().Format(time.RFC3339),
		"cost=0.000%", "allowance=8.000%",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line missing %q: %q", want, line)
		}
	}
}

func TestNextResetRebindDisabledWithoutTracker(t *testing.T) {
	nr := &NextResetSelector{polled: newNextResetPolledStore(), now: func() time.Time { return nrNow }}
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: nr, TTL: time.Hour})
	defer affinity.Stop()
	b := claudeAuth("b", 20, 100*time.Hour)
	a := claudeAuth("a", 20, 10*time.Hour)
	opts := func() cliproxyexecutor.Options {
		return cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"s"}}, Metadata: map[string]any{}}
	}
	if got, _ := affinity.Pick(context.Background(), "claude", rebindModel, opts(), []*Auth{b}); got.ID != "b" {
		t.Fatalf("bind got %s", got.ID)
	}
	if got, _ := affinity.Pick(context.Background(), "claude", rebindModel, opts(), []*Auth{a, b}); got.ID != "b" {
		t.Fatalf("moved without a tracker: %s", got.ID)
	}
}

func TestNextResetLearnerValidAfterThreeSamplesAndFollowsEWMA(t *testing.T) {
	tr := newNextResetRebindTracker(func() time.Time { return nrNow })
	obs := func(input int64, util float64) {
		tr.observeUsage(nextResetUsageObservation{authID: "a", input: input, util7d: util, haveUtil7d: true, at: nrNow})
	}
	obs(999, 0.10) // baseline: tokens before the first reading are discarded
	if _, ok := tr.tokensPerPct("a"); ok {
		t.Fatal("estimate valid with no samples")
	}
	obs(400, 0.10)
	obs(600, 0.11)  // sample 1000
	obs(2000, 0.12) // sample 2000
	if _, ok := tr.tokensPerPct("a"); ok {
		t.Fatal("estimate valid after 2 samples")
	}
	obs(1000, 0.13) // sample 1000
	got, ok := tr.tokensPerPct("a")
	if !ok {
		t.Fatal("estimate not valid after 3 samples")
	}
	// EWMA alpha 0.3: 1000 -> 1300 -> 1210.
	if !approxEqual(got, 1210) {
		t.Fatalf("estimate = %v, want 1210", got)
	}
}

func TestNextResetLearnerDecreaseResetsAccumulatorKeepsEstimate(t *testing.T) {
	tr := newNextResetRebindTracker(func() time.Time { return nrNow })
	obs := func(input int64, util float64) {
		tr.observeUsage(nextResetUsageObservation{authID: "a", input: input, util7d: util, haveUtil7d: true, at: nrNow})
	}
	obs(0, 0.50)
	obs(1000, 0.51)
	obs(1000, 0.52)
	obs(1000, 0.53)
	before, ok := tr.tokensPerPct("a")
	if !ok || !approxEqual(before, 1000) {
		t.Fatalf("estimate = %v, %v", before, ok)
	}
	obs(5000, 0.53) // accumulated, no change yet
	obs(0, 0.01)    // weekly reset: accumulator cleared, estimate kept
	if got, ok := tr.tokensPerPct("a"); !ok || !approxEqual(got, before) {
		t.Fatalf("estimate after reset = %v, %v", got, ok)
	}
	obs(1000, 0.02) // sample is 1000, not 6000
	if got, _ := tr.tokensPerPct("a"); !approxEqual(got, 1000) {
		t.Fatalf("estimate = %v, want 1000 (accumulator not reset)", got)
	}
}

func TestNextResetWeightedTokens(t *testing.T) {
	split := nextResetUsageObservation{input: 100, cacheCreation: 300, cacheCreation5m: 100, cacheCreation1h: 200, cacheRead: 1000, output: 10}
	if got := split.weightedTokens(); !approxEqual(got, 100+125+400+100+50) {
		t.Fatalf("split weighted = %v", got)
	}
	unsplit := nextResetUsageObservation{input: 100, cacheCreation: 300, cacheRead: 1000, output: 10}
	if got := unsplit.weightedTokens(); !approxEqual(got, 100+375+100+50) {
		t.Fatalf("unsplit weighted = %v", got)
	}
}

func TestNextResetRebindHandleUsageRecord(t *testing.T) {
	tr := newNextResetRebindTracker(func() time.Time { return nrNow })
	rec := coreusage.Record{
		Provider:    "claude",
		Model:       "claude-sonnet-5-5-20260101",
		Alias:       rebindModel + "(high)",
		SessionID:   "claude:sess",
		AuthID:      "a",
		RequestedAt: nrNow.Add(-time.Minute),
		Detail:      coreusage.Detail{InputTokens: 10, CacheReadTokens: 1000, CacheCreationTokens: 200, OutputTokens: 5},
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization": []string{"0.25"},
		},
	}
	// Without an affinity model on the context, the alias is the key.
	tr.HandleUsage(context.Background(), rec)
	st, ok := tr.session(nextResetRebindKey("claude:sess", rebindModel))
	if !ok || !st.hasUsage || st.contextTokens != 1210 || !st.usageAt.Equal(nrNow.Add(-time.Minute)) || !st.lastActivity.Equal(nrNow.Add(-time.Minute)) {
		t.Fatalf("alias-keyed session = %+v, %v", st, ok)
	}
	if _, ok := tr.session(nextResetRebindKey("claude:sess", "claude-sonnet-5-5-20260101")); ok {
		t.Fatal("usage also written under the upstream model key")
	}
	tr.mu.Lock()
	l := tr.creds["a"]
	tr.mu.Unlock()
	if l == nil || !l.haveUtil || l.lastUtil != 0.25 {
		t.Fatalf("learner = %+v", l)
	}
	// A failed record without cache usage is ignored; with cache usage it is
	// session activity only, never a context size.
	tr.HandleUsage(context.Background(), coreusage.Record{SessionID: "claude:other", Model: rebindModel, AuthID: "a", Failed: true, Detail: coreusage.Detail{InputTokens: 5}})
	if _, ok := tr.session(nextResetRebindKey("claude:other", rebindModel)); ok {
		t.Fatal("failed record without cache usage tracked")
	}
	tr.HandleUsage(context.Background(), coreusage.Record{SessionID: "claude:other", Model: rebindModel, AuthID: "a", Failed: true,
		RequestedAt: nrNow, Detail: coreusage.Detail{CacheReadTokens: 5}})
	st, ok = tr.session(nextResetRebindKey("claude:other", rebindModel))
	if !ok || st.hasUsage || !st.lastActivity.Equal(nrNow) {
		t.Fatalf("failed record with cache usage = %+v, %v", st, ok)
	}
}

func TestNextResetRebindUsageOnlyUnderAffinityModelKey(t *testing.T) {
	tr := newNextResetRebindTracker(func() time.Time { return nrNow })
	main := withNextResetAffinityModel(context.Background(), rebindModel)
	tr.HandleUsage(main, coreusage.Record{
		SessionID: "claude:sess", AuthID: "a", RequestedAt: nrNow,
		Model: "claude-sonnet-5-5-20260101", Alias: "my-sonnet-alias",
		Detail: coreusage.Detail{InputTokens: 50_000},
	})
	// A helper-model request under the same session ID stays isolated.
	helper := withNextResetAffinityModel(context.Background(), "claude-haiku-5-5")
	tr.HandleUsage(helper, coreusage.Record{
		SessionID: "claude:sess", AuthID: "a", RequestedAt: nrNow.Add(time.Second),
		Model: "claude-haiku-5-5", Alias: rebindModel,
		Detail: coreusage.Detail{InputTokens: 300},
	})
	st, ok := tr.session(nextResetRebindKey("claude:sess", rebindModel))
	if !ok || st.contextTokens != 50_000 {
		t.Fatalf("affinity-model session = %+v, %v (helper request leaked in)", st, ok)
	}
	if st, ok := tr.session(nextResetRebindKey("claude:sess", "claude-haiku-5-5")); !ok || st.contextTokens != 300 {
		t.Fatalf("helper session = %+v, %v", st, ok)
	}
	for _, other := range []string{"claude-sonnet-5-5-20260101", "my-sonnet-alias"} {
		if _, ok := tr.session(nextResetRebindKey("claude:sess", other)); ok {
			t.Fatalf("usage written under non-affinity key %q", other)
		}
	}
}

func TestContextWithRequestedModelAliasCarriesAffinityModel(t *testing.T) {
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.SessionAffinityModelMetadataKey: rebindModel}}
	if got := nextResetAffinityModelFrom(contextWithRequestedModelAlias(context.Background(), opts, "route-model")); got != rebindModel {
		t.Fatalf("affinity model = %q, want %q", got, rebindModel)
	}
	if got := nextResetAffinityModelFrom(contextWithRequestedModelAlias(context.Background(), cliproxyexecutor.Options{}, "route-model")); got != "route-model" {
		t.Fatalf("fallback affinity model = %q", got)
	}
}

func TestNextResetLearnersPrunedAfterSevenDaysIdle(t *testing.T) {
	now := nrNow
	tr := newNextResetRebindTracker(func() time.Time { return now })
	obs := func(authID string) {
		tr.observeUsage(nextResetUsageObservation{authID: authID, input: 1, at: now})
	}
	obs("old")
	now = now.Add(6 * 24 * time.Hour)
	obs("recent")
	now = now.Add(24*time.Hour + time.Minute)
	obs("new")
	tr.mu.Lock()
	_, oldKept := tr.creds["old"]
	_, recentKept := tr.creds["recent"]
	_, newKept := tr.creds["new"]
	tr.mu.Unlock()
	if oldKept {
		t.Fatal("learner idle over 7 days not pruned")
	}
	if !recentKept || !newKept {
		t.Fatalf("recent=%v new=%v, want both kept", recentKept, newKept)
	}
}

func TestNextResetRebindIdleCountsCancelledAndInFlightRequests(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancelled bool
	}{
		{"cancelled at minute 4", true},
		{"in flight since minute 4", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRebindFixture(t)
			b := claudeAuth("b", 20, 100*time.Hour)
			a := claudeAuth("a", 20, 10*time.Hour)
			f.bind(b)
			f.observe("b", 900_000, 0) // last completed request at minute 0
			start := f.now
			f.now = start.Add(4 * time.Minute)
			if got := f.pick(f.opts(""), a, b); got.ID != "b" {
				t.Fatalf("minute 4 got %s, want b (warm)", got.ID)
			}
			if tc.cancelled {
				// The cancelled request reports a failure without usage.
				f.tracker.HandleUsage(context.Background(), coreusage.Record{
					SessionID: f.canon, Alias: rebindModel, AuthID: "b", Failed: true, RequestedAt: f.now,
				})
			}
			f.now = start.Add(6 * time.Minute)
			if got := f.pick(f.opts(""), a, b); got.ID != "b" {
				t.Fatalf("minute 6 got %s: the minute-4 request kept the cache warm", got.ID)
			}
		})
	}
}

func TestNextResetRebindFailedRecordWithCacheUsageIsActivity(t *testing.T) {
	f := newRebindFixture(t)
	b := claudeAuth("b", 20, 100*time.Hour)
	a := claudeAuth("a", 20, 10*time.Hour)
	f.bind(b)
	f.observe("b", 900_000, 0)
	start := f.now
	// A request at minute 4 that failed after reading the cache, picked
	// outside this selector instance (no pick-time note here).
	f.tracker.HandleUsage(withNextResetAffinityModel(context.Background(), rebindModel), coreusage.Record{
		SessionID: f.canon, AuthID: "b", Failed: true, RequestedAt: start.Add(4 * time.Minute),
		Detail: coreusage.Detail{CacheReadTokens: 900_000},
	})
	f.now = start.Add(6 * time.Minute)
	if got := f.pick(f.opts(""), a, b); got.ID != "b" {
		t.Fatalf("got %s: failed request with cache usage should keep the cache warm", got.ID)
	}
}

func TestNextResetRebindCompactionMetadataThroughPick(t *testing.T) {
	f := newRebindFixture(t)
	b := claudeAuth("b", 20, 100*time.Hour)
	a := claudeAuth("a", 20, 10*time.Hour)
	f.bind(b)
	f.observe("b", 900_000, time.Minute) // warm, no estimate for a
	if got := f.pick(f.opts(""), a, b); got.ID != "b" {
		t.Fatalf("warm request without compaction moved to %s", got.ID)
	}
	opts := f.opts("")
	opts.Metadata[cliproxyexecutor.IsCompactionMetadataKey] = true
	if got := f.pick(opts, a, b); got.ID != "a" {
		t.Fatalf("compaction-flagged request got %s, want a", got.ID)
	}
	if _, still := opts.Metadata[cliproxyexecutor.IsCompactionMetadataKey]; still {
		t.Fatal("explicit-session path no longer clears the compaction flag")
	}
}

type rebindRoleKey struct{}

// raceAgainstMove interleaves two requests of one session deterministically:
// the second request (body secondBody) reads binding b and parks; the first
// moves the session to a; then the second resumes. It returns what the second
// request was served.
func raceAgainstMove(t *testing.T, f *rebindFixture, a, b *Auth, secondBody string) *Auth {
	t.Helper()
	secondRead := make(chan struct{})
	moverDone := make(chan struct{})
	f.affinity.afterCacheReadHook = func(ctx context.Context) {
		if ctx.Value(rebindRoleKey{}) == "second" {
			close(secondRead)
			<-moverDone
		}
	}
	defer func() { f.affinity.afterCacheReadHook = nil }()

	secondOpts := f.opts(secondBody)
	type result struct {
		auth *Auth
		err  error
	}
	second := make(chan result, 1)
	go func() {
		ctx := context.WithValue(context.Background(), rebindRoleKey{}, "second")
		got, err := f.affinity.Pick(ctx, "claude", rebindModel, secondOpts, []*Auth{a, b})
		second <- result{got, err}
	}()

	<-secondRead // the second request holds a read of b
	moved, err := f.affinity.Pick(context.Background(), "claude", rebindModel, f.opts(""), []*Auth{a, b})
	if err != nil || moved.ID != "a" {
		t.Fatalf("mover got %v, %v; want a", moved, err)
	}
	close(moverDone)

	res := <-second
	if res.err != nil {
		t.Fatalf("second request: %v", res.err)
	}
	return res.auth
}

func (f *rebindFixture) cachedBinding() string {
	id, _ := f.affinity.cache.Get("claude::" + f.canon + "::" + canonicalModelKey(rebindModel))
	return id
}

func newRaceFixture(t *testing.T) (*rebindFixture, *Auth, *Auth) {
	f := newRebindFixture(t)
	b := claudeAuth("b", 20, 100*time.Hour)
	a := claudeAuth("a", 20, 10*time.Hour)
	f.bind(b)
	f.observe("b", 1000, time.Hour) // cold: a move is free
	return f, a, b
}

const (
	raceAdvisorBody = `{"messages":[{"role":"assistant","content":[{"type":"advisor_redacted_result","data":"x"}]}]}`
	raceThreadBody  = `{"thread":{"type":"continue","previous_message_id":"msg_01"},"messages":[{"role":"user","content":"hi"}]}`
)

// A request pinned to its credential (advisor results) that read b before a
// concurrent move is still served by b, while the move's binding stays.
func TestNextResetRebindConcurrentMovePinnedRequestServedByReadAuth(t *testing.T) {
	f, a, b := newRaceFixture(t)
	if got := raceAgainstMove(t, f, a, b, raceAdvisorBody); got.ID != "b" {
		t.Fatalf("advisor-bearing request got %s, want b (the credential it read)", got.ID)
	}
	if got := f.cachedBinding(); got != "a" {
		t.Fatalf("cache holds %q after the race, want a", got)
	}
	if st, _ := f.tracker.session(nextResetRebindKey(f.canon, rebindModel)); !st.movedAt.Equal(f.now) {
		t.Fatalf("movedAt = %v, want %v", st.movedAt, f.now)
	}
}

// The pinned request writes nothing: the move's binding survives it and later
// requests stay on a.
func TestNextResetRebindConcurrentMoveBindingPreservedByPinnedRequest(t *testing.T) {
	f, a, b := newRaceFixture(t)
	if got := raceAgainstMove(t, f, a, b, raceThreadBody); got.ID != "b" {
		t.Fatalf("thread continuation got %s, want b", got.ID)
	}
	if got := f.cachedBinding(); got != "a" {
		t.Fatalf("pinned request rewrote the binding to %q, want a", got)
	}
	if got := f.pick(f.opts(""), a, b); got.ID != "a" {
		t.Fatalf("next request got %s, want a", got.ID)
	}
}

// A request with no guard that read b before a concurrent move follows the
// move instead of restoring b.
func TestNextResetRebindConcurrentCacheHitCannotUndoMove(t *testing.T) {
	f, a, b := newRaceFixture(t)
	if got := raceAgainstMove(t, f, a, b, ""); got.ID != "a" {
		t.Fatalf("unguarded request got %s, want a (the concurrent move)", got.ID)
	}
	if got := f.cachedBinding(); got != "a" {
		t.Fatalf("cache holds %q after the race, want a", got)
	}
}

// raceFallbackInheritance parks a request on the fallback-binding path after
// it read the fallback key (bound to auth-b), re-binds that key to auth-a with
// a failover, then resumes it. It returns what the parked request was served.
func raceFallbackInheritance(t *testing.T, sel *SessionAffinitySelector, provider string, parked, rebinder cliproxyexecutor.Options) *Auth {
	t.Helper()
	a, b := &Auth{ID: "auth-a"}, &Auth{ID: "auth-b"}
	read := make(chan struct{})
	rebound := make(chan struct{})
	sel.afterCacheReadHook = func(ctx context.Context) {
		if ctx.Value(rebindRoleKey{}) == "parked" {
			close(read)
			<-rebound
		}
	}
	defer func() { sel.afterCacheReadHook = nil }()

	type result struct {
		auth *Auth
		err  error
	}
	done := make(chan result, 1)
	go func() {
		ctx := context.WithValue(context.Background(), rebindRoleKey{}, "parked")
		got, err := sel.Pick(ctx, provider, "gpt-test", parked, []*Auth{a, b})
		done <- result{got, err}
	}()
	<-read
	// auth-b is unavailable to this request, so it fails over and re-binds the
	// fallback key to auth-a.
	if got, err := sel.Pick(context.Background(), provider, "gpt-test", rebinder, []*Auth{a}); err != nil || got.ID != "auth-a" {
		t.Fatalf("failover got %v, %v; want auth-a", got, err)
	}
	close(rebound)
	res := <-done
	if res.err != nil {
		t.Fatalf("parked request: %v", res.err)
	}
	return res.auth
}

func cacheAliases(sel *SessionAffinitySelector, key string) (string, []string) {
	sel.cache.mu.Lock()
	defer sel.cache.mu.Unlock()
	e, ok := sel.cache.entries[key]
	if !ok {
		return "", nil
	}
	return e.authID, append([]string(nil), e.aliases...)
}

// A request inheriting a fallback (conversation) binding must not overwrite
// the alias group when the binding changed after it was read.
func TestSessionAffinityFallbackAliasInheritanceIsConditional(t *testing.T) {
	sel := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour})
	defer sel.Stop()
	const provider = "fallback-race"
	conv := cliproxyexecutor.Options{OriginalRequest: []byte(`{"conversation":{"id":"conv-race"}}`)}
	combined := cliproxyexecutor.Options{OriginalRequest: []byte(`{"conversation":{"id":"conv-race"},"prompt_cache_key":"pck-race"}`)}
	if got, _ := sel.Pick(context.Background(), provider, "gpt-test", conv, []*Auth{{ID: "auth-b"}}); got.ID != "auth-b" {
		t.Fatalf("bind got %s", got.ID)
	}

	if got := raceFallbackInheritance(t, sel, provider, combined, conv); got.ID != "auth-a" {
		t.Fatalf("inheriting request got %s, want auth-a (the current binding)", got.ID)
	}
	convKey := provider + "::conv:conv-race::gpt-test"
	pckKey := provider + "::pck:pck-race::gpt-test"
	for _, key := range []string{convKey, pckKey} {
		if got, _ := sel.cache.Get(key); got != "auth-a" {
			t.Fatalf("%s bound to %q, want auth-a", key, got)
		}
	}
}

// A subagent inheriting its parent's binding binds only its own key: the
// parent group is neither merged nor rewritten, even when the parent was
// re-bound concurrently.
func TestSessionAffinitySubagentInheritanceKeepsParentGroupIsolated(t *testing.T) {
	sel := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour})
	defer sel.Stop()
	const provider = "subagent-race"
	parent := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"S"}}}
	child := cliproxyexecutor.Options{Headers: http.Header{
		"X-Claude-Code-Session-Id": []string{"S"},
		"X-Claude-Code-Agent-Id":   []string{"worker"},
	}}
	parentKey := provider + "::claude:S::gpt-test"
	childKey := provider + "::claude:S:agent:worker::gpt-test"
	if got, _ := sel.Pick(context.Background(), provider, "gpt-test", parent, []*Auth{{ID: "auth-b"}}); got.ID != "auth-b" {
		t.Fatalf("bind got %s", got.ID)
	}

	if got := raceFallbackInheritance(t, sel, provider, child, parent); got.ID != "auth-a" {
		t.Fatalf("subagent got %s, want auth-a (the parent's current binding)", got.ID)
	}
	if id, aliases := cacheAliases(sel, parentKey); id != "auth-a" || len(aliases) != 1 || aliases[0] != parentKey {
		t.Fatalf("parent group = %q %v, want auth-a [%s]", id, aliases, parentKey)
	}
	if id, aliases := cacheAliases(sel, childKey); id != "auth-a" || len(aliases) != 1 || aliases[0] != childKey {
		t.Fatalf("child group = %q %v, want auth-a [%s]", id, aliases, childKey)
	}
}

func TestSessionCacheCompareAndSetAliases(t *testing.T) {
	c := NewSessionCache(time.Hour)
	defer c.Stop()
	if _, ok := c.CompareAndSetAliases("k", "b", "a", "k"); ok {
		t.Fatal("swapped a missing binding")
	}
	c.SetAliases("b", "k", "alias")
	if cur, ok := c.CompareAndSetAliases("k", "x", "a", "k"); ok || cur != "b" {
		t.Fatalf("stale expectation: cur=%q ok=%v", cur, ok)
	}
	if cur, ok := c.CompareAndSetAliases("k", "b", "a", "k", "alias"); !ok || cur != "a" {
		t.Fatalf("swap: cur=%q ok=%v", cur, ok)
	}
	for _, key := range []string{"k", "alias"} {
		if got, _ := c.Get(key); got != "a" {
			t.Fatalf("%s bound to %q, want a", key, got)
		}
	}
	// Comparing one key while writing another leaves the compared group alone.
	if _, ok := c.CompareAndSetAliases("k", "a", "a", "child"); !ok {
		t.Fatal("compare on parent, write child failed")
	}
	c.mu.Lock()
	parentAliases := append([]string(nil), c.entries["k"].aliases...)
	childAliases := append([]string(nil), c.entries["child"].aliases...)
	c.mu.Unlock()
	if len(parentAliases) != 2 || len(childAliases) != 1 || childAliases[0] != "child" {
		t.Fatalf("parent aliases %v, child aliases %v", parentAliases, childAliases)
	}
}

func TestNextResetRebindSessionsAreBounded(t *testing.T) {
	now := nrNow
	tr := newNextResetRebindTracker(func() time.Time { return now })
	tr.maxSessions = 3
	add := func(id string) {
		tr.observeUsage(nextResetUsageObservation{sessionID: id, model: rebindModel, authID: "a", input: 1, at: now})
	}
	add("old")
	now = now.Add(nextResetRebindSessionIdle + time.Minute)
	add("s1")
	if _, ok := tr.session(nextResetRebindKey("old", rebindModel)); ok {
		t.Fatal("idle session not evicted")
	}
	now = now.Add(time.Second)
	add("s2")
	now = now.Add(time.Second)
	add("s3")
	now = now.Add(time.Second)
	add("s4")
	tr.mu.Lock()
	n := len(tr.sessions)
	tr.mu.Unlock()
	if n > 3 {
		t.Fatalf("sessions = %d, cap 3", n)
	}
	if _, ok := tr.session(nextResetRebindKey("s1", rebindModel)); ok {
		t.Fatal("oldest session kept over the cap")
	}
	if _, ok := tr.session(nextResetRebindKey("s4", rebindModel)); !ok {
		t.Fatal("newest session evicted")
	}
}

func TestNextResetRequestFacts(t *testing.T) {
	for _, tc := range []struct {
		body string
		want nextResetRequestFacts
	}{
		{`{"messages":[]}`, nextResetRequestFacts{}},
		{`{"tools":[{"name":"t","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`, nextResetRequestFacts{oneHourTTL: true}},
		{`{"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`, nextResetRequestFacts{oneHourTTL: true}},
		{`{"cache_control":{"type":"ephemeral","ttl":"1h"}}`, nextResetRequestFacts{oneHourTTL: true}},
		{`{"system":[{"type":"text","text":"1h","cache_control":{"type":"ephemeral","ttl":"5m"}}]}`, nextResetRequestFacts{}},
		{`{"thread":{"type":"continue","previous_message_id":"msg"}}`, nextResetRequestFacts{thread: true}},
		{`{"thread":{"type":"continue","previous_message_id":""}}`, nextResetRequestFacts{}},
		{`{"messages":[{"role":"user","content":"advisor_redacted_result"}]}`, nextResetRequestFacts{}},
	} {
		if got := nextResetRequestFactsFrom([]byte(tc.body)); got != tc.want {
			t.Fatalf("%s: got %+v, want %+v", tc.body, got, tc.want)
		}
	}
}
