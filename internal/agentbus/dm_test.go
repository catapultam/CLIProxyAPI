package agentbus

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// lockCheckingBridge answers Users by reading the store, so a Users call made
// while the store lock is held deadlocks the test.
type lockCheckingBridge struct {
	fakeBridge
	store *Store
}

func (f *lockCheckingBridge) Users() []string {
	_ = f.store.Peers()
	return f.fakeBridge.Users()
}

func TestSendToSlackDMGoesToBridge(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	fb := &lockCheckingBridge{fakeBridge: fakeBridge{users: []string{"alex", "jane"}}, store: s}
	s.SetBridge(fb)
	msg, err := s.Send(sidA, " Slack@ALEX ", "private note", "m_0123abcd")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if msg.To != "slack@alex" || msg.ID == "" {
		t.Fatalf("msg = %+v", msg)
	}
	if len(fb.posts) != 1 {
		t.Fatalf("posts = %+v", fb.posts)
	}
	want := Outbound{ID: msg.ID, SessionID: sidA, Address: "pc/flyer-aaaaaa", Name: "flyer", Machine: "pc", Body: "private note", DM: "alex"}
	if got := fb.posts[0]; got != want {
		t.Fatalf("post = %+v, want %+v", got, want)
	}
	if s.Pending(sidA) {
		t.Fatal("a DM landed in an inbox")
	}
}

func TestSendToSlackDMUnknownLabel(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	if _, err := s.Send(sidA, "slack@alex", "x", ""); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("without a bridge: err = %v", err)
	}
	fb := &fakeBridge{users: []string{"alex"}}
	s.SetBridge(fb)
	for _, to := range []string{"slack@eve", "slack@", "slack@ ", "slack@alex2", "slack@al", "slack@@alex"} {
		if _, err := s.Send(sidA, to, "x", ""); !errors.Is(err, ErrUnknownTarget) {
			t.Fatalf("Send(%q) = %v, want ErrUnknownTarget", to, err)
		}
	}
	if len(fb.posts) != 0 {
		t.Fatalf("posts = %+v", fb.posts)
	}
	if _, err := s.Send("ghost", "slack@alex", "x", ""); !errors.Is(err, ErrUnknownSender) {
		t.Fatalf("unknown sender: err = %v", err)
	}
}

// TestSlackDMAddressNeverResolvesToSession: "slack@<label>" is reserved like
// "slack". A client can register any session id (and Hello takes no name
// with "@"), so neither Deliver's id fast path nor Send's resolve may hand
// a "slack@..." target to a session.
func TestSlackDMAddressNeverResolvesToSession(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.Hello("slack@alex", "attacker-pc", "/b", "", true)
	s.Hello("SLACK@x", "attacker-pc", "/c", "", true)
	for _, target := range []string{"slack@alex", "Slack@Alex", " slack@alex ", "SLACK@x", "slack@"} {
		if _, _, err := s.Deliver(target, "x", "alex"); !errors.Is(err, ErrUnknownTarget) {
			t.Fatalf("Deliver(%q) = %v, want ErrUnknownTarget", target, err)
		}
		if _, ok := s.Resolve(target); ok {
			t.Fatalf("Resolve(%q) found a session", target)
		}
		if _, err := s.Send(sidA, target, "x", ""); !errors.Is(err, ErrUnknownTarget) {
			t.Fatalf("Send(%q) without a bridge = %v", target, err)
		}
	}
	if _, _, err := s.DeliverCommand("slack@alex", Command{Name: "compact", Kind: CommandSlash, Command: "compact"}, "alex", "UALEX"); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("DeliverCommand = %v", err)
	}
	if s.Pending("slack@alex") || s.Pending("SLACK@x") {
		t.Fatal("a message reached a session whose id looks like a DM address")
	}
}

func TestHTTPSendToUnknownSlackUser(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	w := do(r, http.MethodPost, "/v1/agentbus/send", `{"from_session":"`+sidA+`","to":"slack@eve","body":"hi"}`)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "allowed Slack user") {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	w = do(r, http.MethodPost, "/v1/agentbus/send", `{"from_session":"`+sidA+`","to":"slack@alex","body":"hi"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"to":"slack@alex"`) {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
}

func TestDeliverViaDMSetsVia(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "flyer", true)
	if _, _, err := s.DeliverVia("flyer", "psst", "alex", ViaDM); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Deliver("flyer", "loud", "alex"); err != nil {
		t.Fatal(err)
	}
	msgs := s.Claim(sidA)
	if len(msgs) != 2 || msgs[0].Via != ViaDM || !msgs[0].FromUser || msgs[1].Via != "" {
		t.Fatalf("msgs = %+v", msgs)
	}
	data, errJSON := json.Marshal(msgs[0])
	if errJSON != nil || !strings.Contains(string(data), `"via":"dm"`) {
		t.Fatalf("json = %s %v", data, errJSON)
	}
	if _, _, err := s.DeliverVia("flyer", "x", "alex", "carrier-pigeon"); !errors.Is(err, ErrInvalidVia) {
		t.Fatalf("unknown via: err = %v", err)
	}
}

