package slackbridge

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseLinksCommands(t *testing.T) {
	cases := []struct {
		in   string
		want botCommand
	}{
		{"<@UBOT> links", botCommand{verb: "links", ok: true}},
		{"<@UBOT> links all", botCommand{verb: "links"}},
		{"<@UBOT> unlink <@UBOB>", botCommand{verb: "unlink", users: []string{"UBOB"}, ok: true}},
		{"<@UBOT> unlink GMPIM1", botCommand{verb: "unlink", conv: "GMPIM1", ok: true}},
		{"<@UBOT> unlink `GMPIM1`", botCommand{verb: "unlink", conv: "GMPIM1", ok: true}},
		{"<@UBOT> unlink a b", botCommand{verb: "unlink"}},
		{"<@UBOT> unlink <#C1|x>", botCommand{verb: "unlink"}},
	}
	for _, c := range cases {
		got, _, _ := parseCommand(c.in, "UBOT")
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseCommand(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestUnlinkPersonFromMainChannelRevokesTheirConversations(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvR1", dmMsg("UALEX", "<@UBOT> dm <@UBOB> with flyer", "1700006000.000001", ""))
	drainJobs(t, b)
	notice := claimOne(t, bus, sidA)
	b.handleEvent("EvR2", msg("UALEX", "<@UBOT> chat <@UBOB> <@UCAROL> with flyer", "1700006000.000002", ""))
	drainJobs(t, b)
	claimOne(t, bus, sidA)
	// A conversation without bob stays linked.
	b.handleEvent("EvR3", msg("UALEX", "<@UBOT> dm <@UCAROL> with flyer", "1700006000.000003", ""))
	drainJobs(t, b)
	claimOne(t, bus, sidA)

	b.handleEvent("EvR4", msg("UALEX", "<@UBOT> unlink <@UBOB>", "1700006000.000004", ""))
	drainJobs(t, b)
	for _, channel := range []string{"DUBOB", groupID} {
		if _, ok := b.state.conversation(channel); ok {
			t.Fatalf("%s still linked", channel)
		}
		posts := postsTo(f, channel)
		if last := posts[len(posts)-1]; last["text"] != noLongerLinked || last["thread_ts"] != "" {
			t.Fatalf("%s: last post = %+v", channel, last)
		}
	}
	if _, ok := b.state.conversation("DUCAROL"); !ok {
		t.Fatal("carol's DM was unlinked too")
	}
	confirm := lastPost(t, f)
	if confirm["channel"] != "CAGENTS" || confirm["thread_ts"] != "1700006000.000004" ||
		!strings.Contains(confirm["text"], "`DUBOB`") || !strings.Contains(confirm["text"], "`"+groupID+"`") || strings.Contains(confirm["text"], "DUCAROL") {
		t.Fatalf("confirm = %+v", confirm)
	}
	msgs := bus.Claim(sidA)
	if len(msgs) != 2 || !strings.Contains(msgs[0].Body, "unlinked") || !strings.Contains(msgs[1].Body, "unlinked") {
		t.Fatalf("unlink notices = %+v", msgs)
	}

	// Bob's next message reaches no one, and the agent's answer to the old
	// notice no longer lands in his DM.
	b.handleEvent("EvR5", dmMsg("UBOB", "still there?", "1700006000.000005", ""))
	drainJobs(t, b)
	if bus.Pending(sidA) {
		t.Fatalf("delivered after unlink: %+v", bus.Claim(sidA))
	}
	sendReply(t, b, bus, sidA, "hello?", notice.ID)
	if got := lastPost(t, f); got["channel"] == "DUBOB" {
		t.Fatalf("answer after unlink = %+v", got)
	}

	// From the owner's DM too; nobody left to unlink.
	b.handleEvent("EvR6", dmMsg("UALEX", "<@UBOT> unlink <@UBOB>", "1700006000.000006", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["text"] != "<@UBOB> isn't in any linked conversation." {
		t.Fatalf("second unlink = %+v", got)
	}
}

func TestUnlinkByConversationID(t *testing.T) {
	b, f, bus := newTestBridge(t)
	linkGroup(t, b, bus, sidA, "flyer", "L1")
	b.handleEvent("EvID1", dmMsg("UALEX", "<@UBOT> unlink GMPIM1", "1700006100.000001", ""))
	drainJobs(t, b)
	if _, ok := b.state.conversation("GMPIM1"); ok {
		t.Fatal("still linked")
	}
	if posts := postsTo(f, "GMPIM1"); posts[len(posts)-1]["text"] != noLongerLinked {
		t.Fatalf("GMPIM1 posts = %+v", posts)
	}
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["text"] != "Unlinked `GMPIM1` (group) from `"+bus.Address(sidA)+"`." {
		t.Fatalf("confirm = %+v", got)
	}
	if n := claimOne(t, bus, sidA); !strings.Contains(n.Body, "unlinked") {
		t.Fatalf("notice = %+v", n)
	}
	b.handleEvent("EvID2", msg("UALEX", "<@UBOT> unlink GMPIM1", "1700006100.000002", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); got["text"] != "No linked conversation `GMPIM1`." {
		t.Fatalf("second unlink = %+v", got)
	}
}

func TestLinksListing(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvLS0", msg("UALEX", "<@UBOT> links", "1700006200.000000", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); got["text"] != "No conversations are linked." {
		t.Fatalf("empty listing = %+v", got)
	}
	b.handleEvent("EvLS1", dmMsg("UALEX", "<@UBOT> dm <@UBOB> with flyer", "1700006200.000001", ""))
	drainJobs(t, b)
	// Linked in place; carol shows up as a participant once she writes.
	nameSession(t, bus, sidB, "bridge")
	linkGroup(t, b, bus, sidB, "bridge", "L1")
	b.handleEvent("EvLS2", foreignMsg("UCAROL", "hi", "1700006200.000002", ""))
	drainJobs(t, b)

	b.handleEvent("EvLS3", msg("UALEX", "<@UBOT> links", "1700006200.000003", ""))
	drainJobs(t, b)
	got := lastPost(t, f)
	if got["channel"] != "CAGENTS" || got["thread_ts"] != "1700006200.000003" {
		t.Fatalf("listing = %+v", got)
	}
	lines := strings.Split(got["text"], "\n")
	if len(lines) != 3 || lines[0] != "Linked conversations:" {
		t.Fatalf("listing = %q", got["text"])
	}
	day := b.state.now().UTC().Format("2006-01-02")
	for _, want := range []string{"`DUBOB` dm with @bob", "→ `" + bus.Address(sidA) + "`", "linked by @alex", day} {
		if !strings.Contains(lines[1], want) {
			t.Fatalf("dm line %q lacks %q", lines[1], want)
		}
	}
	for _, want := range []string{"`GMPIM1` group with @alex, @carol", "→ `" + bus.Address(sidB) + "`", "linked by @alex"} {
		if !strings.Contains(lines[2], want) {
			t.Fatalf("group line %q lacks %q", lines[2], want)
		}
	}
	if strings.Contains(got["text"], "<@") {
		t.Fatalf("listing pings people: %q", got["text"])
	}

	// In the owner's DM it works too; a non-owner is refused; in a group
	// conversation it isn't answered there.
	b.handleEvent("EvLS4", dmMsg("UALEX", "<@UBOT> links", "1700006200.000004", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || !strings.HasPrefix(got["text"], "Linked conversations:") {
		t.Fatalf("DM listing = %+v", got)
	}
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	for i, ev := range []messageEvent{
		msg("UJANE", "<@UBOT> links", "1700006200.000005", ""),
		msg("UJANE", "<@UBOT> unlink <@UBOB>", "1700006200.000006", ""),
		msg("UJANE", "<@UBOT> unlink GMPIM1", "1700006200.000007", ""),
	} {
		b.handleEvent("EvLSn"+string(rune('a'+i)), ev)
		drainJobs(t, b)
		if got := lastPost(t, f); got["text"] != ownersOnlyLinks {
			t.Fatalf("%d: non-owner reply = %+v", i, got)
		}
	}
	b.handleEvent("EvLS8", foreignMsg("UALEX", "<@UBOT> links", "1700006200.000008", ""))
	b.handleEvent("EvLS9", foreignMsg("UALEX", "<@UBOT> unlink <@UBOB>", "1700006200.000009", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); got["channel"] != "GMPIM1" || got["text"] != b.withBot(manageLinksWhere) || !strings.Contains(got["text"], "`@clanker-bro links`") {
		t.Fatalf("group reply = %+v", got)
	}
	for _, channel := range []string{"DUBOB", "GMPIM1"} {
		if _, ok := b.state.conversation(channel); !ok {
			t.Fatalf("%s unlinked by a refused command", channel)
		}
	}
}

func TestQueuedGuestMessageDroppedAfterUnlinkOrRelink(t *testing.T) {
	b, _, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "bridge")
	linkGroup(t, b, bus, sidA, "flyer", "L1")
	// Bob's name isn't cached, so his message waits on the job queue; the
	// owner unlinks before it runs.
	b.handleEvent("EvQ1", foreignMsg("UBOB", "secret plans", "1700006300.000001", ""))
	b.handleEvent("EvQ2", foreignMsg("UALEX", "<@UBOT> unlink", "1700006300.000002", ""))
	drainJobs(t, b)
	if msgs := bus.Claim(sidA); len(msgs) != 1 || !strings.Contains(msgs[0].Body, "unlinked") {
		t.Fatalf("sidA got %+v", msgs)
	}

	// Relinked to another agent while a message waits: dropped as well.
	b.guestMu.Lock()
	delete(b.guestNames, "UBOB")
	b.guestMu.Unlock()
	linkGroup(t, b, bus, sidA, "flyer", "L2")
	b.handleEvent("EvQ3", foreignMsg("UBOB", "for flyer", "1700006300.000003", ""))
	b.handleEvent("EvQ4", foreignMsg("UALEX", "<@UBOT> link bridge", "1700006300.000004", ""))
	drainJobs(t, b)
	if msgs := bus.Claim(sidA); len(msgs) != 1 || !strings.Contains(msgs[0].Body, "unlinked") {
		t.Fatalf("sidA got %+v", msgs)
	}
	if msgs := bus.Claim(sidB); len(msgs) != 1 || !strings.Contains(msgs[0].Body, "You were linked") {
		t.Fatalf("sidB got %+v", msgs)
	}
	// The count is back to zero, so the next message goes through.
	b.handleEvent("EvQ5", foreignMsg("UBOB", "hello bridge", "1700006300.000005", ""))
	drainJobs(t, b)
	if m := claimOne(t, bus, sidB); m.Body != "hello bridge" || !m.Guest {
		t.Fatalf("sidB got %+v", m)
	}
}
