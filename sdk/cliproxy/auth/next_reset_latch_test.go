package auth

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func withNextReset(t *testing.T) {
	t.Helper()
	nextResetEnabled.Store(true)
	nextResetLatches = &nextResetLatchStore{since: make(map[string]time.Time)}
	nextResetPolled = newNextResetPolledStore()
	t.Cleanup(func() {
		nextResetEnabled.Store(false)
		nextResetLatches = &nextResetLatchStore{since: make(map[string]time.Time)}
		nextResetPolled = newNextResetPolledStore()
	})
}

func TestNextResetLatchHoldsAfterResetUntilUsageConfirms(t *testing.T) {
	withNextReset(t)
	a := claudeAuth("a", 100, time.Hour)
	if blocked, until := nextResetBlocked(a, nrNow); !blocked || !until.Equal(nrNow.Add(time.Hour)) {
		t.Fatalf("full account not blocked: %v %v", blocked, until)
	}
	// The stored reset passed but nothing confirmed it: still blocked.
	later := nrNow.Add(2 * time.Hour)
	if blocked, until := nextResetBlocked(a, later); !blocked || !until.Equal(later.Add(nextResetLatchRetry)) {
		t.Fatalf("latched account released on the clock: %v %v", blocked, until)
	}
	// A poll observed after the latch showing room releases it.
	nextResetPolled.set("a", nextResetSnapshot{Weekly: nextResetWindow{Known: true, UsedPct: 3, ResetsAt: later.Add(160 * time.Hour)}, ObservedAt: later})
	if blocked, _ := nextResetBlocked(a, later); blocked {
		t.Fatal("confirmed account still blocked")
	}
}

func TestNextResetLatchIgnoresOlderGoodPoll(t *testing.T) {
	withNextReset(t)
	nextResetPolled.set("a", nextResetSnapshot{Weekly: nextResetWindow{Known: true, UsedPct: 3, ResetsAt: nrNow.Add(100 * time.Hour)}, ObservedAt: nrNow.Add(-time.Hour)})
	a := claudeAuth("a", 100, time.Hour)
	if blocked, _ := nextResetBlocked(a, nrNow.Add(2*time.Hour)); !blocked {
		t.Fatal("an older good poll must not release a newer exhaustion")
	}
}

func TestNextResetLatchOffWhenStrategyOff(t *testing.T) {
	withNextReset(t)
	nextResetEnabled.Store(false)
	if blocked, _ := nextResetBlocked(claudeAuth("a", 100, time.Hour), nrNow); blocked {
		t.Fatal("latch applied with the strategy off")
	}
}

func TestNextResetLatchBlocksAvailability(t *testing.T) {
	withNextReset(t)
	a := claudeAuth("a", 100, time.Hour)
	if blocked, reason, _ := isAuthBlockedForModel(a, "claude-opus-5-5", nrNow); !blocked || reason != blockReasonCooldown {
		t.Fatalf("availability ignored the latch: %v %v", blocked, reason)
	}
}

func TestParseCodexUsageEndpoint(t *testing.T) {
	reset := nrNow.Add(48 * time.Hour).Unix()
	body := []byte(`{"rate_limit":{"allowed":false,"limit_reached":true,
	"primary_window":{"used_percent":"20","limit_window_seconds":18000,"reset_after_seconds":600},
	"secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":` + strconv.FormatInt(reset, 10) + `}}}`)
	snap, err := parseCodexUsageEndpoint(body, nrNow)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Weekly.UsedPct != 100 || !snap.Weekly.Rejected || snap.Weekly.ResetsAt.Unix() != reset {
		t.Fatalf("weekly = %+v", snap.Weekly)
	}
	if snap.Short.UsedPct != 20 || !snap.Short.ResetsAt.Equal(nrNow.Add(10*time.Minute)) {
		t.Fatalf("short = %+v", snap.Short)
	}
}

type fakeUsageDoer struct {
	calls  []string
	status int
	body   string
}

func (f *fakeUsageDoer) do(_ context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	f.calls = append(f.calls, auth.ID+" "+req.URL.String()+" "+req.Header.Get("Chatgpt-Account-Id"))
	return &http.Response{StatusCode: f.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(f.body))}, nil
}

func newTestPoller(auths []*Auth, d *fakeUsageDoer) *nextResetPoller {
	return &nextResetPoller{
		list: func() []*Auth { return auths }, do: d.do, store: nextResetPolled,
		backoff: map[string]time.Time{}, lastPoll: map[string]time.Time{},
	}
}

func tokenAuth(a *Auth) *Auth {
	a.Metadata = map[string]any{"access_token": "tok", "account_id": "acct-1"}
	return a
}

func TestNextResetPollerConfirmsLatchedBeforeRoutine(t *testing.T) {
	withNextReset(t)
	nextResetMarkActive(nrNow)
	full := tokenAuth(claudeAuth("z-full", 100, -time.Minute))
	fresh := tokenAuth(claudeAuth("a-fresh", 10, 100*time.Hour))
	nextResetBlocked(full, nrNow) // latch it
	d := &fakeUsageDoer{status: 200, body: `{"seven_day":{"utilization":4,"resets_at":"2026-10-09T00:00:00Z"}}`}
	p := newTestPoller([]*Auth{fresh, full}, d)
	p.runOnce(context.Background(), nrNow.Add(time.Minute))
	if len(d.calls) != 1 || !strings.HasPrefix(d.calls[0], "z-full https://api.anthropic.com/api/oauth/usage") {
		t.Fatalf("calls = %v", d.calls)
	}
	if nextResetIsLatched("z-full") {
		t.Fatal("confirmed poll did not release the latch")
	}
}

func TestNextResetPollerConfirmsLatchedWhileIdle(t *testing.T) {
	withNextReset(t)
	nextResetMarkActive(nrNow.Add(-10 * time.Hour))
	full := tokenAuth(codexAuth("c", 100, -time.Minute))
	nextResetBlocked(full, nrNow)
	d := &fakeUsageDoer{status: 200, body: `{"rate_limit":{"secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":1}}}`}
	p := newTestPoller([]*Auth{full}, d)
	p.runOnce(context.Background(), nrNow)
	if len(d.calls) != 1 || !strings.Contains(d.calls[0], "chatgpt.com/backend-api/wham/usage acct-1") {
		t.Fatalf("calls = %v", d.calls)
	}
	if !nextResetIsLatched("c") {
		t.Fatal("still-full codex account released")
	}
	// Re-polled only after the latch retry interval.
	p.runOnce(context.Background(), nrNow.Add(time.Minute))
	if len(d.calls) != 1 {
		t.Fatalf("re-polled too soon: %v", d.calls)
	}
	p.runOnce(context.Background(), nrNow.Add(nextResetLatchRetry))
	if len(d.calls) != 2 {
		t.Fatalf("not re-polled after retry interval: %v", d.calls)
	}
}

func TestNextResetPollerSkipsRoutineWhenIdle(t *testing.T) {
	withNextReset(t)
	nextResetMarkActive(nrNow.Add(-10 * time.Hour))
	d := &fakeUsageDoer{status: 200, body: `{}`}
	p := newTestPoller([]*Auth{tokenAuth(claudeAuth("a", 10, 100*time.Hour))}, d)
	p.runOnce(context.Background(), nrNow)
	if len(d.calls) != 0 {
		t.Fatalf("idle routine poll: %v", d.calls)
	}
}
