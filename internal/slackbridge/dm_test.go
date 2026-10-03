package slackbridge

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

// dmMsg is a message event in user's DM with the bot (channel "D" + user, as
// the fake's conversations.open names it).
func dmMsg(user, text, ts, threadTS string) messageEvent {
	return messageEvent{Type: "message", Channel: "D" + user, ChannelType: "im", User: user, Text: text, TS: ts, ThreadTS: threadTS}
}

// lastPost is the form of the last chat.postMessage.
func lastPost(t *testing.T, f *fakeSlack) map[string]string {
	t.Helper()
	posts := f.callsTo("chat.postMessage")
	if len(posts) == 0 {
		t.Fatal("no posts")
	}
	form := posts[len(posts)-1].Form
	return map[string]string{"channel": form.Get("channel"), "text": form.Get("text"), "thread_ts": form.Get("thread_ts")}
}

// sendDM sends body from sid to "slack@<label>" and runs the post.
func sendDM(t *testing.T, b *Bridge, bus *agentbus.Store, sid, label, body string) {
	t.Helper()
	if _, err := bus.Send(sid, "slack@"+label, body, ""); err != nil {
		t.Fatal(err)
	}
	drainJobs(t, b)
}

func TestOutboundDMOpensConversationAndPosts(t *testing.T) {
	b, f, bus := newTestBridge(t)
	sendDM(t, b, bus, sidA, "Alex", "build is green")
	opens := f.callsTo("conversations.open")
	if len(opens) != 1 || opens[0].Form.Get("users") != "UALEX" {
		t.Fatalf("conversations.open = %+v", opens)
	}
	first := lastPost(t, f)
	header := sessionHeader(outboundFor(bus, sidA, "flyer", ""))
	if first["channel"] != "DUALEX" || first["thread_ts"] != "" || first["text"] != header+"\nbuild is green" {
		t.Fatalf("first DM = %+v", first)
	}
	if _, ok := b.state.thread(sidA); ok {
		t.Fatal("a DM opened a channel thread")
	}
	if sid, ok := b.state.dmLast("UALEX"); !ok || sid != sidA {
		t.Fatalf("dmLast = %q %v", sid, ok)
	}

	// The channel is cached and the header goes out once per session.
	sendDM(t, b, bus, sidA, "alex", "and tests too")
	if n := len(f.callsTo("conversations.open")); n != 1 {
		t.Fatalf("conversations.open called %d times", n)
	}
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["text"] != "and tests too" || got["thread_ts"] != "" {
		t.Fatalf("second DM = %+v", got)
	}

	// Another session's first DM in the same channel gets its own header.
	sendDM(t, b, bus, sidB, "alex", "hi from b")
	headerB := sessionHeader(outboundFor(bus, sidB, "", ""))
	if got := lastPost(t, f); got["text"] != headerB+"\nhi from b" {
		t.Fatalf("sidB's first DM = %+v", got)
	}
	if sid, _ := b.state.dmLast("UALEX"); sid != sidB {
		t.Fatalf("dmLast = %q, want sidB", sid)
	}
}

func TestOutboundDMUnknownLabelIsRefused(t *testing.T) {
	b, f, bus := newTestBridge(t)
	for _, to := range []string{"slack@eve", "slack@jane", "slack@"} {
		if _, err := bus.Send(sidA, to, "x", ""); !errors.Is(err, agentbus.ErrUnknownTarget) {
			t.Fatalf("Send(%q) = %v", to, err)
		}
	}
	drainJobs(t, b)
	if len(f.callsTo("conversations.open")) != 0 || len(f.callsTo("chat.postMessage")) != 0 {
		t.Fatal("a refused DM reached Slack")
	}
}

