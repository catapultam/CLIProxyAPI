package slackbridge

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

// groupID is the group DM the fake opens for alex, bob and carol.
const groupID = "GUALEXUBOBUCAROL"

// postsTo lists the chat.postMessage forms sent to channel.
func postsTo(f *fakeSlack, channel string) []map[string]string {
	var out []map[string]string
	for _, c := range f.callsTo("chat.postMessage") {
		if c.Form.Get("channel") == channel {
			out = append(out, map[string]string{"channel": channel, "text": c.Form.Get("text"), "thread_ts": c.Form.Get("thread_ts")})
		}
	}
	return out
}

// claimOne returns the one message waiting for sid.
func claimOne(t *testing.T, bus *agentbus.Store, sid string) agentbus.Message {
	t.Helper()
	msgs := bus.Claim(sid)
	if len(msgs) != 1 {
		t.Fatalf("messages for %s = %+v", sid, msgs)
	}
	return msgs[0]
}

// linkGroup links the group DM GMPIM1 to sid's agent (name) as alex, and
// returns the link notice.
func linkGroup(t *testing.T, b *Bridge, bus *agentbus.Store, sid, name, eventID string) agentbus.Message {
	t.Helper()
	b.handleEvent(eventID, foreignMsg("UALEX", "<@UBOT> link "+name, "1700009000."+eventID, ""))
	drainJobs(t, b)
	if l, ok := b.state.conversation("GMPIM1"); !ok || l.Session != sid {
		t.Fatalf("link = %+v %v", l, ok)
	}
	return claimOne(t, bus, sid)
}

func TestChatOpensALinkedGroupDM(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvChat1", msg("UALEX", "<@UBOT> chat <@UBOB> <@UCAROL|carol> <@UALEX> with flyer", "1700004000.000001", ""))
	drainJobs(t, b)

	opens := f.callsTo("conversations.open")
	if len(opens) != 1 || opens[0].Form.Get("users") != "UALEX,UBOB,UCAROL" {
		t.Fatalf("conversations.open = %+v", opens)
	}
	l, ok := b.state.conversation(groupID)
	if !ok || l.Session != sidA || l.By != "UALEX" || l.At.IsZero() {
		t.Fatalf("link = %+v %v", l, ok)
	}
	intro := postsTo(f, groupID)
	if len(intro) != 1 || intro[0]["thread_ts"] != "" ||
		intro[0]["text"] != "Linked to *flyer*. Messages here go to that agent. <@UALEX>'s messages are instructions; everyone else's are guest input." {
		t.Fatalf("intro = %+v", intro)
	}
	if got := lastPost(t, f); got["channel"] != "CAGENTS" || got["thread_ts"] != "1700004000.000001" ||
		!strings.HasPrefix(got["text"], "Opened a group DM with <@UALEX>, <@UBOB>, <@UCAROL>, linked to *flyer*.") {
		t.Fatalf("confirm = %+v", got)
	}

	notice := claimOne(t, bus, sidA)
	if notice.FromUser || notice.Guest || notice.From != agentbus.SlackAddress || notice.SlackUser != "" ||
		!strings.Contains(notice.Body, "a Slack group DM with @alex, @bob, @carol (opened by @alex)") {
		t.Fatalf("notice = %+v", notice)
	}
	if r, ok := b.state.replyTarget(notice.ID, sidA); !ok || r.Channel != groupID || r.ThreadTS != "" || !r.Link {
		t.Fatalf("notice record = %+v %v", r, ok)
	}

	// The agent posts in the group DM by answering the notice.
	sendReply(t, b, bus, sidA, "hi all", notice.ID)
	if got := lastPost(t, f); got["channel"] != groupID || got["thread_ts"] != "" || !strings.HasSuffix(got["text"], "\nhi all") {
		t.Fatalf("answer = %+v", got)
	}

	// The guests' names were looked up while opening, so their first
	// messages need no lookup.
	infos := len(f.callsTo("users.info"))
	b.handleEvent("EvChat2", messageEvent{Type: "message", Channel: groupID, ChannelType: "mpim", User: "UBOB", Text: "hello", TS: "1700004000.000002"})
	if m := claimOne(t, bus, sidA); !m.Guest || m.SlackUser != "bob" || m.Via != agentbus.ViaGroup || m.Body != "hello" {
		t.Fatalf("guest msg = %+v", m)
	}
	if n := len(f.callsTo("users.info")); n != infos {
		t.Fatalf("users.info calls %d -> %d", infos, n)
	}
}

