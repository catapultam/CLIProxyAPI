package agentbus

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// fakeReceiptBridge records receipts. With store set, each receipt also calls
// into the store, which would deadlock if the store held its lock.
type fakeReceiptBridge struct {
	fakeOwnerBridge
	store    *Store
	rmu      sync.Mutex
	received [][]string
	read     [][]string
}

func (f *fakeReceiptBridge) Received(ids []string) {
	if f.store != nil {
		f.store.Peers()
	}
	f.rmu.Lock()
	defer f.rmu.Unlock()
	f.received = append(f.received, append([]string(nil), ids...))
}

func (f *fakeReceiptBridge) Read(ids []string) {
	if f.store != nil {
		f.store.Peers()
	}
	f.rmu.Lock()
	defer f.rmu.Unlock()
	f.read = append(f.read, append([]string(nil), ids...))
}

func (f *fakeReceiptBridge) receipts() (received, read [][]string) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	return append([][]string(nil), f.received...), append([][]string(nil), f.read...)
}

func newReceiptServer(t *testing.T) (*Store, *gin.Engine, *fakeReceiptBridge) {
	t.Helper()
	s, r := newTestServer(t)
	b := &fakeReceiptBridge{fakeOwnerBridge: fakeOwnerBridge{fakeBridge: fakeBridge{users: []string{"alex"}}, owners: map[string]bool{"UALEX": true}}}
	b.store = s
	s.SetBridge(b)
	return s, r, b
}

// waitIDs runs one /wait for sid as a current mod and returns the ids it got.
func waitIDs(t *testing.T, r *gin.Engine, sid string) []string {
	t.Helper()
	w := do(r, http.MethodGet, "/v1/agentbus/wait?session="+sid+"&mod=1&v=0.3.4", "")
	if w.Code != http.StatusOK {
		t.Fatalf("wait = %d %s", w.Code, w.Body)
	}
	var resp struct {
		Messages []Message `json:"messages"`
	}
	if errJSON := json.Unmarshal(w.Body.Bytes(), &resp); errJSON != nil {
		t.Fatal(errJSON)
	}
	var ids []string
	for _, m := range resp.Messages {
		ids = append(ids, m.ID)
	}
	return ids
}

func ack(r *gin.Engine, sid string, ids ...string) (int, string) {
	body, _ := json.Marshal(map[string]any{"session": sid, "ids": ids})
	w := do(r, http.MethodPost, "/v1/agentbus/ack", string(body))
	return w.Code, w.Body.String()
}

func TestWaitClaimReportsReceivedForSlackMessages(t *testing.T) {
	s, r, b := newReceiptServer(t)
	capableSession(s, sidA, "/a", "flyer")
	s.Hello(sidB, "pc", "/b", "", true)
	_, slackID, err := s.Deliver(sidA, "please rebase", "alex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Send(sidB, s.Address(sidA), "peer note", ""); err != nil {
		t.Fatal(err)
	}
	if ids := waitIDs(t, r, sidA); len(ids) != 2 {
		t.Fatalf("wait ids = %v", ids)
	}
	received, read := b.receipts()
	if !reflect.DeepEqual(received, [][]string{{slackID}}) || len(read) != 0 {
		t.Fatalf("received = %v, read = %v", received, read)
	}
}

func TestWaitClaimOfCommandReportsReceived(t *testing.T) {
	s, r, b := newReceiptServer(t)
	capableSession(s, sidA, "/a", "flyer")
	_, cmdID, err := s.DeliverCommand(sidA, compactCmd, "alex", "UALEX")
	if err != nil {
		t.Fatal(err)
	}
	if ids := waitIDs(t, r, sidA); !reflect.DeepEqual(ids, []string{cmdID}) {
		t.Fatalf("wait ids = %v", ids)
	}
	if received, _ := b.receipts(); !reflect.DeepEqual(received, [][]string{{cmdID}}) {
		t.Fatalf("received = %v", received)
	}
	if code, body := ack(r, sidA, cmdID); code != http.StatusOK || !strings.Contains(body, `"acked":1`) {
		t.Fatalf("ack = %d %s", code, body)
	}
	if _, read := b.receipts(); !reflect.DeepEqual(read, [][]string{{cmdID}}) {
		t.Fatalf("read = %v", read)
	}
}