func TestOutboundDMToRemovedUserIsDropped(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Send(sidA, "slack@jane", "secret plan", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.state.remove("UJANE"); err != nil {
		t.Fatal(err)
	}
	drainJobs(t, b)
	if len(f.callsTo("conversations.open")) != 0 || len(f.callsTo("chat.postMessage")) != 0 {
		t.Fatal("a DM went to a user removed before it was posted")
	}
}

func TestInboundDMFromAllowedUserIsDelivered(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvD1", dmMsg("UALEX", "flyer: check the logs <@UALEX>", "1700000700.000001", ""))
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 || !msgs[0].FromUser || msgs[0].SlackUser != "alex" || msgs[0].Via != agentbus.ViaDM || msgs[0].Body != "check the logs @alex" {
		t.Fatalf("msgs = %+v", msgs)
	}
	drainJobs(t, b)
	r := f.callsTo("reactions.add")
	if len(r) != 1 || r[0].Form.Get("channel") != "DUALEX" || r[0].Form.Get("timestamp") != "1700000700.000001" {
		t.Fatalf("reactions = %+v", r)
	}
	if _, ok := b.state.thread(sidA); ok {
		t.Fatal("a DM became the session's channel thread")
	}
	b.state.mu.Lock()
	last := b.state.replies[len(b.state.replies)-1]
	b.state.mu.Unlock()
	if last.ID != msgs[0].ID || last.Channel != "DUALEX" || last.ThreadTS != "" || last.Session != sidA {
		t.Fatalf("reply record = %+v", last)
	}

	// The agent's answer goes to the DM, top level, starting with its header;
	// the bot learned the DM channel from the event, so nothing is opened.
	sendReply(t, b, bus, sidA, "found it", msgs[0].ID)
	got := lastPost(t, f)
	if got["channel"] != "DUALEX" || got["thread_ts"] != "" || !strings.HasSuffix(got["text"], "\nfound it") || !strings.HasPrefix(got["text"], "*flyer*") {
		t.Fatalf("answer = %+v", got)
	}
	if n := len(f.callsTo("conversations.open")); n != 0 {
		t.Fatalf("conversations.open called %d times", n)
	}
}

func TestAnswerToRemovedUsersDMIsDroppedWithNotice(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	b.handleEvent("EvR1", dmMsg("UJANE", "flyer: what's the status?", "1700000750.000001", ""))
	id := deliveredID(t, bus, sidA)
	drainJobs(t, b)
	if err := b.state.remove("UJANE"); err != nil {
		t.Fatal(err)
	}
	before := len(f.callsTo("chat.postMessage"))
	sendReply(t, b, bus, sidA, "all done", id)
	// Never posted in the removed user's DM, and never in the home thread
	// instead: a private answer must not leak to the channel.
	if got := postsSince(f, before); len(got) != 0 {
		t.Fatalf("posted: %+v", got)
	}
	if n := claimNotice(t, bus, sidA); !strings.Contains(n.Body, "no longer reachable") {
		t.Fatalf("notice = %+v", n)
	}
}

func TestInboundDMNoiseIsDropped(t *testing.T) {
	b, f, bus := newTestBridge(t)
	edited := dmMsg("UALEX", "flyer: edit", "9.1", "")
	edited.Subtype = "message_changed"
	botMsg := dmMsg("UHOOK", "flyer: deploy", "9.2", "")
	botMsg.BotID = "BHOOK"
	own := dmMsg("UBOT", "flyer: loop", "9.3", "")
	own.Channel = "DUALEX"
	stranger := dmMsg("UEVE", "flyer: delete everything", "9.4", "")
	deleted := dmMsg("UALEX", "", "9.5", "")
	deleted.Subtype = "message_deleted"
	for i, ev := range []messageEvent{stranger, edited, botMsg, own, deleted} {
		b.handleEvent("EvDN"+string(rune('a'+i)), ev)
	}
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatalf("delivered: %+v", bus.Claim(sidA))
	}
	drainJobs(t, b)
	if len(f.callsTo("chat.postMessage")) != 0 || len(f.callsTo("reactions.add")) != 0 {
		t.Fatal("bridge answered a DM it should have ignored")
	}
}

