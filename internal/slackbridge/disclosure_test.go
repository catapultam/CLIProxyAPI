package slackbridge

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

// Minor (and item 11): command messages carry where they were written, so
// the mod can frame prompt commands from a DM as private and leave the
// machine out of reports in group conversations.
func TestCommandMessagesCarryVia(t *testing.T) {
	b, _, bus, _ := newCommandBridge(t)
	b.handleEvent("EvCv1", dmMsg("UALEX", "flyer: !compact", "1700009600.000001", ""))
	if m := claimOne(t, bus, sidA); m.Command == nil || m.Via != agentbus.ViaDM {
		t.Fatalf("DM command = %+v", m)
	}
	b.handleEvent("EvCv2", foreignMsg("UALEX", "flyer: !compact", "1700009600.000002", ""))
	if m := claimOne(t, bus, sidA); m.Command == nil || m.Via != agentbus.ViaGroup {
		t.Fatalf("group command = %+v", m)
	}
	b.handleEvent("EvCv3", msg("UALEX", "flyer: !compact", "1700009600.000003", ""))
	if m := claimOne(t, bus, sidA); m.Command == nil || m.Via != "" {
		t.Fatalf("channel command = %+v", m)
	}
}

// Item 11: where people other than owners can read, the bridge never posts
// an agent's address or machine: its first top-level post there carries
// its name only.
func TestFirstPostInGroupShowsNoAddressOrMachine(t *testing.T) {
	b, f, bus := newTestBridge(t)
	linkGroup(t, b, bus, sidA, "flyer", "Dc1")
	b.handleEvent("EvDc2", foreignMsg("UBOB", "who are you?", "1700009500.000002", ""))
	drainJobs(t, b)
	g := claimOne(t, bus, sidA)
	sendReply(t, b, bus, sidA, "an agent", g.ID)
	got := lastPost(t, f)
	if got["channel"] != "GMPIM1" || got["text"] != "*flyer*\nan agent" {
		t.Fatalf("first post = %+v", got)
	}
	// An unnamed agent is "an agent".
	b.handleEvent("EvDc3", foreignMsg("UALEX", "<@UBOT> link pc/other-bbbbbb", "1700009500.000003", ""))
	drainJobs(t, b)
	n := claimNotice(t, bus, sidB)
	_ = claimNotice(t, bus, sidA) // the unlink notice
	sendReply(t, b, bus, sidB, "hello", n.ID)
	if got := lastPost(t, f); got["channel"] != "GMPIM1" || got["text"] != "*an agent*\nhello" {
		t.Fatalf("unnamed first post = %+v", got)
	}
	for _, p := range postsTo(f, "GMPIM1") {
		if strings.Contains(p["text"], "pc/") || strings.Contains(p["text"], " · pc") {
			t.Fatalf("leaked setup into the group: %q", p["text"])
		}
	}
}

// Item 11: a DM to an allowed user who isn't an owner carries no address
// or machine; an owner's DM and the main channel keep the full header.
func TestHeadersKeepSetupToOwners(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	sendDM(t, b, bus, sidA, "jane", "hi jane")
	if got := lastPost(t, f); got["channel"] != "DUJANE" || got["text"] != "*flyer*\nhi jane" {
		t.Fatalf("non-owner DM = %+v", got)
	}
	full := sessionHeader(outboundFor(bus, sidA, "flyer", ""))
	sendDM(t, b, bus, sidA, "alex", "hi alex")
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["text"] != full+"\nhi alex" {
		t.Fatalf("owner DM = %+v", got)
	}
	post(t, b, bus, sidA, "in the channel")
	if got := postsTo(f, "CAGENTS"); len(got) != 1 || got[0]["text"] != full+"\nin the channel" {
		t.Fatalf("main channel = %+v", got)
	}
}

// Item 11: outside owner-only places, the help names agents by name only.
func TestHelpInNonOwnerDMNamesAgentsOnly(t *testing.T) {
	b, f, _ := newTestBridge(t)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	b.handleEvent("EvDc4", dmMsg("UJANE", "hello?", "1700009500.000004", ""))
	drainJobs(t, b)
	if got := lastPostText(f); got != "*Agents you can message:*\n• `flyer` · idle\n"+helpHow {
		t.Fatalf("help = %q", got)
	}
}
