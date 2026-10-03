package agentbus

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestStore(t *testing.T) (*Store, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: t0}
	return NewStore(filepath.Join(t.TempDir(), "agentbus-state.json"), clock.Now), clock
}

const (
	sidA = "aaaaaa11-2222-3333-4444-555555555555"
	sidB = "bbbbbb11-2222-3333-4444-555555555555"
)

func TestAddressBeforeAndAfterHello(t *testing.T) {
	s, _ := newTestStore(t)
	s.Touch(sidA)
	if got := s.Address(sidA); got != "unknown/session-aaaaaa" {
		t.Fatalf("address = %q", got)
	}
	s.Hello(sidA, "My-PC", `C:\Users\alex\Some Project`, "")
	if got := s.Address(sidA); got != "my-pc/some-project-aaaaaa" {
		t.Fatalf("address = %q", got)
	}
}

func TestNamesAreUniqueAndResolve(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/home/a/proj", "")
	s.Hello(sidB, "fedora", "/srv/ci", "")
	if err := s.SetName(sidA, "Builder"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetName(sidB, "builder"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("err = %v, want ErrNameTaken", err)
	}
	if id, ok := s.Resolve("builder"); !ok || id != sidA {
		t.Fatalf("resolve name = %q %v", id, ok)
	}
	if id, ok := s.Resolve("fedora/ci-bbbbbb"); !ok || id != sidB {
		t.Fatalf("resolve address = %q %v", id, ok)
	}
	if err := s.SetName(sidA, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Resolve("builder"); ok {
		t.Fatal("cleared name still resolves")
	}
}

func TestHelloNameIgnoredWhenTaken(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "ci")
	s.Hello(sidB, "pc", "/b", "ci")
	if id, _ := s.Resolve("ci"); id != sidA {
		t.Fatalf("name moved to %q", id)
	}
}

func TestSendClaimReturn(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "")
	s.Hello(sidB, "pc", "/b", "")
	m1, err := s.Send(sidA, s.Address(sidB), "first", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Send(sidA, s.Address(sidB), "second", m1.ID); err != nil {
		t.Fatal(err)
	}
	got := s.Claim(sidB)
	if len(got) != 2 || got[0].Body != "first" || got[1].ReplyTo != m1.ID || got[0].From != s.Address(sidA) {
		t.Fatalf("claimed = %+v", got)
	}
	if again := s.Claim(sidB); len(again) != 0 {
		t.Fatalf("claimed twice: %+v", again)
	}
	m3, _ := s.Send(sidA, s.Address(sidB), "third", "")
	s.Return(sidB, got)
	all := s.Claim(sidB)
	if len(all) != 3 || all[0].ID != m1.ID || all[2].ID != m3.ID {
		t.Fatalf("returned order = %+v", all)
	}
}

func TestSendErrors(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "")
	if _, err := s.Send(sidA, "nobody", "x", ""); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Send("not-a-session", s.Address(sidA), "x", ""); !errors.Is(err, ErrUnknownSender) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Send(sidA, s.Address(sidA), strings.Repeat("x", MaxBodyBytes+1), ""); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Send(sidA, s.Address(sidA), "  ", ""); !errors.Is(err, ErrEmptyBody) {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusTransitions(t *testing.T) {
	s, clock := newTestStore(t)
	s.Touch(sidA)
	if st := peerStatus(t, s, sidA); st != StatusAway {
		t.Fatalf("status = %s", st)
	}
	s.Hello(sidA, "pc", "/a", "")
	if st := peerStatus(t, s, sidA); st != StatusIdle {
		t.Fatalf("status = %s", st)
	}
	s.BeginRequest(sidA)
	if st := peerStatus(t, s, sidA); st != StatusBusy {
		t.Fatalf("status = %s", st)
	}
	s.EndRequest(sidA)
	clock.Advance(3 * time.Minute)
	if st := peerStatus(t, s, sidA); st != StatusAway {
		t.Fatalf("status after waiter gone = %s", st)
	}
	clock.Advance(31 * time.Minute)
	if st := peerStatus(t, s, sidA); st != StatusOffline {
		t.Fatalf("status = %s", st)
	}
	clock.Advance(25 * time.Hour)
	for _, p := range s.Peers() {
		if strings.HasSuffix(p.Address, "-aaaaaa") {
			t.Fatal("offline session older than 24h still listed")
		}
	}
}

func peerStatus(t *testing.T, s *Store, id string) string {
	t.Helper()
	for _, p := range s.Peers() {
		if p.Address == s.Address(id) {
			return p.Status
		}
	}
	t.Fatalf("session %s not listed", id)
	return ""
}

func TestMessagesExpireAfterSevenDays(t *testing.T) {
	s, clock := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "")
	if _, err := s.Send(sidA, s.Address(sidA), "old", ""); err != nil {
		t.Fatal(err)
	}
	clock.Advance(7*24*time.Hour + time.Minute)
	if got := s.Claim(sidA); len(got) != 0 {
		t.Fatalf("expired message delivered: %+v", got)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "builder")
	s.Hello(sidB, "pc", "/b", "")
	if _, err := s.Send(sidB, "builder", "persist me", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: t0}
	r := NewStore(s.path, clock.Now)
	if err := r.Load(); err != nil {
		t.Fatal(err)
	}
	if id, ok := r.Resolve("builder"); !ok || id != sidA {
		t.Fatalf("name not restored: %q %v", id, ok)
	}
	if got := r.Claim(sidA); len(got) != 1 || got[0].Body != "persist me" {
		t.Fatalf("inbox not restored: %+v", got)
	}
	if err := NewStore(filepath.Join(t.TempDir(), "missing.json"), clock.Now).Load(); err != nil {
		t.Fatalf("missing state file: %v", err)
	}
}

// TestLoadIgnoresLegacySetupHinted confirms a state file saved by a version
// that still had the removed setup_hinted field loads without error.
func TestLoadIgnoresLegacySetupHinted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentbus-state.json")
	legacy := `{"version":1,"sessions":[{"id":"` + sidA + `","setup_hinted":true}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewStore(path, nil)
	if err := r.Load(); err != nil {
		t.Fatalf("load legacy state: %v", err)
	}
	if r.Address(sidA) == "" {
		t.Fatal("session not restored from legacy state")
	}
}

func TestNotifyFiresOnSend(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "")
	ch := s.Notify(sidA)
	if _, err := s.Send(sidA, s.Address(sidA), "ping", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	default:
		t.Fatal("notify channel not closed by Send")
	}
}
