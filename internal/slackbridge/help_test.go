package slackbridge

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

// wantHelp is the one help reply for the test bridge's two sessions (both
// on machine pc; sidB, unnamed, by its address).
const wantHelp = "*Agents you can message:*\n" +
	"• `pc/other-bbbbbb` · pc · idle\n" +
	"• `flyer` · pc · idle\n" +
	"Reply in an agent's thread, or start with `name: …` / `@name …`."

// Item 7: every message the bridge can't route gets the same reply.
func TestUnroutableMessagesGetTheSameHelp(t *testing.T) {
	clock := newTestClock()
	b, f, bus := newClockBridge(t, clock)
	clock.advance(time.Second)
	bus.Hello(sidB, "pc", "/work/other", "", true)
	for i, ev := range []messageEvent{
		msg("UALEX", "hello?", "1700009100.000001", ""),                      // channel top level, no name:
		msg("UALEX", "hm", "1700009100.000003", "1700009100.000002"),         // unlinked channel thread
		dmMsg("UALEX", "hello?", "1700009100.000004", ""),                    // DM, no dm_last
		dmMsg("UALEX", "hm", "1700009100.000006", "1700009100.000005"),       // unlinked DM thread
		dmMsg("UALEX", "!compact", "1700009100.000007", ""),                  // bare top-level !cmd in a DM
		msg("UALEX", "!compact", "1700009100.000008", ""),                    // bare top-level !cmd in the channel
		msg("UALEX", "still hm", "1700009100.000009", "1700009100.000002"),   // same unlinked thread: once only
		dmMsg("UALEX", "still hm", "1700009100.000010", "1700009100.000005"), // same unlinked DM thread: once only
		foreignMsg("UALEX", "hello?", "1700009100.000011", ""),               // conversation the bot was added to: silent
		foreignMsg("UALEX", "hm", "1700009100.000013", "1700009100.000012"),  // and its threads
	} {
		b.handleEvent(fmt.Sprintf("EvHelp%d", i), ev)
	}
	drainJobs(t, b)
	posts := f.callsTo("chat.postMessage")
	if len(posts) != 6 {
		t.Fatalf("posts = %d, want 6", len(posts))
	}
	for _, p := range posts {
		if got := p.Form.Get("text"); got != wantHelp {
			t.Fatalf("help in %s = %q, want %q", p.Form.Get("channel"), got, wantHelp)
		}
	}
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("delivered")
	}
}

func TestHelpWithNoAgentsOnline(t *testing.T) {
	clock := newTestClock()
	b, f, _ := newClockBridge(t, clock)
	clock.advance(time.Hour)
	b.handleEvent("EvNone", msg("UALEX", "hello?", "1700009200.000001", ""))
	drainJobs(t, b)
	if got := lastPostText(f); got != "No agents are online right now.\nReply in an agent's thread, or start with `name: …` / `@name …`." {
		t.Fatalf("help = %q", got)
	}
}

func TestHelpListsAtMostFifteenMostRecentFirst(t *testing.T) {
	clock := newTestClock()
	b, f, bus := newClockBridge(t, clock)
	for i := 0; i < 20; i++ {
		clock.advance(time.Second)
		bus.Hello(fmt.Sprintf("%02d0000-2222-3333-4444-555555555555", i), "pc", "/w", fmt.Sprintf("agent%02d", i), true)
	}
	b.handleEvent("EvMany", msg("UALEX", "hello?", "1700009300.000001", ""))
	drainJobs(t, b)
	lines := strings.Split(lastPostText(f), "\n")
	if len(lines) != 17 || lines[1] != "• `agent19` · pc · idle" || lines[15] != "• `agent05` · pc · idle" {
		t.Fatalf("help = %q", lines)
	}
}

// Item 8: bot-authored texts use the bot's live display name.
func TestBotNameComesFromSlackAndIsRefreshed(t *testing.T) {
	b, f, _ := newTestBridge(t)
	if got := b.BotName(); got != "clanker-bro" {
		t.Fatalf("BotName = %q", got)
	}
	b.handleEvent("EvBn1", msg("UALEX", "<@UBOT> what now", "1700009400.000001", ""))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "`@clanker-bro allow @person`") || strings.Contains(got, "@agents") {
		t.Fatalf("command help = %q", got)
	}
	f.setUser("UBOT", func(u *fakeUser) { u.Display = ""; u.Real = "Clanker Bro II" })
	b.refreshBotName(context.Background())
	if got := b.BotName(); got != "Clanker Bro II" {
		t.Fatalf("refreshed BotName = %q", got)
	}
	b.handleEvent("EvBn2", msg("UALEX", "<@UBOT> chat <@UBOT> with flyer", "1700009400.000002", ""))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "`@Clanker Bro II chat @person with <agent>`") {
		t.Fatalf("chat refusal = %q", got)
	}
	// A failed lookup keeps the last known name.
	f.setFail("users.info", "ratelimited")
	b.refreshBotName(context.Background())
	if got := b.BotName(); got != "Clanker Bro II" {
		t.Fatalf("BotName after a failed refresh = %q", got)
	}
}

var _ agentbus.BotNamer = (*Bridge)(nil)
