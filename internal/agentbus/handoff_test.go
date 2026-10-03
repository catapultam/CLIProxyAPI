package agentbus

import (
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

const sidC = "cccccc11-2222-3333-4444-555555555555"

// movingBridge records SessionMoved calls. With store set, each call also
// calls into the store, which would deadlock if the store held its lock.
type movingBridge struct {
	fakeReceiptBridge
	mmu   sync.Mutex
	moves [][2]string
}

func (m *movingBridge) SessionMoved(oldID, newID string) {
	if m.store != nil {
		m.store.Peers()
	}
	m.mmu.Lock()
	defer m.mmu.Unlock()
	m.moves = append(m.moves, [2]string{oldID, newID})
}

func (m *movingBridge) moved() [][2]string {
	m.mmu.Lock()
	defer m.mmu.Unlock()
	return append([][2]string(nil), m.moves...)
}

func newMovingStore(t *testing.T) (*Store, *fakeClock, *movingBridge) {
	t.Helper()
	s, clock := newTestStore(t)
	b := &movingBridge{fakeReceiptBridge: fakeReceiptBridge{fakeOwnerBridge: fakeOwnerBridge{fakeBridge: fakeBridge{users: []string{"alex"}}, owners: map[string]bool{"UALEX": true}}}}
	b.store = s
	s.SetBridge(b)
	return s, clock, b
}

func TestDeliveriesToAClosedSessionAreRefused(t *testing.T) {
	s, _, _ := newMovingStore(t)
	capableSession(s, sidA, "/work/flyer", "flyer")
	s.Bye(sidA)
	for name, deliver := range map[string]func() error{
		"via":     func() error { _, _, err := s.DeliverVia(sidA, "hi", "alex", ""); return err },
		"name":    func() error { _, _, err := s.Deliver("flyer", "hi", "alex"); return err },
		"guest":   func() error { _, _, err := s.DeliverGuest(sidA, "hi", "bob", ViaGroup); return err },
		"notice":  func() error { _, _, err := s.DeliverNotice(sidA, "linked"); return err },
		"command": func() error { _, _, err := s.DeliverCommand(sidA, compactCmd, "alex", "UALEX"); return err },
	} {
		if err := deliver(); !errors.Is(err, ErrUnknownTarget) {
			t.Fatalf("%s: err = %v, want ErrUnknownTarget", name, err)
		}
	}
	if s.Pending(sidA) {
		t.Fatalf("queued for a closed session: %+v", s.Claim(sidA))
	}
	if _, _, err := s.CommandCapable(sidA); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("CommandCapable err = %v", err)
	}
}

