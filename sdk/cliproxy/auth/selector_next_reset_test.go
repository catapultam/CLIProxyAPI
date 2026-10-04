package auth

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

var nrNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func nrSelector() *NextResetSelector {
	return &NextResetSelector{polled: newNextResetPolledStore(), now: func() time.Time { return nrNow }}
}

func claudeAuth(id string, weeklyUsed float64, weeklyResetIn time.Duration) *Auth {
	return &Auth{ID: id, Provider: "claude", Status: StatusActive, Quota: QuotaState{
		ObservedAt: nrNow,
		Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(weeklyUsed/100, 'f', 4, 64),
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(nrNow.Add(weeklyResetIn).Unix(), 10),
			"Anthropic-Ratelimit-Unified-7d-Status":      "allowed",
		},
	}}
}

func codexAuth(id string, weeklyUsed float64, weeklyResetIn time.Duration) *Auth {
	return &Auth{ID: id, Provider: "codex", Status: StatusActive, Quota: QuotaState{
		ObservedAt: nrNow,
		Signals: map[string]string{
			"X-Codex-Primary-Used-Percent":     "5",
			"X-Codex-Primary-Window-Minutes":   "300",
			"X-Codex-Primary-Reset-At":         strconv.FormatInt(nrNow.Add(time.Hour).Unix(), 10),
			"X-Codex-Secondary-Used-Percent":   strconv.FormatFloat(weeklyUsed, 'f', 1, 64),
			"X-Codex-Secondary-Window-Minutes": "10080",
			"X-Codex-Secondary-Reset-At":       strconv.FormatInt(nrNow.Add(weeklyResetIn).Unix(), 10),
		},
	}}
}

func nrPick(t *testing.T, s *NextResetSelector, model string, auths ...*Auth) *Auth {
	t.Helper()
	got, err := s.Pick(context.Background(), "", model, cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	return got
}

func TestNextResetPrefersQuotaAboutToBeLost(t *testing.T) {
	// a: 50% left over 10h (5/h); b: 90% left over 144h (0.625/h).
	a := claudeAuth("a", 50, 10*time.Hour)
	b := claudeAuth("b", 10, 144*time.Hour)
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", b, a); got.ID != "a" {
		t.Fatalf("got %s", got.ID)
	}
}

func TestNextResetCodexUsesLongWindow(t *testing.T) {
	a := codexAuth("a", 50, 10*time.Hour)
	b := codexAuth("b", 10, 144*time.Hour)
	if got := nrPick(t, nrSelector(), "gpt-5.5", b, a); got.ID != "a" {
		t.Fatalf("got %s", got.ID)
	}
}

func TestNextResetCodexLongWindowNotAssumedSecondary(t *testing.T) {
	snap, ok := codexSignalsSnapshot(map[string]string{
		"X-Codex-Primary-Used-Percent":     "70",
		"X-Codex-Primary-Window-Minutes":   "10080",
		"X-Codex-Primary-Reset-At":         strconv.FormatInt(nrNow.Add(48*time.Hour).Unix(), 10),
		"X-Codex-Secondary-Used-Percent":   "10",
		"X-Codex-Secondary-Window-Minutes": "300",
	}, nrNow)
	if !ok || snap.Weekly.UsedPct != 70 || snap.Short.UsedPct != 10 {
		t.Fatalf("snap = %+v", snap)
	}
}

func TestNextResetCodexResetAfterSecondsIsRelativeToObservation(t *testing.T) {
	snap, _ := codexSignalsSnapshot(map[string]string{
		"X-Codex-Secondary-Used-Percent":        "10",
		"X-Codex-Secondary-Window-Minutes":      "10080",
		"X-Codex-Secondary-Reset-After-Seconds": "3600",
	}, nrNow)
	if !snap.Weekly.ResetsAt.Equal(nrNow.Add(time.Hour)) {
		t.Fatalf("resets at %v", snap.Weekly.ResetsAt)
	}
}

func TestNextResetSkipsExhaustedUntilReset(t *testing.T) {
	a := claudeAuth("a", 10, 48*time.Hour)
	a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Status"] = "rejected"
	a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(nrNow.Add(time.Hour).Unix(), 10)
	b := claudeAuth("b", 90, 100*time.Hour)
	s := nrSelector()
	if got := nrPick(t, s, "", a, b); got.ID != "b" {
		t.Fatalf("got %s, want b while a's 5h window is rejected", got.ID)
	}
	s.now = func() time.Time { return nrNow.Add(61 * time.Minute) }
	if got := nrPick(t, s, "", a, b); got.ID != "a" {
		t.Fatalf("got %s, want a after its 5h reset passed", got.ID)
	}
}

func TestNextResetFableSubLimitOnlyBlocksFable(t *testing.T) {
	a := claudeAuth("a", 10, 10*time.Hour)
	b := claudeAuth("b", 10, 100*time.Hour)
	s := nrSelector()
	s.polled.set("a", nextResetSnapshot{Fable: nextResetWindow{Known: true, UsedPct: 100, ResetsAt: nrNow.Add(24 * time.Hour)}, ObservedAt: nrNow.Add(-time.Minute)})
	if got := nrPick(t, s, "claude-fable-5-1", a, b); got.ID != "b" {
		t.Fatalf("fable pick got %s", got.ID)
	}
	if got := nrPick(t, s, "claude-opus-5-5", a, b); got.ID != "a" {
		t.Fatalf("opus pick got %s", got.ID)
	}
}