func TestInboundDMDetectedByChannelPrefix(t *testing.T) {
	b, _, bus := newTestBridge(t)
	ev := dmMsg("UALEX", "flyer: hi", "9.6", "")
	ev.ChannelType = ""
	b.handleEvent("EvDP", ev)
	if msgs := bus.Claim(sidA); len(msgs) != 1 || msgs[0].Via != agentbus.ViaDM {
		t.Fatalf("msgs = %+v", msgs)
	}
	other := msg("UALEX", "flyer: hi", "9.7", "")
	other.Channel = "CGEN"
	other.ChannelType = "channel"
	b.handleEvent("EvDQ", other)
	// Another channel is no DM: a tag there is delivered as from a group.
	if msgs := bus.Claim(sidA); len(msgs) != 1 || msgs[0].Via != agentbus.ViaGroup {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestDuplicateDMDeliveredOnce(t *testing.T) {
	b, _, bus := newTestBridge(t)
	ev := dmMsg("UALEX", "flyer: once", "9.8", "")
	b.handleEvent("EvDD", ev)
	b.handleEvent("EvDD", ev)
	b.handleEvent("", ev)
	b.handleEvent("", ev)
	if msgs := bus.Claim(sidA); len(msgs) != 2 {
		// One by event id, one by channel:ts (no event id).
		t.Fatalf("msgs = %d", len(msgs))
	}
}

func TestDMPlainMessageGoesToDMLastUntilItExpires(t *testing.T) {
	b, f, bus := newTestBridge(t)
	clock := newTestClock()
	b.state.now = clock.now

	b.handleEvent("EvL0", dmMsg("UALEX", "what's up?", "1700000800.000001", ""))
	drainJobs(t, b)
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("a plain DM with no dmLast was delivered")
	}
	help := lastPost(t, f)
	if help["channel"] != "DUALEX" || help["thread_ts"] != "" || !strings.HasSuffix(help["text"], helpHow) || !strings.Contains(help["text"], "flyer") {
		t.Fatalf("help = %+v", help)
	}

	b.handleEvent("EvL1", dmMsg("UALEX", "flyer: start", "1700000800.000002", ""))
	_ = deliveredID(t, bus, sidA)
	b.handleEvent("EvL2", dmMsg("UALEX", "and keep going", "1700000800.000003", ""))
	if msgs := bus.Claim(sidA); len(msgs) != 1 || msgs[0].Body != "and keep going" || msgs[0].Via != agentbus.ViaDM {
		t.Fatalf("msgs = %+v", msgs)
	}

	// Addressing another agent moves dmLast.
	b.handleEvent("EvL3", dmMsg("UALEX", bus.Address(sidB)+": over to you", "1700000800.000004", ""))
	_ = deliveredID(t, bus, sidB)
	b.handleEvent("EvL4", dmMsg("UALEX", "status?", "1700000800.000005", ""))
	_ = deliveredID(t, bus, sidB)

	clock.advance(dmLastTTL - time.Minute)
	b.handleEvent("EvL5", dmMsg("UALEX", "still there?", "1700000800.000006", ""))
	_ = deliveredID(t, bus, sidB)

	// Each delivery refreshed it; it expires a full TTL after the last one.
	clock.advance(dmLastTTL + time.Minute)
	drainJobs(t, b)
	before := len(f.callsTo("chat.postMessage"))
	b.handleEvent("EvL6", dmMsg("UALEX", "hello?", "1700000800.000007", ""))
	drainJobs(t, b)
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("a plain DM went to an expired dmLast")
	}
	if len(f.callsTo("chat.postMessage")) != before+1 || !strings.HasSuffix(lastPost(t, f)["text"], helpHow) {
		t.Fatalf("no help after expiry: %+v", lastPost(t, f))
	}
}

