package slackbridge

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// Guest presence comes from Slack's member list, not from who has written.

func TestConversationMembersPaginates(t *testing.T) {
	f := newFakeSlack(t)
	f.setMembers("C1", "U1", "U2", "U3", "U4", "U5")
	f.memberPage = 2
	ids, err := newAPI(f.apiBase()).conversationMembers(context.Background(), "xoxb-1", "C1")
	if err != nil || !reflect.DeepEqual(ids, []string{"U1", "U2", "U3", "U4", "U5"}) {
		t.Fatalf("members = %v, %v", ids, err)
	}
	if n := len(f.callsTo("conversations.members")); n != 3 {
		t.Fatalf("calls = %d, want 3 pages", n)
	}
}

// groupAnswer delivers "flyer: status?" at the top level of the group and
// returns the agent's answer there.
func groupAnswer(t *testing.T, b *Bridge, f *fakeSlack, ts string) map[string]string {
	t.Helper()
	b.handleEvent("Ev"+ts, foreignMsg("UALEX", "flyer: status?", ts, ""))
	m := claimOne(t, b.bus, sidA)
	sendReply(t, b, b.bus, sidA, "green", m.ID)
	return lastPost(t, f)
}

// A guest who is in the conversation but never wrote still counts: shell
// output is refused and the header names the agent only.
func TestSilentGuestRefusesShellAndOmitsHeader(t *testing.T) {
	b, f, bus, dir := newCommandBridge(t)
	writeCommand(t, dir, "screenshot.yaml", shellYAML, mtime0)
	f.setMembers("GMPIM1", "UALEX", "UBOB", "UBOT")
	if got := groupAnswer(t, b, f, "1700010000.000001"); got["channel"] != "GMPIM1" || got["text"] != "*flyer*\ngreen" {
		t.Fatalf("answer = %+v", got)
	}
	b.handleEvent("EvMb2", foreignMsg("UALEX", "flyer: !screenshot", "1700010000.000002", ""))
	drainJobs(t, b)
	if bus.Pending(sidA) {
		t.Fatalf("shell command delivered with a silent guest there: %+v", bus.Claim(sidA))
	}
	if got := lastPostText(f); got != shellWhereGuests {
		t.Fatalf("reply = %q", got)
	}
}

// A group of owners (and the bot) is owner-only: full header, shell runs.
func TestOwnerOnlyGroupPasses(t *testing.T) {
	b, f, bus, dir := newCommandBridge(t)
	writeCommand(t, dir, "screenshot.yaml", shellYAML, mtime0)
	f.setMembers("GMPIM1", "UALEX", "UBOT")
	full := sessionHeader(outboundFor(bus, sidA, "flyer", ""))
	if got := groupAnswer(t, b, f, "1700010100.000001"); got["text"] != full+"\ngreen" {
		t.Fatalf("answer = %+v", got)
	}
	b.handleEvent("EvMo2", foreignMsg("UALEX", "flyer: !screenshot", "1700010100.000002", ""))
	drainJobs(t, b)
	if m := claimOne(t, bus, sidA); m.Command == nil || m.Command.Kind != "shell" {
		t.Fatalf("shell command = %+v", m)
	}
}

// A member lookup that fails counts as guests being there.
func TestMemberLookupFailsClosed(t *testing.T) {
	b, f, bus, dir := newCommandBridge(t)
	writeCommand(t, dir, "screenshot.yaml", shellYAML, mtime0)
	f.setMembers("GMPIM1", "UALEX", "UBOT")
	f.setFail("conversations.members", "ratelimited")
	if got := groupAnswer(t, b, f, "1700010200.000001"); got["text"] != "*flyer*\ngreen" {
		t.Fatalf("answer = %+v", got)
	}
	b.handleEvent("EvMf2", foreignMsg("UALEX", "flyer: !screenshot", "1700010200.000002", ""))
	drainJobs(t, b)
	if bus.Pending(sidA) {
		t.Fatalf("shell command delivered after a failed lookup: %+v", bus.Claim(sidA))
	}
	if got := lastPostText(f); got != shellWhereGuests {
		t.Fatalf("reply = %q", got)
	}
}

// The member list is cached for 5 minutes; a member joining or leaving
// drops the cache at once.
func TestMemberListCacheAndInvalidation(t *testing.T) {
	clock := newTestClock()
	b, f, bus, dir := newCommandBridge(t)
	b.state.now = clock.now
	writeCommand(t, dir, "screenshot.yaml", shellYAML, mtime0)
	f.setMembers("GMPIM1", "UALEX", "UBOT")
	run := func(ts string) {
		b.handleEvent("Ev"+ts, foreignMsg("UALEX", "flyer: !screenshot", ts, ""))
		drainJobs(t, b)
	}
	run("1700010300.000001")
	run("1700010300.000002")
	if got := len(bus.Claim(sidA)); got != 2 {
		t.Fatalf("delivered %d of 2", got)
	}
	if n := len(f.callsTo("conversations.members")); n != 1 {
		t.Fatalf("lookups = %d, want 1 (cached)", n)
	}
	// Bob joins: the event drops the cache, and the next command sees him.
	f.setMembers("GMPIM1", "UALEX", "UBOB", "UBOT")
	b.handleEvent("EvJoin", messageEvent{Type: "member_joined_channel", Channel: "GMPIM1", User: "UBOB"})
	run("1700010300.000003")
	if bus.Pending(sidA) || lastPostText(f) != shellWhereGuests {
		t.Fatalf("join not seen: pending=%v reply=%q", bus.Pending(sidA), lastPostText(f))
	}
	// Bob leaves without an event: the cache expires after 5 minutes.
	f.setMembers("GMPIM1", "UALEX", "UBOT")
	run("1700010300.000004")
	if bus.Pending(sidA) {
		t.Fatal("cache ignored before it expired")
	}
	clock.advance(memberCacheTTL + time.Second)
	run("1700010300.000005")
	if got := len(bus.Claim(sidA)); got != 1 {
		t.Fatalf("after expiry delivered %d, want 1", got)
	}
}
