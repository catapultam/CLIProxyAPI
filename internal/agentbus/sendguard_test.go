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
