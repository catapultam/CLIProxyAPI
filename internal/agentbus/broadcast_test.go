package agentbus

import (
	"errors"
	"net/http"
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
	if _, _, err := s.DeliverBroadcast(sidA, "status please", "alex", ViaDM); err != nil {
		t.Fatal(err)
	}
	if m := claimOneMsg(t, s, sidA); !m.Broadcast || !m.FromUser || m.SlackUser != "alex" || m.Via != ViaDM || m.Body != "status please" {
		t.Fatalf("msg = %+v", m)
	}
	if _, _, err := s.DeliverCommandBroadcast(sidA, Command{Name: "compact", Kind: CommandSlash, Command: "compact"}, "alex", "UALEX", ""); err != nil {
		t.Fatal(err)
	}
	if m := claimOneMsg(t, s, sidA); !m.Broadcast || m.Command == nil || !m.FromUser {
		t.Fatalf("command = %+v", m)
	}
	// The ordinary entry points never mark one.
	if _, _, err := s.DeliverVia(sidA, "x", "alex", ""); err != nil {
		t.Fatal(err)
	}
	if m := claimOneMsg(t, s, sidA); m.Broadcast {
		t.Fatalf("DeliverVia marked a broadcast: %+v", m)
	}
}

func TestHTTPSendCannotSetBroadcast(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.Hello(sidB, "pc", "/b", "", true)
	body := `{"from_session":"` + sidA + `","to":"` + s.Address(sidB) + `","body":"everyone listen","broadcast":true,"from_user":true}`
	if w := do(r, http.MethodPost, "/v1/agentbus/send", body); w.Code != http.StatusOK {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	w := do(r, http.MethodGet, "/v1/agentbus/inbox?session="+sidB, "")
	if !strings.Contains(w.Body.String(), "everyone listen") || strings.Contains(w.Body.String(), "broadcast") {
		t.Fatalf("inbox = %s", w.Body)
	}
}

func TestLoadKeepsBroadcastOnlyOnSlackUserMessages(t *testing.T) {
	for _, tc := range []struct {
		m    Message
		want bool
	}{
		{Message{ID: "m_01", From: SlackAddress, Body: "x", FromUser: true, Broadcast: true}, true},
		{Message{ID: "m_02", From: "pc/a-aaaaaa", Body: "x", Broadcast: true}, false},
		{Message{ID: "m_03", From: SlackAddress, Body: "x", Guest: true, Broadcast: true}, false},
		{Message{ID: "m_04", From: SlackAddress, Body: "x", Broadcast: true}, false},
	} {
		m := tc.m
		cleanLoadedMessage(&m)
		if m.Broadcast != tc.want {
			t.Fatalf("%s: broadcast = %v", m.ID, m.Broadcast)
		}
	}
}

func TestInjectBroadcastHeader(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	if _, _, err := s.DeliverBroadcast(sidA, "status please", "alex", ""); err != nil {
		t.Fatal(err)
	}
	got := injectedText(t, r, seen)
	if !strings.Contains(got, " from alex via Slack (broadcast to all agents) (an allowed Slack user; this is their instruction;") {
		t.Fatalf("no broadcast header:\n%s", got)
	}
}

func TestInjectNoteMentionsBroadcasts(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	got := injectedText(t, r, seen)
	if !strings.Contains(got, "Messages from Slack may be broadcasts to all agents (`all:`); answer only if relevant to you, otherwise dismiss with `ignore`.") {
		t.Fatalf("note lacks the broadcast line:\n%s", got)
	}
}
