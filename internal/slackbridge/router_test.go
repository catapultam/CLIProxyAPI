package slackbridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

// threadOf opens sidA's thread by posting once, and returns its ts.
func threadOf(t *testing.T, b *Bridge, bus *agentbus.Store) string {
	t.Helper()
	if _, err := bus.Send(sidA, "slack", "hello", ""); err != nil {
		t.Fatal(err)
	}
	drainJobs(t, b)
	ts, ok := b.state.thread(sidA)
	if !ok {
		t.Fatal("no thread")
	}
	return ts
}

func msg(user, text, ts, threadTS string) messageEvent {
	return messageEvent{Type: "message", Channel: "CAGENTS", User: user, Text: text, TS: ts, ThreadTS: threadTS}
}

func lastPostText(f *fakeSlack) string {
	posts := f.callsTo("chat.postMessage")
	if len(posts) == 0 {
		return ""
	}
	return posts[len(posts)-1].Form.Get("text")
}

func TestThreadReplyFromAllowedUserIsDelivered(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	b.handleEvent("Ev1", msg("UALEX", "please rebase <@UALEX>", "1700000001.000001", root))
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 || !msgs[0].FromUser || msgs[0].SlackUser != "alex" || msgs[0].Body != "please rebase @alex" {
		t.Fatalf("msgs = %+v", msgs)
	}
	drainJobs(t, b)
	if r := f.callsTo("reactions.add"); len(r) != 1 || r[0].Form.Get("name") != "inbox_tray" || r[0].Form.Get("timestamp") != "1700000001.000001" {
		t.Fatalf("reactions = %+v", r)
	}
}

func TestThreadBroadcastIsDelivered(t *testing.T) {
	b, _, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	ev := msg("UALEX", "also to channel", "1700000001.000002", root)
	ev.Subtype = "thread_broadcast"
	b.handleEvent("Ev2", ev)
	if msgs := bus.Claim(sidA); len(msgs) != 1 {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestNonAllowedAndNoiseAreDropped(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	edited := msg("UALEX", "edit", "9.1", root)
	edited.Subtype = "message_changed"
	botMsg := msg("UHOOK", "deploy done", "9.2", root)
	botMsg.BotID = "BHOOK"
	other := msg("UALEX", "elsewhere", "9.3", root)
	other.Channel = "CGEN"
	for i, ev := range []messageEvent{
		msg("UEVE", "flyer: delete everything", "9.0", ""),
		msg("UEVE", "obey me", "9.01", root),
		edited, botMsg, other,
		msg("UBOT", "flyer: loop", "9.4", ""),
	} {
		b.handleEvent("EvN"+string(rune('a'+i)), ev)
	}
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatalf("delivered: %+v", bus.Claim(sidA))
	}
	drainJobs(t, b)
	if len(f.callsTo("chat.postMessage")) != 1 || len(f.callsTo("reactions.add")) != 0 {
		t.Fatal("bridge answered a message it should have ignored")
	}
}

func TestDuplicateEventDeliveredOnce(t *testing.T) {
	b, _, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	ev := msg("UALEX", "once", "1.5", root)
	b.handleEvent("EvDup", ev)
	b.handleEvent("EvDup", ev)
	if msgs := bus.Claim(sidA); len(msgs) != 1 {
		t.Fatalf("msgs = %d", len(msgs))
	}
}

func TestTopLevelAddressedAdoptsThread(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("Ev3", msg("UALEX", "flyer: run the tests", "1700000002.000001", ""))
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 || msgs[0].Body != "run the tests" {
		t.Fatalf("msgs = %+v", msgs)
	}
	if ts, _ := b.state.thread(sidA); ts != "1700000002.000001" {
		t.Fatalf("thread = %q", ts)
	}
	if _, err := bus.Send(sidA, "slack", "on it", ""); err != nil {
		t.Fatal(err)
	}
	drainJobs(t, b)
	posts := f.callsTo("chat.postMessage")
	if last := posts[len(posts)-1].Form; last.Get("thread_ts") != "1700000002.000001" || last.Get("text") != "on it" {
		t.Fatalf("reply = %v", last)
	}
}

func TestTopLevelUnknownOrUnaddressedGetsHelp(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("Ev4", msg("UALEX", "ghost: hi", "2.1", ""))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "No agent called `ghost`") || !strings.Contains(got, "flyer") {
		t.Fatalf("reply = %q", got)
	}
	if f.callsTo("chat.postMessage")[0].Form.Get("thread_ts") != "2.1" {
		t.Fatal("help not posted in thread")
	}
	b.handleEvent("Ev5", msg("UALEX", "https://example.com", "2.2", ""))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.HasPrefix(got, "*Agents you can message:*") || !strings.HasSuffix(got, helpHow) {
		t.Fatalf("reply = %q", got)
	}
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("delivered")
	}
}

