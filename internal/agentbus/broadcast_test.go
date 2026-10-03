package agentbus

import (
	"errors"
	"strings"
	"testing"
)

func TestAllIsReserved(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "ALL", true)
	if id, ok := s.Resolve("all"); ok {
		t.Fatalf("hello claimed all: %s", id)
	}
	if err := s.SetName(sidA, "all"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("SetName(all) = %v", err)
	}
	if err := s.SetName(sidA, "All"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("SetName(All) = %v", err)
	}
	// A session still named "all" from before never resolves.
	s.mu.Lock()
	s.byID[sidA].Name = "all"
	s.mu.Unlock()
	if _, ok := s.Resolve(" All "); ok {
		t.Fatal("all resolved to a session")
	}
	if _, _, err := s.DeliverVia("all", "hi", "alex", ""); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("DeliverVia(all) = %v", err)
	}
	s.Hello(sidB, "pc", "/b", "", true)
	if _, err := s.Send(sidB, "all", "hi", ""); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("Send(all) = %v", err)
	}
}

func TestInjectNoteMentionsBroadcasts(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	got := injectedText(t, r, seen)
	if !strings.Contains(got, "Messages from Slack may be broadcasts to all agents (`all:`); answer only if relevant to you, otherwise dismiss with `ignore`.") {
		t.Fatalf("note lacks the broadcast line:\n%s", got)
	}
}
