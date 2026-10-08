package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
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

// claudeAuthWithShortWindow is claudeAuth plus a 5h-window usage reading
// (allowed, not rejected) whose reset time is explicit, for deterministic
// boundary tests on the short-window near-full floor. A zero resetAt omits
// the 5h-Reset signal entirely, so Short.Known is still true (via
// Utilization) but Short.ResetsAt stays the zero time.
func claudeAuthWithShortWindow(id string, weeklyUsed float64, weeklyResetIn time.Duration, shortUsed float64, resetAt time.Time) *Auth {
	a := claudeAuth(id, weeklyUsed, weeklyResetIn)
	a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = strconv.FormatFloat(shortUsed/100, 'f', 4, 64)
	a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Status"] = "allowed"
	if !resetAt.IsZero() {
		a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(resetAt.Unix(), 10)
	}
	return a
}

// claudeAuthWithShortUsed is claudeAuthWithShortWindow with the short window
// resetting comfortably in the future (1h out), for tests that only care
// about the usage floor, not the reset boundary.
func claudeAuthWithShortUsed(id string, weeklyUsed float64, weeklyResetIn time.Duration, shortUsed float64) *Auth {
	return claudeAuthWithShortWindow(id, weeklyUsed, weeklyResetIn, shortUsed, nrNow.Add(time.Hour))
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
	got, err := s.Pick(withNextResetColdPick(context.Background()), "claude", "claude-sonnet-5-5", cliproxyexecutor.Options{}, []*Auth{b, a})
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
	sum := sha256.Sum256([]byte("a"))
	wantHash := hex.EncodeToString(sum[:6])
	if !strings.Contains(line, "auth="+wantHash) {
		t.Fatalf("log line missing chosen auth's opaque hash %q: %q", wantHash, line)
	}
	if !strings.Contains(line, "provider=claude") {
		t.Fatalf("log line missing provider: %q", line)
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

func TestNextResetColdPickLogsOpaqueHashNeverEmailOrFileName(t *testing.T) {
	hook := setupNextResetLogHook(t)
	// Realistic file-based credential: both the ID and the file name embed
	// the account's email, exactly like the OAuth file synthesizer produces
	// (internal/watcher/synthesizer/file.go). Label is also the email.
	const id = "claude-12345678-alice@example.com.json"
	a := claudeAuth(id, 20, 10*time.Hour)
	a.FileName = "claude-12345678-alice@example.com.json"
	a.Label = "alice@example.com"
	s := nrSelector()
	got, err := s.Pick(withNextResetColdPick(context.Background()), "claude", "claude-sonnet-5-5", cliproxyexecutor.Options{}, []*Auth{a})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.ID != id {
		t.Fatalf("got %s, want %s", got.ID, id)
	}

	var line string
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "next-reset: cold pick") {
			line = e.Message
		}
	}
	if line == "" {
		t.Fatalf("expected a cold pick log line")
	}
	if strings.Contains(line, "alice@example.com") || strings.Contains(line, "@") {
		t.Fatalf("log line leaked the account email: %q", line)
	}
	if strings.Contains(line, "claude-12345678") {
		t.Fatalf("log line leaked the credential file name or ID: %q", line)
	}
	sum := sha256.Sum256([]byte(id))
	wantHash := hex.EncodeToString(sum[:6])
	if !strings.Contains(line, "auth="+wantHash) {
		t.Fatalf("log line missing the opaque hash %q: %q", wantHash, line)
	}
}

func TestNextResetNearFullBoundaryBelow98IsNotNearFull(t *testing.T) {
	// a resets sooner and is at 97.99% weekly usage, just under the floor, so
	// it still competes on reset deadline and wins.
	a := claudeAuth("a", 97.99, 10*time.Hour)
	b := claudeAuth("b", 50, 100*time.Hour)
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a, b); got.ID != "a" {
		t.Fatalf("got %s, want a (97.99%% is below the near-full floor)", got.ID)
	}
}

func TestNextResetNearFullBoundaryAtExactly98IsNearFull(t *testing.T) {
	// a resets sooner but is at exactly 98% weekly usage, at the floor, so it
	// is pushed behind b. This fails if ">=" is weakened to ">".
	a := claudeAuth("a", 98, 10*time.Hour)
	b := claudeAuth("b", 50, 100*time.Hour)
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a, b); got.ID != "b" {
		t.Fatalf("got %s, want b (a is at the near-full floor)", got.ID)
	}
}