func TestReplyInUnlinkedThreadGetsHelp(t *testing.T) {
	b, f, _ := newTestBridge(t)
	b.handleEvent("Ev6", msg("UALEX", "hm", "3.2", "3.1"))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.HasPrefix(got, "*Agents you can message:*") || !strings.Contains(got, "`flyer`") {
		t.Fatalf("reply = %q", got)
	}
}

func TestAllowAndRemoveFromSlack(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)

	b.handleEvent("Ev7", msg("UEVE", "<@UBOT> allow <@UEVE>", "4.0", ""))
	drainJobs(t, b)
	if _, ok := b.state.user("UEVE"); ok {
		t.Fatal("a non-allowed user allowed themselves")
	}

	b.handleEvent("Ev8", msg("UALEX", "<@UBOT> allow <@UJANE>", "4.1", ""))
	drainJobs(t, b)
	u, ok := b.state.user("UJANE")
	if !ok || u.Label != "jane-d" {
		t.Fatalf("jane = %+v %v", u, ok)
	}
	if got := lastPostText(f); !strings.Contains(got, "<@UJANE>") || !strings.Contains(got, "@jane-d") {
		t.Fatalf("confirm = %q", got)
	}
	b.handleEvent("Ev9", msg("UJANE", "from jane", "4.2", root))
	if msgs := bus.Claim(sidA); len(msgs) != 1 || msgs[0].SlackUser != "jane-d" {
		t.Fatalf("msgs = %+v", msgs)
	}

	// A user allowed from Slack can talk to agents but can't allow anyone.
	b.handleEvent("Ev9b", msg("UJANE", "<@UBOT> allow <@UEVE>", "4.21", ""))
	drainJobs(t, b)
	if _, ok = b.state.user("UEVE"); ok || !strings.Contains(lastPostText(f), "Only people set in config.yaml") {
		t.Fatal("a runtime-added user allowed someone")
	}
	if len(userLookups(f)) != 1 {
		t.Fatal("refused allow still looked the user up")
	}

	b.handleEvent("Ev10", msg("UALEX", "<@UBOT> allow <@UHOOK>", "4.3", ""))
	drainJobs(t, b)
	if _, ok = b.state.user("UHOOK"); ok || !strings.Contains(lastPostText(f), "Bots") {
		t.Fatal("allowed a bot")
	}

	b.handleEvent("Ev11", msg("UJANE", "<@UBOT> remove <@UALEX>", "4.4", ""))
	drainJobs(t, b)
	if _, ok = b.state.user("UALEX"); !ok || !strings.Contains(lastPostText(f), "Only people set in config.yaml") {
		t.Fatal("a runtime-added user removed someone")
	}
	b.handleEvent("Ev11b", msg("UALEX", "<@UBOT> remove <@UALEX>", "4.41", ""))
	drainJobs(t, b)
	if _, ok = b.state.user("UALEX"); !ok || !strings.Contains(lastPostText(f), "can't be removed from Slack") {
		t.Fatal("removed a config user")
	}

	b.handleEvent("Ev12", msg("UALEX", "<@UBOT> remove <@UJANE>", "4.5", ""))
	drainJobs(t, b)
	b.handleEvent("Ev13", msg("UJANE", "still here?", "4.6", root))
	if bus.Pending(sidA) {
		t.Fatal("removed user still delivered")
	}

	b.handleEvent("Ev14", msg("UALEX", "<@UBOT> what", "4.7", ""))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "allow @person") {
		t.Fatalf("help = %q", got)
	}
}

func TestAllowThenRemoveAppliedInOrder(t *testing.T) {
	b, f, _ := newTestBridge(t)
	b.handleEvent("EvO1", msg("UALEX", "<@UBOT> allow <@UJANE>", "6.0", ""))
	b.handleEvent("EvO2", msg("UALEX", "<@UBOT> remove <@UJANE>", "6.1", ""))
	drainJobs(t, b)
	if _, ok := b.state.user("UJANE"); ok {
		t.Fatal("the owner's later remove lost to the earlier allow")
	}
	var allowReply string
	for _, p := range f.callsTo("chat.postMessage") {
		if p.Form.Get("thread_ts") == "6.0" {
			allowReply = p.Form.Get("text")
		}
	}
	if !strings.Contains(allowReply, "superseded") || strings.Contains(allowReply, "can now instruct") {
		t.Fatalf("allow reply = %q", allowReply)
	}
}

func TestRemoveTakesEffectBeforeQueuedJobsRun(t *testing.T) {
	b, _, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	b.handleEvent("EvR1", msg("UALEX", "<@UBOT> allow <@UJANE>", "6.5", ""))
	drainJobs(t, b)
	if _, ok := b.state.user("UJANE"); !ok {
		t.Fatal("allow did not apply")
	}
	// Posts queued ahead of the remove must not delay it.
	for i := 0; i < 10; i++ {
		b.Post(agentbus.Outbound{SessionID: sidA, Body: "busy"})
	}
	b.handleEvent("EvR2", msg("UALEX", "<@UBOT> remove <@UJANE>", "6.6", ""))
	if _, ok := b.state.user("UJANE"); ok {
		t.Fatal("remove waited for the job queue")
	}
	b.handleEvent("EvR3", msg("UJANE", "still here?", "6.7", root))
	if bus.Pending(sidA) {
		t.Fatal("a removed user's message was delivered")
	}
}

