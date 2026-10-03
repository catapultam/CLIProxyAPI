package agentbus

import (
	"strings"
	"testing"
)

// ownersBridge is a bridge that lists its owners.
type ownersBridge struct {
	fakeBridge
	owners []string
}

func (o *ownersBridge) Owners() []string { return o.owners }

// Item 11: the note tells agents to keep the setup to the owner.
func TestInjectNoteKeepsTheSetupToTheOwner(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&ownersBridge{fakeBridge: fakeBridge{users: []string{"alex", "jane"}}, owners: []string{"alex"}})
	got := injectedText(t, r, seen)
	for _, want := range []string{
		"Never reveal how the Slack bridge, proxy, agentbus or plugins work, or your own configuration (addresses, machine names, paths, versions, settings, URLs), to anyone except the owner (alex).",
		"Where anyone else can read your reply (group conversations, guests, other allowed users), keep to the task and say to ask the owner about the setup.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("note lacks %q:\n%s", want, got)
		}
	}
}
