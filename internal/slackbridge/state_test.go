package slackbridge

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

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
	if !st.setThread("sid-a", "1.1") || st.setThread("sid-a", "2.2") {
		t.Fatal("only the first thread is the session's posting thread")
	}
	if ts, _ := st.thread("sid-a"); ts != "1.1" {
		t.Fatalf("posting thread moved to %q", ts)
	}
	if sid, ok := st.session("2.2"); !ok || sid != "sid-a" {
		t.Fatalf("second thread not linked: %q %v", sid, ok)
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
