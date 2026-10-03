package slackbridge

import (
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

// nameSession gives sid a bus name.
func nameSession(t *testing.T, bus *agentbus.Store, sid, name string) {
	t.Helper()
	if err := bus.SetName(sid, name); err != nil {
		t.Fatal(err)
	}
}

// foreignMsg is a message event in a group DM the bot was added to.
func foreignMsg(user, text, ts, threadTS string) messageEvent {
	return messageEvent{Type: "message", Channel: "GMPIM1", ChannelType: "mpim", User: user, Text: text, TS: ts, ThreadTS: threadTS}
}

// lastRecord is the newest reply-map record.
func lastRecord(b *Bridge) replyRecord {
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	return b.state.replies[len(b.state.replies)-1]
}

// nothingSent fails when anything reached a session or Slack.
func nothingSent(t *testing.T, b *Bridge, f *fakeSlack, bus *agentbus.Store) {
	t.Helper()
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatalf("delivered: %+v %+v", bus.Claim(sidA), bus.Claim(sidB))
	}
	drainJobs(t, b)
	if n := len(f.callsTo("chat.postMessage")); n != 0 {
		t.Fatalf("posted %d messages: %+v", n, lastPost(t, f))
	}
	if n := len(f.callsTo("reactions.add")); n != 0 {
		t.Fatalf("reacted %d times", n)
	}
}

func TestThreadTagGoesToTheTaggedAgent(t *testing.T) {
	b, f, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "flyer2")
	root := threadOf(t, b, bus)
	b.handleEvent("EvG1", msg("UALEX", "flyer2: look here", "1700002000.000001", root))
	msgs := bus.Claim(sidB)
	if len(msgs) != 1 || msgs[0].Body != "look here" || !msgs[0].FromUser || msgs[0].SlackUser != "alex" || msgs[0].Via != "" {
		t.Fatalf("msgs = %+v", msgs)
	}
	if bus.Pending(sidA) {
		t.Fatalf("the thread's own agent got it: %+v", bus.Claim(sidA))
	}
	if r := lastRecord(b); r.ID != msgs[0].ID || r.Channel != "CAGENTS" || r.ThreadTS != root || r.Session != sidB || r.TS != "1700002000.000001" {
		t.Fatalf("reply record = %+v", r)
	}
	if _, ok := b.state.thread(sidB); ok {
		t.Fatal("a tag adopted the thread for the tagged agent")
	}
	if own, _ := b.state.thread(sidA); own != root {
		t.Fatalf("sidA's thread = %q", own)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "1700002000.000001"); !reflect.DeepEqual(got, []string{reactionQueued}) {
		t.Fatalf("reactions = %v", got)
	}

	// Receipts move on as for any delivery.
	b.Received([]string{msgs[0].ID})
	b.Read([]string{msgs[0].ID})
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "1700002000.000001"); !reflect.DeepEqual(got, []string{reactionRead}) {
		t.Fatalf("reactions after read = %v", got)
	}

	// The tagged agent's answer lands in this thread.
	sendReply(t, b, bus, sidB, "seen it", msgs[0].ID)
	if got := lastPost(t, f); got["channel"] != "CAGENTS" || got["thread_ts"] != root || got["text"] != "seen it" {
		t.Fatalf("answer = %+v", got)
	}

	// A plain reply afterwards still goes to the thread's own agent.
	b.handleEvent("EvG2", msg("UALEX", "back to you", "1700002000.000002", root))
	if got := bus.Claim(sidA); len(got) != 1 || got[0].Body != "back to you" {
		t.Fatalf("sidA msgs = %+v", got)
	}
}

