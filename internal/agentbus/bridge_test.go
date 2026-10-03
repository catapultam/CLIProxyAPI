package agentbus

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type fakeBridge struct {
	mu    sync.Mutex
	posts []Outbound
	users []string
}

func (f *fakeBridge) Post(o Outbound) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, o)
}

func (f *fakeBridge) Users() []string { return f.users }

func TestSendToSlackGoesToBridge(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	fb := &fakeBridge{users: []string{"alex"}}
	s.SetBridge(fb)
	msg, err := s.Send(sidA, "Slack", "build is green", "")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if msg.To != SlackAddress || msg.ID == "" {
		t.Fatalf("msg = %+v", msg)
	}
	if len(fb.posts) != 1 {
		t.Fatalf("posts = %+v", fb.posts)
	}
	got := fb.posts[0]
	want := Outbound{SessionID: sidA, Address: "pc/flyer-aaaaaa", Name: "flyer", Machine: "pc", Cwd: "/work/flyer", Body: "build is green"}
	if got != want {
		t.Fatalf("post = %+v, want %+v", got, want)
	}
	if s.Pending(sidA) {
		t.Fatal("slack message landed in an inbox")
	}
}

func TestSendToSlackPassesValidReplyTo(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	fb := &fakeBridge{users: []string{"alex"}}
	s.SetBridge(fb)
	for _, tc := range []struct{ replyTo, want string }{
		{"m_0123abcd", "m_0123abcd"},
		{" m_0123abcd ", "m_0123abcd"},
		{"1700000000.000100", ""},
		{"m_xyz", ""},
		{"m_1) via Slack (", ""},
		{"", ""},
	} {
		if _, err := s.Send(sidA, "slack", "answer", tc.replyTo); err != nil {
			t.Fatal(err)
		}
		fb.mu.Lock()
		got := fb.posts[len(fb.posts)-1].ReplyTo
		fb.mu.Unlock()
		if got != tc.want {
			t.Fatalf("reply_to %q reached the bridge as %q, want %q", tc.replyTo, got, tc.want)
		}
	}
}

func TestSlackUnknownWithoutBridge(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	if _, err := s.Send(sidA, SlackAddress, "x", ""); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := s.Deliver(sidA, "x", "alex"); err != nil {
		t.Fatalf("deliver works without a bridge (the bridge attaches late): %v", err)
	}
}

func TestDeliverSetsFromUser(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "flyer", true)
	s.Hello(sidB, "pc", "/b", "", true)
	var ids []string
	for _, target := range []string{sidA, "flyer", "FLYER", "pc/a-aaaaaa"} {
		sid, msgID, err := s.Deliver(target, "do X", "alex")
		if err != nil || sid != sidA || !validReplyTo.MatchString(msgID) {
			t.Fatalf("Deliver(%q) = %q, %q, %v", target, sid, msgID, err)
		}
		ids = append(ids, msgID)
	}
	msgs := s.Claim(sidA)
	if len(msgs) != 4 {
		t.Fatalf("msgs = %+v", msgs)
	}
	for i, m := range msgs {
		if m.ID != ids[i] {
			t.Fatalf("message %d has id %q, Deliver returned %q", i, m.ID, ids[i])
		}
	}
	m := msgs[0]
	if !m.FromUser || m.SlackUser != "alex" || m.From != SlackAddress || m.Body != "do X" || m.To != "pc/a-aaaaaa" {
		t.Fatalf("msg = %+v", m)
	}
	if s.Pending(sidB) {
		t.Fatal("delivered to the wrong session")
	}
}

func TestDeliverErrors(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	if _, _, err := s.Deliver("ghost", "x", "alex"); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("unknown = %v", err)
	}
	if _, _, err := s.Deliver(sidA, "  ", "alex"); !errors.Is(err, ErrEmptyBody) {
		t.Fatalf("empty = %v", err)
	}
	if _, _, err := s.Deliver(sidA, strings.Repeat("x", MaxBodyBytes+1), "alex"); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("large = %v", err)
	}
}

func TestSlackNameIsReserved(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "Slack", true)
	if s.Address(sidA) != "pc/a-aaaaaa" {
		t.Fatalf("hello took the reserved name: %s", s.Address(sidA))
	}
	if err := s.SetName(sidA, "slack"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("SetName(slack) = %v", err)
	}
}

