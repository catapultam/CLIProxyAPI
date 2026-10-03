package slackbridge

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

// A top-level DM that names no agent ("armor-console: confirmed", where
// armor-console is how the agent introduced itself) goes, whole, to the
// agent the user last talked to there.
func TestDMUnresolvedNameFallsBackToDMLast(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvF0", dmMsg("UALEX", "flyer: who are you?", "1700012000.000001", ""))
	_ = deliveredID(t, bus, sidA)

	b.handleEvent("EvF1", dmMsg("UALEX", "armor-console: confirmed", "1700012000.000002", ""))
	m := claimOne(t, bus, sidA)
	if m.Body != "armor-console: confirmed" || !m.FromUser || m.Via != agentbus.ViaDM {
		t.Fatalf("delivered = %+v", m)
	}
	if bus.Pending(sidB) {
		t.Fatal("delivered elsewhere")
	}
	drainJobs(t, b)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["text"] != "→ sent to `flyer`" {
		t.Fatalf("reply = %+v", got)
	}
	// The answer goes to the DM's top level.
	sendReply(t, b, bus, sidA, "ok", m.ID)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["thread_ts"] != "" {
		t.Fatalf("answer = %+v", got)
	}

	// "@name …" too.
	b.handleEvent("EvF2", dmMsg("UALEX", "@armor-console go ahead", "1700012000.000003", ""))
	if m := claimOne(t, bus, sidA); m.Body != "@armor-console go ahead" {
		t.Fatalf("delivered = %+v", m)
	}
}

func TestDMUnresolvedNameWithoutDMLastGetsHelp(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvF3", dmMsg("UALEX", "armor-console: confirmed", "1700012100.000001", ""))
	drainJobs(t, b)
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("delivered without a dm_last")
	}
	if got := lastPost(t, f); got["channel"] != "DUALEX" || !strings.HasPrefix(got["text"], "No agent called `armor-console`.") || !strings.HasSuffix(got["text"], helpHow) {
		t.Fatalf("reply = %+v", got)
	}
}

func TestDMUnresolvedNameNeverFallsBackForCommands(t *testing.T) {
	b, f, bus := newTestBridge(t)
	bus.SetModVersion(sidA, agentbus.MinCommandModVersion)
	b.handleEvent("EvF4", dmMsg("UALEX", "flyer: hi", "1700012200.000001", ""))
	_ = deliveredID(t, bus, sidA)
	for i, text := range []string{"armor-console: !compact", "@armor-console !compact"} {
		b.handleEvent("EvF5"+string(rune('a'+i)), dmMsg("UALEX", text, "1700012200.00001"+string(rune('0'+i)), ""))
		drainJobs(t, b)
		if bus.Pending(sidA) || bus.Pending(sidB) {
			t.Fatalf("%q was delivered: %+v", text, bus.Claim(sidA))
		}
		if got := lastPost(t, f); !strings.HasPrefix(got["text"], "No agent called `armor-console`.") {
			t.Fatalf("%q: reply = %+v", text, got)
		}
	}
}

func TestDMThreadUnresolvedNameGoesToTheThreadsAgent(t *testing.T) {
	b, f, bus := newTestBridge(t)
	sendDM(t, b, bus, sidB, "alex", "need a decision")
	rootTS := lastPostTS(f)
	b.handleEvent("EvF6", dmMsg("UALEX", "armor-console: option B", "1700012300.000001", rootTS))
	if m := claimOne(t, bus, sidB); m.Body != "armor-console: option B" {
		t.Fatalf("delivered = %+v", m)
	}
}

func TestMainChannelUnresolvedNameStillGetsHelp(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvF7", dmMsg("UALEX", "flyer: hi", "1700012400.000001", ""))
	_ = deliveredID(t, bus, sidA)
	b.handleEvent("EvF8", msg("UALEX", "armor-console: confirmed", "1700012400.000002", ""))
	drainJobs(t, b)
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("the main channel fell back")
	}
	if got := lastPost(t, f); got["channel"] != "CAGENTS" || !strings.HasPrefix(got["text"], "No agent called `armor-console`.") {
		t.Fatalf("reply = %+v", got)
	}
}
