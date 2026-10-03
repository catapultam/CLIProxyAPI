package agentbus

import (
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// workingBridge accepts "working" marks of the ids it was told belong to a
// session. With store set, each call also calls into the store, which would
// deadlock if the store held its lock.
type workingBridge struct {
	fakeReceiptBridge
	owned map[string]string
	wmu   sync.Mutex
	calls [][]string
}

func (w *workingBridge) Working(sessionID string, ids []string) int {
	if w.store != nil {
		w.store.Peers()
	}
	w.wmu.Lock()
	defer w.wmu.Unlock()
	w.calls = append(w.calls, append([]string(nil), ids...))
	n := 0
	for _, id := range ids {
		if w.owned[id] == sessionID {
			n++
		}
	}
	return n
}

// Task 9 addendum: POST /working (the curl path, or the mod's 15s timer)
// hands the session's ids to the bridge, which counts only those delivered
// to it. Unlike /done and /dismiss, the ids still count for /ack.
func TestHTTPWorking(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.SetModVersion(sidA, MinAckModVersion)
	b := &workingBridge{fakeReceiptBridge: fakeReceiptBridge{fakeOwnerBridge: fakeOwnerBridge{fakeBridge: fakeBridge{users: []string{"alex"}}}}}
	b.store = s
	s.SetBridge(b)
	_, mine, err := s.DeliverVia(sidA, "a long job", "alex", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ClaimForWait(sidA, true, MinAckModVersion); len(got) != 1 {
		t.Fatalf("claimed %+v", got)
	}
	b.owned = map[string]string{mine: sidA, "m_0bb": sidB}
	w := do(r, http.MethodPost, "/v1/agentbus/working", `{"session":"`+sidA+`","ids":["`+mine+`","m_0bb","bogus"]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"working":1`) {
		t.Fatalf("working = %d %s", w.Code, w.Body)
	}
	if !reflect.DeepEqual(b.calls, [][]string{{mine, "m_0bb"}}) {
		t.Fatalf("bridge got %v (invalid ids must be dropped first)", b.calls)
	}
	// Unlike /done and /dismiss, /working leaves Unacked alone: the eventual
	// /ack still finds the message there.
	if n := s.Ack(sidA, []string{mine}); n != 1 {
		t.Fatalf("a working message couldn't still be acked: %d", n)
	}
	for _, body := range []string{`{"ids":["` + mine + `"]}`, `not json`} {
		if w := do(r, http.MethodPost, "/v1/agentbus/working", body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, w.Code)
		}
	}
	if w := do(r, http.MethodPost, "/v1/agentbus/working", `{"session":"ghost","ids":["`+mine+`"]}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"working":0`) {
		t.Fatalf("unknown session = %d %s", w.Code, w.Body)
	}
}