func TestDMThreadReplyRoutesToTheAgent(t *testing.T) {
	b, f, bus := newTestBridge(t)
	sendDM(t, b, bus, sidB, "alex", "need a decision")
	if got := lastPost(t, f); got["channel"] != "DUALEX" {
		t.Fatalf("post = %+v", got)
	}
	rootTS := lastPostTS(f)
	// dmLast says sidA, but a thread reply goes to the thread's agent.
	b.handleEvent("EvT0", dmMsg("UALEX", "flyer: hi", "1700000900.000001", ""))
	_ = deliveredID(t, bus, sidA)
	b.handleEvent("EvT1", dmMsg("UALEX", "go with option B", "1700000900.000002", rootTS))
	msgs := bus.Claim(sidB)
	if len(msgs) != 1 || msgs[0].Body != "go with option B" || msgs[0].Via != agentbus.ViaDM {
		t.Fatalf("msgs = %+v", msgs)
	}
	if bus.Pending(sidA) {
		t.Fatal("the thread reply went to dmLast")
	}
	drainJobs(t, b)

	// The agent answers in that thread.
	sendReply(t, b, bus, sidB, "on it", msgs[0].ID)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["thread_ts"] != rootTS || got["text"] != "on it" {
		t.Fatalf("answer = %+v", got)
	}

	// A thread under the user's own top-level DM reaches the agent it went to.
	b.handleEvent("EvT2", dmMsg("UALEX", "one more thing", "1700000900.000003", "1700000900.000001"))
	if msgs := bus.Claim(sidA); len(msgs) != 1 || msgs[0].Body != "one more thing" {
		t.Fatalf("msgs = %+v", msgs)
	}

	// An unlinked thread gets one help reply, in that thread.
	before := len(f.callsTo("chat.postMessage"))
	b.handleEvent("EvT3", dmMsg("UALEX", "huh", "1700000900.000004", "1600000000.000001"))
	b.handleEvent("EvT4", dmMsg("UALEX", "hello?", "1700000900.000005", "1600000000.000001"))
	drainJobs(t, b)
	posts := f.callsTo("chat.postMessage")
	if len(posts) != before+1 || posts[len(posts)-1].Form.Get("thread_ts") != "1600000000.000001" || posts[len(posts)-1].Form.Get("channel") != "DUALEX" {
		t.Fatalf("unlinked replies = %d, last %+v", len(posts)-before, posts[len(posts)-1].Form)
	}
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("an unlinked thread reply was delivered")
	}
}