func TestThreadUnresolvedTagGoesToTheThreadsAgent(t *testing.T) {
	b, f, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "flyer2")
	root := threadOf(t, b, bus)
	for i, text := range []string{"note: remember x", "TODO: tests", "slack: hi", "@ghost look", "@slack look"} {
		b.handleEvent("EvU"+string(rune('a'+i)), msg("UALEX", text, "1700002100.00000"+string(rune('1'+i)), root))
		if got := bus.Claim(sidA); len(got) != 1 || got[0].Body != text {
			t.Fatalf("%q: sidA msgs = %+v", text, got)
		}
	}
	if bus.Pending(sidB) {
		t.Fatal("an unresolved tag reached another agent")
	}
	drainJobs(t, b)
	if n := len(f.callsTo("chat.postMessage")); n != 1 {
		t.Fatalf("posts = %d, want only the thread header (no 'no agent called' replies)", n)
	}
}

func TestThreadAtTagGoesToTheTaggedAgent(t *testing.T) {
	b, _, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "flyer2")
	root := threadOf(t, b, bus)
	b.handleEvent("EvAt1", msg("UALEX", "@flyer2 look", "1700002200.000001", root))
	if got := bus.Claim(sidB); len(got) != 1 || got[0].Body != "look" {
		t.Fatalf("sidB msgs = %+v", got)
	}
	b.handleEvent("EvAt2", msg("UALEX", "@flyer2: and this", "1700002200.000002", root))
	if got := bus.Claim(sidB); len(got) != 1 || got[0].Body != "and this" {
		t.Fatalf("sidB msgs = %+v", got)
	}
	// By address too.
	b.handleEvent("EvAt3", msg("UALEX", "@"+bus.Address(sidB)+" by address", "1700002200.000003", root))
	if got := bus.Claim(sidB); len(got) != 1 || got[0].Body != "by address" {
		t.Fatalf("sidB msgs = %+v", got)
	}
	// A real Slack mention is never a tag, even when it reads as a session's
	// name once rendered.
	nameSession(t, bus, sidB, "alex")
	b.handleEvent("EvAt4", msg("UALEX", "<@UALEX> have a look", "1700002200.000004", root))
	if got := bus.Claim(sidA); len(got) != 1 || got[0].Body != "@alex have a look" {
		t.Fatalf("sidA msgs = %+v", got)
	}
	// A bare tag with no message is not a tag.
	b.handleEvent("EvAt5", msg("UALEX", "@alex", "1700002200.000005", root))
	if got := bus.Claim(sidA); len(got) != 1 || got[0].Body != "@alex" {
		t.Fatalf("sidA msgs = %+v", got)
	}
	if bus.Pending(sidB) {
		t.Fatalf("sidB got %+v", bus.Claim(sidB))
	}
}

func TestThreadTagOfOwnAgentIsUnchanged(t *testing.T) {
	b, _, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	b.handleEvent("EvO1", msg("UALEX", "flyer: hi", "1700002300.000001", root))
	if got := bus.Claim(sidA); len(got) != 1 || got[0].Body != "flyer: hi" {
		t.Fatalf("sidA msgs = %+v", got)
	}
}

func TestThreadTagCommand(t *testing.T) {
	b, f, bus, _ := newCommandBridge(t)
	nameSession(t, bus, sidB, "flyer2")
	bus.SetModVersion(sidB, "0.3.4")
	root := threadOf(t, b, bus)
	b.handleEvent("EvC1", msg("UALEX", "flyer2: !compact", "1700002400.000001", root))
	msgs := bus.Claim(sidB)
	want := agentbus.Command{Name: "compact", Kind: "slash", Command: "compact"}
	if len(msgs) != 1 || msgs[0].Command == nil || !reflect.DeepEqual(*msgs[0].Command, want) {
		t.Fatalf("sidB msgs = %+v", msgs)
	}
	if bus.Pending(sidA) {
		t.Fatal("the command went to the thread's own agent")
	}
	if r := lastRecord(b); r.ID != msgs[0].ID || r.ThreadTS != root || r.Session != sidB {
		t.Fatalf("reply record = %+v", r)
	}
	if _, ok := b.state.thread(sidB); ok {
		t.Fatal("a tagged command adopted the thread")
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "1700002400.000001"); !reflect.DeepEqual(got, []string{reactionCommand}) {
		t.Fatalf("reactions = %v", got)
	}
	sendReply(t, b, bus, sidB, "done", msgs[0].ID)
	if got := lastPost(t, f); got["channel"] != "CAGENTS" || got["thread_ts"] != root {
		t.Fatalf("report = %+v", got)
	}

	// A non-owner is refused, in the thread, and nothing is delivered.
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	b.handleEvent("EvC2", msg("UJANE", "flyer2: !compact", "1700002400.000002", root))
	drainJobs(t, b)
	if got := lastPost(t, f); got["text"] != ownersOnlyCommands || got["thread_ts"] != root {
		t.Fatalf("reply = %+v", got)
	}
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("a non-owner's tagged command was delivered")
	}
}