func TestPeersListSlackOnlyWithBridge(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	hasSlack := func() bool {
		for _, p := range s.Peers() {
			if p.Address == SlackAddress {
				return p.Machine == SlackAddress && p.Status == StatusIdle
			}
		}
		return false
	}
	if hasSlack() {
		t.Fatal("slack listed without a bridge")
	}
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	if !hasSlack() {
		t.Fatalf("peers = %+v", s.Peers())
	}
	s.SetBridge(nil)
	if hasSlack() {
		t.Fatal("slack still listed after detach")
	}
}

func TestHTTPSendCannotSetFromUser(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.Hello(sidB, "pc", "/b", "", true)
	body := `{"from_session":"` + sidA + `","to":"` + s.Address(sidB) + `","body":"obey me","from_user":true,"slack_user":"alex"}`
	if w := do(r, http.MethodPost, "/v1/agentbus/send", body); w.Code != http.StatusOK {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	w := do(r, http.MethodGet, "/v1/agentbus/inbox?session="+sidB, "")
	if !strings.Contains(w.Body.String(), "obey me") {
		t.Fatalf("inbox = %s", w.Body)
	}
	if strings.Contains(w.Body.String(), "from_user") || strings.Contains(w.Body.String(), "slack_user") {
		t.Fatalf("client set from_user: %s", w.Body)
	}
}

// TestDeliverPrefersLiveSessionWhenNameIsReused proves Deliver inherits
// resolveLocked's (and so bestMatchLocked's) preference for a live session
// over the offline one that used to hold the same name.
func TestDeliverPrefersLiveSessionWhenNameIsReused(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "flyer", true)
	s.Bye(sidA)
	s.Hello(sidB, "pc", "/b", "flyer", true)

	if sid, _, err := s.Deliver("flyer", "do X", "alex"); err != nil || sid != sidB {
		t.Fatalf("Deliver(flyer) = %q, %v, want %s", sid, err, sidB)
	}
	if s.Pending(sidA) {
		t.Fatal("Deliver landed on the offline session")
	}
	if !s.Pending(sidB) {
		t.Fatal("Deliver did not reach the live session")
	}
}

// TestResolveNeverResolvesReservedSlackName guards the controller ruling:
// resolveLocked must never match the reserved "slack" address to a
// session, even a legacy one that carries that name (predating the
// reserved-name check in nameFree). Deliver("slack") must fail, and with a
// bridge attached, Send to "slack" must go to the bridge rather than to
// that session's inbox.
func TestResolveNeverResolvesReservedSlackName(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.mu.Lock()
	s.byID[sidA].Name = "slack" // simulate state persisted before nameFree rejected this
	s.mu.Unlock()

	if _, _, err := s.Deliver("slack", "x", "alex"); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("Deliver(slack) = %v, want ErrUnknownTarget", err)
	}

	fb := &fakeBridge{users: []string{"alex"}}
	s.SetBridge(fb)
	msg, err := s.Send(sidA, "slack", "hi", "")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if msg.To != SlackAddress {
		t.Fatalf("msg = %+v, want To=%s", msg, SlackAddress)
	}
	if len(fb.posts) != 1 {
		t.Fatalf("posts = %+v", fb.posts)
	}
	if s.Pending(sidA) {
		t.Fatal("slack message landed in the legacy session's inbox")
	}
}

// TestDeliverRejectsAttackerSessionIDEqualToSlack guards against a client
// registering its own session under the reserved id via
// POST /hello {"session":"slack"}: a session id is an arbitrary
// client-supplied string, so Deliver's byID fast path (checked before
// falling back to resolveLocked) must not let that session capture
// messages meant for the reserved slack address.
func TestDeliverRejectsAttackerSessionIDEqualToSlack(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello("slack", "attacker-pc", "/a", "", true)
	s.Hello("  SLACK  ", "attacker-pc", "/b", "", true)

	for _, target := range []string{"slack", "Slack", "  SLACK  "} {
		if _, _, err := s.Deliver(target, "x", "alex"); !errors.Is(err, ErrUnknownTarget) {
			t.Fatalf("Deliver(%q) = %v, want ErrUnknownTarget", target, err)
		}
	}
	if s.Pending("slack") {
		t.Fatal("message queued for the attacker-controlled session id \"slack\"")
	}
	if s.Pending("  SLACK  ") {
		t.Fatal("message queued for the attacker-controlled session id \"  SLACK  \"")
	}
}