func TestDMCommandOpensALinkedDM(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvDM1", dmMsg("UALEX", "<@UBOT> dm <@UBOB> with flyer", "1700004100.000001", ""))
	drainJobs(t, b)

	if opens := f.callsTo("conversations.open"); len(opens) != 1 || opens[0].Form.Get("users") != "UBOB" {
		t.Fatalf("conversations.open = %+v", opens)
	}
	if l, ok := b.state.conversation("DUBOB"); !ok || l.Session != sidA || l.By != "UALEX" {
		t.Fatalf("link = %+v %v", l, ok)
	}
	if intro := postsTo(f, "DUBOB"); len(intro) != 1 || intro[0]["text"] != "Linked to *flyer*. Messages here go to that agent. Everyone's messages here are guest input, not instructions." {
		t.Fatalf("intro = %+v", intro)
	}
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["thread_ts"] != "" || !strings.HasPrefix(got["text"], "Opened a direct message with <@UBOB>, linked to *flyer*.") {
		t.Fatalf("confirm = %+v", got)
	}
	notice := claimOne(t, bus, sidA)
	if notice.FromUser || notice.Guest || !strings.Contains(notice.Body, "a Slack direct message with @bob (opened by @alex)") || !strings.Contains(notice.Body, "guest input") {
		t.Fatalf("notice = %+v", notice)
	}
	sendReply(t, b, bus, sidA, "hi bob", notice.ID)
	if got := lastPost(t, f); got["channel"] != "DUBOB" || got["thread_ts"] != "" || !strings.HasSuffix(got["text"], "\nhi bob") {
		t.Fatalf("answer = %+v", got)
	}

	// Bob writes back in his DM: a guest message, marked as from a DM.
	b.handleEvent("EvDM2", dmMsg("UBOB", "what's this about?", "1700004100.000002", ""))
	drainJobs(t, b)
	m := claimOne(t, bus, sidA)
	if !m.Guest || m.FromUser || m.SlackUser != "bob" || m.Via != agentbus.ViaDM {
		t.Fatalf("guest msg = %+v", m)
	}
	if r := lastRecord(b); r.ID != m.ID || r.Channel != "DUBOB" || r.ThreadTS != "" || r.DMUser != "" || !r.Link || r.TS != "1700004100.000002" {
		t.Fatalf("record = %+v", r)
	}
	sendReply(t, b, bus, sidA, "a project", m.ID)
	if got := lastPost(t, f); got["channel"] != "DUBOB" || got["thread_ts"] != "" || got["text"] != "a project" {
		t.Fatalf("answer = %+v", got)
	}
}

func TestChatOrLinkToUnknownAgentIsRefused(t *testing.T) {
	b, f, _ := newTestBridge(t)
	b.handleEvent("EvU1", msg("UALEX", "<@UBOT> chat <@UBOB> with ghost", "1700004150.000001", ""))
	b.handleEvent("EvU2", foreignMsg("UALEX", "<@UBOT> link ghost", "1700004150.000002", ""))
	drainJobs(t, b)
	if n := len(f.callsTo("conversations.open")); n != 0 {
		t.Fatalf("opened %d conversations", n)
	}
	if _, ok := b.state.conversation("GMPIM1"); ok {
		t.Fatal("linked to an unknown agent")
	}
	if got := lastPost(t, f); !strings.HasPrefix(got["text"], "No agent called `ghost`.") || got["channel"] != "GMPIM1" {
		t.Fatalf("reply = %+v", got)
	}
}

