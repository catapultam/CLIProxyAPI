package agentbus

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Minor: the one-shot hint follows instructions from allowed users, never a
// guest's message alone.
func TestOneShotHintOnlyForAllowedUsers(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	if _, _, err := s.DeliverGuest(sidA, "hello", "bob", ViaGroup); err != nil {
		t.Fatal(err)
	}
	if got := injectedText(t, r, seen); strings.Contains(got, oneShotHint) {
		t.Fatalf("hint after a guest message:\n%s", got)
	}
	if _, _, err := s.DeliverVia(sidA, "do it", "alex", ""); err != nil {
		t.Fatal(err)
	}
	if got := injectedText(t, r, seen); !strings.Contains(got, oneShotHint) {
		t.Fatalf("no hint after an instruction:\n%s", got)
	}
}

// Minor: /inbox claims report their receipts; nothing acks them, so they
// are read.
func TestHTTPInboxReportsReceipts(t *testing.T) {
	s, r, b := newReceiptServer(t)
	s.Hello(sidA, "pc", "/a", "", false)
	_, id, err := s.DeliverVia(sidA, "check", "alex", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := do(r, http.MethodGet, "/v1/agentbus/inbox?session="+sidA, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), id) {
		t.Fatalf("inbox = %d %s", w.Code, w.Body)
	}
	if _, read := b.receipts(); !reflect.DeepEqual(read, [][]string{{id}}) {
		t.Fatalf("read receipts = %v", read)
	}
}
