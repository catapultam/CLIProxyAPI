package slackbridge

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

const approvalNoteText = "_Needs approval: an allowed user reacts 👍 to approve._"

// reactionEv is a reaction_added event: user reacted name on message ts in
// channel.
func reactionEv(user, name, channel, ts string) messageEvent {
	return messageEvent{Type: "reaction_added", User: user, Reaction: name, Item: reactionItem{Type: "message", Channel: channel, TS: ts}}
}

// guestAsks links GMPIM1 to flyer, has guest bob ask for something there
// and returns his message as flyer got it.
func guestAsks(t *testing.T, b *Bridge, bus *agentbus.Store) agentbus.Message {
	t.Helper()
	linkGroup(t, b, bus, sidA, "flyer", "L1")
	b.handleEvent("EvGA1", foreignMsg("UBOB", "please restart the build", "1700007000.000001", ""))
	drainJobs(t, b)
	return claimOne(t, bus, sidA)
}

// confirmPost has flyer ask for approval in answer to replyTo and returns
// the request's bus id and the ts of the bot's post.
func confirmPost(t *testing.T, b *Bridge, bus *agentbus.Store, f *fakeSlack, body, replyTo string) (string, string) {
	t.Helper()
	sent, err := bus.Send(sidA, "slack", body, replyTo)
	if err != nil {
		t.Fatal(err)
	}
	drainJobs(t, b)
	return sent.ID, lastPostTS(f)
}

func TestConfirmPostsAnApprovalRequest(t *testing.T) {
	b, f, bus := newTestBridge(t)
	g := guestAsks(t, b, bus)

	// The first post there carries the header (the "opened" path).
	reqID, ts := confirmPost(t, b, bus, f, "  Confirm:  restart the build ", g.ID)
	if got := lastPost(t, f); got["channel"] != "GMPIM1" || got["thread_ts"] != "" || got["text"] != "*flyer*\nrestart the build\n"+approvalNoteText {
		t.Fatalf("post = %+v", got)
	}
	p, ok := b.state.approval("GMPIM1", ts)
	if !ok || p.Session != sidA || p.Request != reqID || p.Text != "restart the build" || p.Thread != "" || !p.Link || p.Done || p.Created.IsZero() {
		t.Fatalf("pending = %+v %v", p, ok)
	}

	// A later one is posted on its own.
	reqID2, ts2 := confirmPost(t, b, bus, f, "CONFIRM: delete <the> cache", g.ID)
	if got := lastPost(t, f); got["text"] != "delete &lt;the&gt; cache\n"+approvalNoteText {
		t.Fatalf("second post = %+v", got)
	}
	if p, ok := b.state.approval("GMPIM1", ts2); !ok || p.Request != reqID2 {
		t.Fatalf("second pending = %+v %v", p, ok)
	}

	// Anything else is a plain post, with no pending approval.
	_, ts3 := confirmPost(t, b, bus, f, "confirmed: all good", g.ID)
	if got := lastPost(t, f); got["text"] != "confirmed: all good" {
		t.Fatalf("plain post = %+v", got)
	}
	if _, ok := b.state.approval("GMPIM1", ts3); ok {
		t.Fatal("a plain post was recorded as an approval request")
	}
	_, ts4 := confirmPost(t, b, bus, f, "confirm:   ", g.ID)
	if _, ok := b.state.approval("GMPIM1", ts4); ok {
		t.Fatal("an empty confirm was recorded")
	}
}

func TestConfirmInAThreadKeepsTheThread(t *testing.T) {
	b, f, bus := newTestBridge(t)
	home := threadOf(t, b, bus)
	_, ts := confirmPost(t, b, bus, f, "confirm: deploy to prod", "")
	if got := lastPost(t, f); got["channel"] != "CAGENTS" || got["thread_ts"] != home || got["text"] != "deploy to prod\n"+approvalNoteText {
		t.Fatalf("post = %+v", got)
	}
	if p, ok := b.state.approval("CAGENTS", ts); !ok || p.Thread != home || p.Link {
		t.Fatalf("pending = %+v %v", p, ok)
	}
}

