package agentbus

import (
	"strings"
	"testing"
)

// namedBridge is a bridge that knows its Slack bot's display name.
type namedBridge struct {
	fakeBridge
	name string
}

func (n *namedBridge) BotName() string { return n.name }

// Item 8: the note names the Slack bot as Slack shows it now.
func TestInjectNoteNamesTheSlackBot(t *testing.T) {
	s, r, seen := newInjectServer(t, &fakeClock{now: t0})
	s.Touch(sidA)
	s.SetBridge(&namedBridge{fakeBridge: fakeBridge{users: []string{"alex"}}, name: "clanker-bro"})
	got := injectedText(t, r, seen)
	if !strings.Contains(got, "@clanker-bro") {
		t.Fatalf("note doesn't name the bot:\n%s", got)
	}
}