func TestNextResetRotatesCredentialsWithoutData(t *testing.T) {
	a := &Auth{ID: "a", Provider: "claude", Status: StatusActive}
	b := &Auth{ID: "b", Provider: "claude", Status: StatusActive}
	s := nrSelector()
	first := nrPick(t, s, "", a, b)
	second := nrPick(t, s, "", a, b)
	if first.ID == second.ID {
		t.Fatalf("no-data credentials should rotate, got %s twice", first.ID)
	}
}

func TestNextResetRanksDataBeforeNoData(t *testing.T) {
	known := claudeAuth("z", 99, 140*time.Hour)
	unknown := &Auth{ID: "a", Provider: "claude", Status: StatusActive}
	if got := nrPick(t, nrSelector(), "", unknown, known); got.ID != "z" {
		t.Fatalf("got %s", got.ID)
	}
}

func TestNextResetAPIKeyAfterOAuth(t *testing.T) {
	key := &Auth{ID: "a-key", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"auth_kind": "apikey"}}
	oauth := &Auth{ID: "b-oauth", Provider: "claude", Status: StatusActive}
	if got := nrPick(t, nrSelector(), "", key, oauth); got.ID != "b-oauth" {
		t.Fatalf("got %s", got.ID)
	}
}

func TestNextResetAllExhaustedReturnsCooldown(t *testing.T) {
	a := claudeAuth("a", 100, 10*time.Hour)
	got, err := nrSelector().Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, []*Auth{a})
	if err == nil || got != nil {
		t.Fatalf("want cooldown error, got auth=%v err=%v", got, err)
	}
	var cooldown *modelCooldownError
	if !errors.As(err, &cooldown) || cooldown.resetIn < 9*time.Hour {
		t.Fatalf("err = %v", err)
	}
}

func TestNextResetNewerPollOverridesOlderSignals(t *testing.T) {
	a := claudeAuth("a", 10, 100*time.Hour)
	a.Quota.ObservedAt = nrNow.Add(-time.Hour)
	b := claudeAuth("b", 50, 20*time.Hour)
	s := nrSelector()
	s.polled.set("a", nextResetSnapshot{Weekly: nextResetWindow{Known: true, UsedPct: 50, ResetsAt: nrNow.Add(5 * time.Hour)}, ObservedAt: nrNow})
	if got := nrPick(t, s, "", a, b); got.ID != "a" {
		t.Fatalf("got %s", got.ID)
	}
}

// setupNextResetLogHook captures Info-and-above log entries written to the
// standard logrus logger for the duration of the test, restoring the prior
// level and hooks on cleanup.
func setupNextResetLogHook(t *testing.T) *logtest.Hook {
	t.Helper()
	_, hook := logtest.NewNullLogger()
	oldLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)

	savedHooks := make(log.LevelHooks)
	for lvl, hs := range log.StandardLogger().Hooks {
		savedHooks[lvl] = append([]log.Hook(nil), hs...)
	}
	log.AddHook(hook)
	t.Cleanup(func() {
		log.SetLevel(oldLevel)
		log.StandardLogger().ReplaceHooks(savedHooks)
	})
	return hook
}

func TestNextResetEarliestResetWinsOverOldScoreFormula(t *testing.T) {
	// a resets in 20h at 60% used (old score (100-60)/20=2.0); b resets in 40h
	// at 10% used (old score (100-10)/40=2.25). The old formula favored b;
	// earliest-deadline-first means a is picked since neither is near full.
	a := claudeAuth("a", 60, 20*time.Hour)
	b := claudeAuth("b", 10, 40*time.Hour)
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", b, a); got.ID != "a" {
		t.Fatalf("got %s, want a", got.ID)
	}
}

func TestNextResetNoFlipFlopAsUsageRises(t *testing.T) {
	// Same reset deadlines as above, but a's usage has climbed to 80%. a is
	// still picked: usage rising does not flip the pick back to b as long as
	// a's reset remains the sooner one and a is not near full.
	a := claudeAuth("a", 80, 20*time.Hour)
	b := claudeAuth("b", 10, 40*time.Hour)
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", b, a); got.ID != "a" {
		t.Fatalf("got %s, want a (no flip-flop)", got.ID)
	}
}

func TestNextResetSoonerResetBeatsFresherAccount(t *testing.T) {
	// fresh resets in 7 days at 0% used; soon resets in 2 days at 50% used.
	// Earliest-deadline-first picks soon even though fresh has far more
	// unused headroom.
	fresh := claudeAuth("fresh", 0, 7*24*time.Hour)
	soon := claudeAuth("soon", 50, 2*24*time.Hour)
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", fresh, soon); got.ID != "soon" {
		t.Fatalf("got %s, want soon", got.ID)
	}
}