func TestNonOwnersCannotOpenLinkOrUnlink(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	for i, ev := range []messageEvent{
		msg("UJANE", "<@UBOT> chat <@UBOB> with flyer", "1700004200.000001", ""),
		dmMsg("UJANE", "<@UBOT> dm <@UBOB> with flyer", "1700004200.000002", ""),
		foreignMsg("UJANE", "<@UBOT> link flyer", "1700004200.000003", ""),
	} {
		b.handleEvent("EvNO"+string(rune('a'+i)), ev)
		drainJobs(t, b)
		if got := lastPost(t, f); got["text"] != ownersOnlyLinks || got["channel"] != ev.Channel {
			t.Fatalf("%d: reply = %+v", i, got)
		}
	}
	if n := len(f.callsTo("conversations.open")); n != 0 {
		t.Fatalf("opened %d conversations", n)
	}
	if _, ok := b.state.conversation("GMPIM1"); ok {
		t.Fatal("a non-owner linked a conversation")
	}

	// In a linked conversation, a non-owner can't unlink, and neither can a
	// guest; a guest can't link, allow or remove either.
	linkGroup(t, b, bus, sidA, "flyer", "L1")
	for i, ev := range []messageEvent{
		foreignMsg("UJANE", "<@UBOT> unlink", "1700004200.000010", ""),
		foreignMsg("UBOB", "<@UBOT> unlink", "1700004200.000011", ""),
		foreignMsg("UBOB", "<@UBOT> link bridge", "1700004200.000012", ""),
		foreignMsg("UBOB", "<@UBOT> allow <@UBOB>", "1700004200.000013", ""),
	} {
		b.handleEvent("EvNL"+string(rune('a'+i)), ev)
		drainJobs(t, b)
	}
	if l, ok := b.state.conversation("GMPIM1"); !ok || l.Session != sidA {
		t.Fatalf("link = %+v %v", l, ok)
	}
	if _, ok := b.state.user("UBOB"); ok {
		t.Fatal("a guest allowed themselves")
	}
	if bus.Pending(sidA) {
		t.Fatalf("delivered: %+v", bus.Claim(sidA))
	}
	if got := lastPost(t, f); got["text"] != ownersOnly {
		t.Fatalf("last refusal = %+v", got)
	}
}

