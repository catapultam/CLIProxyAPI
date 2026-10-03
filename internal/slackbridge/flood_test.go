package slackbridge

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Item 3: guests in a linked conversation get 10 messages a minute (burst
// 10). Over that, the conversation gets one "slowing down" reply a minute,
// and the rest is dropped silently.
func TestGuestFloodIsRateLimited(t *testing.T) {
	clock := newTestClock()
	b, f, bus := newClockBridge(t, clock)
	linkGroup(t, b, bus, sidA, "flyer", "Fl1")
	before := len(postsTo(f, "GMPIM1"))
	seq := 0
	send := func(n int) {
		for i := 0; i < n; i++ {
			seq++
			b.handleEvent("", foreignMsg("UBOB", "spam", "1700007000."+strconv.Itoa(100000+seq), ""))
		}
		drainJobs(t, b)
	}
	send(13)
	if got := len(bus.Claim(sidA)); got != guestBurst {
		t.Fatalf("delivered %d of 13, want %d", got, guestBurst)
	}
	slow := 0
	for _, p := range postsTo(f, "GMPIM1")[before:] {
		if p["text"] == slowingDown {
			slow++
		}
	}
	if slow != 1 {
		t.Fatalf("slowing-down replies = %d, want 1", slow)
	}
	// Half a minute refills half the bucket; the reply isn't repeated yet.
	clock.advance(30 * time.Second)
	send(7)
	if got := len(bus.Claim(sidA)); got != 5 {
		t.Fatalf("after 30s delivered %d of 7, want 5", got)
	}
	// A minute after the first reply, a new overflow gets one more reply.
	clock.advance(31 * time.Second)
	send(12)
	slow = 0
	for _, p := range postsTo(f, "GMPIM1")[before:] {
		if p["text"] == slowingDown {
			slow++
		}
	}
	if slow != 2 {
		t.Fatalf("slowing-down replies = %d, want 2", slow)
	}
	// Owners in the conversation are not limited.
	bus.Claim(sidA)
	b.handleEvent("EvFlOwner", foreignMsg("UALEX", "owner here", "1700007999.000001", ""))
	if m := claimOne(t, bus, sidA); m.Body != "owner here" {
		t.Fatalf("owner msg = %+v", m)
	}
}

// Item 3: state saves are debounced; Stop flushes what is pending.
func TestStateSavesAreDebouncedAndFlushedOnStop(t *testing.T) {
	b, _, _ := newTestBridge(t)
	b.state.setDMLast("UALEX", sidA)
	if data, _ := os.ReadFile(b.cfg.StatePath); strings.Contains(string(data), "dm_last") {
		t.Fatalf("saved synchronously: %s", data)
	}
	b.Stop()
	data, err := os.ReadFile(b.cfg.StatePath)
	if err != nil || !strings.Contains(string(data), `"dm_last"`) {
		t.Fatalf("not flushed on Stop: %s (%v)", data, err)
	}
}

// State growth: threads, links and homes of sessions the bus hasn't seen
// for over 7 days are pruned; a session the bus doesn't know gets 7 days
// from when the bridge first noticed it.
func TestStatePrunesSessionsAbsentForAWeek(t *testing.T) {
	clock := newTestClock()
	b, _, bus := newClockBridge(t, clock)
	bus.Hello(sidB, "pc", "/work/other", "", true)
	rootA := threadOf(t, b, bus)
	b.state.moveThread(sidB, "CAGENTS", "1700008000.000001", homeDM)
	b.state.setThread(sidB, "CAGENTS", "1700008000.000002", false)
	const ghost = "dddddd11-2222-3333-4444-555555555555"
	b.state.setThread(ghost, "CAGENTS", "1700008000.000003", true)
	b.maintain()

	clock.advance(8 * 24 * time.Hour)
	bus.Touch(sidA)
	b.maintain()
	if ts, ok := b.state.thread(sidA); !ok || ts != rootA {
		t.Fatalf("live session's thread = %q %v", ts, ok)
	}
	if _, ok := b.state.thread(sidB); ok {
		t.Fatal("absent session's thread kept")
	}
	if b.state.home(sidB) != "" {
		t.Fatal("absent session's home kept")
	}
	for _, ts := range []string{"1700008000.000001", "1700008000.000002", "1700008000.000003"} {
		if sid, ok := b.state.session(ts); ok {
			t.Fatalf("thread %s still linked to %s", ts, sid)
		}
	}
	if _, ok := b.state.thread(ghost); ok {
		t.Fatal("unknown session's thread kept past a week")
	}
}

// A session the bus doesn't know keeps its thread for 7 days after the
// bridge first noticed it.
func TestStateKeepsUnknownSessionsForAWeek(t *testing.T) {
	clock := newTestClock()
	b, _, _ := newClockBridge(t, clock)
	const ghost = "dddddd11-2222-3333-4444-555555555555"
	b.state.setThread(ghost, "CAGENTS", "1700008100.000001", true)
	b.maintain()
	clock.advance(6 * 24 * time.Hour)
	b.maintain()
	if _, ok := b.state.thread(ghost); !ok {
		t.Fatal("pruned before a week")
	}
}
