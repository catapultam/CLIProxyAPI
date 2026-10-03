package agentbus

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type captured struct {
	body   string
	status int
}

func newInjectServer(t *testing.T, clock *fakeClock) (*Store, *gin.Engine, *captured) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s := NewStore("", clock.Now)
	got := &captured{status: http.StatusOK}
	r := gin.New()
	r.POST("/v1/messages", s.InjectMiddleware(), func(c *gin.Context) {
		raw, _ := c.GetRawData()
		got.body = string(raw)
		c.Status(got.status)
	})
	return s, r, got
}

func post(r http.Handler, session, agent, body string) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	if session != "" {
		req.Header.Set("X-Claude-Code-Session-Id", session)
	}
	if agent != "" {
		req.Header.Set("X-Claude-Code-Agent-Id", agent)
	}
	r.ServeHTTP(httptest.NewRecorder(), req)
}

const stringContentBody = `{"model":"m","system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":"hello"}]}`
const blockContentBody = `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"q"}]},{"role":"assistant","content":[{"type":"text","text":"a"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`

func lastUserTexts(body string) []string {
	msgs := gjson.Get(body, "messages").Array()
	last := msgs[len(msgs)-1]
	var out []string
	for _, b := range last.Get("content").Array() {
		if b.Get("type").String() == "text" {
			out = append(out, b.Get("text").String())
		}
	}
	return out
}

func TestInjectNoteIntoStringContentOnFirstRequest(t *testing.T) {
	_, r, got := newInjectServer(t, &fakeClock{now: t0})
	post(r, sidA, "", stringContentBody)
	texts := lastUserTexts(got.body)
	if len(texts) != 2 || texts[0] != "hello" || !strings.Contains(texts[1], "<agentbus>") || !strings.Contains(texts[1], sidA) {
		t.Fatalf("texts = %q", texts)
	}
	if gjson.Get(got.body, "system.0.text").String() != "sys" {
		t.Fatal("system array changed")
	}
}

func TestInjectAppendsBlockAfterToolResult(t *testing.T) {
	s, r, got := newInjectServer(t, &fakeClock{now: t0})
	s.Hello(sidB, "pc", "/b", "", true)
	if _, err := s.Send(sidB, "unknown/session-aaaaaa", "x", ""); err == nil {
		t.Fatal("send to an unseen session should fail")
	}
	post(r, sidA, "", blockContentBody)
	content := gjson.Get(got.body, "messages.2.content").Array()
	if len(content) != 2 || content[0].Get("type").String() != "tool_result" || content[1].Get("type").String() != "text" {
		t.Fatalf("content = %s", gjson.Get(got.body, "messages.2.content").Raw)
	}
}

func TestInjectDeliversMessagesOnce(t *testing.T) {
	s, r, got := newInjectServer(t, &fakeClock{now: t0})
	post(r, sidA, "", stringContentBody) // first request: note only
	s.Hello(sidB, "pc", "/b", "", true)
	if _, err := s.Send(sidB, s.Address(sidA), "please review PR 12", ""); err != nil {
		t.Fatal(err)
	}
	post(r, sidA, "", stringContentBody)
	if !strings.Contains(got.body, "please review PR 12") {
		t.Fatalf("message not injected: %s", got.body)
	}
	post(r, sidA, "", stringContentBody)
	if strings.Contains(got.body, "please review PR 12") {
		t.Fatal("message injected twice")
	}
}

func TestInjectSkipsSubagentRequests(t *testing.T) {
	s, r, got := newInjectServer(t, &fakeClock{now: t0})
	s.Hello(sidB, "pc", "/b", "", true)
	post(r, sidA, "", stringContentBody)
	if _, err := s.Send(sidB, s.Address(sidA), "for the main thread", ""); err != nil {
		t.Fatal(err)
	}
	post(r, sidA, "agent-123", stringContentBody)
	if got.body != stringContentBody {
		t.Fatalf("subagent request modified: %s", got.body)
	}
	if !s.Pending(sidA) {
		t.Fatal("subagent request consumed the message")
	}
}

func TestInjectReturnsMessagesWhenRequestFails(t *testing.T) {
	s, r, got := newInjectServer(t, &fakeClock{now: t0})
	s.Hello(sidB, "pc", "/b", "", true)
	post(r, sidA, "", stringContentBody)
	if _, err := s.Send(sidB, s.Address(sidA), "do not lose me", ""); err != nil {
		t.Fatal(err)
	}
	got.status = http.StatusTooManyRequests
	post(r, sidA, "", stringContentBody)
	if !s.Pending(sidA) {
		t.Fatal("message lost after failed request")
	}
	got.status = http.StatusOK
	post(r, sidA, "", stringContentBody)
	if !strings.Contains(got.body, "do not lose me") {
		t.Fatal("returned message not delivered on retry")
	}
}

func TestInjectLeavesBodyAloneWhenLastMessageNotUser(t *testing.T) {
	s, r, got := newInjectServer(t, &fakeClock{now: t0})
	s.Hello(sidB, "pc", "/b", "", true)
	post(r, sidA, "", stringContentBody)
	if _, err := s.Send(sidB, s.Address(sidA), "keep", ""); err != nil {
		t.Fatal(err)
	}
	body := `{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":"prefill"}]}`
	post(r, sidA, "", body)
	if got.body != body {
		t.Fatalf("body changed: %s", got.body)
	}
	if !s.Pending(sidA) {
		t.Fatal("message consumed without injection")
	}
}