func TestDMThreadTagGoesToTheTaggedAgent(t *testing.T) {
	b, f, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "flyer2")
	sendDM(t, b, bus, sidA, "alex", "need input")
	rootTS := lastPostTS(f)
	b.handleEvent("EvDG1", dmMsg("UALEX", "flyer2: look", "1700002500.000001", rootTS))
	msgs := bus.Claim(sidB)
	if len(msgs) != 1 || msgs[0].Body != "look" || msgs[0].Via != agentbus.ViaDM {
		t.Fatalf("sidB msgs = %+v", msgs)
	}
	if bus.Pending(sidA) {
		t.Fatal("the thread's own agent got it")
	}
	drainJobs(t, b)
	if got := f.reactionsOn("DUALEX", "1700002500.000001"); !reflect.DeepEqual(got, []string{reactionQueued}) {
		t.Fatalf("reactions = %v", got)
	}
	sendReply(t, b, bus, sidB, "looking", msgs[0].ID)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["thread_ts"] != rootTS || got["text"] != "looking" {
		t.Fatalf("answer = %+v", got)
	}
	// The thread still belongs to sidA.
	b.handleEvent("EvDG2", dmMsg("UALEX", "thanks", "1700002500.000002", rootTS))
	if got := bus.Claim(sidA); len(got) != 1 || got[0].Body != "thanks" {
		t.Fatalf("sidA msgs = %+v", got)
	}
}

func TestDMTopLevelAtTag(t *testing.T) {
	b, _, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "flyer2")
	b.handleEvent("EvDT1", dmMsg("UALEX", "@flyer2 status?", "1700002550.000001", ""))
	if got := bus.Claim(sidB); len(got) != 1 || got[0].Body != "status?" || got[0].Via != agentbus.ViaDM {
		t.Fatalf("sidB msgs = %+v", got)
	}
	if sid, _ := b.state.dmLast("UALEX"); sid != sidB {
		t.Fatalf("dmLast = %q", sid)
	}
	// A thread under it reaches sidB.
	b.handleEvent("EvDT2", dmMsg("UALEX", "more", "1700002550.000002", "1700002550.000001"))
	if got := bus.Claim(sidB); len(got) != 1 || got[0].Body != "more" {
		t.Fatalf("sidB msgs = %+v", got)
	}
}

func TestChannelTopLevelAtTagAdoptsThread(t *testing.T) {
	b, _, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "flyer2")
	b.handleEvent("EvCT1", msg("UALEX", "@flyer2 run the tests", "1700002560.000001", ""))
	if got := bus.Claim(sidB); len(got) != 1 || got[0].Body != "run the tests" {
		t.Fatalf("sidB msgs = %+v", got)
	}
	if ts, _ := b.state.thread(sidB); ts != "1700002560.000001" {
		t.Fatalf("thread = %q", ts)
	}
}