func TestNextResetBothNearFullOrdersByEarliestResetWithinGroup(t *testing.T) {
	// Both a and b are at or above the near-full floor. Within the near-full
	// group, ordering still follows earliest reset first.
	a := claudeAuth("a", 98, 10*time.Hour)
	b := claudeAuth("b", 99, 100*time.Hour)
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", b, a); got.ID != "a" {
		t.Fatalf("got %s, want a (earlier reset wins within the near-full group)", got.ID)
	}
}

func TestNextResetShortWindowNearFullDemotesEvenWithWeeklyHeadroom(t *testing.T) {
	// a resets sooner and has plenty of weekly headroom, but its 5h window is
	// at 92% (>= nextResetShortWindowNearFullPct), so it is demoted behind b.
	a := claudeAuthWithShortUsed("a", 10, 10*time.Hour, 92)
	b := claudeAuth("b", 50, 100*time.Hour)
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a, b); got.ID != "b" {
		t.Fatalf("got %s, want b (a's short window is near full)", got.ID)
	}
}

func TestNextResetShortWindowBelowFloorDoesNotDemote(t *testing.T) {
	// Same as above but a's short window is at 89%, just under the floor, so
	// a still wins on its earlier weekly reset.
	a := claudeAuthWithShortUsed("a", 10, 10*time.Hour, 89)
	b := claudeAuth("b", 50, 100*time.Hour)
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a, b); got.ID != "a" {
		t.Fatalf("got %s, want a (89%% is below the short-window near-full floor)", got.ID)
	}
}

func TestNextResetShortWindowDemotesWhenStillOpenByOneNanosecond(t *testing.T) {
	// a's short window resets 1ns after now, i.e. it is still (barely) open,
	// and sits above the floor, so a is demoted despite its earlier weekly
	// reset and weekly headroom. Injected directly into the polled store: the
	// wire signals only carry whole-second Unix timestamps and would truncate
	// away the 1ns offset.
	a := &Auth{ID: "a", Provider: "claude", Status: StatusActive}
	b := claudeAuth("b", 50, 100*time.Hour)
	s := nrSelector()
	s.polled.set("a", nextResetSnapshot{
		Weekly:     nextResetWindow{Known: true, UsedPct: 10, ResetsAt: nrNow.Add(10 * time.Hour)},
		Short:      nextResetWindow{Known: true, UsedPct: 92, ResetsAt: nrNow.Add(time.Nanosecond)},
		ObservedAt: nrNow,
	})
	if got := nrPick(t, s, "claude-sonnet-5-5", a, b); got.ID != "b" {
		t.Fatalf("got %s, want b (a's short window is still open and near full)", got.ID)
	}
}

func TestNextResetShortWindowNotDemotedAtOrAfterItsReset(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resetAt time.Time
	}{
		{"exactly at reset", nrNow},
		{"reset already passed", nrNow.Add(-time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// a's short window has reset (or is resetting this instant), so
			// its stale 92% reading must not demote it: the window no longer
			// blocks or burdens the credential, and a wins on its earlier
			// weekly reset.
			a := claudeAuthWithShortWindow("a", 10, 10*time.Hour, 92, tc.resetAt)
			b := claudeAuth("b", 50, 100*time.Hour)
			if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a, b); got.ID != "a" {
				t.Fatalf("got %s, want a (a's short window reset is not in the future)", got.ID)
			}
		})
	}
}

func TestNextResetShortWindowZeroOrMissingResetsAtDoesNotDemote(t *testing.T) {
	// No 5h-Reset signal at all: Short.Known is still true (Utilization is
	// present) but Short.ResetsAt stays the zero time, which must never
	// satisfy ResetsAt.After(now).
	a := claudeAuthWithShortWindow("a", 10, 10*time.Hour, 92, time.Time{})
	b := claudeAuth("b", 50, 100*time.Hour)
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a, b); got.ID != "a" {
		t.Fatalf("got %s, want a (zero/missing short-window ResetsAt must not demote)", got.ID)
	}
}

