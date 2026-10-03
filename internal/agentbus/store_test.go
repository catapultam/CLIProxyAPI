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
	s.Hello(sidA, "My-PC", `C:\Users\alex\Some Project`, "", true)
	if got := s.Address(sidA); got != "my-pc/some-project-aaaaaa" {
		t.Fatalf("address = %q", got)
	}
}

func TestNamesAreUniqueAndResolve(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/home/a/proj", "", true)
	s.Hello(sidB, "fedora", "/srv/ci", "", true)
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
	s.Hello(sidA, "pc", "/a", "ci", true)
	s.Hello(sidB, "pc", "/b", "ci", true)
	if id, _ := s.Resolve("ci"); id != sidA {
		t.Fatalf("name moved to %q", id)
	}
}

func TestSendClaimReturn(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.Hello(sidB, "pc", "/b", "", true)
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
	s.Hello(sidA, "pc", "/a", "", true)
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
	s.Hello(sidA, "pc", "/a", "", true)
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
	// Peers() no longer lists offline sessions, so check the status directly
	// instead of through peerStatus (which looks the address up in Peers()).
	if st := statusOf(s, sidA); st != StatusOffline {
		t.Fatalf("status = %s", st)
	}
	for _, p := range s.Peers() {
		if strings.HasSuffix(p.Address, "-aaaaaa") {
			t.Fatal("offline session still listed")
		}
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

// statusOf reads a session's status directly, including offline sessions
// that Peers() would no longer list.
func statusOf(s *Store, id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return ""
	}
	return s.statusLocked(sess, s.now())
}

func sessionExists(s *Store, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.byID[id]
	return ok
}

func modOf(s *Store, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	return ok && sess.Mod
}

func listedAddresses(s *Store) map[string]bool {
	out := make(map[string]bool)
	for _, p := range s.Peers() {
		out[p.Address] = true
	}
	return out
}

func TestInteractiveWaiterLeaseExpires(t *testing.T) {
	s, clock := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.NewWaiter(sidA)
	if st := statusOf(s, sidA); st != StatusIdle {
		t.Fatalf("status = %s", st)
	}
	clock.Advance(89 * time.Second)
	if st := statusOf(s, sidA); st != StatusIdle {
		t.Fatalf("status before lease expiry = %s", st)
	}
	if !listedAddresses(s)[s.Address(sidA)] {
		t.Fatal("fresh waiter not listed")
	}
	clock.Advance(2 * time.Second) // 91s since the waiter last refreshed
	if st := statusOf(s, sidA); st != StatusOffline {
		t.Fatalf("status after lease expiry = %s", st)
	}
	if listedAddresses(s)[s.Address(sidA)] {
		t.Fatal("expired waiter still listed")
	}
	if !sessionExists(s, sidA) {
		t.Fatal("expired waiter's session id is no longer resolvable")
	}
}

func TestByeMakesSessionOfflineImmediately(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.NewWaiter(sidA)
	s.Bye(sidA)
	if st := statusOf(s, sidA); st != StatusOffline {
		t.Fatalf("status after bye = %s", st)
	}
	if listedAddresses(s)[s.Address(sidA)] {
		t.Fatal("session still listed after bye")
	}
	if !sessionExists(s, sidA) {
		t.Fatal("closed session id is no longer resolvable")
	}
}

func TestByeUnknownSessionDoesNotCreateOne(t *testing.T) {
	s, _ := newTestStore(t)
	s.Bye(sidA)
	if sessionExists(s, sidA) {
		t.Fatal("bye created a session for an unknown id")
	}
}

func TestHelloAfterByeReopensSession(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.Bye(sidA)
	if st := statusOf(s, sidA); st != StatusOffline {
		t.Fatalf("status after bye = %s", st)
	}
	s.Hello(sidA, "pc", "/a", "", true)
	if st := statusOf(s, sidA); st != StatusIdle {
		t.Fatalf("status after hello reopened the session = %s", st)
	}
}

// TestNonInteractiveModSessionKeepsAwayBehaviour covers a mod session that
// never long-polls (Hello only, then plain Touch requests): it should still
// age from idle to away to offline on the old schedule, not the lease.
func TestNonInteractiveModSessionKeepsAwayBehaviour(t *testing.T) {
	s, clock := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	if st := statusOf(s, sidA); st != StatusIdle {
		t.Fatalf("status = %s", st)
	}
	clock.Advance(3 * time.Minute)
	s.Touch(sidA)
	if st := statusOf(s, sidA); st != StatusAway {
		t.Fatalf("status after waiter window passed = %s", st)
	}
	clock.Advance(31 * time.Minute)
	if st := statusOf(s, sidA); st != StatusOffline {
		t.Fatalf("status = %s", st)
	}
}

func TestClosedAndWaitsPersistThroughSaveLoad(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.NewWaiter(sidA)
	s.Bye(sidA)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"waits":true`) || !strings.Contains(string(data), `"closed":true`) {
		t.Fatalf("saved state missing lease fields: %s", data)
	}
	clock := &fakeClock{now: t0}
	r := NewStore(s.path, clock.Now)
	if err := r.Load(); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(r, sidA); st != StatusOffline {
		t.Fatalf("status after reload = %s", st)
	}
}

func TestMessagesExpireAfterSevenDays(t *testing.T) {
	s, clock := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
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
	s.Hello(sidA, "pc", "/a", "builder", true)
	s.Hello(sidB, "pc", "/b", "", true)
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

func TestModPersistsThroughSaveLoad(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"mod":true`) {
		t.Fatalf("saved state missing mod flag: %s", data)
	}
	clock := &fakeClock{now: t0}
	r := NewStore(s.path, clock.Now)
	if err := r.Load(); err != nil {
		t.Fatal(err)
	}
	if !modOf(r, sidA) {
		t.Fatal("mod flag not restored after load")
	}
}

// TestHelloMarksModOnlyWithExplicitMarker covers the fix for Hello setting
// Mod for any caller: curl and the legacy wait.sh hook can reach /hello and
// /wait too, so only an explicit marker may set Mod, and it is never cleared
// back to false afterward.
func TestHelloMarksModOnlyWithExplicitMarker(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", false)
	if modOf(s, sidA) {
		t.Fatal("hello without the marker set Mod")
	}
	s.Hello(sidA, "pc", "/a", "", true)
	if !modOf(s, sidA) {
		t.Fatal("hello with the marker did not set Mod")
	}
	s.Hello(sidA, "pc", "/a", "", false)
	if !modOf(s, sidA) {
		t.Fatal("a later hello without the marker cleared Mod")
	}
}

func TestNotifyFiresOnSend(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
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

func TestResolvePrefersLiveSessionWhenNameIsReused(t *testing.T) {
	// The dead holder of a name stays in the store, so a reused name has two
	// matches. Delivery must go to the live one, every time.
	for i := 0; i < 50; i++ {
		s, clock := newTestStore(t)
		s.Hello(sidA, "pc", "/a", "builder", true)
		s.Bye(sidA)
		clock.Advance(time.Second)
		s.Hello(sidB, "pc", "/b", "builder", true)
		if got, ok := s.Resolve("builder"); !ok || got != sidB {
			t.Fatalf("Resolve(builder) = %q, %v; want the live session %q", got, ok, sidB)
		}
	}
}

func TestResolveFallsBackToOfflineSession(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "builder", true)
	s.Bye(sidA)
	if got, ok := s.Resolve("builder"); !ok || got != sidA {
		t.Fatalf("Resolve(builder) = %q, %v; want the offline session so its message queues", got, ok)
	}
}