func TestHTTPSendCannotSetVia(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.Hello(sidB, "pc", "/b", "", true)
	body := `{"from_session":"` + sidA + `","to":"` + s.Address(sidB) + `","body":"psst","via":"dm"}`
	if w := do(r, http.MethodPost, "/v1/agentbus/send", body); w.Code != http.StatusOK {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	w := do(r, http.MethodGet, "/v1/agentbus/inbox?session="+sidB, "")
	if !strings.Contains(w.Body.String(), "psst") || strings.Contains(w.Body.String(), "via") {
		t.Fatalf("inbox = %s", w.Body)
	}
}

// injectedText runs one main-thread request for sidA and returns what the
// last user message carried.
func injectedText(t *testing.T, r http.Handler, got *captured) string {
	t.Helper()
	post(r, sidA, "", stringContentBody)
	return strings.Join(lastUserTexts(got.body), "\n")
}

func TestInjectDMHeader(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"jane"}})
	if _, _, err := s.DeliverVia(sidA, "check the logs", "jane", ViaDM); err != nil {
		t.Fatal(err)
	}
	got := injectedText(t, r, seen)
	if !strings.Contains(got, "via Slack (DM) (an allowed Slack user") {
		t.Fatalf("no DM header:\n%s", got)
	}
	if !strings.Contains(got, `"slack@jane"`) {
		t.Fatalf("no DM reply hint:\n%s", got)
	}
}

func TestInjectSlackLineMentionsDMs(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex", "jane"}})
	got := injectedText(t, r, seen)
	for _, want := range []string{`"slack@<name>"`, `agentbus:slack@<name>`, "-F to=slack@<name>", "Your messages go to your own thread in Slack;"} {
		if !strings.Contains(got, want) {
			t.Fatalf("note lacks %q:\n%s", want, got)
		}
	}
	// The home thread may be in a DM (slack.home: dm), so the note names no
	// channel.
	if strings.Contains(got, "thread in their Slack channel") {
		t.Fatalf("note still says the thread is in a channel:\n%s", got)
	}
}

func TestSessionOutboundIsByIDOnly(t *testing.T) {
	s := NewStore("", nil)
	s.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	o, err := s.SessionOutbound(sidA)
	if err != nil || o.SessionID != sidA || o.Name != "flyer" || o.Machine != "pc" || o.Address != s.Address(sidA) || o.Body != "" {
		t.Fatalf("SessionOutbound = %+v, %v", o, err)
	}
	if _, err = s.SessionOutbound("flyer"); !errors.Is(err, ErrUnknownSender) {
		t.Fatalf("by name: %v", err)
	}
}

func TestLoadClearsViaOnNonSlackMessages(t *testing.T) {
	for _, tc := range []struct {
		m    Message
		want string
	}{
		{Message{ID: "m_01", From: "pc/a-aaaaaa", Body: "x", Via: ViaDM}, ""},
		{Message{ID: "m_02", From: SlackAddress, Body: "x", FromUser: true, Via: "weird"}, ""},
		{Message{ID: "m_03", From: SlackAddress, Body: "x", FromUser: true, Via: ViaDM}, ViaDM},
	} {
		m := tc.m
		cleanLoadedMessage(&m)
		if m.Via != tc.want {
			t.Fatalf("%s: via = %q, want %q", m.ID, m.Via, tc.want)
		}
	}
}

func TestHTTPSlackUploadToDM(t *testing.T) {
	s, r, fb := newUploadServer(t)
	fb.users = []string{"alex"}
	req := uploadRequest(t, []uploadField{{"session", sidA}, {"caption", "c"}, {"to", "Slack@Alex"}, {"reply_to", "m_0123abcd"}}, "a.png", tinyPNG(t))
	if w := serve(r, req); w.Code != http.StatusOK {
		t.Fatalf("upload = %d %s", w.Code, w.Body)
	}
	if got := fb.posted(); len(got) != 1 || got[0].out.DM != "alex" || got[0].out.ReplyTo != "" {
		t.Fatalf("posted = %+v", got)
	}

	for _, tc := range []struct {
		to   string
		code int
	}{
		{"slack@eve", http.StatusNotFound},
		{"slack@", http.StatusNotFound},
		{"flyer", http.StatusBadRequest},
		{s.Address(sidA), http.StatusBadRequest},
	} {
		req = uploadRequest(t, []uploadField{{"session", sidA}, {"to", tc.to}}, "a.png", tinyPNG(t))
		if w := serve(r, req); w.Code != tc.code {
			t.Fatalf("to %q: %d %s, want %d", tc.to, w.Code, w.Body, tc.code)
		}
	}
	req = uploadRequest(t, []uploadField{{"session", sidA}, {"to", "slack"}}, "a.png", tinyPNG(t))
	if w := serve(r, req); w.Code != http.StatusOK {
		t.Fatalf("to slack: %d %s", w.Code, w.Body)
	}
	if got := fb.posted(); len(got) != 2 || got[1].out.DM != "" {
		t.Fatalf("posted = %+v", got)
	}
}

func TestHTTPSlackUploadJSONToDM(t *testing.T) {
	_, r, fb := newUploadServer(t)
	fb.users = []string{"alex"}
	if w := serve(r, jsonUpload(t, map[string]string{"session": sidA, "to": "slack@alex"}, tinyPNG(t))); w.Code != http.StatusOK {
		t.Fatalf("upload = %d %s", w.Code, w.Body)
	}
	if got := fb.posted(); len(got) != 1 || got[0].out.DM != "alex" {
		t.Fatalf("posted = %+v", got)
	}
	if w := serve(r, jsonUpload(t, map[string]string{"session": sidA, "to": "slack@eve"}, tinyPNG(t))); w.Code != http.StatusNotFound {
		t.Fatalf("unknown label = %d %s", w.Code, w.Body)
	}
}