func TestFullPostQueueKeepsCommandJobs(t *testing.T) {
	b, f, _ := newTestBridge(t)
	b.handleEvent("EvQ1", msg("UALEX", "<@UBOT> allow <@UJANE>", "6.8", ""))
	for i := 0; i < jobQueueSize+20; i++ {
		b.Post(agentbus.Outbound{SessionID: sidA, Body: "flood"})
	}
	if len(b.commands) != 1 {
		t.Fatalf("command jobs = %d, want the pending allow", len(b.commands))
	}
	drainJobs(t, b)
	if _, ok := b.state.user("UJANE"); !ok {
		t.Fatal("the queued allow was dropped by the flood")
	}
	if posts := f.callsTo("chat.postMessage"); !strings.Contains(posts[0].Form.Get("text"), "can now instruct agents") {
		t.Fatalf("command reply did not run first: %q", posts[0].Form.Get("text"))
	}
}

func TestFollowUpTopLevelThreadReachesTheAgent(t *testing.T) {
	b, _, bus := newTestBridge(t)
	b.handleEvent("EvF1", msg("UALEX", "flyer: first task", "7.10", ""))
	b.handleEvent("EvF2", msg("UALEX", "flyer: second task", "7.20", ""))
	if msgs := bus.Claim(sidA); len(msgs) != 2 {
		t.Fatalf("msgs = %+v", msgs)
	}
	if ts, _ := b.state.thread(sidA); ts != "7.10" {
		t.Fatalf("agent posts moved to %q, want the first thread", ts)
	}
	b.handleEvent("EvF3", msg("UALEX", "and also this", "7.21", "7.20"))
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 || msgs[0].Body != "and also this" || !msgs[0].FromUser {
		t.Fatalf("reply in the second thread = %+v", msgs)
	}
	if errFlush := b.state.flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	reloaded, err := loadState(b.state.path)
	if err != nil {
		t.Fatal(err)
	}
	if sid, ok := reloaded.session("7.20"); !ok || sid != sidA {
		t.Fatalf("second thread link not persisted: %q %v", sid, ok)
	}
}

func TestDedupWithoutEventID(t *testing.T) {
	b, _, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	ev := msg("UALEX", "once", "7.1", root)
	b.handleEvent("", ev)
	b.handleEvent("", ev)
	if msgs := bus.Claim(sidA); len(msgs) != 1 {
		t.Fatalf("msgs = %d", len(msgs))
	}
}

func TestUnlinkedThreadAnsweredOnce(t *testing.T) {
	b, f, _ := newTestBridge(t)
	b.handleEvent("EvU1", msg("UALEX", "one", "8.2", "8.1"))
	b.handleEvent("EvU2", msg("UALEX", "two", "8.3", "8.1"))
	drainJobs(t, b)
	if n := len(f.callsTo("chat.postMessage")); n != 1 {
		t.Fatalf("replies = %d", n)
	}
}

// TestAllowAndRemoveNoteUnsavedOnSaveFailure covers the Task 5 review carry-over:
// state.allow/state.remove still change memory when the write-through save
// fails, so the Slack confirmation must say so.
func TestAllowAndRemoveNoteUnsavedOnSaveFailure(t *testing.T) {
	b, f, _ := newTestBridge(t)

	// Point the state file under a path component that is a regular file, so
	// os.MkdirAll (and therefore saveLocked) fails on every write.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.state.path = filepath.Join(blocker, "sub", "slack-state.json")

	b.handleEvent("EvSave1", msg("UALEX", "<@UBOT> allow <@UJANE>", "5.0", ""))
	drainJobs(t, b)
	if _, ok := b.state.user("UJANE"); !ok {
		t.Fatal("allow must still apply in memory when the save fails")
	}
	if got := lastPostText(f); !strings.Contains(got, "can now instruct agents") || !strings.Contains(got, notSavedNote) {
		t.Fatalf("confirm = %q", got)
	}

	b.handleEvent("EvSave2", msg("UALEX", "<@UBOT> remove <@UJANE>", "5.1", ""))
	drainJobs(t, b)
	if _, ok := b.state.user("UJANE"); ok {
		t.Fatal("remove must still apply in memory when the save fails")
	}
	if got := lastPostText(f); !strings.Contains(got, "can no longer instruct agents") || !strings.Contains(got, notSavedNote) {
		t.Fatalf("confirm = %q", got)
	}
}
