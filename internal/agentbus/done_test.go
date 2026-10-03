package agentbus

import (
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// doneBridge accepts "done" marks of the ids it was told belong to a
// session. With store set, each call also calls into the store, which would
// deadlock if the store held its lock.
type doneBridge struct {
	fakeReceiptBridge
	owned map[string]string
	dmu   sync.Mutex
	calls [][]string
}

func (d *doneBridge) Done(sessionID string, ids []string) int {
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

// Task 9: POST /done (the curl path) hands the session's ids to the bridge,
// which counts only those delivered to it, and the ids no longer count for
// /ack either.
func TestHTTPDone(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.SetModVersion(sidA, MinAckModVersion)
	b := &doneBridge{fakeReceiptBridge: fakeReceiptBridge{fakeOwnerBridge: fakeOwnerBridge{fakeBridge: fakeBridge{users: []string{"alex"}}}}}
	b.store = s
	s.SetBridge(b)
	_, mine, err := s.DeliverVia(sidA, "fully answered", "alex", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ClaimForWait(sidA, true, MinAckModVersion); len(got) != 1 {
		t.Fatalf("claimed %+v", got)
	}
	b.owned = map[string]string{mine: sidA, "m_0bb": sidB}
	w := do(r, http.MethodPost, "/v1/agentbus/done", `{"session":"`+sidA+`","ids":["`+mine+`","m_0bb","bogus"]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"done":1`) {
		t.Fatalf("done = %d %s", w.Code, w.Body)
	}
	if !reflect.DeepEqual(b.calls, [][]string{{mine, "m_0bb"}}) {
		t.Fatalf("bridge got %v (invalid ids must be dropped first)", b.calls)
	}
	if n := s.Ack(sidA, []string{mine}); n != 0 {
		t.Fatalf("a done message was still acked: %d", n)
	}
	for _, body := range []string{`{"ids":["` + mine + `"]}`, `not json`} {
		if w := do(r, http.MethodPost, "/v1/agentbus/done", body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, w.Code)
		}
	}
	if w := do(r, http.MethodPost, "/v1/agentbus/done", `{"session":"ghost","ids":["`+mine+`"]}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"done":0`) {
		t.Fatalf("unknown session = %d %s", w.Code, w.Body)
	}
}
