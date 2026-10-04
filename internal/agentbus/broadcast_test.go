package agentbus

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestAllIsReserved(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "ALL", true)
	if id, ok := s.Resolve("all"); ok {
		t.Fatalf("hello claimed all: %s", id)
	}
	if err := s.SetName(sidA, "all"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("SetName(all) = %v", err)
	}
	if err := s.SetName(sidA, "All"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("SetName(All) = %v", err)
	}
	// A session still named "all" from before never resolves.
	s.mu.Lock()
	s.byID[sidA].Name = "all"
	s.mu.Unlock()
	if _, ok := s.Resolve(" All "); ok {
		t.Fatal("all resolved to a session")
	}
	if _, _, err := s.DeliverVia("all", "hi", "alex", ""); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("DeliverVia(all) = %v", err)
	}
	s.Hello(sidB, "pc", "/b", "", true)
	if _, err := s.Send(sidB, "all", "hi", ""); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("Send(all) = %v", err)
	}
}

func TestDeliverBroadcastMarksTheMessage(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "flyer", true)
	s.SetModVersion(sidA, MinCommandModVersion)
	if _, _, err := s.DeliverBroadcast(sidA, "status please", "alex", ViaDM, []string{"beta", "gamma"}); err != nil {
		t.Fatal(err)
	}
	if m := claimOneMsg(t, s, sidA); !m.Broadcast || !m.FromUser || m.SlackUser != "alex" || m.Via != ViaDM || m.Body != "status please" ||
		!reflect.DeepEqual(m.BroadcastTo, []string{"beta", "gamma"}) {
		t.Fatalf("msg = %+v", m)
	}
	if _, _, err := s.DeliverCommandBroadcast(sidA, Command{Name: "compact", Kind: CommandSlash, Command: "compact"}, "alex", "UALEX", "", []string{"beta"}); err != nil {
		t.Fatal(err)
	}
	if m := claimOneMsg(t, s, sidA); !m.Broadcast || m.Command == nil || !m.FromUser || !reflect.DeepEqual(m.BroadcastTo, []string{"beta"}) {
		t.Fatalf("command = %+v", m)
	}
	// The ordinary entry points never mark one, or set BroadcastTo.
	if _, _, err := s.DeliverVia(sidA, "x", "alex", ""); err != nil {
		t.Fatal(err)
	}
	if m := claimOneMsg(t, s, sidA); m.Broadcast || m.BroadcastTo != nil {
		t.Fatalf("DeliverVia marked a broadcast: %+v", m)
	}
}

// Task: BroadcastTo is capped at MaxBroadcastTo entries even when the Slack
// bridge hands it more (a broadcast to a large fleet).
func TestDeliverBroadcastCapsBroadcastTo(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "flyer", true)
	names := make([]string, 40)
	for i := range names {
		names[i] = fmt.Sprintf("n%02d", i)
	}
	if _, _, err := s.DeliverBroadcast(sidA, "status please", "alex", "", names); err != nil {
		t.Fatal(err)
	}
	m := claimOneMsg(t, s, sidA)
	if len(m.BroadcastTo) != MaxBroadcastTo {
		t.Fatalf("broadcast_to = %d entries, want %d", len(m.BroadcastTo), MaxBroadcastTo)
	}
	if !reflect.DeepEqual(m.BroadcastTo, names[:MaxBroadcastTo]) {
		t.Fatalf("broadcast_to = %v", m.BroadcastTo)
	}
}