func TestHandOffCarriesNameInboxAndUnacked(t *testing.T) {
	s, _, b := newMovingStore(t)
	capableSession(s, sidA, "/work/flyer", "flyer")
	s.SetModVersion(sidA, MinAckModVersion)
	// One message handed out and not acked yet, one still queued.
	_, handed, err := s.DeliverVia(sidA, "first", "alex", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ClaimForWait(sidA, true, MinAckModVersion); len(got) != 1 {
		t.Fatalf("claimed %+v", got)
	}
	_, queued, err := s.DeliverVia(sidA, "second", "alex", "")
	if err != nil {
		t.Fatal(err)
	}
	s.Bye(sidA)
	s.Hello(sidC, "pc", "", "", true)
	if err = s.HandOff(sidA, sidC); err != nil {
		t.Fatalf("HandOff = %v", err)
	}
	if got := b.moved(); !reflect.DeepEqual(got, [][2]string{{sidA, sidC}}) {
		t.Fatalf("SessionMoved calls = %v", got)
	}
	if id, ok := s.Resolve("flyer"); !ok || id != sidC {
		t.Fatalf("flyer resolves to %q %v", id, ok)
	}
	if got := s.Claim(sidC); len(got) != 1 || got[0].ID != queued {
		t.Fatalf("new session's inbox = %+v", got)
	}
	if s.Pending(sidA) {
		t.Fatal("the old session kept its inbox")
	}
	// The mod acks what it was handed under the old id (it captured it), and
	// the ack still counts.
	if n := s.Ack(sidA, []string{handed}); n != 1 {
		t.Fatalf("ack through the old id counted %d", n)
	}
	if _, read := b.receipts(); !reflect.DeepEqual(read, [][]string{{handed}}) {
		t.Fatalf("read receipts = %v", read)
	}
	// Deliveries by the old name now reach the new session.
	if id, _, errDeliver := s.Deliver("flyer", "third", "alex"); errDeliver != nil || id != sidC {
		t.Fatalf("Deliver(flyer) = %q, %v", id, errDeliver)
	}
}

func TestHandOffRefusals(t *testing.T) {
	s, clock, b := newMovingStore(t)
	s.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	s.Hello(sidC, "pc", "", "", true)
	// A live session can't be inherited.
	if err := s.HandOff(sidA, sidC); !errors.Is(err, ErrHandOffRefused) {
		t.Fatalf("live: err = %v", err)
	}
	// Nor one on another machine, even once it is closed.
	s.Hello(sidB, "laptop", "/work/other", "other", true)
	s.Bye(sidB)
	if err := s.HandOff(sidB, sidC); !errors.Is(err, ErrHandOffRefused) {
		t.Fatalf("other machine: err = %v", err)
	}
	// Nor an unknown, empty or same id.
	for _, prev := range []string{"", "  ", sidC, "ghost"} {
		if err := s.HandOff(prev, sidC); !errors.Is(err, ErrHandOffRefused) {
			t.Fatalf("previous %q: err = %v", prev, err)
		}
	}
	if len(b.moved()) != 0 {
		t.Fatalf("SessionMoved called: %v", b.moved())
	}
	if id, _ := s.Resolve("flyer"); id != sidA {
		t.Fatalf("flyer moved to %q", id)
	}
	// An offline (not closed) session on the same machine can be inherited,
	// once.
	clock.Advance(time.Hour)
	s.Hello(sidC, "pc", "", "", true)
	if err := s.HandOff(sidA, sidC); err != nil {
		t.Fatalf("offline: err = %v", err)
	}
	s.Hello("dddddd11-2222-3333-4444-555555555555", "pc", "", "", true)
	if err := s.HandOff(sidA, "dddddd11-2222-3333-4444-555555555555"); !errors.Is(err, ErrHandOffRefused) {
		t.Fatalf("second inheritance: err = %v", err)
	}
}

func TestHTTPHelloWithPreviousHandsOff(t *testing.T) {
	s, r := newTestServer(t)
	b := &movingBridge{fakeReceiptBridge: fakeReceiptBridge{fakeOwnerBridge: fakeOwnerBridge{fakeBridge: fakeBridge{users: []string{"alex"}}}}}
	s.SetBridge(b)
	s.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	s.Bye(sidA)
	w := do(r, http.MethodPost, "/v1/agentbus/hello", `{"session":"`+sidC+`","machine":"pc","mod":true,"version":"0.3.7","previous":" `+sidA+` "}`)
	if w.Code != http.StatusOK {
		t.Fatalf("hello = %d %s", w.Code, w.Body)
	}
	if id, ok := s.Resolve("flyer"); !ok || id != sidC {
		t.Fatalf("flyer resolves to %q %v", id, ok)
	}
	if got := b.moved(); !reflect.DeepEqual(got, [][2]string{{sidA, sidC}}) {
		t.Fatalf("SessionMoved calls = %v", got)
	}
}

func TestResolveLiveSkipsOfflineSessions(t *testing.T) {
	s, clock := newTestStore(t)
	s.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	if id, ok := s.ResolveLive("flyer"); !ok || id != sidA {
		t.Fatalf("live: %q %v", id, ok)
	}
	if got := s.SessionStatus(sidA); got != StatusIdle {
		t.Fatalf("status = %q", got)
	}
	clock.Advance(time.Hour)
	if id, ok := s.ResolveLive("flyer"); ok {
		t.Fatalf("offline session resolved live: %q", id)
	}
	if id, ok := s.Resolve("flyer"); !ok || id != sidA {
		t.Fatalf("Resolve still finds offline sessions: %q %v", id, ok)
	}
	if got := s.SessionStatus(sidA); got != StatusOffline {
		t.Fatalf("status = %q", got)
	}
	if got := s.SessionStatus("ghost"); got != "" {
		t.Fatalf("unknown status = %q", got)
	}
}