func TestInjectNothingWhenNothingNew(t *testing.T) {
	_, r, got := newInjectServer(t, &fakeClock{now: t0})
	post(r, sidA, "", stringContentBody)
	post(r, sidA, "", stringContentBody)
	if got.body != stringContentBody {
		t.Fatalf("second request modified: %s", got.body)
	}
}

// TestInjectNeverMentionsTheRetiredHook covers the scenarios that used to
// produce the setup hint (first request, then an idle request after the old
// grace period, then a request with a pending message) and asserts none of
// them mention the retired wait.sh hook or its install endpoint.
func TestInjectNeverMentionsTheRetiredHook(t *testing.T) {
	clock := &fakeClock{now: t0}
	s, r, got := newInjectServer(t, clock)

	post(r, sidA, "", stringContentBody)
	assertNoHookMention(t, got.body)
	if !strings.Contains(got.body, "/v1/agentbus/name") {
		t.Fatalf("name recipe missing: %s", got.body)
	}

	clock.Advance(3 * time.Minute)
	s.Hello(sidB, "pc", "/b", "", true)
	if _, err := s.Send(sidB, s.Address(sidA), "nightly build", ""); err != nil {
		t.Fatal(err)
	}
	post(r, sidA, "", stringContentBody)
	assertNoHookMention(t, got.body)
	if !strings.Contains(got.body, "nightly build") {
		t.Fatalf("pending message not delivered: %s", got.body)
	}
}

func assertNoHookMention(t *testing.T, body string) {
	t.Helper()
	for _, needle := range []string{"wait.sh", "/setup", "set up", "set it up"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(needle)) {
			t.Fatalf("body mentions retired hook (%q): %s", needle, body)
		}
	}
}

func TestInjectNoteAgainWhenPeersChange(t *testing.T) {
	s, r, got := newInjectServer(t, &fakeClock{now: t0})
	post(r, sidA, "", stringContentBody)
	s.Hello(sidB, "fedora", "/srv/ci", "ci", true)
	post(r, sidA, "", stringContentBody)
	if !strings.Contains(got.body, "ci (fedora/ci-bbbbbb)") {
		t.Fatalf("new peer not announced: %s", got.body)
	}
}

func TestInjectIgnoresRequestsWithoutSession(t *testing.T) {
	_, r, got := newInjectServer(t, &fakeClock{now: t0})
	post(r, "", "", stringContentBody)
	if got.body != stringContentBody {
		t.Fatal("request without session modified")
	}
	_ = io.EOF
}

// bodyText un-escapes the JSON string content under test so assertions can
// use plain text (appendToLastUser's JSON encoding escapes <, >, and &).
func bodyText(t *testing.T, body string) string {
	t.Helper()
	texts := lastUserTexts(body)
	if len(texts) == 0 {
		t.Fatalf("no text blocks in body: %s", body)
	}
	return texts[len(texts)-1]
}

func TestInjectModSessionGetsListAgentsInstructions(t *testing.T) {
	s, r, got := newInjectServer(t, &fakeClock{now: t0})
	s.Hello(sidA, "pc", "/a", "", true)
	post(r, sidA, "", stringContentBody)
	text := bodyText(t, got.body)
	if !strings.Contains(text, "ListAgents") || !strings.Contains(text, `SendMessage, to: "agentbus:<address>"`) {
		t.Fatalf("mod instructions missing: %s", text)
	}
	if strings.Contains(text, "curl") {
		t.Fatalf("mod session still told to curl: %s", text)
	}
}

func TestInjectNoModSessionGetsInstallInstructions(t *testing.T) {
	_, r, got := newInjectServer(t, &fakeClock{now: t0})
	post(r, sidA, "", stringContentBody)
	text := bodyText(t, got.body)
	if !strings.Contains(text, "/v1/agentbus/send") {
		t.Fatalf("curl instructions missing: %s", text)
	}
	want := "the user can install it with: claude plugin marketplace add https://example.com/plugins/marketplace.json and claude plugin install agentbus@homelab"
	if !strings.Contains(text, want) {
		t.Fatalf("install instructions missing: %s", text)
	}
	if !strings.Contains(text, "new-machine.md in the private repo catapultam/homelab-notes") {
		t.Fatalf("note does not point at the setup runbook: %s", text)
	}
	assertNoHookMention(t, text)
}

func TestInjectResendsNoteWhenModStarts(t *testing.T) {
	s, r, got := newInjectServer(t, &fakeClock{now: t0})
	post(r, sidA, "", stringContentBody)
	if !strings.Contains(got.body, "curl") {
		t.Fatalf("first note should use curl instructions: %s", got.body)
	}

	s.Hello(sidA, "pc", "/a", "", true)
	post(r, sidA, "", stringContentBody)
	if strings.Contains(got.body, "curl") {
		t.Fatalf("note not re-sent after mod started: %s", got.body)
	}
	if !strings.Contains(got.body, "ListAgents") {
		t.Fatalf("mod instructions missing after flip: %s", got.body)
	}
}
