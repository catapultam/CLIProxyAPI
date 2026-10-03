package agentbus

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// sendReq JSON-encodes a /send body properly, so a test body with newlines
// or quotes in it is never hand-escaped.
func sendReq(from, to, replyTo, body string) string {
	data, _ := json.Marshal(sendRequest{FromSession: from, To: to, ReplyTo: replyTo, Body: body})
	return string(data)
}

// guardBridge is a fakeBridge that also answers Dismissed, Done and
// Working, each counting an id only when owned maps it to the session that
// asked. It records every Post (inherited from fakeBridge), so a test can
// confirm nothing was posted.
type guardBridge struct {
	fakeBridge
	owned  map[string]string
	r      *gin.Engine
	marked [][]string
}

func (g *guardBridge) Dismissed(sessionID string, ids []string) int { return g.count(sessionID, ids) }
func (g *guardBridge) Done(sessionID string, ids []string) int {
	g.marked = append(g.marked, ids)
	return g.count(sessionID, ids)
}
func (g *guardBridge) Working(sessionID string, ids []string) int { return g.count(sessionID, ids) }

func (g *guardBridge) count(sessionID string, ids []string) int {
	n := 0
	for _, id := range ids {
		if g.owned[id] == sessionID {
			n++
		}
	}
	return n
}

// newGuardServer wires sidA with a delivered reply_to id the bridge owns for
// it, and returns the store, router and that id.
func newGuardServer(t *testing.T) (*Store, *guardBridge, string) {
	t.Helper()
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	b := &guardBridge{fakeBridge: fakeBridge{users: []string{"alex"}}}
	s.SetBridge(b)
	_, mine, err := s.DeliverVia(sidA, "fix the build", "alex", "")
	if err != nil {
		t.Fatal(err)
	}
	b.owned = map[string]string{mine: sidA}
	b.r = r
	return s, b, mine
}

// Task 9: a bare "ignore" body with a reply_to to slack dismisses it and
// posts nothing.
func TestSendGuardIgnoreDismissesAndPostsNothing(t *testing.T) {
	_, b, mine := newGuardServer(t)
	w := do(b.r, http.MethodPost, "/v1/agentbus/send", sendReq(sidA, "slack", mine, "  Ignore  "))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"dismissed":1`) {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	if len(b.posts) != 0 {
		t.Fatalf("posts = %+v", b.posts)
	}
}

// Task 9: a bare "done" body with a reply_to to slack marks it done and
// posts nothing.
func TestSendGuardDoneMarksDoneAndPostsNothing(t *testing.T) {
	_, b, mine := newGuardServer(t)
	w := do(b.r, http.MethodPost, "/v1/agentbus/send", sendReq(sidA, "slack", mine, "Done"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"done":1`) {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	if len(b.posts) != 0 {
		t.Fatalf("posts = %+v", b.posts)
	}
}

