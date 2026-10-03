package slackbridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// deliveredID returns the id of the one message waiting for sid.
func deliveredID(t *testing.T, bus *agentbus.Store, sid string) string {
	t.Helper()
	msgs := bus.Claim(sid)
	if len(msgs) != 1 {
		t.Fatalf("messages for %s = %+v", sid, msgs)
	}
	return msgs[0].ID
}

// lastPostThread is the thread_ts of the last chat.postMessage.
func lastPostThread(t *testing.T, f *fakeSlack) string {
	t.Helper()
	posts := f.callsTo("chat.postMessage")
	if len(posts) == 0 {
		t.Fatal("no posts")
	}
	return posts[len(posts)-1].Form.Get("thread_ts")
}

// sendReply sends body from sid to slack with replyTo and runs the post.
func sendReply(t *testing.T, b *Bridge, bus *agentbus.Store, sid, body, replyTo string) {
	t.Helper()
	if _, err := bus.Send(sid, "slack", body, replyTo); err != nil {
		t.Fatal(err)
	}
	drainJobs(t, b)
}

func TestReplyToThreadReplyLandsInThatThread(t *testing.T) {
	b, f, bus := newTestBridge(t)
	own := threadOf(t, b, bus)
	// A second thread linked to sidA: a top-level post addressed to it.
	b.handleEvent("Ev1", msg("UALEX", "flyer: first", "1700000100.000001", ""))
	_ = deliveredID(t, bus, sidA)
	b.handleEvent("Ev2", msg("UALEX", "and a follow-up", "1700000100.000002", "1700000100.000001"))
	id := deliveredID(t, bus, sidA)
	drainJobs(t, b)

	sendReply(t, b, bus, sidA, "answer", id)
	if got := lastPostThread(t, f); got != "1700000100.000001" {
		t.Fatalf("reply posted in %q, want the asked thread (own thread %q)", got, own)
	}
	if text := lastPostText(f); text != "answer" {
		t.Fatalf("reply text = %q", text)
	}
	sendReply(t, b, bus, sidA, "status", "")
	if got := lastPostThread(t, f); got != own {
		t.Fatalf("post without reply_to went to %q, want own thread %q", got, own)
	}
}

func TestReplyToTopLevelPostLandsInItsThread(t *testing.T) {
	b, f, bus := newTestBridge(t)
	own := threadOf(t, b, bus)
	b.handleEvent("Ev1", msg("UALEX", "flyer: what changed?", "1700000200.000001", ""))
	id := deliveredID(t, bus, sidA)
	drainJobs(t, b)

	sendReply(t, b, bus, sidA, "this changed", id)
	if got := lastPostThread(t, f); got != "1700000200.000001" || got == own {
		t.Fatalf("reply posted in %q, want the top-level post's thread", got)
	}
}

func TestReplyToAnotherSessionsMessageUsesOwnThread(t *testing.T) {
	b, f, bus := newTestBridge(t)
	own := threadOf(t, b, bus)
	b.handleEvent("Ev1", msg("UALEX", "pc/other-bbbbbb: for B only", "1700000300.000001", ""))
	idB := deliveredID(t, bus, sidB)
	drainJobs(t, b)

	prev := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewLocal(log.StandardLogger())
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(prev) })

	sendReply(t, b, bus, sidA, "secret body", idB)
	if got := lastPostThread(t, f); got != own {
		t.Fatalf("reply posted in %q, want sidA's own thread %q", got, own)
	}
	var warned bool
	for _, e := range hook.AllEntries() {
		if e.Level != log.WarnLevel {
			continue
		}
		if strings.Contains(e.Message, "secret body") {
			t.Fatalf("warning logs the body: %q", e.Message)
		}
		if strings.Contains(e.Message, bus.Address(sidA)) && strings.Contains(e.Message, bus.Address(sidB)) {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warning naming both sessions; entries = %+v", hook.AllEntries())
	}
}

func TestReplyToUnknownOrExpiredIDUsesOwnThread(t *testing.T) {
	b, f, bus := newTestBridge(t)
	clock := newTestClock()
	b.state.now = clock.now
	own := threadOf(t, b, bus)

	sendReply(t, b, bus, sidA, "x", "m_0123456789abcdef")
	if got := lastPostThread(t, f); got != own {
		t.Fatalf("unknown id: posted in %q, want own thread %q", got, own)
	}

	b.handleEvent("Ev1", msg("UALEX", "flyer: old question", "1700000400.000001", ""))
	id := deliveredID(t, bus, sidA)
	drainJobs(t, b)
	clock.advance(replyTTL + time.Minute)
	sendReply(t, b, bus, sidA, "late answer", id)
	if got := lastPostThread(t, f); got != own {
		t.Fatalf("expired id: posted in %q, want own thread %q", got, own)
	}
}

func TestReplyMapRecordsChannelAndSession(t *testing.T) {
	b, _, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	b.handleEvent("Ev1", msg("UALEX", "go", "1700000500.000001", root))
	id := deliveredID(t, bus, sidA)
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	last := b.state.replies[len(b.state.replies)-1]
	if last.ID != id || last.Channel != "CAGENTS" || last.ThreadTS != root || last.Session != sidA {
		t.Fatalf("recorded = %+v (want %s in %s)", last, id, root)
	}
}

func TestImageWithReplyToUploadsIntoRecordedThread(t *testing.T) {
	b, f, bus := newTestBridge(t)
	own := threadOf(t, b, bus)
	b.handleEvent("Ev1", msg("UALEX", "flyer: show me the chart", "1700000600.000001", ""))
	id := deliveredID(t, bus, sidA)
	drainJobs(t, b)

	o := outboundFor(bus, sidA, "flyer", "here it is")
	o.ReplyTo = id
	if err := b.PostImage(context.Background(), o, "chart.png", pngBytes); err != nil {
		t.Fatal(err)
	}
	form := f.callsTo("files.completeUploadExternal")[0].Form
	if got := form.Get("thread_ts"); got != "1700000600.000001" || got == own {
		t.Fatalf("image went to %q, want the asked thread", got)
	}
	if form.Get("initial_comment") != "here it is" {
		t.Fatalf("caption = %q", form.Get("initial_comment"))
	}

	// The same id from another session falls back to that session's thread.
	o = outboundFor(bus, sidB, "", "")
	o.ReplyTo = id
	if err := b.PostImage(context.Background(), o, "b.png", pngBytes); err != nil {
		t.Fatal(err)
	}
	ownB, _ := b.state.thread(sidB)
	if got := f.callsTo("files.completeUploadExternal")[1].Form.Get("thread_ts"); got != ownB {
		t.Fatalf("sidB's image went to %q, want its own thread %q", got, ownB)
	}
}