func TestUnlinkedChannelThreadTag(t *testing.T) {
	b, f, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "flyer2")
	b.handleEvent("EvUT1", msg("UALEX", "flyer2: over here", "1700002600.000002", "1700002600.000001"))
	msgs := bus.Claim(sidB)
	if len(msgs) != 1 || msgs[0].Body != "over here" {
		t.Fatalf("sidB msgs = %+v", msgs)
	}
	sendReply(t, b, bus, sidB, "hi", msgs[0].ID)
	if got := lastPost(t, f); got["channel"] != "CAGENTS" || got["thread_ts"] != "1700002600.000001" {
		t.Fatalf("answer = %+v", got)
	}
	if _, ok := b.state.thread(sidB); ok {
		t.Fatal("a tag adopted an unlinked thread")
	}
}

func TestForeignConversationTagIsDelivered(t *testing.T) {
	b, f, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "bridge")
	b.handleEvent("EvF1", foreignMsg("UALEX", "bridge: hi", "1700002700.000001", ""))
	msgs := bus.Claim(sidB)
	if len(msgs) != 1 || msgs[0].Body != "hi" || !msgs[0].FromUser || msgs[0].Via != agentbus.ViaGroup {
		t.Fatalf("sidB msgs = %+v", msgs)
	}
	if r := lastRecord(b); r.Channel != "GMPIM1" || r.ThreadTS != "1700002700.000001" || r.Session != sidB || r.DMUser != "" {
		t.Fatalf("reply record = %+v", r)
	}
	if _, ok := b.state.thread(sidB); ok {
		t.Fatal("a foreign message adopted a channel thread")
	}
	drainJobs(t, b)
	if got := f.reactionsOn("GMPIM1", "1700002700.000001"); !reflect.DeepEqual(got, []string{reactionQueued}) {
		t.Fatalf("reactions = %v", got)
	}
	sendReply(t, b, bus, sidB, "hello there", msgs[0].ID)
	if got := lastPost(t, f); got["channel"] != "GMPIM1" || got["thread_ts"] != "1700002700.000001" || got["text"] != "hello there" {
		t.Fatalf("answer = %+v", got)
	}

	// In the thread under it, which now goes to bridge: a tag of bridge
	// itself leaves the text whole, as in a channel thread.
	b.handleEvent("EvF2", foreignMsg("UALEX", "@bridge and this", "1700002700.000003", "1700002700.000001"))
	msgs = bus.Claim(sidB)
	if len(msgs) != 1 || msgs[0].Body != "@bridge and this" {
		t.Fatalf("sidB msgs = %+v", msgs)
	}
	sendReply(t, b, bus, sidB, "ok", msgs[0].ID)
	if got := lastPost(t, f); got["channel"] != "GMPIM1" || got["thread_ts"] != "1700002700.000001" {
		t.Fatalf("answer = %+v", got)
	}

	// Another channel the bot is in works the same way.
	other := msg("UALEX", "bridge: from general", "1700002700.000004", "")
	other.Channel, other.ChannelType = "CGEN", "channel"
	b.handleEvent("EvF3", other)
	if got := bus.Claim(sidB); len(got) != 1 || got[0].Body != "from general" || got[0].Via != agentbus.ViaGroup {
		t.Fatalf("sidB msgs = %+v", got)
	}
}

func TestForeignConversationFromNonAllowedUserIsDropped(t *testing.T) {
	b, f, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "bridge")
	b.handleEvent("EvFN1", foreignMsg("UEVE", "bridge: hi", "1700002800.000001", ""))
	b.handleEvent("EvFN2", foreignMsg("UEVE", "bridge: !compact", "1700002800.000002", ""))
	bot := foreignMsg("UHOOK", "bridge: deploy", "1700002800.000003", "")
	bot.BotID = "BHOOK"
	b.handleEvent("EvFN3", bot)
	edited := foreignMsg("UALEX", "bridge: edit", "1700002800.000004", "")
	edited.Subtype = "message_changed"
	b.handleEvent("EvFN4", edited)
	nothingSent(t, b, f, bus)
}

