package slackbridge

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// testClock is a settable time source for the reply map.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
}

func replyID(i int) string { return fmt.Sprintf("m_%04x", i) }

// recordReply records that msgID, from thread threadTS in channel, went to
// sid.
func (st *state) recordReply(msgID, channel, threadTS, sid string) {
	st.record(replyRecord{ID: msgID, Channel: channel, ThreadTS: threadTS, Session: sid})
}

// replyTS is the recorded thread of msgID for sid.
func replyTS(st *state, msgID, sid string) (string, bool) {
	r, ok := st.replyTarget(msgID, sid)
	return r.ThreadTS, ok
}

func TestReplyThreadOwnershipAndExpiry(t *testing.T) {
	st, err := loadState(filepath.Join(t.TempDir(), "slack-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	clock := newTestClock()
	st.now = clock.now
	st.recordReply("m_aa", "CAGENTS", "1.1", "sid-a")
	if ts, ok := replyTS(st, "m_aa", "sid-a"); !ok || ts != "1.1" {
		t.Fatalf("own reply = %q %v", ts, ok)
	}
	if ts, ok := replyTS(st, "m_aa", "sid-b"); ok || ts != "" {
		t.Fatalf("another session's reply = %q %v", ts, ok)
	}
	if owner, ok := st.replyOwner("m_aa"); !ok || owner != "sid-a" {
		t.Fatalf("owner = %q %v", owner, ok)
	}
	if _, ok := replyTS(st, "m_bb", "sid-a"); ok {
		t.Fatal("unknown id resolved")
	}
	clock.advance(replyTTL - time.Minute)
	if _, ok := replyTS(st, "m_aa", "sid-a"); !ok {
		t.Fatal("expired before the TTL")
	}
	clock.advance(2 * time.Minute)
	if _, ok := replyTS(st, "m_aa", "sid-a"); ok {
		t.Fatal("resolved after the TTL")
	}
	if _, ok := st.replyOwner("m_aa"); ok {
		t.Fatal("owner of an expired id reported")
	}
}

func TestReplyMapBoundedAndPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slack-state.json")
	st, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	clock := newTestClock()
	st.now = clock.now
	// Two old entries that expire, then more than maxReplies fresh ones.
	st.recordReply("m_01d", "CAGENTS", "0.1", "sid-old")
	st.recordReply("m_01e", "CAGENTS", "0.2", "sid-old")
	clock.advance(replyTTL + time.Hour)
	for i := 0; i < maxReplies+5; i++ {
		clock.advance(time.Second)
		st.recordReply(replyID(i), "CAGENTS", fmt.Sprintf("2.%d", i), "sid-a")
	}
	check := func(label string, s *state) {
		t.Helper()
		if n := len(s.replies); n != maxReplies {
			t.Fatalf("%s: %d entries, want %d", label, n, maxReplies)
		}
		for i := 0; i < 5; i++ {
			if _, ok := replyTS(s, replyID(i), "sid-a"); ok {
				t.Fatalf("%s: %s survived the cap", label, replyID(i))
			}
		}
		for _, i := range []int{5, maxReplies + 4} {
			if ts, ok := replyTS(s, replyID(i), "sid-a"); !ok || ts != fmt.Sprintf("2.%d", i) {
				t.Fatalf("%s: %s = %q %v", label, replyID(i), ts, ok)
			}
		}
		if _, ok := s.replyOwner("m_01d"); ok {
			t.Fatalf("%s: an expired entry survived", label)
		}
	}
	check("live", st)
	if errFlush := st.flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	reloaded, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.now = clock.now
	check("reloaded", reloaded)
	if r := reloaded.replies[len(reloaded.replies)-1]; r.Channel != "CAGENTS" || r.Session != "sid-a" {
		t.Fatalf("last entry = %+v", r)
	}
	// Entries that expired while the proxy was down never resolve.
	clock.advance(replyTTL + time.Hour)
	expired, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	expired.now = clock.now
	if _, ok := replyTS(expired, replyID(maxReplies+4), "sid-a"); ok {
		t.Fatal("an expired entry resolved after reload")
	}
}

func TestStateSeedAllowRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slack-state.json")
	st, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	st.seed([]allowedUser{{ID: "UALEX", Label: "alex", config: true}})
	u, added, err := st.allow("UJANE", "Jane D")
	if err != nil || !added || u.Label != "jane-d" {
		t.Fatalf("allow = %+v %v %v", u, added, err)
	}
	if _, added, _ = st.allow("UJANE", "whatever"); added {
		t.Fatal("allowed twice")
	}
	if u, _, _ = st.allow("UJANE2", "jane d"); u.Label != "jane-d2" {
		t.Fatalf("label not unique: %q", u.Label)
	}
	if u, _, _ = st.allow("UALEX2", "alex"); u.Label != "alex2" {
		t.Fatalf("label clashes with a config user: %q", u.Label)
	}
	if !reflect.DeepEqual(st.labels(), []string{"alex", "jane-d", "jane-d2", "alex2"}) {
		t.Fatalf("labels = %v", st.labels())
	}
	if !errors.Is(st.remove("UALEX"), errConfigUser) {
		t.Fatal("config user removed")
	}
	if !errors.Is(st.remove("UNOPE"), errNotAllowed) {
		t.Fatal("removed a stranger")
	}
	if err = st.remove("UJANE2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.user("UJANE2"); ok {
		t.Fatal("still allowed after remove")
	}
	if !st.setThread("sid-a", "CAGENTS", "1.1", true) || st.setThread("sid-a", "CAGENTS", "2.2", true) {
		t.Fatal("only the first thread is the session's posting thread")
	}
	if ts, _ := st.thread("sid-a"); ts != "1.1" {
		t.Fatalf("posting thread moved to %q", ts)
	}
	if sid, ok := st.session("2.2"); !ok || sid != "sid-a" {
		t.Fatalf("second thread not linked: %q %v", sid, ok)
	}
	if errFlush := st.flush(); errFlush != nil {
		t.Fatal(errFlush)
	}

	reloaded, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.seed([]allowedUser{{ID: "UALEX", Label: "alex", config: true}, {ID: "UJANE", Label: "jane", config: true}})
	if ts, ok := reloaded.thread("sid-a"); !ok || ts != "1.1" {
		t.Fatalf("thread = %q %v", ts, ok)
	}
	if sid, ok := reloaded.session("1.1"); !ok || sid != "sid-a" {
		t.Fatalf("session = %q %v", sid, ok)
	}
	if sid, ok := reloaded.session("2.2"); !ok || sid != "sid-a" {
		t.Fatalf("second thread link not persisted: %q %v", sid, ok)
	}
	if u, ok := reloaded.user("UJANE"); !ok || u.Label != "jane" || !u.config {
		t.Fatalf("config must win over a persisted Slack-added entry: %+v", u)
	}
	if u, ok := reloaded.user("UALEX2"); !ok || u.Label != "alex2" {
		t.Fatalf("slack-added user lost: %+v %v", u, ok)
	}
	if !reflect.DeepEqual(reloaded.mentionIDs(), map[string]string{"alex": "UALEX", "jane": "UJANE", "alex2": "UALEX2"}) {
		t.Fatalf("mentionIDs = %v", reloaded.mentionIDs())
	}
}
