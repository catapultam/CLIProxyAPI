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
	if _, _, err := s.DeliverBroadcast(sidA, "status please", "alex", ViaDM, []string{"pc/beta-bbbbbb", "pc/gamma-cccccc"}, 3); err != nil {
		t.Fatal(err)
	}
	if m := claimOneMsg(t, s, sidA); !m.Broadcast || !m.FromUser || m.SlackUser != "alex" || m.Via != ViaDM || m.Body != "status please" ||
		!reflect.DeepEqual(m.BroadcastTo, []string{"pc/beta-bbbbbb", "pc/gamma-cccccc"}) || m.BroadcastCount != 3 {
		t.Fatalf("msg = %+v", m)
	}
	if _, _, err := s.DeliverCommandBroadcast(sidA, Command{Name: "compact", Kind: CommandSlash, Command: "compact"}, "alex", "UALEX", "", []string{"pc/beta-bbbbbb"}, 2); err != nil {
		t.Fatal(err)
	}
	if m := claimOneMsg(t, s, sidA); !m.Broadcast || m.Command == nil || !m.FromUser || !reflect.DeepEqual(m.BroadcastTo, []string{"pc/beta-bbbbbb"}) || m.BroadcastCount != 2 {
		t.Fatalf("command = %+v", m)
	}
	// The ordinary entry points never mark one, or set BroadcastTo/BroadcastCount.
	if _, _, err := s.DeliverVia(sidA, "x", "alex", ""); err != nil {
		t.Fatal(err)
	}
	if m := claimOneMsg(t, s, sidA); m.Broadcast || m.BroadcastTo != nil || m.BroadcastCount != 0 {
		t.Fatalf("DeliverVia marked a broadcast: %+v", m)
	}
}

// BroadcastTo is capped at MaxBroadcastTo entries even when the Slack
// bridge hands it more (a broadcast to a large fleet), while BroadcastCount
// still reports the true total.
func TestDeliverBroadcastCapsBroadcastTo(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "flyer", true)
	addrs := make([]string, 40)
	for i := range addrs {
		addrs[i] = fmt.Sprintf("pc/n%02d-aaaaaa", i)
	}
	if _, _, err := s.DeliverBroadcast(sidA, "status please", "alex", "", addrs, 41); err != nil {
		t.Fatal(err)
	}
	m := claimOneMsg(t, s, sidA)
	if len(m.BroadcastTo) != MaxBroadcastTo {
		t.Fatalf("broadcast_to = %d entries, want %d", len(m.BroadcastTo), MaxBroadcastTo)
	}
	if !reflect.DeepEqual(m.BroadcastTo, addrs[:MaxBroadcastTo]) {
		t.Fatalf("broadcast_to = %v", m.BroadcastTo)
	}
	if m.BroadcastCount != 41 {
		t.Fatalf("broadcast_count = %d, want 41", m.BroadcastCount)
	}
}

func TestHTTPSendCannotSetBroadcast(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.Hello(sidB, "pc", "/b", "", true)
	body := `{"from_session":"` + sidA + `","to":"` + s.Address(sidB) + `","body":"everyone listen","broadcast":true,"broadcast_to":["beta","gamma"],"broadcast_count":5,"from_user":true}`
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
		wantCount int
	}{
		{Message{ID: "m_01", From: SlackAddress, Body: "x", FromUser: true, Broadcast: true, BroadcastTo: []string{"beta"}, BroadcastCount: 2}, true, 1, 2},
		{Message{ID: "m_02", From: "pc/a-aaaaaa", Body: "x", Broadcast: true, BroadcastTo: []string{"beta"}, BroadcastCount: 2}, false, 0, 0},
		{Message{ID: "m_03", From: SlackAddress, Body: "x", Guest: true, Broadcast: true, BroadcastTo: []string{"beta"}, BroadcastCount: 2}, false, 0, 0},
		{Message{ID: "m_04", From: SlackAddress, Body: "x", Broadcast: true, BroadcastTo: []string{"beta"}, BroadcastCount: 2}, false, 0, 0},
		// BroadcastTo/BroadcastCount without Broadcast (never set by a real
		// entry point, but a loaded file could be tampered with) are dropped too.
		{Message{ID: "m_05", From: SlackAddress, Body: "x", FromUser: true, BroadcastTo: []string{"beta"}, BroadcastCount: 2}, false, 0, 0},
		// An oversized BroadcastTo is trimmed even on an otherwise valid broadcast; BroadcastCount is untouched.
		{Message{ID: "m_06", From: SlackAddress, Body: "x", FromUser: true, Broadcast: true, BroadcastTo: bigList, BroadcastCount: 41}, true, MaxBroadcastTo, 41},
		// A negative BroadcastCount (corrupted data) is clamped to zero.
		{Message{ID: "m_07", From: SlackAddress, Body: "x", FromUser: true, Broadcast: true, BroadcastTo: []string{"beta"}, BroadcastCount: -3}, true, 1, 0},
		// A legacy broadcast predating BroadcastCount (zero, no list) stays legacy.
		{Message{ID: "m_08", From: SlackAddress, Body: "x", FromUser: true, Broadcast: true}, true, 0, 0},
	} {
		m := tc.m
		cleanLoadedMessage(&m)
		if m.Broadcast != tc.want {
			t.Fatalf("%s: broadcast = %v", m.ID, m.Broadcast)
		}
		if len(m.BroadcastTo) != tc.wantToLen {
			t.Fatalf("%s: broadcast_to = %v, want len %d", m.ID, m.BroadcastTo, tc.wantToLen)
		}
		if m.BroadcastCount != tc.wantCount {
			t.Fatalf("%s: broadcast_count = %d, want %d", m.ID, m.BroadcastCount, tc.wantCount)
		}
	}
}