func TestNextResetNearFullFloorUsesSoonerAccountLast(t *testing.T) {
	// a resets sooner but is at 98.5% weekly usage (>= nextResetNearFullPct),
	// so it is pushed behind b even though a's reset is earlier.
	a := claudeAuth("a", 98.5, 10*time.Hour)
	b := claudeAuth("b", 50, 100*time.Hour)
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a, b); got.ID != "b" {
		t.Fatalf("got %s, want b (a is near full)", got.ID)
	}
	// With only the near-full credential ready, it is still picked: the floor
	// only reorders among Ready candidates, it never removes the last one.
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a); got.ID != "a" {
		t.Fatalf("got %s, want a when it is the only ready candidate", got.ID)
	}
}

func TestNextResetEqualResetTimesBreakOnUsedPctThenID(t *testing.T) {
	resetIn := 24 * time.Hour
	a := claudeAuth("a", 50, resetIn)
	b := claudeAuth("b", 20, resetIn)
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a, b); got.ID != "b" {
		t.Fatalf("got %s, want b (lower weekly used pct at equal reset time)", got.ID)
	}
	c := claudeAuth("c", 20, resetIn)
	d := claudeAuth("d", 20, resetIn)
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", d, c); got.ID != "c" {
		t.Fatalf("got %s, want c (lower ID at equal reset time and used pct)", got.ID)
	}
}

func TestNextResetColdPickLogsUsageAndReadyCount(t *testing.T) {
	hook := setupNextResetLogHook(t)
	a := claudeAuth("a", 20, 10*time.Hour)
	b := claudeAuth("b", 50, 100*time.Hour)
	s := nrSelector()
	got, err := s.Pick(withNextResetColdPick(context.Background()), "", "claude-sonnet-5-5", cliproxyexecutor.Options{}, []*Auth{b, a})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.ID != "a" {
		t.Fatalf("got %s, want a", got.ID)
	}

	var line string
	for _, e := range hook.AllEntries() {
		if e.Level == log.InfoLevel && strings.Contains(e.Message, "next-reset: cold pick") {
			line = e.Message
		}
	}
	if line == "" {
		t.Fatalf("expected an Info log for the cold pick, got entries: %v", hook.AllEntries())
	}
	if !strings.Contains(line, "auth=a") {
		t.Fatalf("log line missing chosen auth: %q", line)
	}
	if !strings.Contains(line, "weekly_used=20.0%") {
		t.Fatalf("log line missing weekly used pct: %q", line)
	}
	if !strings.Contains(line, "ready=2") {
		t.Fatalf("log line missing ready candidate count: %q", line)
	}
}

func TestNextResetPerRequestPickDoesNotLog(t *testing.T) {
	hook := setupNextResetLogHook(t)
	a := claudeAuth("a", 20, 10*time.Hour)
	s := nrSelector()
	// No withNextResetColdPick marker: this simulates a sessionless, per-request
	// pick, which must not produce an Info line on every request.
	if _, err := s.Pick(context.Background(), "", "claude-sonnet-5-5", cliproxyexecutor.Options{}, []*Auth{a}); err != nil {
		t.Fatalf("Pick: %v", err)
	}
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "next-reset: cold pick") {
			t.Fatalf("unexpected cold-pick log for a per-request pick: %q", e.Message)
		}
	}
}

func TestParseClaudeUsageEndpointFable(t *testing.T) {
	body := []byte(`{"five_hour":{"utilization":12.5,"resets_at":"2026-10-02T15:00:00Z"},
	"seven_day":{"utilization":40,"resets_at":"2026-10-05T00:00:00+00:00"},
	"iguana_necktie":{"utilization":41,"resets_at":"2026-10-08T00:00:00Z"},
	"limits":[
	 {"kind":"weekly_scoped","percent":12,"resets_at":"2026-10-06T00:00:00Z","is_active":false,"scope":{"model":{"display_name":"Fable"}}},
	 {"kind":"weekly_scoped","percent":64,"resets_at":"2026-10-07T00:00:00Z","is_active":true,"scope":{"model":{"display_name":"Fable 5"}}}]}`)
	snap, err := parseClaudeUsageEndpoint(body, nrNow)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Short.UsedPct != 12.5 || snap.Weekly.UsedPct != 40 || snap.Fable.UsedPct != 64 {
		t.Fatalf("snap = %+v", snap)
	}
	legacy, _ := parseClaudeUsageEndpoint([]byte(`{"iguana_necktie":{"utilization":41,"resets_at":"2026-10-08T00:00:00Z"}}`), nrNow)
	if !legacy.Fable.Known || legacy.Fable.UsedPct != 41 {
		t.Fatalf("legacy fable = %+v", legacy.Fable)
	}
}

func TestSelectorUsesNextResetUnwrapsAffinity(t *testing.T) {
	if !selectorUsesNextReset(NewSessionAffinitySelector(NewNextResetSelector())) {
		t.Fatal("affinity-wrapped next-reset not detected")
	}
	if selectorUsesNextReset(NewSessionAffinitySelector(&RoundRobinSelector{})) {
		t.Fatal("round-robin misdetected")
	}
}