func TestHTTPSendCannotSetBroadcast(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.Hello(sidB, "pc", "/b", "", true)
	body := `{"from_session":"` + sidA + `","to":"` + s.Address(sidB) + `","body":"everyone listen","broadcast":true,"broadcast_to":["beta","gamma"],"from_user":true}`
	if w := do(r, http.MethodPost, "/v1/agentbus/send", body); w.Code != http.StatusOK {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	w := do(r, http.MethodGet, "/v1/agentbus/inbox?session="+sidB, "")
	if !strings.Contains(w.Body.String(), "everyone listen") || strings.Contains(w.Body.String(), "broadcast") {
		t.Fatalf("inbox = %s", w.Body)
	}
}

func TestLoadKeepsBroadcastOnlyOnSlackUserMessages(t *testing.T) {
	bigList := make([]string, 40)
	for i := range bigList {
		bigList[i] = fmt.Sprintf("n%02d", i)
	}
	for _, tc := range []struct {
		m         Message
		want      bool
		wantToLen int
	}{
		{Message{ID: "m_01", From: SlackAddress, Body: "x", FromUser: true, Broadcast: true, BroadcastTo: []string{"beta"}}, true, 1},
		{Message{ID: "m_02", From: "pc/a-aaaaaa", Body: "x", Broadcast: true, BroadcastTo: []string{"beta"}}, false, 0},
		{Message{ID: "m_03", From: SlackAddress, Body: "x", Guest: true, Broadcast: true, BroadcastTo: []string{"beta"}}, false, 0},
		{Message{ID: "m_04", From: SlackAddress, Body: "x", Broadcast: true, BroadcastTo: []string{"beta"}}, false, 0},
		// BroadcastTo without Broadcast (never set by a real entry point, but
		// a loaded file could be tampered with) is dropped too.
		{Message{ID: "m_05", From: SlackAddress, Body: "x", FromUser: true, BroadcastTo: []string{"beta"}}, false, 0},
		// An oversized BroadcastTo is trimmed even on an otherwise valid broadcast.
		{Message{ID: "m_06", From: SlackAddress, Body: "x", FromUser: true, Broadcast: true, BroadcastTo: bigList}, true, MaxBroadcastTo},
	} {
		m := tc.m
		cleanLoadedMessage(&m)
		if m.Broadcast != tc.want {
			t.Fatalf("%s: broadcast = %v", m.ID, m.Broadcast)
		}
		if len(m.BroadcastTo) != tc.wantToLen {
			t.Fatalf("%s: broadcast_to = %v, want len %d", m.ID, m.BroadcastTo, tc.wantToLen)
		}
	}
}

func TestInjectBroadcastHeader(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	if _, _, err := s.DeliverBroadcast(sidA, "status please", "alex", "", []string{"beta", "gamma", "delta"}); err != nil {
		t.Fatal(err)
	}
	got := injectedText(t, r, seen)
	if !strings.Contains(got, " from alex via Slack (broadcast from alex to all 4 agents; also sent to: beta, gamma, delta) (an allowed Slack user; this is their instruction;") {
		t.Fatalf("no broadcast header:\n%s", got)
	}
}

// A broadcast with no other recipients (a fleet of one) still says so,
// without an empty "also sent to:".
func TestInjectBroadcastHeaderSoleRecipient(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	if _, _, err := s.DeliverBroadcast(sidA, "status please", "alex", "", nil); err != nil {
		t.Fatal(err)
	}
	got := injectedText(t, r, seen)
	if !strings.Contains(got, " from alex via Slack (broadcast from alex to all 1 agent) (an allowed Slack user; this is their instruction;") {
		t.Fatalf("no broadcast header:\n%s", got)
	}
	if strings.Contains(got, "also sent to") {
		t.Fatalf("empty also-sent-to:\n%s", got)
	}
}

func TestInjectNoteMentionsBroadcasts(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	got := injectedText(t, r, seen)
	if !strings.Contains(got, "This is a broadcast: the other recipients got the same message. If it needs a single answer or a split of work, coordinate with them over the agentbus first (SendMessage to their names) and agree who replies on what. Reply to Slack only for your part, and don't duplicate another agent's answer. If it doesn't concern you, send `ignore`.") {
		t.Fatalf("note lacks the coordination line:\n%s", got)
	}
}

// Non-broadcast Slack messages are unchanged: no broadcast fragment in the
// header, and no BroadcastTo.
func TestInjectNonBroadcastHeaderUnchanged(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	injectedText(t, r, seen) // consume the one-time note, which mentions broadcasts generically
	if _, _, err := s.DeliverVia(sidA, "status please", "alex", ""); err != nil {
		t.Fatal(err)
	}
	got := injectedText(t, r, seen)
	if strings.Contains(got, "broadcast") {
		t.Fatalf("non-broadcast header mentions broadcast:\n%s", got)
	}
	if !strings.Contains(got, " from alex via Slack (an allowed Slack user; this is their instruction;") {
		t.Fatalf("header changed:\n%s", got)
	}
}