func TestAckReportsReadOnlyForIDsWaitedBySession(t *testing.T) {
	s, r, b := newReceiptServer(t)
	capableSession(s, sidA, "/a", "flyer")
	capableSession(s, sidB, "/b", "other")
	_, idA, _ := s.Deliver(sidA, "for a", "alex")
	_, idB, _ := s.Deliver(sidB, "for b", "alex")
	_, idSecond, _ := s.Deliver(sidA, "second", "alex")
	waitIDs(t, r, sidA)
	waitIDs(t, r, sidB)
	// idInbox is never waited for, so acknowledging it does nothing.
	_, idInbox, _ := s.Deliver(sidA, "not waited", "alex")

	code, body := ack(r, sidA, idA, idB, idInbox, "m_ffff", "junk")
	if code != http.StatusOK || !strings.Contains(body, `"acked":1`) {
		t.Fatalf("ack = %d %s", code, body)
	}
	if _, read := b.receipts(); !reflect.DeepEqual(read, [][]string{{idA}}) {
		t.Fatalf("read = %v", read)
	}
	// An id is read once.
	if code, body = ack(r, sidA, idA); code != http.StatusOK || !strings.Contains(body, `"acked":0`) {
		t.Fatalf("second ack = %d %s", code, body)
	}
	if code, _ = ack(r, sidA, idSecond); code != http.StatusOK {
		t.Fatalf("ack queued = %d", code)
	}
	if _, read := b.receipts(); !reflect.DeepEqual(read, [][]string{{idA}, {idSecond}}) {
		t.Fatalf("read = %v", read)
	}
	// idB is sidB's to acknowledge.
	ack(r, sidB, idB)
	if _, read := b.receipts(); !reflect.DeepEqual(read, [][]string{{idA}, {idSecond}, {idB}}) {
		t.Fatalf("read = %v", read)
	}
	if !s.Pending(sidA) {
		t.Fatal("an ack took a queued message")
	}
}

func TestAckRejectsBadRequests(t *testing.T) {
	_, r, b := newReceiptServer(t)
	for _, body := range []string{`not json`, `{"ids":["m_1"]}`, `{"session":"  ","ids":["m_1"]}`} {
		if w := do(r, http.MethodPost, "/v1/agentbus/ack", body); w.Code != http.StatusBadRequest {
			t.Fatalf("ack %s = %d", body, w.Code)
		}
	}
	many := make([]string, maxAckIDs+1)
	for i := range many {
		many[i] = "m_1"
	}
	if code, _ := ack(r, sidA, many...); code != http.StatusBadRequest {
		t.Fatalf("too many ids = %d", code)
	}
	// An unknown session acknowledges nothing.
	if code, body := ack(r, "ghost", "m_1"); code != http.StatusOK || !strings.Contains(body, `"acked":0`) {
		t.Fatalf("unknown session = %d %s", code, body)
	}
	if _, read := b.receipts(); len(read) != 0 {
		t.Fatalf("read = %v", read)
	}
}

func TestAckWithoutReceiptsBridge(t *testing.T) {
	s, r := newTestServer(t)
	capableSession(s, sidA, "/a", "flyer")
	s.SetBridge(&fakeOwnerBridge{fakeBridge: fakeBridge{users: []string{"alex"}}})
	_, id, _ := s.Deliver(sidA, "x", "alex")
	waitIDs(t, r, sidA)
	if code, body := ack(r, sidA, id); code != http.StatusOK || !strings.Contains(body, `"acked":1`) {
		t.Fatalf("ack = %d %s", code, body)
	}
}