func TestAllowedThumbsUpApprovesOnce(t *testing.T) {
	b, f, bus := newTestBridge(t)
	g := guestAsks(t, b, bus)
	long := strings.Repeat("x", 250)
	reqID, ts := confirmPost(t, b, bus, f, "confirm: "+long, g.ID)

	b.handleEvent("EvAp1", reactionEv("UALEX", "+1", "GMPIM1", ts))
	m := claimOne(t, bus, sidA)
	if !m.FromUser || m.Guest || m.Approval != reqID || m.SlackUser != "alex" || m.Body != "approved: "+strings.Repeat("x", 200) || m.From != agentbus.SlackAddress {
		t.Fatalf("approval = %+v", m)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("GMPIM1", ts); !reflect.DeepEqual(got, []string{"white_check_mark"}) {
		t.Fatalf("reactions = %v", got)
	}
	if p, ok := b.state.approval("GMPIM1", ts); !ok || !p.Done {
		t.Fatalf("pending after approval = %+v %v", p, ok)
	}

	// The agent's answer to the approval goes where the request was.
	sendReply(t, b, bus, sidA, "restarted", m.ID)
	if got := lastPost(t, f); got["channel"] != "GMPIM1" || got["thread_ts"] != "" || got["text"] != "restarted" {
		t.Fatalf("answer = %+v", got)
	}

	// A second 👍, from anyone, does nothing.
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	posts := len(f.callsTo("chat.postMessage"))
	b.handleEvent("EvAp2", reactionEv("UJANE", "thumbsup", "GMPIM1", ts))
	b.handleEvent("EvAp3", reactionEv("UALEX", "+1", "GMPIM1", ts))
	drainJobs(t, b)
	if bus.Pending(sidA) {
		t.Fatalf("approved twice: %+v", bus.Claim(sidA))
	}
	if n := len(f.callsTo("chat.postMessage")); n != posts {
		t.Fatalf("posts %d -> %d", posts, n)
	}
}

func TestGuestThumbsUpIsIgnored(t *testing.T) {
	b, f, bus := newTestBridge(t)
	g := guestAsks(t, b, bus)
	_, ts := confirmPost(t, b, bus, f, "confirm: restart", g.ID)
	calls := len(f.methods())
	for i, ev := range []messageEvent{
		reactionEv("UBOB", "+1", "GMPIM1", ts),
		reactionEv("UCAROL", "thumbsup::skin-tone-2", "GMPIM1", ts),
		reactionEv("UBOT", "+1", "GMPIM1", ts),
		// An allowed user's other reactions, or a 👍 on another post.
		reactionEv("UALEX", "heart", "GMPIM1", ts),
		reactionEv("UALEX", "+1::skin-tone-9", "GMPIM1", ts),
		reactionEv("UALEX", "+1", "GMPIM1", "1700000000.999999"),
		reactionEv("UALEX", "+1", "CAGENTS", ts),
	} {
		b.handleEvent("EvGt"+string(rune('a'+i)), ev)
	}
	drainJobs(t, b)
	if bus.Pending(sidA) {
		t.Fatalf("delivered: %+v", bus.Claim(sidA))
	}
	if n := len(f.methods()); n != calls {
		t.Fatalf("Slack calls %d -> %d: %v", calls, n, f.methods()[calls:])
	}
	if p, ok := b.state.approval("GMPIM1", ts); !ok || p.Done {
		t.Fatalf("pending = %+v %v", p, ok)
	}
}

func TestSkinToneThumbsUpApproves(t *testing.T) {
	for _, name := range []string{"+1::skin-tone-3", "thumbsup::skin-tone-6", "thumbsup"} {
		t.Run(name, func(t *testing.T) {
			b, f, bus := newTestBridge(t)
			if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
				t.Fatal(err)
			}
			g := guestAsks(t, b, bus)
			reqID, ts := confirmPost(t, b, bus, f, "confirm: restart", g.ID)
			b.handleEvent("EvSk1", reactionEv("UJANE", name, "GMPIM1", ts))
			if m := claimOne(t, bus, sidA); m.Approval != reqID || m.SlackUser != "jane" || !m.FromUser {
				t.Fatalf("approval = %+v", m)
			}
		})
	}
}

func TestApprovalsExpireAndArePruned(t *testing.T) {
	clock := newTestClock()
	b, f, bus := newClockBridge(t, clock)
	_, ts := confirmPost(t, b, bus, f, "confirm: restart", "")
	clock.advance(approvalTTL + time.Second)
	b.handleEvent("EvEx1", reactionEv("UALEX", "+1", b.channelID, ts))
	if bus.Pending(sidA) {
		t.Fatalf("an expired request was approved: %+v", bus.Claim(sidA))
	}
	b.maintain()
	b.state.mu.Lock()
	n := len(b.state.approvals)
	b.state.mu.Unlock()
	if n != 0 {
		t.Fatalf("approvals after prune = %d", n)
	}
}

func TestApprovalsPersist(t *testing.T) {
	b, f, bus := newTestBridge(t)
	reqID, ts := confirmPost(t, b, bus, f, "confirm: restart", "")
	if err := b.state.flush(); err != nil {
		t.Fatal(err)
	}
	st, err := loadState(b.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := st.approval("CAGENTS", ts); !ok || p.Request != reqID || p.Session != sidA || p.Text != "restart" {
		t.Fatalf("reloaded = %+v %v", p, ok)
	}
}
