package auth

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// TestNextResetPollerLatchedFarExpectedResetPolledEvery15Minutes reproduces
// the live bug: a latched credential whose expected reset is still days
// away (e.g. the owner cleared the limit upstream well ahead of the
// account's natural weekly reset) used to wait for the routine 3h poll
// before due() would recheck it. due() must now recheck it every
// nextResetLatchedPoll (15 minutes) instead. Applies equally to Claude;
// Codex is used here only to keep this test independent of the Claude
// plan-size polling added alongside it.
func TestNextResetPollerLatchedFarExpectedResetPolledEvery15Minutes(t *testing.T) {
	withNextReset(t)
	full := tokenAuth(codexAuth("far-full", 100, 10*24*time.Hour))
	nextResetBlocked(full, nrNow) // latch it; expected reset ~10 days out
	resetAt := strconv.FormatInt(nrNow.Add(10*24*time.Hour).Unix(), 10)
	d := &fakeUsageDoer{status: 200, body: `{"rate_limit":{"secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":` + resetAt + `}}}`}
	p := newTestPoller([]*Auth{full}, d)

	p.runOnce(context.Background(), nrNow) // startup poll: establishes lastPoll
	if len(d.calls) != 1 {
		t.Fatalf("startup poll missing: %v", d.calls)
	}
	if !nextResetIsLatched("far-full") {
		t.Fatal("still-full account released")
	}

	p.runOnce(context.Background(), nrNow.Add(10*time.Minute))
	if len(d.calls) != 1 {
		t.Fatalf("polled before the 15-minute latched floor: %v", d.calls)
	}

	p.runOnce(context.Background(), nrNow.Add(15*time.Minute+time.Second))
	if len(d.calls) != 2 {
		t.Fatalf("not polled after the 15-minute latched floor: %v", d.calls)
	}
}

// TestNextResetPollerLatchedPollRespectsBackoff covers that the new
// 15-minute latched floor does not bypass p.backoff: due() still checks
// "now.Before(until)" ahead of any latch logic.
func TestNextResetPollerLatchedPollRespectsBackoff(t *testing.T) {
	withNextReset(t)
	full := tokenAuth(codexAuth("far-full-2", 100, 10*24*time.Hour))
	nextResetBlocked(full, nrNow)
	d := &fakeUsageDoer{status: 500}
	p := newTestPoller([]*Auth{full}, d)

	p.runOnce(context.Background(), nrNow) // startup poll fails
	if len(d.calls) != 1 {
		t.Fatalf("startup poll missing: %v", d.calls)
	}

	// Force a backoff well past the 15-minute latched floor.
	p.backoff["far-full-2"] = nrNow.Add(time.Hour)
	p.runOnce(context.Background(), nrNow.Add(20*time.Minute))
	if len(d.calls) != 1 {
		t.Fatalf("backoff did not suppress the latched poll: %v", d.calls)
	}

	p.runOnce(context.Background(), nrNow.Add(time.Hour+time.Minute))
	if len(d.calls) != 2 {
		t.Fatalf("poll did not resume once backoff expired: %v", d.calls)
	}
}