func TestInjectCommitReportsRead(t *testing.T) {
	clock := &fakeClock{now: t0}
	s, r, got := newInjectServer(t, clock)
	b := &fakeReceiptBridge{fakeOwnerBridge: fakeOwnerBridge{fakeBridge: fakeBridge{users: []string{"alex"}}}}
	b.store = s
	s.SetBridge(b)
	s.Hello(sidA, "pc", "/a", "", false)
	s.Hello(sidB, "pc", "/b", "", false)
	_, slackID, _ := s.Deliver(sidA, "hi from slack", "alex")
	if _, err := s.Send(sidB, s.Address(sidA), "peer", ""); err != nil {
		t.Fatal(err)
	}

	// A failed request returns the messages: nothing was read.
	got.status = http.StatusInternalServerError
	post(r, sidA, "", stringContentBody)
	if received, read := b.receipts(); len(received) != 0 || len(read) != 0 {
		t.Fatalf("after a failed request: received = %v, read = %v", received, read)
	}
	got.status = http.StatusOK
	post(r, sidA, "", stringContentBody)
	received, read := b.receipts()
	if len(received) != 0 || !reflect.DeepEqual(read, [][]string{{slackID}}) {
		t.Fatalf("received = %v, read = %v", received, read)
	}
}

func TestOneShotHintOnlyWithoutModAndOnlyForSlack(t *testing.T) {
	clock := &fakeClock{now: t0}
	s, r, got := newInjectServer(t, clock)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	s.Hello(sidA, "pc", "/a", "", false)
	s.Hello(sidB, "pc", "/b", "", true)

	// A peer message alone gets no hint.
	if _, err := s.Send(sidB, s.Address(sidA), "peer", ""); err != nil {
		t.Fatal(err)
	}
	post(r, sidA, "", stringContentBody)
	if strings.Contains(got.body, oneShotHint) {
		t.Fatal("hint for a peer message")
	}
	// A Slack message to a session without the mod gets it, once per batch.
	_, _, _ = s.Deliver(sidA, "one", "alex")
	_, _, _ = s.Deliver(sidA, "two", "alex")
	post(r, sidA, "", stringContentBody)
	if n := strings.Count(got.body, oneShotHint); n != 1 {
		t.Fatalf("hint count = %d in %s", n, got.body)
	}
	// A session with the mod gets none.
	_, _, _ = s.Deliver(sidB, "three", "alex")
	post(r, sidB, "", stringContentBody)
	if !strings.Contains(got.body, "three") || strings.Contains(got.body, oneShotHint) {
		t.Fatalf("mod session body = %s", got.body)
	}
}

func TestUnackedSurvivesSaveLoadAndExpires(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: t0}
	s := NewStore(filepath.Join(dir, "state.json"), clock.Now)
	capableSession(s, sidA, "/a", "flyer")
	_, id, _ := s.Deliver(sidA, "x", "alex")
	if msgs := s.ClaimForWait(sidA, "0.3.4"); len(msgs) != 1 {
		t.Fatalf("msgs = %+v", msgs)
	}
	if errSave := s.Save(); errSave != nil {
		t.Fatal(errSave)
	}
	loaded := NewStore(filepath.Join(dir, "state.json"), clock.Now)
	if errLoad := loaded.Load(); errLoad != nil {
		t.Fatal(errLoad)
	}
	b := &fakeReceiptBridge{}
	loaded.SetBridge(b)
	if n := loaded.Ack(sidA, []string{id}); n != 1 {
		t.Fatalf("ack after load = %d", n)
	}

	// An entry older than the message TTL is dropped.
	_, id2, _ := s.Deliver(sidA, "y", "alex")
	s.ClaimForWait(sidA, "0.3.4")
	clock.Advance(messageTTL + 1)
	s.Pending(sidA)
	if n := s.Ack(sidA, []string{id2}); n != 0 {
		t.Fatalf("expired ack = %d", n)
	}
}

func TestUnackedIsCapped(t *testing.T) {
	s, clock := newTestStore(t)
	capableSession(s, sidA, "/a", "flyer")
	var ids []string
	for i := 0; i <= maxUnacked; i++ {
		_, id, _ := s.Deliver(sidA, "x", "alex")
		ids = append(ids, id)
		s.ClaimForWait(sidA, "0.3.4")
		clock.Advance(time.Second)
	}
	if n := s.Ack(sidA, ids[:1]); n != 0 {
		t.Fatal("the oldest unacked id was kept past the cap")
	}
	if n := s.Ack(sidA, ids[1:]); n != maxUnacked {
		t.Fatalf("acked %d of the newest %d", n, maxUnacked)
	}
}