func TestForeignConversationUntaggedIsIgnored(t *testing.T) {
	b, f, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "bridge")
	for i, text := range []string{"hello all", "note: lunch at noon", "!commands", "!compact", "<@UBOT> allow <@UJANE>", "<@UBOT> hi", "@ghost hi"} {
		b.handleEvent("EvFI"+string(rune('a'+i)), foreignMsg("UALEX", text, "1700002900.00000"+string(rune('1'+i)), ""))
	}
	b.handleEvent("EvFIt", foreignMsg("UALEX", "a reply", "1700002900.000020", "1700002900.000001"))
	nothingSent(t, b, f, bus)
	if _, ok := b.state.user("UJANE"); ok {
		t.Fatal("an allow ran in a foreign conversation")
	}
}

func TestForeignConversationTagCommand(t *testing.T) {
	b, f, bus, _ := newCommandBridge(t)
	nameSession(t, bus, sidB, "bridge")
	bus.SetModVersion(sidB, "0.3.4")
	b.handleEvent("EvFC1", foreignMsg("UALEX", "bridge: !compact", "1700003000.000001", ""))
	if got := bus.Claim(sidB); len(got) != 1 || got[0].Command == nil || got[0].Command.Command != "compact" {
		t.Fatalf("sidB msgs = %+v", got)
	}
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	b.handleEvent("EvFC2", foreignMsg("UJANE", "bridge: !compact", "1700003000.000002", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); got["channel"] != "GMPIM1" || got["text"] != ownersOnlyCommands || got["thread_ts"] != "1700003000.000002" {
		t.Fatalf("reply = %+v", got)
	}
	if bus.Pending(sidB) {
		t.Fatal("a non-owner's command was delivered")
	}
}

func TestBotMentionThenTagIsATag(t *testing.T) {
	b, f, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "bridge")
	root := threadOf(t, b, bus)
	posts := len(f.callsTo("chat.postMessage"))
	for i, ev := range []messageEvent{
		msg("UALEX", "<@UBOT> bridge: top level", "1700003100.000001", ""),
		msg("UALEX", "<@UBOT> @bridge in a thread", "1700003100.000002", root),
		dmMsg("UALEX", "<@UBOT> bridge: in a DM", "1700003100.000003", ""),
		foreignMsg("UALEX", "<@UBOT> bridge: in a group", "1700003100.000004", ""),
	} {
		b.handleEvent("EvBM"+string(rune('a'+i)), ev)
		want := []string{"top level", "in a thread", "in a DM", "in a group"}[i]
		if got := bus.Claim(sidB); len(got) != 1 || got[0].Body != want {
			t.Fatalf("%d: sidB msgs = %+v", i, got)
		}
	}
	if bus.Pending(sidA) {
		t.Fatalf("sidA got %+v", bus.Claim(sidA))
	}
	drainJobs(t, b)
	// Only receipts went out, and the in-thread tag's "sent to": no help
	// replies.
	if n := len(f.callsTo("chat.postMessage")); n != posts+1 || lastPostText(f) != "→ sent to `bridge`" {
		t.Fatalf("posts %d -> %d: %q", posts, n, lastPostText(f))
	}
	// A mention that isn't a command or a tag still gets help in the
	// channel, and nothing in a group the bot was only added to.
	b.handleEvent("EvBMh", msg("UALEX", "<@UBOT> what now", "1700003100.000005", ""))
	b.handleEvent("EvBMg", foreignMsg("UALEX", "<@UBOT> what now", "1700003100.000006", ""))
	drainJobs(t, b)
	if n := len(f.callsTo("chat.postMessage")); n != posts+2 || !strings.Contains(lastPostText(f), "allow @person") {
		t.Fatalf("posts %d -> %d: %q", posts, n, lastPostText(f))
	}
}