func TestNextResetUnknownShortWindowDoesNotDemote(t *testing.T) {
	// No 5h signals at all: Short.Known is false, so shortUsedPct stays zero
	// and can never trigger the near-full floor.
	a := claudeAuth("a", 10, 10*time.Hour)
	b := claudeAuth("b", 50, 100*time.Hour)
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a, b); got.ID != "a" {
		t.Fatalf("got %s, want a (unknown short window must not demote)", got.ID)
	}
}

func TestNextResetSessionAffinityLogsColdPickAndFailoverOnly(t *testing.T) {
	hook := setupNextResetLogHook(t)

	a := claudeAuth("a", 20, 10*time.Hour)
	b := claudeAuth("b", 50, 100*time.Hour)
	fallback := &NextResetSelector{polled: newNextResetPolledStore(), now: func() time.Time { return nrNow }}
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: fallback, TTL: time.Hour})
	defer affinity.Stop()

	opts := cliproxyexecutor.Options{
		Headers:  http.Header{"X-Claude-Code-Session-Id": []string{"sess-cold"}},
		Metadata: make(map[string]any),
	}
	coldPicks := func() int {
		n := 0
		for _, e := range hook.AllEntries() {
			if strings.Contains(e.Message, "next-reset: cold pick") {
				n++
			}
		}
		return n
	}

	// Cold binding: the session has no cached auth, so the fallback selector
	// is consulted and should log once.
	picked, err := affinity.Pick(context.Background(), "claude", "claude-sonnet-5-5", opts, []*Auth{a, b})
	if err != nil {
		t.Fatalf("cold pick: %v", err)
	}
	if picked.ID != "a" {
		t.Fatalf("cold pick got %s, want a", picked.ID)
	}
	if got := coldPicks(); got != 1 {
		t.Fatalf("after cold pick, cold-pick logs = %d, want 1", got)
	}

	// Cache hit: same session, same bound auth. Returned directly from the
	// affinity cache without consulting the fallback selector, so no
	// additional log line.
	picked, err = affinity.Pick(context.Background(), "claude", "claude-sonnet-5-5", opts, []*Auth{a, b})
	if err != nil {
		t.Fatalf("cache hit pick: %v", err)
	}
	if picked.ID != "a" {
		t.Fatalf("cache hit got %s, want a", picked.ID)
	}
	if got := coldPicks(); got != 1 {
		t.Fatalf("after cache hit, cold-pick logs = %d, want still 1", got)
	}

	// Failover: the bound auth becomes unavailable, so the fallback selector
	// is consulted again and should log once more.
	a.Disabled = true
	picked, err = affinity.Pick(context.Background(), "claude", "claude-sonnet-5-5", opts, []*Auth{a, b})
	if err != nil {
		t.Fatalf("failover pick: %v", err)
	}
	if picked.ID != "b" {
		t.Fatalf("failover got %s, want b", picked.ID)
	}
	if got := coldPicks(); got != 2 {
		t.Fatalf("after failover, cold-pick logs = %d, want 2", got)
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

// sized sets a's hand-set size attribute (see ApplyAuthSizeMetadata/authSize)
// and returns a, for building next-reset ranking test fixtures.
func sized(a *Auth, size string) *Auth {
	if a.Attributes == nil {
		a.Attributes = map[string]string{}
	}
	a.Attributes[AttributeSize] = size
	return a
}

func TestNextResetSmallerAccountWithResetUpTo24hLaterBeatsLarger(t *testing.T) {
	// a is the largest known size (90), so it never shifts. b is smaller
	// (10) and resets only 10h after a, well inside the 24h flat shift, so
	// b's effective reset (30h - 24h = 6h) beats a's real 20h.
	a := sized(claudeAuth("a", 50, 20*time.Hour), "90")
	b := sized(claudeAuth("b", 50, 30*time.Hour), "10")
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a, b); got.ID != "b" {
		t.Fatalf("got %s, want b (smaller account within the 24h window)", got.ID)
	}
}

func TestNextResetSmallerAccountResettingMoreThan24hLaterLoses(t *testing.T) {
	// Same sizes as above, but b now resets 30h after a (outside the 24h
	// shift), so even after the 24h bias b's effective reset (50h - 24h =
	// 26h) is still later than a's real 20h: a keeps winning.
	a := sized(claudeAuth("a", 50, 20*time.Hour), "90")
	b := sized(claudeAuth("b", 50, 50*time.Hour), "10")
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a, b); got.ID != "a" {
		t.Fatalf("got %s, want a (b's reset is more than 24h later even after the shift)", got.ID)
	}
}

