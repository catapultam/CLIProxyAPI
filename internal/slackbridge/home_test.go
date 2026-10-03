package slackbridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// newHomeBridge is newTestBridge with cfg changed by edit before New (for
// example home: dm), and an optional state file written first.
func newHomeBridge(t *testing.T, edit func(*Config), stateJSON string) (*Bridge, *fakeSlack, *agentbus.Store) {
	t.Helper()
	f := newFakeSlack(t)
	bus := agentbus.NewStore("", nil)
	bus.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	bus.Hello(sidB, "pc", "/work/other", "", true)
	cfg := testConfig(f, t.TempDir())
	if edit != nil {
		edit(&cfg)
	}
	if stateJSON != "" {
		if err := os.WriteFile(cfg.StatePath, []byte(stateJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := New(cfg, bus)
	if err != nil || b == nil {
		t.Fatalf("New = %v, %v", b, err)
	}
	b.backoff = func(int) time.Duration { return 0 }
	b.retryDelay = 0
	if err = b.resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	bus.SetBridge(b)
	return b, f, bus
}

func withHomeDM(c *Config) { c.Home = "dm" }

func withHomeDMNoChannel(c *Config) { c.Home = "DM"; c.Channel = "" }

// post sends body from sid to slack and runs the post.
func post(t *testing.T, b *Bridge, bus *agentbus.Store, sid, body string) {
	t.Helper()
	if _, err := bus.Send(sid, "slack", body, ""); err != nil {
		t.Fatal(err)
	}
	drainJobs(t, b)
}

func TestHomeDMNeedsNoChannel(t *testing.T) {
	f := newFakeSlack(t)
	cfg := testConfig(f, t.TempDir())
	withHomeDMNoChannel(&cfg)
	if !cfg.complete() {
		t.Fatal("home dm without a channel is incomplete")
	}
	cfg.Home = "channel"
	if cfg.complete() {
		t.Fatal("home channel without a channel is complete")
	}
	b, _, _ := newHomeBridge(t, withHomeDMNoChannel, "")
	if b.channelID != "" || b.homeChannelID != "DUALEX" {
		t.Fatalf("channel %q home %q", b.channelID, b.homeChannelID)
	}
}

func TestHomeDMFirstPostOpensThreadInOwnersDM(t *testing.T) {
	for _, edit := range []func(*Config){withHomeDM, withHomeDMNoChannel} {
		b, f, bus := newHomeBridge(t, edit, "")
		post(t, b, bus, sidA, "starting")
		post(t, b, bus, sidA, "done")
		opens := f.callsTo("conversations.open")
		if len(opens) != 1 || opens[0].Form.Get("users") != "UALEX" {
			t.Fatalf("conversations.open = %+v", opens)
		}
		posts := postsTo(f, "DUALEX")
		header := sessionHeader(outboundFor(bus, sidA, "flyer", ""))
		if len(posts) != 2 || posts[0]["thread_ts"] != "" || posts[0]["text"] != header+"\nstarting" {
			t.Fatalf("posts = %+v", posts)
		}
		ts, _ := b.state.thread(sidA)
		if posts[1]["thread_ts"] != ts || posts[1]["text"] != "done" {
			t.Fatalf("second post = %+v (thread %q)", posts[1], ts)
		}
		if len(postsTo(f, "CAGENTS")) != 0 {
			t.Fatal("a home dm post went to the channel")
		}
	}
}

func TestHomeDMImageOpensThreadInOwnersDM(t *testing.T) {
	b, f, bus := newHomeBridge(t, withHomeDMNoChannel, "")
	if err := b.PostImage(context.Background(), outboundFor(bus, sidA, "flyer", "chart"), "c.png", pngBytes); err != nil {
		t.Fatal(err)
	}
	posts := postsTo(f, "DUALEX")
	if len(posts) != 1 || !strings.HasSuffix(posts[0]["text"], "\nchart") {
		t.Fatalf("posts = %+v", posts)
	}
	done := f.callsTo("files.completeUploadExternal")
	ts, _ := b.state.thread(sidA)
	if len(done) != 1 || done[0].Form.Get("channel_id") != "DUALEX" || done[0].Form.Get("thread_ts") != ts {
		t.Fatalf("complete = %+v", done)
	}
}

func TestHomeDMThreadReplyIsDelivered(t *testing.T) {
	b, f, bus := newHomeBridge(t, withHomeDMNoChannel, "")
	post(t, b, bus, sidA, "hello")
	root, _ := b.state.thread(sidA)
	b.handleEvent("EvH1", dmMsg("UALEX", "check the logs", "1700000800.000001", root))
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 || !msgs[0].FromUser || msgs[0].Body != "check the logs" || msgs[0].Via != agentbus.ViaDM {
		t.Fatalf("msgs = %+v", msgs)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("DUALEX", "1700000800.000001"); len(got) != 1 || got[0] != reactionQueued {
		t.Fatalf("reactions = %v", got)
	}
	sendReply(t, b, bus, sidA, "found it", msgs[0].ID)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["thread_ts"] != root || got["text"] != "found it" {
		t.Fatalf("answer = %+v", got)
	}
}

func TestHomeDMTopLevelAddressedIsRouted(t *testing.T) {
	b, _, bus := newHomeBridge(t, withHomeDMNoChannel, "")
	b.handleEvent("EvH2", dmMsg("UALEX", "flyer: run the tests", "1700000801.000001", ""))
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 || msgs[0].Body != "run the tests" {
		t.Fatalf("msgs = %+v", msgs)
	}
	drainJobs(t, b)
	// A plain top-level message now goes to dmLast.
	b.handleEvent("EvH3", dmMsg("UALEX", "and the lint", "1700000802.000001", ""))
	if msgs = bus.Claim(sidA); len(msgs) != 1 || msgs[0].Body != "and the lint" {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestHomeSwitchKeepsOldThreadsWhereTheyAre(t *testing.T) {
	// sidA's thread is a legacy bare ts in the channel; home is now dm.
	b, f, bus := newHomeBridge(t, withHomeDM, `{"threads":{"`+sidA+`":"1600000000.000001"},"allowed":[]}`)
	post(t, b, bus, sidA, "still here")
	if got := lastPost(t, f); got["channel"] != "CAGENTS" || got["thread_ts"] != "1600000000.000001" {
		t.Fatalf("sidA's post = %+v", got)
	}
	post(t, b, bus, sidB, "new session")
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["thread_ts"] != "" {
		t.Fatalf("sidB's post = %+v", got)
	}
	// A reply in the old channel thread still reaches sidA.
	b.handleEvent("EvH4", msg("UALEX", "ok", "1700000803.000001", "1600000000.000001"))
	if msgs := bus.Claim(sidA); len(msgs) != 1 {
		t.Fatalf("msgs = %+v", msgs)
	}
	drainJobs(t, b)
	// A top-level post in the channel reaches the agent but doesn't become
	// sidB's home thread: home is the DM.
	b.handleEvent("EvH5", msg("UALEX", "pc/other-bbbbbb: hi", "1700000804.000001", ""))
	if msgs := bus.Claim(sidB); len(msgs) != 1 {
		t.Fatalf("msgs = %+v", msgs)
	}
	drainJobs(t, b)
	ref, _ := b.state.homeThread(sidB)
	if ref.Channel != "DUALEX" {
		t.Fatalf("sidB's home thread = %+v", ref)
	}
	if sid, ok := b.state.session("1700000804.000001"); !ok || sid != sidB {
		t.Fatalf("the channel post isn't linked to sidB: %q %v", sid, ok)
	}
}

func TestBareThreadTSMigratesOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slack-state.json")
	legacy := `{"threads":{"sid-a":"1.1","sid-b":{"channel":"DUALEX","ts":"2.2"}},"allowed":[]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if ref, ok := st.homeThread("sid-a"); !ok || ref.TS != "1.1" || ref.Channel != "" {
		t.Fatalf("sid-a = %+v %v", ref, ok)
	}
	if ref, ok := st.homeThread("sid-b"); !ok || ref.TS != "2.2" || ref.Channel != "DUALEX" {
		t.Fatalf("sid-b = %+v %v", ref, ok)
	}
	if sid, ok := st.session("1.1"); !ok || sid != "sid-a" {
		t.Fatalf("legacy thread not linked: %q %v", sid, ok)
	}
	st.fillThreadChannels("CAGENTS")
	if errFlush := st.flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var saved struct {
		Threads map[string]map[string]string `json:"threads"`
	}
	if errJSON := json.Unmarshal(data, &saved); errJSON != nil {
		t.Fatalf("saved threads aren't objects: %v\n%s", errJSON, data)
	}
	if got := saved.Threads["sid-a"]; got["channel"] != "CAGENTS" || got["ts"] != "1.1" {
		t.Fatalf("saved sid-a = %v", got)
	}
	if got := saved.Threads["sid-b"]; got["channel"] != "DUALEX" {
		t.Fatalf("saved sid-b = %v", got)
	}
}

func TestUnknownHomeFallsBackToChannel(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	b, f, bus := newHomeBridge(t, func(c *Config) { c.Home = "elsewhere" }, "")
	if b.home != homeChannel || b.homeChannelID != "CAGENTS" {
		t.Fatalf("home %q at %q", b.home, b.homeChannelID)
	}
	warned := false
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && strings.Contains(e.Message, "home") {
			warned = true
		}
	}
	if !warned {
		t.Fatal("no warning for an unknown home")
	}
	post(t, b, bus, sidA, "hi")
	if got := lastPost(t, f); got["channel"] != "CAGENTS" {
		t.Fatalf("post = %+v", got)
	}
}

// claimNotice returns the one message waiting for sid, which must be a
// bridge notice (neither a user's nor a guest's).
func claimNotice(t *testing.T, bus *agentbus.Store, sid string) agentbus.Message {
	t.Helper()
	m := claimOne(t, bus, sid)
	if m.FromUser || m.Guest || m.Command != nil || m.From != agentbus.SlackAddress {
		t.Fatalf("not a notice: %+v", m)
	}
	return m
}

func TestOwnerMovesHomeThreadToTheChannelAndBack(t *testing.T) {
	b, f, bus := newHomeBridge(t, withHomeDM, "")
	post(t, b, bus, sidA, "hello")
	oldRoot, _ := b.state.thread(sidA)
	header := sessionHeader(outboundFor(bus, sidA, "flyer", ""))

	b.handleEvent("EvM1", dmMsg("UALEX", "!channel", "1700000900.000001", oldRoot))
	drainJobs(t, b)
	chanPosts := postsTo(f, "CAGENTS")
	if len(chanPosts) != 1 || chanPosts[0]["thread_ts"] != "" || chanPosts[0]["text"] != header+" (moved from DM)" {
		t.Fatalf("channel posts = %+v", chanPosts)
	}
	newRoot, _ := b.state.thread(sidA)
	if ref, _ := b.state.homeThread(sidA); ref.Channel != "CAGENTS" || newRoot == oldRoot {
		t.Fatalf("home thread = %+v", ref)
	}
	pointer := lastPost(t, f)
	if pointer["channel"] != "DUALEX" || pointer["thread_ts"] != oldRoot ||
		!strings.HasPrefix(pointer["text"], "Moved to <#CAGENTS>") || !strings.Contains(pointer["text"], "https://example.slack.com/archives/CAGENTS/p") {
		t.Fatalf("pointer = %+v", pointer)
	}
	notice := claimNotice(t, bus, sidA)
	if !strings.Contains(notice.Body, "Your Slack home thread moved to") || !strings.Contains(notice.Body, "#agents") {
		t.Fatalf("notice = %q", notice.Body)
	}
	if b.state.home(sidA) != homeChannel {
		t.Fatalf("override = %q", b.state.home(sidA))
	}

	// Later posts go to the new thread; a reply in the old one still routes.
	post(t, b, bus, sidA, "over here now")
	if got := lastPost(t, f); got["channel"] != "CAGENTS" || got["thread_ts"] != newRoot {
		t.Fatalf("later post = %+v", got)
	}
	b.handleEvent("EvM2", dmMsg("UALEX", "still reading this", "1700000901.000001", oldRoot))
	if m := claimOne(t, bus, sidA); m.Body != "still reading this" {
		t.Fatalf("old thread reply = %+v", m)
	}
	drainJobs(t, b)

	// Already there.
	b.handleEvent("EvM3", msg("UALEX", "!channel", "1700000902.000001", newRoot))
	drainJobs(t, b)
	if got := lastPost(t, f); got["text"] != alreadyThere || got["thread_ts"] != newRoot {
		t.Fatalf("already there = %+v", got)
	}

	// !dm moves it back, into a new DM thread.
	b.handleEvent("EvM4", msg("UALEX", "!dm", "1700000903.000001", newRoot))
	drainJobs(t, b)
	backRoot, _ := b.state.thread(sidA)
	if ref, _ := b.state.homeThread(sidA); ref.Channel != "DUALEX" || backRoot == oldRoot || backRoot == newRoot {
		t.Fatalf("home thread = %+v", ref)
	}
	dmPosts := postsTo(f, "DUALEX")
	var reopened bool
	for _, p := range dmPosts {
		if p["thread_ts"] == "" && p["text"] == header+" (moved from <#CAGENTS>)" {
			reopened = true
		}
	}
	if !reopened {
		t.Fatalf("no new DM header: %+v", dmPosts)
	}
	if got := lastPost(t, f); got["channel"] != "CAGENTS" || got["thread_ts"] != newRoot || !strings.HasPrefix(got["text"], "Moved to DM") {
		t.Fatalf("pointer = %+v", got)
	}
	if notice = claimNotice(t, bus, sidA); !strings.Contains(notice.Body, "DM") {
		t.Fatalf("notice = %q", notice.Body)
	}
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("a move command reached an agent")
	}
}

func TestMovePhraseAndTopLevelForms(t *testing.T) {
	b, f, bus := newHomeBridge(t, withHomeDM, "")
	post(t, b, bus, sidA, "hello")
	oldRoot, _ := b.state.thread(sidA)
	b.handleEvent("EvP1", dmMsg("UALEX", "Take it to the channel!", "1700000910.000001", oldRoot))
	drainJobs(t, b)
	if ref, _ := b.state.homeThread(sidA); ref.Channel != "CAGENTS" {
		t.Fatalf("phrase didn't move: %+v", ref)
	}
	claimNotice(t, bus, sidA)

	// A top-level "name: !dm" in the channel moves it back, and the bridge
	// answers in place as well as in the old thread.
	b.handleEvent("EvP2", msg("UALEX", "flyer: move it to dm.", "1700000911.000001", ""))
	drainJobs(t, b)
	if ref, _ := b.state.homeThread(sidA); ref.Channel != "DUALEX" {
		t.Fatalf("top-level didn't move: %+v", ref)
	}
	claimNotice(t, bus, sidA)
	if got := lastPost(t, f); got["thread_ts"] != "1700000911.000001" || !strings.HasPrefix(got["text"], "Moved to DM") {
		t.Fatalf("in-place reply = %+v", got)
	}

	// Anywhere else: the help line, and nothing reaches an agent.
	for i, ev := range []messageEvent{
		msg("UALEX", "!channel", "1700000912.000001", ""),
		dmMsg("UALEX", "move to the channel", "1700000913.000001", ""),
	} {
		b.handleEvent("EvP3"+string(rune('a'+i)), ev)
		drainJobs(t, b)
		if got := lastPostText(f); got != howToMove {
			t.Fatalf("reply %d = %q", i, got)
		}
	}
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("a move command reached an agent")
	}
}

func TestNonOwnerCannotMoveHomeThread(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	hook := logtest.NewGlobal()
	defer hook.Reset()
	for i, text := range []string{"!dm", "take it to my DM"} {
		b.handleEvent("EvN"+string(rune('0'+i)), msg("UJANE", text, "1700000920.00000"+string(rune('1'+i)), root))
		drainJobs(t, b)
		if got := lastPostText(f); got != ownersOnlyMoves {
			t.Fatalf("reply = %q", got)
		}
	}
	if ref, _ := b.state.homeThread(sidA); ref.TS != root || ref.Channel != "CAGENTS" {
		t.Fatalf("home thread moved: %+v", ref)
	}
	if bus.Pending(sidA) || len(f.callsTo("conversations.open")) != 0 {
		t.Fatal("a refused move did something")
	}
	logged := false
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "refused") && strings.Contains(e.Message, "UJANE") {
			logged = true
		}
	}
	if !logged {
		t.Fatal("refusal not logged")
	}
}

func TestMoveToChannelWithoutChannel(t *testing.T) {
	b, f, bus := newHomeBridge(t, withHomeDMNoChannel, "")
	post(t, b, bus, sidA, "hello")
	root, _ := b.state.thread(sidA)
	b.handleEvent("EvC1", dmMsg("UALEX", "!channel", "1700000930.000001", root))
	drainJobs(t, b)
	if got := lastPost(t, f); got["text"] != noChannel || got["thread_ts"] != root {
		t.Fatalf("reply = %+v", got)
	}
	if ref, _ := b.state.homeThread(sidA); ref.TS != root {
		t.Fatalf("home thread moved: %+v", ref)
	}
	if bus.Pending(sidA) {
		t.Fatal("the agent was told about a move that didn't happen")
	}
	// !dm while already in the DM.
	b.handleEvent("EvC2", dmMsg("UALEX", "!dm", "1700000931.000001", root))
	drainJobs(t, b)
	if got := lastPostText(f); got != alreadyThere {
		t.Fatalf("reply = %q", got)
	}
}

func TestMoveCommandNamesAreBuiltIn(t *testing.T) {
	b, f, bus, dir := newCommandBridge(t)
	writeCommand(t, dir, "channel.yaml", promptYAML, mtime0)
	b.handleEvent("EvB1", msg("UALEX", "!commands", "1700000940.000001", ""))
	drainJobs(t, b)
	got := lastPostText(f)
	if !strings.Contains(got, "`!channel`") || !strings.Contains(got, "`!dm`") || strings.Contains(got, "`!channel` (prompt)") {
		t.Fatalf("listing = %q", got)
	}
	if bus.Pending(sidA) {
		t.Fatal("!commands reached an agent")
	}
}