// Task 9 addendum: a bare "working" body with a reply_to to slack marks it
// working and posts nothing.
func TestSendGuardWorkingMarksWorkingAndPostsNothing(t *testing.T) {
	_, b, mine := newGuardServer(t)
	w := do(b.r, http.MethodPost, "/v1/agentbus/send", sendReq(sidA, "slack", mine, "working"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"working":1`) {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	if len(b.posts) != 0 {
		t.Fatalf("posts = %+v", b.posts)
	}
}

// Task 9: a body whose last non-empty line is exactly "done", with other
// content above it, posts the rest without that line, then marks it done.
func TestSendGuardTrailingDoneLinePostsThenMarksDone(t *testing.T) {
	_, b, mine := newGuardServer(t)
	body := "Rebased and pushed.\n\ndone\n"
	w := do(b.r, http.MethodPost, "/v1/agentbus/send", sendReq(sidA, "slack", mine, body))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"m_`) {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	if len(b.posts) != 1 || b.posts[0].Body != "Rebased and pushed." {
		t.Fatalf("posts = %+v", b.posts)
	}
	if len(b.marked) != 1 || len(b.marked[0]) != 1 || b.marked[0][0] != mine {
		t.Fatalf("marked done = %+v", b.marked)
	}
}

// A body that is only the "done" line (no other content) counts as the bare
// case: no post, just a mark.
func TestSendGuardDoneLineAloneCountsAsBare(t *testing.T) {
	_, b, mine := newGuardServer(t)
	w := do(b.r, http.MethodPost, "/v1/agentbus/send", sendReq(sidA, "slack", mine, "\n\ndone\n"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"done":1`) {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	if len(b.posts) != 0 {
		t.Fatalf("posts = %+v", b.posts)
	}
}

// Task 9: the guard only applies with a reply_to to slack; a peer send of
// the same words posts as usual.
func TestSendGuardDoesNotApplyToPeers(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.Hello(sidB, "pc", "/b", "", true)
	for _, body := range []string{"ignore", "done", "working"} {
		w := do(r, http.MethodPost, "/v1/agentbus/send", sendReq(sidA, s.Address(sidB), "", body))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"m_`) {
			t.Fatalf("%s: send = %d %s", body, w.Code, w.Body)
		}
	}
	msgs := s.Claim(sidB)
	if len(msgs) != 3 {
		t.Fatalf("msgs = %+v", msgs)
	}
	for i, want := range []string{"ignore", "done", "working"} {
		if msgs[i].Body != want {
			t.Fatalf("msg %d = %+v", i, msgs[i])
		}
	}
}

// And a /send to slack without a reply_to posts "ignore"/"done" as usual:
// the guard needs a reply_to to act on.
func TestSendGuardDoesNotApplyWithoutReplyTo(t *testing.T) {
	_, b, _ := newGuardServer(t)
	w := do(b.r, http.MethodPost, "/v1/agentbus/send", sendReq(sidA, "slack", "", "done"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"m_`) {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	if len(b.posts) != 1 || b.posts[0].Body != "done" {
		t.Fatalf("posts = %+v", b.posts)
	}
}

// M7: a foreign reply_to (delivered to someone else) counts zero and posts
// nothing, the same as a direct /dismiss or /done would.
func TestSendGuardForeignReplyToCountsZeroNoPost(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.Hello(sidB, "pc", "/b", "", true)
	b := &guardBridge{fakeBridge: fakeBridge{users: []string{"alex"}}}
	s.SetBridge(b)
	_, theirs, err := s.DeliverVia(sidB, "for b", "alex", "")
	if err != nil {
		t.Fatal(err)
	}
	b.owned = map[string]string{theirs: sidB}
	w := do(r, http.MethodPost, "/v1/agentbus/send", sendReq(sidA, "slack", theirs, "ignore"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"dismissed":0`) {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	if len(b.posts) != 0 {
		t.Fatalf("posts = %+v", b.posts)
	}
}

// M7: when the post itself fails (here, an unknown from_session), Done must
// not be called for the trailing-done-line path.
func TestSendGuardTrailingDoneNeverMarksDoneWhenSendFails(t *testing.T) {
	_, b, mine := newGuardServer(t)
	w := do(b.r, http.MethodPost, "/v1/agentbus/send", sendReq("ghost-session", "slack", mine, "Rebased.\n\ndone\n"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	if len(b.posts) != 0 {
		t.Fatalf("posts = %+v", b.posts)
	}
	if len(b.marked) != 0 {
		t.Fatalf("marked done on a failed send = %+v", b.marked)
	}
}

// M7: the guard never applies to a DM target ("slack@<label>"), which
// ignores reply_to entirely; a control word there posts as usual.
func TestSendGuardDoesNotApplyToSlackDM(t *testing.T) {
	_, b, mine := newGuardServer(t)
	w := do(b.r, http.MethodPost, "/v1/agentbus/send", sendReq(sidA, "slack@alex", mine, "ignore"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"m_`) {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	if len(b.posts) != 1 || b.posts[0].Body != "ignore" {
		t.Fatalf("posts = %+v", b.posts)
	}
}

// M1: CRLF line breaks in the body never leave a stray "\r" or blank line
// in what gets posted.
func TestSendGuardTrailingDoneLineHandlesCRLF(t *testing.T) {
	_, b, mine := newGuardServer(t)
	w := do(b.r, http.MethodPost, "/v1/agentbus/send", sendReq(sidA, "slack", mine, "Rebased.\r\n\r\ndone\r\n"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"m_`) {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	if len(b.posts) != 1 || b.posts[0].Body != "Rebased." {
		t.Fatalf("posts = %+v", b.posts)
	}
}

// A body that merely ends with "Done." (trailing punctuation) is not a
// bare match and has no other content above a bare "done" line either: it
// posts as ordinary text.
func TestSendGuardDoneWithPunctuationPostsAsText(t *testing.T) {
	_, b, mine := newGuardServer(t)
	w := do(b.r, http.MethodPost, "/v1/agentbus/send", sendReq(sidA, "slack", mine, "Done."))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"m_`) {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	if len(b.posts) != 1 || b.posts[0].Body != "Done." {
		t.Fatalf("posts = %+v", b.posts)
	}
	if len(b.marked) != 0 {
		t.Fatalf("marked done = %+v", b.marked)
	}
}

// M2: a shell loop's own closing "done" keyword, inside a still-open code
// fence, is never mistaken for the control word.
func TestSendGuardDoesNotStripDoneInsideOpenFence(t *testing.T) {
	_, b, mine := newGuardServer(t)
	body := "Patched the backup script:\n```\nfor f in *.log; do\n  gzip \"$f\"\ndone"
	w := do(b.r, http.MethodPost, "/v1/agentbus/send", sendReq(sidA, "slack", mine, body))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"m_`) {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	if len(b.posts) != 1 || b.posts[0].Body != body {
		t.Fatalf("posts = %+v", b.posts)
	}
	if len(b.marked) != 0 {
		t.Fatalf("marked done = %+v", b.marked)
	}
}

// M3: a trailing "done" line whose remainder is itself a bare control word
// is left alone (sent unchanged) rather than posted as text and then
// marked done on top of it.
func TestSendGuardDoesNotStripWhenRemainderIsControlWord(t *testing.T) {
	_, b, mine := newGuardServer(t)
	w := do(b.r, http.MethodPost, "/v1/agentbus/send", sendReq(sidA, "slack", mine, "ignore\ndone"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"m_`) {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	if len(b.posts) != 1 || b.posts[0].Body != "ignore\ndone" {
		t.Fatalf("posts = %+v", b.posts)
	}
	if len(b.marked) != 0 {
		t.Fatalf("marked done = %+v", b.marked)
	}
}