func TestUnlinkedThreadReplyAfterTagGoesToTheTaggedAgent(t *testing.T) {
	b, f, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "bridge")
	b.handleEvent("EvTR1", foreignMsg("UALEX", "bridge: look at this", "1700003200.000001", ""))
	tagged := claimOne(t, bus, sidB)
	sendReply(t, b, bus, sidB, "looked", tagged.ID)
	if got := lastPost(t, f); got["channel"] != "GMPIM1" || got["thread_ts"] != "1700003200.000001" {
		t.Fatalf("answer = %+v", got)
	}
	// A plain reply under it reaches the tagged agent, marked as from a group.
	b.handleEvent("EvTR2", foreignMsg("UALEX", "and now?", "1700003200.000002", "1700003200.000001"))
	if m := claimOne(t, bus, sidB); m.Body != "and now?" || !m.FromUser || m.Via != agentbus.ViaGroup {
		t.Fatalf("thread reply = %+v", m)
	}
	// A tag inside a thread makes its thread reach that agent too.
	b.handleEvent("EvTR3", foreignMsg("UALEX", "flyer: you too", "1700003200.000004", "1700003200.000003"))
	claimOne(t, bus, sidA)
	b.handleEvent("EvTR4", foreignMsg("UALEX", "follow-up", "1700003200.000005", "1700003200.000003"))
	if m := claimOne(t, bus, sidA); m.Body != "follow-up" {
		t.Fatalf("follow-up = %+v", m)
	}
	// Not for someone who isn't allowed, and not in a thread no agent was
	// tagged in.
	b.handleEvent("EvTR5", foreignMsg("UEVE", "me too", "1700003200.000006", "1700003200.000001"))
	b.handleEvent("EvTR6", foreignMsg("UALEX", "chat", "1700003200.000008", "1700003200.000007"))
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatalf("delivered: %+v %+v", bus.Claim(sidA), bus.Claim(sidB))
	}
	// Under the agent's own top-level post there (an answer to its link
	// notice), a reply reaches it.
	b.handleEvent("EvTR7", foreignMsg("UALEX", "<@UBOT> link bridge", "1700003200.000009", ""))
	drainJobs(t, b)
	notice := claimOne(t, bus, sidB)
	b.handleEvent("EvTR8", foreignMsg("UALEX", "<@UBOT> unlink", "1700003200.000010", ""))
	drainJobs(t, b)
	claimOne(t, bus, sidB) // the unlink notice
	before := len(f.callsTo("chat.postMessage"))
	sendReply(t, b, bus, sidB, "posting at the top", notice.ID)
	if got := postsSince(f, before); len(got) != 0 {
		t.Fatalf("answer to the notice after unlink = %+v", got)
	}
	if n := claimNotice(t, bus, sidB); !strings.Contains(n.Body, "no longer reachable") {
		t.Fatalf("notice = %+v", n)
	}
}

func TestThreadUnderAgentTopLevelPostInGroup(t *testing.T) {
	b, f, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "bridge")
	b.handleEvent("EvAT1", foreignMsg("UALEX", "<@UBOT> link bridge", "1700003300.000001", ""))
	drainJobs(t, b)
	notice := claimOne(t, bus, sidB)
	sendReply(t, b, bus, sidB, "hello group", notice.ID)
	post := lastPost(t, f)
	if post["channel"] != "GMPIM1" || post["thread_ts"] != "" {
		t.Fatalf("post = %+v", post)
	}
	ts := lastPostTS(f)
	b.handleEvent("EvAT2", foreignMsg("UALEX", "<@UBOT> unlink", "1700003300.000002", ""))
	drainJobs(t, b)
	claimOne(t, bus, sidB)
	// Unlinked, a reply under the agent's own post still reaches it.
	b.handleEvent("EvAT3", foreignMsg("UALEX", "about that", "1700003300.000003", ts))
	if m := claimOne(t, bus, sidB); m.Body != "about that" {
		t.Fatalf("msg = %+v", m)
	}
}