func TestLinkRelinkAndUnlinkInPlace(t *testing.T) {
	b, f, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "bridge")
	b.handleEvent("EvL1", foreignMsg("UALEX", "<@UBOT> link flyer", "1700004300.000001", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); got["channel"] != "GMPIM1" || got["thread_ts"] != "1700004300.000001" ||
		!strings.HasPrefix(got["text"], "Linked this conversation to *flyer*. Messages here go to that agent.") {
		t.Fatalf("confirm = %+v", got)
	}
	if n := claimOne(t, bus, sidA); !strings.Contains(n.Body, "You were linked to a Slack group DM by @alex") {
		t.Fatalf("notice = %+v", n)
	}

	// Relinking replaces the link and says so; both agents are told.
	b.handleEvent("EvL2", foreignMsg("UALEX", "<@UBOT> link bridge", "1700004300.000002", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); !strings.Contains(got["text"], "It was linked to `"+bus.Address(sidA)+"` before") {
		t.Fatalf("relink confirm = %+v", got)
	}
	if l, _ := b.state.conversation("GMPIM1"); l.Session != sidB {
		t.Fatalf("link = %+v", l)
	}
	if n := claimOne(t, bus, sidA); !strings.Contains(n.Body, "unlinked") {
		t.Fatalf("unlink notice = %+v", n)
	}
	bNotice := claimOne(t, bus, sidB)

	// The old agent's answers to the old notice no longer land there.
	b.handleEvent("EvL2g", foreignMsg("UBOB", "anyone?", "1700004300.000003", ""))
	drainJobs(t, b)
	guest := claimOne(t, bus, sidB)
	if !guest.Guest {
		t.Fatalf("guest msg = %+v", guest)
	}

	// Unlink.
	b.handleEvent("EvL3", foreignMsg("UALEX", "<@UBOT> unlink", "1700004300.000004", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); !strings.HasPrefix(got["text"], "Unlinked this conversation from `"+bus.Address(sidB)+"`.") {
		t.Fatalf("unlink confirm = %+v", got)
	}
	if _, ok := b.state.conversation("GMPIM1"); ok {
		t.Fatal("still linked")
	}
	if n := claimOne(t, bus, sidB); !strings.Contains(n.Body, "unlinked") {
		t.Fatalf("unlink notice = %+v", n)
	}
	// After the unlink the agent can't post there through the notice or a
	// guest's message: its answer is dropped (never posted in its own thread
	// instead), and it is told.
	for _, id := range []string{bNotice.ID, guest.ID} {
		before := len(f.callsTo("chat.postMessage"))
		sendReply(t, b, bus, sidB, "still here?", id)
		if got := postsSince(f, before); len(got) != 0 {
			t.Fatalf("answer to %s after unlink posted: %+v", id, got)
		}
		if n := claimNotice(t, bus, sidB); !strings.Contains(n.Body, "no longer reachable") {
			t.Fatalf("notice = %+v", n)
		}
	}
	// Guests no longer reach it.
	b.handleEvent("EvL4", foreignMsg("UBOB", "hello?", "1700004300.000005", ""))
	drainJobs(t, b)
	if bus.Pending(sidB) {
		t.Fatalf("delivered after unlink: %+v", bus.Claim(sidB))
	}
	b.handleEvent("EvL5", foreignMsg("UALEX", "<@UBOT> unlink", "1700004300.000006", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); got["text"] != notLinked {
		t.Fatalf("second unlink = %+v", got)
	}
}

func TestMainChannelCantBeLinked(t *testing.T) {
	b, f, bus := newTestBridge(t)
	for i, text := range []string{"<@UBOT> link flyer", "<@UBOT> unlink"} {
		b.handleEvent("EvM"+string(rune('a'+i)), msg("UALEX", text, "1700004400.00000"+string(rune('1'+i)), ""))
		drainJobs(t, b)
		if got := lastPost(t, f); got["text"] != mainNotLinkable || got["channel"] != "CAGENTS" {
			t.Fatalf("%q: reply = %+v", text, got)
		}
	}
	if _, ok := b.state.conversation("CAGENTS"); ok {
		t.Fatal("main channel linked")
	}
	if bus.Pending(sidA) {
		t.Fatal("notice for a refused link")
	}
}

func TestLinkedConversationInbound(t *testing.T) {
	b, f, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "bridge")
	linkGroup(t, b, bus, sidA, "flyer", "L1")

	// An allowed user's message is their instruction, marked as from a group.
	b.handleEvent("EvI1", foreignMsg("UALEX", "status?", "1700004500.000001", ""))
	if m := claimOne(t, bus, sidA); !m.FromUser || m.Guest || m.SlackUser != "alex" || m.Via != agentbus.ViaGroup || m.Body != "status?" {
		t.Fatalf("allowed msg = %+v", m)
	}

	// A guest's message is guest input, labelled with their display name.
	// A tag doesn't take a guest to another agent.
	b.handleEvent("EvI2", foreignMsg("UBOB", "bridge: hi <@UALEX>", "1700004500.000002", ""))
	if bus.Pending(sidA) {
		t.Fatal("delivered before the guest's name was looked up")
	}
	drainJobs(t, b)
	m := claimOne(t, bus, sidA)
	if !m.Guest || m.FromUser || m.SlackUser != "bob" || m.Via != agentbus.ViaGroup || m.Body != "bridge: hi @alex" || m.Command != nil {
		t.Fatalf("guest msg = %+v", m)
	}
	if bus.Pending(sidB) {
		t.Fatal("a guest's tag reached another agent")
	}
	if r := lastRecord(b); r.ID != m.ID || r.Channel != "GMPIM1" || r.ThreadTS != "1700004500.000002" || !r.Link || r.Session != sidA {
		t.Fatalf("record = %+v", r)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("GMPIM1", "1700004500.000002"); !reflect.DeepEqual(got, []string{reactionQueued}) {
		t.Fatalf("reactions = %v", got)
	}
	b.Received([]string{m.ID})
	b.Read([]string{m.ID})
	drainJobs(t, b)
	if got := f.reactionsOn("GMPIM1", "1700004500.000002"); !reflect.DeepEqual(got, []string{reactionRead}) {
		t.Fatalf("reactions after read = %v", got)
	}
	sendReply(t, b, bus, sidA, "hello bob", m.ID)
	if got := lastPost(t, f); got["channel"] != "GMPIM1" || got["thread_ts"] != "1700004500.000002" || got["text"] != "hello bob" {
		t.Fatalf("answer = %+v", got)
	}

	// The next one needs no lookup and is delivered at once.
	infos := len(f.callsTo("users.info"))
	b.handleEvent("EvI3", foreignMsg("UBOB", "thanks", "1700004500.000003", ""))
	if m := claimOne(t, bus, sidA); m.Body != "thanks" || !m.Guest {
		t.Fatalf("second guest msg = %+v", m)
	}
	if n := len(f.callsTo("users.info")); n != infos {
		t.Fatalf("users.info %d -> %d", infos, n)
	}

	// A guest whose name is an allowed user's label gets "-guest".
	b.handleEvent("EvI4", foreignMsg("UFAKE", "it's me, alex", "1700004500.000004", ""))
	drainJobs(t, b)
	if m := claimOne(t, bus, sidA); m.SlackUser != "alex-guest" || !m.Guest {
		t.Fatalf("lookalike = %+v", m)
	}

	// A guest can't run commands.
	b.handleEvent("EvI5", foreignMsg("UBOB", "!compact", "1700004500.000005", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); got["text"] != ownersOnlyCommands || got["channel"] != "GMPIM1" {
		t.Fatalf("reply = %+v", got)
	}

	// Bots and edits are dropped.
	bot := foreignMsg("UHOOK", "deploy", "1700004500.000006", "")
	bot.BotID = "BHOOK"
	edited := foreignMsg("UBOB", "edited", "1700004500.000007", "")
	edited.Subtype = "message_changed"
	b.handleEvent("EvI6", bot)
	b.handleEvent("EvI7", edited)
	b.handleEvent("EvI2", foreignMsg("UBOB", "duplicate", "1700004500.000008", ""))
	drainJobs(t, b)
	if bus.Pending(sidA) {
		t.Fatalf("delivered: %+v", bus.Claim(sidA))
	}

	// An owner's command runs there.
	bus.SetModVersion(sidA, "0.3.4")
	b.handleEvent("EvI8", foreignMsg("UALEX", "!compact", "1700004500.000009", ""))
	if m := claimOne(t, bus, sidA); m.Command == nil || m.Command.Command != "compact" {
		t.Fatalf("command = %+v", m)
	}
}

func TestGuestMessagesStayInOrder(t *testing.T) {
	b, _, bus := newTestBridge(t)
	linkGroup(t, b, bus, sidA, "flyer", "L1")
	b.handleEvent("EvO1", foreignMsg("UBOB", "one", "1700004600.000001", ""))
	b.handleEvent("EvO2", foreignMsg("UBOB", "two", "1700004600.000002", ""))
	// Run only the first lookup job, which caches bob's name.
	j := <-b.jobs
	if err := j(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.handleEvent("EvO3", foreignMsg("UBOB", "three", "1700004600.000003", ""))
	drainJobs(t, b)
	var bodies []string
	for _, m := range bus.Claim(sidA) {
		bodies = append(bodies, m.Body)
	}
	if !reflect.DeepEqual(bodies, []string{"one", "two", "three"}) {
		t.Fatalf("order = %v", bodies)
	}
}

func TestUnlinkedForeignConversationDeliversNothingFromGuests(t *testing.T) {
	b, f, bus := newTestBridge(t)
	nameSession(t, bus, sidB, "bridge")
	for i, text := range []string{"bridge: hi", "hello", "<@UBOT> link bridge", "!compact"} {
		b.handleEvent("EvUF"+string(rune('a'+i)), foreignMsg("UBOB", text, "1700004700.00000"+string(rune('1'+i)), ""))
	}
	nothingSent(t, b, f, bus)
	if n := len(f.callsTo("users.info")); n != 0 {
		t.Fatalf("looked up a stranger %d times", n)
	}
}

func TestLinkedOwnDMFallsBackToTheLink(t *testing.T) {
	b, _, bus := newTestBridge(t)
	b.handleEvent("EvOD1", dmMsg("UALEX", "<@UBOT> link flyer", "1700004800.000001", ""))
	drainJobs(t, b)
	claimOne(t, bus, sidA) // the notice
	b.handleEvent("EvOD2", dmMsg("UALEX", "plain", "1700004800.000002", ""))
	if m := claimOne(t, bus, sidA); m.Body != "plain" || !m.FromUser || m.Via != agentbus.ViaDM {
		t.Fatalf("msg = %+v", m)
	}
}

func TestConversationLinksPersist(t *testing.T) {
	b, _, bus := newTestBridge(t)
	linkGroup(t, b, bus, sidA, "flyer", "L1")
	want, _ := b.state.conversation("GMPIM1")
	reloaded, err := loadState(b.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.conversation("GMPIM1")
	if !ok || got.Session != sidA || got.By != "UALEX" || !got.At.Equal(want.At) || !got.Seen.Equal(want.Seen) {
		t.Fatalf("reloaded = %+v %v, want %+v", got, ok, want)
	}
}

// newClockBridge is newTestBridge with bus and state on clock.
func newClockBridge(t *testing.T, clock *testClock) (*Bridge, *fakeSlack, *agentbus.Store) {
	t.Helper()
	f := newFakeSlack(t)
	bus := agentbus.NewStore("", clock.now)
	bus.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	cfg := testConfig(f, t.TempDir())
	b, err := New(cfg, bus)
	if err != nil || b == nil {
		t.Fatalf("New = %v, %v", b, err)
	}
	b.state.now = clock.now
	b.retryDelay = 0
	if err = b.resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	bus.SetBridge(b)
	return b, f, bus
}

func TestLinkPrunedAfterSevenDaysAbsent(t *testing.T) {
	clock := newTestClock()
	b, f, bus := newClockBridge(t, clock)
	linkGroup(t, b, bus, sidA, "flyer", "L1")

	// Six days on, the session shows up again: the link lives on.
	clock.advance(6 * 24 * time.Hour)
	bus.Touch(sidA)
	b.handleEvent("EvP1", foreignMsg("UALEX", "still there?", "1700005000.000001", ""))
	if m := claimOne(t, bus, sidA); m.Body != "still there?" {
		t.Fatalf("msg = %+v", m)
	}
	// Seven days after that, absent the whole time: the link is gone.
	clock.advance(7*24*time.Hour + time.Minute)
	b.handleEvent("EvP2", foreignMsg("UALEX", "hello?", "1700005000.000002", ""))
	b.handleEvent("EvP3", foreignMsg("UBOB", "hello?", "1700005000.000003", ""))
	drainJobs(t, b)
	if bus.Pending(sidA) {
		t.Fatalf("delivered over a pruned link: %+v", bus.Claim(sidA))
	}
	if _, ok := b.state.conversation("GMPIM1"); ok {
		t.Fatal("link not pruned")
	}
	// The next save leaves it out of the file.
	b.state.setDMLast("UALEX", sidA)
	reloaded, err := loadState(b.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := reloaded.convs["GMPIM1"]; present {
		t.Fatal("pruned link still saved")
	}
	_ = f
}

func TestLinkPrunedOnLoad(t *testing.T) {
	clock := newTestClock()
	path := filepath.Join(t.TempDir(), "slack-state.json")
	st, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	st.now = clock.now
	if _, err = st.linkConversation("GOLD", sidA, "UALEX", kindGroup, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = st.linkConversation("GNEW", sidB, "UALEX", kindGroup, nil); err != nil {
		t.Fatal(err)
	}

	// Eight days later the proxy restarts. sidA was last on the bus at the
	// start; sidB is on it now.
	clock.advance(8 * 24 * time.Hour)
	bus := agentbus.NewStore("", clock.now)
	bus.Hello(sidB, "pc", "/b", "", true)
	reloaded, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.now = clock.now
	b := &Bridge{bus: bus, state: reloaded}
	b.refreshLinks()
	if _, ok := reloaded.conversation("GOLD"); ok {
		t.Fatal("absent session's link survived the load")
	}
	if l, ok := reloaded.conversation("GNEW"); !ok || l.Session != sidB || !l.Seen.Equal(clock.t) {
		t.Fatalf("live link = %+v %v", l, ok)
	}
	again, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := again.convs["GOLD"]; present {
		t.Fatal("pruned link still saved")
	}
}