func TestNextResetUnknownSizeGetsNoShift(t *testing.T) {
	// c holds the largest known size and resets soonest in real terms. a
	// resets later than c but would, if its size were known and smaller,
	// shift ahead of c (15h - 24h < 1h). With a's size known and smaller
	// than c's, a wins; with a's size unknown, a gets no shift and c wins.
	c := sized(claudeAuth("c", 50, time.Hour), "100")
	t.Run("known smaller size shifts ahead", func(t *testing.T) {
		a := sized(claudeAuth("a", 50, 15*time.Hour), "10")
		if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a, c); got.ID != "a" {
			t.Fatalf("got %s, want a (known smaller size shifts its effective reset earlier)", got.ID)
		}
	})
	t.Run("unknown size is not shifted", func(t *testing.T) {
		a := claudeAuth("a", 50, 15*time.Hour) // no size attribute
		if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a, c); got.ID != "c" {
			t.Fatalf("got %s, want c (unknown size must not shift a ahead)", got.ID)
		}
	})
}

func TestNextResetEqualKnownSizesOrderUnaffected(t *testing.T) {
	// Equal sizes mean neither is "smaller than the largest known size", so
	// neither shifts: ordering is exactly the earliest-reset baseline.
	a := sized(claudeAuth("a", 60, 20*time.Hour), "50")
	b := sized(claudeAuth("b", 10, 40*time.Hour), "50")
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", b, a); got.ID != "a" {
		t.Fatalf("got %s, want a (equal sizes must not change earliest-reset ordering)", got.ID)
	}
}

func TestNextResetNearFullGoesLastRegardlessOfSize(t *testing.T) {
	// a is near full but tiny (size 10, well under b's 100), which would
	// shift its effective reset 24h earlier if the near-full floor did not
	// apply first. The near-full floor (step (a)) is still evaluated before
	// the effective-reset comparison (step (b)), so b wins regardless.
	a := sized(claudeAuth("a", 98.5, 10*time.Hour), "10")
	b := sized(claudeAuth("b", 50, 100*time.Hour), "100")
	if got := nrPick(t, nrSelector(), "claude-sonnet-5-5", a, b); got.ID != "b" {
		t.Fatalf("got %s, want b (a is near full regardless of its smaller size)", got.ID)
	}
}

func TestNextResetColdPickLogsSizeAndEffectiveReset(t *testing.T) {
	hook := setupNextResetLogHook(t)
	a := sized(claudeAuth("a", 50, 30*time.Hour), "10")
	b := sized(claudeAuth("b", 50, 20*time.Hour), "90")
	s := nrSelector()
	got, err := s.Pick(withNextResetColdPick(context.Background()), "claude", "claude-sonnet-5-5", cliproxyexecutor.Options{}, []*Auth{a, b})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.ID != "a" {
		t.Fatalf("got %s, want a (effective reset 30h-24h=6h beats b's 20h)", got.ID)
	}
	var line string
	for _, e := range hook.AllEntries() {
		if e.Level == log.InfoLevel && strings.Contains(e.Message, "next-reset: cold pick") {
			line = e.Message
		}
	}
	if line == "" {
		t.Fatalf("expected an Info log for the cold pick")
	}
	if !strings.Contains(line, "size=10") {
		t.Fatalf("log line missing size: %q", line)
	}
	wantEffective := nrNow.Add(30 * time.Hour).Add(-nextResetSizeBiasShift).Local().Format(time.RFC3339)
	if !strings.Contains(line, "effective_reset="+wantEffective) {
		t.Fatalf("log line missing effective_reset=%s: %q", wantEffective, line)
	}
}

func TestNextResetColdPickLogsSizeUnknown(t *testing.T) {
	hook := setupNextResetLogHook(t)
	a := claudeAuth("a", 20, 10*time.Hour) // no size attribute
	s := nrSelector()
	if _, err := s.Pick(withNextResetColdPick(context.Background()), "claude", "claude-sonnet-5-5", cliproxyexecutor.Options{}, []*Auth{a}); err != nil {
		t.Fatalf("Pick: %v", err)
	}
	var line string
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "next-reset: cold pick") {
			line = e.Message
		}
	}
	if !strings.Contains(line, "size=unknown") {
		t.Fatalf("log line missing size=unknown: %q", line)
	}
}
