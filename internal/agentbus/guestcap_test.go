package agentbus

import (
	"strconv"
	"testing"
)

// Item 3: a session's inbox holds at most maxQueuedGuest guest messages; the
// oldest guest messages go first, and nothing else is dropped.
func TestGuestMessagesAreCappedPerSession(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	if _, _, err := s.DeliverVia(sidA, "from alex", "alex", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxQueuedGuest+5; i++ {
		if _, _, err := s.DeliverGuest(sidA, "guest "+strconv.Itoa(i), "bob", ViaGroup); err != nil {
			t.Fatal(err)
		}
	}
	msgs := s.Claim(sidA)
	if len(msgs) != maxQueuedGuest+1 {
		t.Fatalf("queued %d messages, want %d", len(msgs), maxQueuedGuest+1)
	}
	if msgs[0].Body != "from alex" || msgs[1].Body != "guest 5" || msgs[len(msgs)-1].Body != "guest "+strconv.Itoa(maxQueuedGuest+4) {
		t.Fatalf("kept %q, %q … %q", msgs[0].Body, msgs[1].Body, msgs[len(msgs)-1].Body)
	}
}