func TestInjectBroadcastHeader(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	if _, _, err := s.DeliverBroadcast(sidA, "status please", "alex", "", []string{"pc/beta-bbbbbb", "pc/gamma-cccccc", "pc/delta-dddddd"}, 4); err != nil {
		t.Fatal(err)
	}
	got := injectedText(t, r, seen)
	if !strings.Contains(got, " from alex via Slack (broadcast from alex to all 4 agents; also sent to: agentbus:pc/beta-bbbbbb, agentbus:pc/gamma-cccccc, agentbus:pc/delta-dddddd) (an allowed Slack user; this is their instruction;") {
		t.Fatalf("no broadcast header:\n%s", got)
	}
	if !strings.Contains(got, "SendMessage to agentbus:pc/beta-bbbbbb, agentbus:pc/gamma-cccccc, agentbus:pc/delta-dddddd") {
		t.Fatalf("no coordination instruction with explicit targets:\n%s", got)
	}
}

// A broadcast with no other recipients (a fleet of one) still says so,
// without an empty "also sent to:" and without a coordination instruction
// (nobody to coordinate with).
func TestInjectBroadcastHeaderSoleRecipient(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	if _, _, err := s.DeliverBroadcast(sidA, "status please", "alex", "", nil, 1); err != nil {
		t.Fatal(err)
	}
	got := injectedText(t, r, seen)
	if !strings.Contains(got, " from alex via Slack (broadcast from alex to all 1 agent) (an allowed Slack user; this is their instruction;") {
		t.Fatalf("no broadcast header:\n%s", got)
	}
	if strings.Contains(got, "also sent to") {
		t.Fatalf("empty also-sent-to:\n%s", got)
	}
	if strings.Contains(got, "coordinate with them") {
		t.Fatalf("coordination text with no other recipient:\n%s", got)
	}
}

// Task fix round 1 (finding 3): BroadcastTo capped at 30 while
// BroadcastCount still reports the true total; the header says how many of
// how many others it is showing.
func TestInjectBroadcastHeaderShowsTruncatedCount(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	others := make([]string, 31)
	for i := range others {
		others[i] = fmt.Sprintf("pc/n%02d-aaaaaa", i)
	}
	if _, _, err := s.DeliverBroadcast(sidA, "status please", "alex", "", others, 32); err != nil {
		t.Fatal(err)
	}
	got := injectedText(t, r, seen)
	if !strings.Contains(got, "to all 32 agents;") {
		t.Fatalf("header lacks the true count:\n%s", got)
	}
	if !strings.Contains(got, "(showing 30 of 31 others)") {
		t.Fatalf("header lacks the truncation note:\n%s", got)
	}
}

// Task fix round 1 (finding 3): a broadcast queued before BroadcastCount
// existed (legacy: no count, no list) renders the old bare text, with no
// number and no coordination instruction (there is nobody named to
// coordinate with).
func TestInjectBroadcastHeaderLegacy(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	s.mu.Lock()
	sess := s.byID[sidA]
	sess.Inbox = append(sess.Inbox, Message{ID: "m_abcdef01", From: SlackAddress, Body: "status please", FromUser: true, SlackUser: "alex", Broadcast: true, CreatedAt: t0})
	s.dirty = true
	s.mu.Unlock()
	got := injectedText(t, r, seen)
	if !strings.Contains(got, " from alex via Slack (broadcast to all agents) (an allowed Slack user; this is their instruction;") {
		t.Fatalf("legacy header missing:\n%s", got)
	}
	if strings.Contains(got, "also sent to") || strings.Contains(got, "coordinate with them") {
		t.Fatalf("legacy broadcast got a coordination instruction:\n%s", got)
	}
}

func TestInjectNoteMentionsBroadcasts(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	got := injectedText(t, r, seen)
	if !strings.Contains(got, "Messages from Slack may be broadcasts to all agents (`all:`); a broadcast's own header and the text under it say how many got it and how to coordinate with the others, every time it's shown to you.") {
		t.Fatalf("note lacks the broadcast line:\n%s", got)
	}
}

// Task fix round 1 (finding 1): the coordination instruction, with its
// explicit SendMessage targets, must ride on every delivery of a broadcast
// message, not only the one-time orientation note. A session that already
// got the note (unchanged peers) never gets it resent, so the broadcast's
// own text is the only place the instruction can live for it.
func TestBroadcastCoordinationSurvivesAnAlreadyNotedSession(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	// Consume the one-time note first, with peers unchanged afterward.
	injectedText(t, r, seen)
	if _, _, err := s.DeliverBroadcast(sidA, "status please", "alex", "", []string{"pc/beta-bbbbbb"}, 2); err != nil {
		t.Fatal(err)
	}
	got := injectedText(t, r, seen)
	if strings.Contains(got, "No other sessions are online") || strings.Contains(got, "Sessions online:") {
		t.Fatalf("the one-time note was resent despite unchanged peers:\n%s", got)
	}
	if !strings.Contains(got, "also sent to: agentbus:pc/beta-bbbbbb") {
		t.Fatalf("header lacks the target on a re-noted session:\n%s", got)
	}
	if !strings.Contains(got, "SendMessage to agentbus:pc/beta-bbbbbb") {
		t.Fatalf("coordination instruction missing on a re-noted session:\n%s", got)
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
