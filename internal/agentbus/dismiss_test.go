package agentbus

import (
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// dismissBridge accepts dismissals of the ids it was told belong to a
// session. With store set, each call also calls into the store, which would
// deadlock if the store held its lock.
type dismissBridge struct {
	fakeReceiptBridge
	owned map[string]string
	dmu   sync.Mutex
	calls [][]string
}

func (d *dismissBridge) Dismissed(sessionID string, ids []string) int {
	if d.store != nil {
		d.store.Peers()
	}
	d.dmu.Lock()
	defer d.dmu.Unlock()
	d.calls = append(d.calls, append([]string(nil), ids...))
	n := 0
	for _, id := range ids {
		if d.owned[id] == sessionID {
			n++
		}
	}
	return n
}

// Item 9: the note tells agents how to dismiss a message not meant for them.
func TestInjectNoteExplainsDismiss(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	got := injectedText(t, r, seen)
	want := `If a Slack message clearly wasn't meant for you (people talking to each other in a linked chat, a tag for someone else), dismiss it instead of replying: SendMessage to "agentbus:slack#<id>" with message ` + "`ignore`" + ` (curl: POST /v1/agentbus/dismiss {"session":"` + sidA + `","ids":["<id>"]}).`
	if !strings.Contains(got, want) {
		t.Fatalf("note lacks %q:\n%s", want, got)
	}
}

// Task 9: the note also tells agents how to mark a message done, and how to
// flag a long task as working, in one short line merged with the done hint.
func TestInjectNoteExplainsDoneAndWorking(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	got := injectedText(t, r, seen)
	if !strings.Contains(got, "mark it done: reply with `done` on its own last line, or send `done` to agentbus:slack#<id>") {
		t.Fatalf("note lacks the done hint:\n%s", got)
	}
	if !strings.Contains(got, "Long task? Send `working` to agentbus:slack#<id>; finish with `done`.") {
		t.Fatalf("note lacks the working hint:\n%s", got)
	}
	// Fix round 1: the broadcast line no longer says "ignore" itself (that
	// instruction now rides on each broadcast message); only the dismiss
	// line does, and the done hint doesn't add a second mention.
	if n := strings.Count(got, "`ignore`"); n != 1 {
		t.Fatalf("note mentions `ignore` %d times, want 1:\n%s", n, got)
	}
}

// Item 9: POST /dismiss (the curl path) hands the session's ids to the
// bridge, which counts only those delivered to it, and the ids no longer
// count for /ack.
func TestHTTPDismiss(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.SetModVersion(sidA, MinAckModVersion)
	b := &dismissBridge{fakeReceiptBridge: fakeReceiptBridge{fakeOwnerBridge: fakeOwnerBridge{fakeBridge: fakeBridge{users: []string{"alex"}}}}}
	b.store = s
	s.SetBridge(b)
	_, mine, err := s.DeliverVia(sidA, "not for you", "alex", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ClaimForWait(sidA, true, MinAckModVersion); len(got) != 1 {
		t.Fatalf("claimed %+v", got)
	}
	b.owned = map[string]string{mine: sidA, "m_0bb": sidB}
	w := do(r, http.MethodPost, "/v1/agentbus/dismiss", `{"session":"`+sidA+`","ids":["`+mine+`","m_0bb","bogus"]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"dismissed":1`) {
		t.Fatalf("dismiss = %d %s", w.Code, w.Body)
	}
	if !reflect.DeepEqual(b.calls, [][]string{{mine, "m_0bb"}}) {
		t.Fatalf("bridge got %v (invalid ids must be dropped first)", b.calls)
	}
	if n := s.Ack(sidA, []string{mine}); n != 0 {
		t.Fatalf("a dismissed message was still acked: %d", n)
	}
	for _, body := range []string{`{"ids":["` + mine + `"]}`, `not json`} {
		if w := do(r, http.MethodPost, "/v1/agentbus/dismiss", body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, w.Code)
		}
	}
	if w := do(r, http.MethodPost, "/v1/agentbus/dismiss", `{"session":"ghost","ids":["`+mine+`"]}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"dismissed":0`) {
		t.Fatalf("unknown session = %d %s", w.Code, w.Body)
	}
}
