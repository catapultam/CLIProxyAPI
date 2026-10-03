package slackbridge

import (
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// Minor: in a linked conversation with guests, an owner's shell (and image)
// commands are refused: their output would be posted where guests read it.
// Slash and prompt commands still run.
func TestShellCommandsRefusedWhereGuestsRead(t *testing.T) {
	b, f, bus, dir := newCommandBridge(t)
	writeCommand(t, dir, "screenshot.yaml", shellYAML, mtime0)
	linkGroup(t, b, bus, sidA, "flyer", "Gs1")
	// No guest has written yet: only the owner is known to be there.
	b.handleEvent("EvGs2", foreignMsg("UALEX", "!screenshot", "1700009700.000002", ""))
	if m := claimOne(t, bus, sidA); m.Command == nil || m.Command.Kind != "shell" {
		t.Fatalf("owner-only shell = %+v", m)
	}
	b.handleEvent("EvGs3", foreignMsg("UBOB", "hi all", "1700009700.000003", ""))
	drainJobs(t, b)
	_ = claimOne(t, bus, sidA)
	b.handleEvent("EvGs4", foreignMsg("UALEX", "!screenshot", "1700009700.000004", ""))
	drainJobs(t, b)
	if bus.Pending(sidA) {
		t.Fatalf("shell command ran where guests read: %+v", bus.Claim(sidA))
	}
	if got := lastPostText(f); got != shellWhereGuests {
		t.Fatalf("reply = %q", got)
	}
	b.handleEvent("EvGs5", foreignMsg("UALEX", "!compact", "1700009700.000005", ""))
	if m := claimOne(t, bus, sidA); m.Command == nil || m.Command.Name != "compact" {
		t.Fatalf("slash command = %+v", m)
	}
}

// Minor: a command's outcome reaches the proxy log (✅/❌, name, machine),
// never its output.
func TestCommandOutcomeIsLogged(t *testing.T) {
	b, _, bus, _ := newCommandBridge(t)
	root := threadOf(t, b, bus)
	b.handleEvent("EvOl1", msg("UALEX", "!compact", "1700009800.000001", root))
	m := claimOne(t, bus, sidA)
	drainJobs(t, b)
	old := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	defer log.StandardLogger().ReplaceHooks(old)
	hook := logtest.NewGlobal()
	sendReply(t, b, bus, sidA, "❌ !compact: exit 1\nasked by alex · ran on pc · !compact\n```\nTOP-SECRET-OUTPUT\n```", m.ID)
	sendReply(t, b, bus, sidA, "just chatting", "")
	var lines []string
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "TOP-SECRET-OUTPUT") {
			t.Fatalf("output logged: %q", e.Message)
		}
		if strings.Contains(e.Message, "!compact") {
			lines = append(lines, e.Message)
			if e.Level != log.InfoLevel {
				t.Fatalf("level = %v", e.Level)
			}
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "❌") || !strings.Contains(lines[0], "on pc") {
		t.Fatalf("outcome log lines = %q", lines)
	}
}

// Minor: a home thread moved to a second owner's DM reopens in that DM.
func TestHomeDMRemembersWhichOwner(t *testing.T) {
	b, f, bus := newHomeBridge(t, func(c *Config) { c.AllowedEmails = append(c.AllowedEmails, "jane@example.com") }, "")
	root := threadOf(t, b, bus)
	b.handleEvent("EvHo1", msg("UJANE", "!dm", "1700009900.000001", root))
	drainJobs(t, b)
	ref, ok := b.state.homeThread(sidA)
	if !ok || ref.Channel != "DUJANE" {
		t.Fatalf("home after jane's !dm = %+v %v", ref, ok)
	}
	// The thread is gone (pruned); the next post reopens it in jane's DM.
	b.state.mu.Lock()
	delete(b.state.threads, sidA)
	b.state.mu.Unlock()
	post(t, b, bus, sidA, "back again")
	if got := lastPost(t, f); got["channel"] != "DUJANE" || got["thread_ts"] != "" {
		t.Fatalf("reopened in %+v", got)
	}
	// Persisted.
	if err := b.state.flush(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadState(b.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.homeOwner(sidA); got != "UJANE" {
		t.Fatalf("reloaded home owner = %q", got)
	}
}