func TestDMCommandFromOwnerIsDelivered(t *testing.T) {
	b, f, bus, _ := newCommandBridge(t)
	b.handleEvent("EvK1", dmMsg("UALEX", "flyer: !compact", "1700001000.000001", ""))
	msgs := bus.Claim(sidA)
	want := agentbus.Command{Name: "compact", Kind: "slash", Command: "compact"}
	if len(msgs) != 1 || msgs[0].Command == nil || !reflect.DeepEqual(*msgs[0].Command, want) || msgs[0].SlackUserID != "UALEX" {
		t.Fatalf("msgs = %+v", msgs)
	}
	if _, ok := b.state.thread(sidA); ok {
		t.Fatal("a DM command became the session's channel thread")
	}
	drainJobs(t, b)
	r := f.callsTo("reactions.add")
	if len(r) != 1 || r[0].Form.Get("name") != "gear" || r[0].Form.Get("channel") != "DUALEX" {
		t.Fatalf("reactions = %+v", r)
	}
	// The mod's report lands in the DM.
	sendReply(t, b, bus, sidA, "done", msgs[0].ID)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["thread_ts"] != "" {
		t.Fatalf("report = %+v", got)
	}

	// A bare top-level !command never goes to dmLast: it needs "name: !cmd".
	// !commands lists, in the DM.
	b.handleEvent("EvK2", dmMsg("UALEX", "!clear", "1700001000.000002", ""))
	if msgs := bus.Claim(sidA); len(msgs) != 0 {
		t.Fatalf("msgs = %+v", msgs)
	}
	b.handleEvent("EvK3", dmMsg("UALEX", "!commands", "1700001000.000003", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || !strings.Contains(got["text"], "/command") {
		t.Fatalf("listing = %+v", got)
	}
}

func TestDMCommandFromNonOwnerIsRefused(t *testing.T) {
	b, f, bus, _ := newCommandBridge(t)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	b.handleEvent("EvK4", dmMsg("UJANE", "flyer: !compact", "1700001100.000001", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); got["channel"] != "DUJANE" || got["text"] != ownersOnlyCommands {
		t.Fatalf("reply = %+v", got)
	}
	// A plain message from the allowed non-owner is fine.
	b.handleEvent("EvK5", dmMsg("UJANE", "flyer: hello", "1700001100.000002", ""))
	if msgs := bus.Claim(sidA); len(msgs) != 1 || msgs[0].Command != nil || msgs[0].SlackUser != "jane" {
		t.Fatalf("msgs = %+v", msgs)
	}
	b.handleEvent("EvK6", dmMsg("UJANE", "!clear", "1700001100.000003", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); got["text"] != ownersOnlyCommands {
		t.Fatalf("reply = %+v", got)
	}
	if bus.Pending(sidA) {
		t.Fatalf("a non-owner's DM command was delivered: %+v", bus.Claim(sidA))
	}
}

func TestDMImageGoesToTheDM(t *testing.T) {
	b, f, bus := newTestBridge(t)
	o := outboundFor(bus, sidA, "flyer", "the chart")
	o.DM = "alex"
	if err := b.PostImage(context.Background(), o, "chart.png", pngBytes); err != nil {
		t.Fatal(err)
	}
	header := lastPost(t, f)
	if header["channel"] != "DUALEX" || header["thread_ts"] != "" || header["text"] != sessionHeader(o)+"\nthe chart" {
		t.Fatalf("header = %+v", header)
	}
	done := f.callsTo("files.completeUploadExternal")
	if len(done) != 1 || done[0].Form.Get("channel_id") != "DUALEX" || done[0].Form.Has("thread_ts") || done[0].Form.Get("initial_comment") != "" {
		t.Fatalf("complete = %+v", done)
	}

	// The second image carries its caption itself.
	o.Body = "another"
	if err := b.PostImage(context.Background(), o, "b.png", pngBytes); err != nil {
		t.Fatal(err)
	}
	if n := len(f.callsTo("chat.postMessage")); n != 1 {
		t.Fatalf("posts = %d", n)
	}
	if form := f.callsTo("files.completeUploadExternal")[1].Form; form.Get("initial_comment") != "another" || form.Get("channel_id") != "DUALEX" {
		t.Fatalf("complete = %+v", form)
	}

	o.DM = "eve"
	if err := b.PostImage(context.Background(), o, "c.png", pngBytes); err == nil || err.Error() != "user_not_allowed" {
		t.Fatalf("image to a non-allowed label: %v", err)
	}
}

func TestDMStatePersists(t *testing.T) {
	b, f, bus := newTestBridge(t)
	sendDM(t, b, bus, sidA, "alex", "hello")
	rootTS := lastPostTS(f)
	if errFlush := b.state.flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	reloaded, err := loadState(b.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if sid, ok := reloaded.dmLast("UALEX"); !ok || sid != sidA {
		t.Fatalf("dmLast = %q %v", sid, ok)
	}
	if sid, ok := reloaded.dmSession("DUALEX", rootTS); !ok || sid != sidA {
		t.Fatalf("dm link = %q %v", sid, ok)
	}
	if !reloaded.dmHeaded("DUALEX", sidA) || reloaded.dmHeaded("DUALEX", sidB) {
		t.Fatal("header record didn't survive the reload")
	}
}

func TestDMLinksExpireAndAreCapped(t *testing.T) {
	st, err := loadState("")
	if err != nil {
		t.Fatal(err)
	}
	clock := newTestClock()
	st.now = clock.now
	st.linkDM("D1", "1.1", "sid-a", true)
	clock.advance(replyTTL + time.Minute)
	if _, ok := st.dmSession("D1", "1.1"); ok {
		t.Fatal("an expired DM link still routes")
	}
	if st.dmHeaded("D1", "sid-a") {
		t.Fatal("an expired header record still counts")
	}
	for i := 0; i < maxDMLinks+5; i++ {
		st.linkDM("D1", replyID(i), "sid-a", false)
	}
	st.mu.Lock()
	n := len(st.dmLinks)
	st.mu.Unlock()
	if n != maxDMLinks {
		t.Fatalf("links = %d", n)
	}
	if _, ok := st.dmSession("D1", replyID(0)); ok {
		t.Fatal("the oldest link survived the cap")
	}
	if _, ok := st.dmSession("D1", replyID(maxDMLinks+4)); !ok {
		t.Fatal("the newest link is gone")
	}
}

// lastPostTS is the ts the fake gave the last chat.postMessage.
func lastPostTS(f *fakeSlack) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "1700000000." + strconv.Itoa(100000+f.nextTS)
}
