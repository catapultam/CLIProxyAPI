package slackbridge

import (
	"context"
	"strings"
	"testing"
)

// Item 10: outside the main channel an answer lands at the level the
// message was written at: top-level stays top-level, a thread reply stays in
// its thread.

func TestGroupTopLevelMessageGetsTopLevelAnswer(t *testing.T) {
	b, f, bus := newTestBridge(t)
	linkGroup(t, b, bus, sidA, "flyer", "Lv1")
	b.handleEvent("EvLv2", foreignMsg("UALEX", "how is it going?", "1700006000.000002", ""))
	m := claimOne(t, bus, sidA)
	sendReply(t, b, bus, sidA, "fine", m.ID)
	got := lastPost(t, f)
	if got["channel"] != "GMPIM1" || got["thread_ts"] != "" || !strings.HasSuffix(got["text"], "fine") {
		t.Fatalf("answer = %+v", got)
	}
	if r := lastRecordFor(b, m.ID); !r.TopLevel || r.TS != "1700006000.000002" {
		t.Fatalf("record = %+v", r)
	}
	// A guest's top-level message too.
	b.handleEvent("EvLv3", foreignMsg("UBOB", "and me?", "1700006000.000003", ""))
	drainJobs(t, b)
	g := claimOne(t, bus, sidA)
	sendReply(t, b, bus, sidA, "you too", g.ID)
	if got := lastPost(t, f); got["channel"] != "GMPIM1" || got["thread_ts"] != "" || got["text"] != "you too" {
		t.Fatalf("answer to guest = %+v", got)
	}
}

func TestGroupThreadMessageGetsThreadAnswer(t *testing.T) {
	b, f, bus := newTestBridge(t)
	linkGroup(t, b, bus, sidA, "flyer", "Lv4")
	b.handleEvent("EvLv5", foreignMsg("UBOB", "in a thread", "1700006000.000005", "1700006000.000001"))
	drainJobs(t, b)
	m := claimOne(t, bus, sidA)
	sendReply(t, b, bus, sidA, "answered there", m.ID)
	if got := lastPost(t, f); got["channel"] != "GMPIM1" || got["thread_ts"] != "1700006000.000001" || got["text"] != "answered there" {
		t.Fatalf("answer = %+v", got)
	}
}

func TestTopLevelDMGetsTopLevelAnswer(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvLv6", dmMsg("UALEX", "flyer: status?", "1700006000.000006", ""))
	id := deliveredID(t, bus, sidA)
	sendReply(t, b, bus, sidA, "green", id)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["thread_ts"] != "" || !strings.HasSuffix(got["text"], "green") {
		t.Fatalf("answer = %+v", got)
	}
}

func TestMainChannelAnswersStayInThreads(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvLv7", msg("UALEX", "flyer: top", "1700006000.000007", ""))
	id := deliveredID(t, bus, sidA)
	sendReply(t, b, bus, sidA, "under it", id)
	if got := lastPost(t, f); got["channel"] != "CAGENTS" || got["thread_ts"] != "1700006000.000007" {
		t.Fatalf("answer = %+v", got)
	}
	if r := lastRecordFor(b, id); r.TopLevel {
		t.Fatalf("main-channel record marked top-level: %+v", r)
	}
}

func TestGroupTopLevelImageIsPostedTopLevel(t *testing.T) {
	b, f, bus := newTestBridge(t)
	linkGroup(t, b, bus, sidA, "flyer", "Lv8")
	b.handleEvent("EvLv9", foreignMsg("UBOB", "show me", "1700006000.000009", ""))
	drainJobs(t, b)
	g := claimOne(t, bus, sidA)
	o := outboundFor(bus, sidA, "flyer", "here")
	o.ReplyTo = g.ID
	if err := b.PostImage(context.Background(), o, "shot.png", pngBytes); err != nil {
		t.Fatal(err)
	}
	done := f.callsTo("files.completeUploadExternal")
	if len(done) != 1 || done[0].Form.Get("channel_id") != "GMPIM1" || done[0].Form.Has("thread_ts") {
		t.Fatalf("complete = %+v", done)
	}
}

func TestLinkNoticeAnswerIsTopLevel(t *testing.T) {
	b, f, bus := newTestBridge(t)
	notice := linkGroup(t, b, bus, sidA, "flyer", "Lv10")
	sendReply(t, b, bus, sidA, "hello all", notice.ID)
	if got := lastPost(t, f); got["channel"] != "GMPIM1" || got["thread_ts"] != "" || !strings.HasSuffix(got["text"], "hello all") {
		t.Fatalf("answer = %+v", got)
	}
}

// lastRecordFor is the reply record of msgID.
func lastRecordFor(b *Bridge, msgID string) replyRecord {
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	r, _ := b.state.replyLocked(msgID)
	return r
}
