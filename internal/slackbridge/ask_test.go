package slackbridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	"github.com/tidwall/gjson"
)

// humanDM is a 1:1 DM between two people, which the bot can't read.
const humanDM = "D0HUMAN"

// shortcutPayload is a message_action payload: user ran "Ask an agent" on
// bob's message in humanDM.
func shortcutPayload(user, text string) string {
	p := map[string]any{
		"type": "message_action", "callback_id": askCallbackID, "trigger_id": "TRIG1",
		"user":    map[string]any{"id": user},
		"channel": map[string]any{"id": humanDM, "name": "directmessage"},
		"message": map[string]any{"type": "message", "user": "UBOB", "text": text, "ts": "1700008000.000001"},
	}
	data, _ := json.Marshal(p)
	return string(data)
}

// submission is a view_submission payload for the modal opened with view
// (views.open's view JSON): option is the chosen agent's value.
func submission(user, view, option, note string) string {
	values := map[string]any{
		askAgentBlock: map[string]any{askAgentBlock: map[string]any{"type": "static_select", "selected_option": map[string]any{"value": option}}},
		askNoteBlock:  map[string]any{askNoteBlock: map[string]any{"type": "plain_text_input", "value": note}},
	}
	p := map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": user},
		"view": map[string]any{"callback_id": gjson.Get(view, "callback_id").String(), "private_metadata": gjson.Get(view, "private_metadata").String(), "state": map[string]any{"values": values}},
	}
	data, _ := json.Marshal(p)
	return string(data)
}

// openedView runs queued jobs and returns the view of the one views.open.
func openedView(t *testing.T, b *Bridge, f *fakeSlack) string {
	t.Helper()
	b.viewsWG.Wait()
	drainJobs(t, b)
	opens := f.callsTo("views.open")
	if len(opens) != 1 || opens[0].Form.Get("trigger_id") != "TRIG1" {
		t.Fatalf("views.open = %+v", opens)
	}
	return opens[0].Form.Get("view")
}

// optionTexts lists the agent choices a view offers.
func optionTexts(view string) []string {
	var out []string
	for _, block := range gjson.Get(view, "blocks").Array() {
		if block.Get("block_id").String() != askAgentBlock {
			continue
		}
		for _, o := range block.Get("element.options").Array() {
			out = append(out, o.Get("text.text").String())
		}
	}
	return out
}

func TestAskShortcutEndToEnd(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	// The envelope comes over the socket, which acks it at once; views.open
	// runs on its own.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.runSocket(ctx)
	}()
	payload := shortcutPayload("UJANE", "can you <@UALEX> check the deploy?\nline two &lt;b&gt;")
	f.push(`{"type":"interactive","envelope_id":"env-s1","payload":` + payload + `}`)
	waitAck(t, f, "env-s1")
	if got := f.ackPayload("env-s1"); got != "" {
		t.Fatalf("ack payload = %s", got)
	}
	cancel()
	<-done

	view := openedView(t, b, f)
	// jane isn't an owner: named agents only, by name.
	if got := optionTexts(view); len(got) != 1 || got[0] != "flyer" {
		t.Fatalf("options = %v", got)
	}
	meta := gjson.Get(view, "private_metadata").String()
	if gjson.Get(meta, "channel").String() != humanDM || gjson.Get(meta, "ts").String() != "1700008000.000001" || gjson.Get(meta, "hash").String() == "" {
		t.Fatalf("private_metadata = %s", meta)
	}
	if strings.Contains(view, "check the deploy") {
		t.Fatalf("the message text went into the view: %s", view)
	}

	if ack := b.handleInteractive(json.RawMessage(submission("UJANE", view, "0", "  what do you think?  "))); ack != nil {
		t.Fatalf("submission ack = %+v", ack)
	}
	drainJobs(t, b)
	m := claimOne(t, bus, sidA)
	wantBody := "what do you think?\n\nQuoted message from bob in a DM:\n> can you @alex check the deploy?\n> line two <b>"
	if !m.FromUser || m.Guest || m.SlackUser != "jane" || m.Via != agentbus.ViaShortcut || m.Body != wantBody {
		t.Fatalf("delivered = %+v", m)
	}
	// The person who asked gets a line in their DM with the bot.
	dm := postsTo(f, "DUJANE")
	if len(dm) != 1 || dm[0]["thread_ts"] != "" ||
		dm[0]["text"] != "Sent to flyer — they'll answer here.\n> can you @alex check the deploy?\n> line two &lt;b&gt;" {
		t.Fatalf("DM posts = %+v", dm)
	}
	// The answer goes to that DM, at the top level, privately.
	sendReply(t, b, bus, sidA, "looks fine", m.ID)
	if got := lastPost(t, f); got["channel"] != "DUJANE" || got["thread_ts"] != "" {
		t.Fatalf("answer = %+v", got)
	}
	// Nothing is ever posted where the message was.
	if p := postsTo(f, humanDM); len(p) != 0 {
		t.Fatalf("posts in the original conversation: %+v", p)
	}
	for _, c := range f.callsTo("chat.postEphemeral") {
		if c.Form.Get("channel") == humanDM {
			t.Fatalf("ephemeral in the original conversation: %+v", c)
		}
	}
	// One submission per shortcut.
	if ack := b.handleInteractive(json.RawMessage(submission("UJANE", view, "0", ""))); ack == nil || ack["response_action"] != "errors" {
		t.Fatalf("second submission ack = %+v", ack)
	}
	drainJobs(t, b)
	if bus.Pending(sidA) {
		t.Fatalf("delivered twice: %+v", bus.Claim(sidA))
	}
}

func TestAskShortcutOwnerSeesFullLabelsAndDefaultNote(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleInteractive(json.RawMessage(shortcutPayload("UALEX", "hi")))
	view := openedView(t, b, f)
	got := optionTexts(view)
	if len(got) != 2 || !strings.Contains(strings.Join(got, "|"), "pc/other-bbbbbb · pc") || !strings.Contains(strings.Join(got, "|"), "flyer · pc") {
		t.Fatalf("owner options = %v", got)
	}
	// Choose the unnamed agent, with no note.
	option := "0"
	if strings.HasPrefix(got[0], "flyer") {
		option = "1"
	}
	b.handleInteractive(json.RawMessage(submission("UALEX", view, option, "")))
	drainJobs(t, b)
	m := claimOne(t, bus, sidB)
	if m.Body != "Please look at this message.\n\nQuoted message from bob in a DM:\n> hi" || m.Via != agentbus.ViaShortcut {
		t.Fatalf("delivered = %+v", m)
	}
	if dm := postsTo(f, "DUALEX"); len(dm) != 1 || !strings.HasPrefix(dm[0]["text"], "Sent to pc/other-bbbbbb — they'll answer here.") {
		t.Fatalf("DM posts = %+v", dm)
	}
}

func TestAskShortcutWhereLabels(t *testing.T) {
	b, _, _ := newTestBridge(t)
	for _, tc := range []struct{ channel, name, want string }{
		{"D0HUMAN", "directmessage", "a DM"},
		{"G0GROUP", "mpdm-alex--bob--carol-1", "a group DM"},
		{"C0OPS", "ops", "#ops"},
		{"C0OPS", "", "a channel"},
	} {
		if got := b.askWhere(tc.channel, tc.name); got != tc.want {
			t.Fatalf("askWhere(%q, %q) = %q, want %q", tc.channel, tc.name, got, tc.want)
		}
	}
}

// Task 9: a non-allowed user's shortcut gets no modal and no reply at all:
// no ephemeral, no DM, nothing posted. Only a Debug log notes it.
func TestAskShortcutRefusesNonAllowed(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if ack := b.handleInteractive(json.RawMessage(shortcutPayload("UBOB", "hi"))); ack != nil {
		t.Fatalf("ack = %+v", ack)
	}
	drainJobs(t, b)
	if n := len(f.callsTo("views.open")); n != 0 {
		t.Fatalf("views.open = %d", n)
	}
	if n := len(f.callsTo("chat.postEphemeral")); n != 0 {
		t.Fatalf("ephemerals = %d", n)
	}
	if n := len(f.callsTo("chat.postMessage")); n != 0 {
		t.Fatalf("posts = %d", n)
	}
	b.dmMu.Lock()
	_, cached := b.dmChannels["UBOB"]
	b.dmMu.Unlock()
	if cached {
		t.Fatal("a refused user's DM was cached")
	}
	// A submission from someone not allowed (say, removed meanwhile) is
	// refused too.
	b.handleInteractive(json.RawMessage(shortcutPayload("UALEX", "hi")))
	view := openedView(t, b, f)
	if ack := b.handleInteractive(json.RawMessage(submission("UBOB", view, "0", ""))); ack == nil || ack["response_action"] != "errors" {
		t.Fatalf("stranger's submission ack = %+v", ack)
	}
	drainJobs(t, b)
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("a stranger's submission was delivered")
	}
}

func TestAskSubmissionMustMatchTheShortcut(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleInteractive(json.RawMessage(shortcutPayload("UALEX", "hi")))
	view := openedView(t, b, f)
	for name, raw := range map[string]string{
		"other hash": strings.Replace(submission("UALEX", view, "0", ""), gjson.Get(gjson.Get(view, "private_metadata").String(), "hash").String(), "0000", 1),
		"bad option": submission("UALEX", view, "7", ""),
		"no option":  submission("UALEX", view, "x", ""),
	} {
		if ack := b.handleInteractive(json.RawMessage(raw)); ack == nil || ack["response_action"] != "errors" {
			t.Fatalf("%s: ack = %+v", name, ack)
		}
	}
	// None of those used the shortcut up.
	if ack := b.handleInteractive(json.RawMessage(submission("UALEX", view, "0", ""))); ack != nil {
		t.Fatalf("valid submission ack = %+v", ack)
	}
	drainJobs(t, b)
	if !bus.Pending(sidA) && !bus.Pending(sidB) {
		t.Fatal("not delivered")
	}
}

// slash runs /clanker text as user and returns the ack's text.
func slash(t *testing.T, b *Bridge, user, text string) string {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"command": "/clanker", "text": text, "user_id": user, "channel_id": humanDM, "trigger_id": "TRIG2"})
	ack := b.handleSlash(json.RawMessage(data))
	s, _ := ack["text"].(string)
	return s
}

func TestClankerRoutesToTheAgent(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	if got := slash(t, b, "UJANE", "flyer: check the logs"); got != "Sent to flyer — answer arrives in your DM with @clanker-bro." {
		t.Fatalf("ack = %q", got)
	}
	drainJobs(t, b)
	m := claimOne(t, bus, sidA)
	if !m.FromUser || m.SlackUser != "jane" || m.Via != agentbus.ViaSlash || m.Body != "check the logs" {
		t.Fatalf("delivered = %+v", m)
	}
	sendReply(t, b, bus, sidA, "logs are clean", m.ID)
	if got := lastPost(t, f); got["channel"] != "DUJANE" || got["thread_ts"] != "" {
		t.Fatalf("answer = %+v", got)
	}
	if p := postsTo(f, humanDM); len(p) != 0 {
		t.Fatalf("posts where the command was typed: %+v", p)
	}

	// "@name message" works too.
	slash(t, b, "UJANE", "@flyer and the metrics")
	drainJobs(t, b)
	if m := claimOne(t, bus, sidA); m.Body != "and the metrics" || m.Via != agentbus.ViaSlash {
		t.Fatalf("delivered = %+v", m)
	}
}

func TestClankerHelpAndRefusal(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"", "  help "} {
		got := slash(t, b, "UJANE", text)
		if !strings.Contains(got, "`flyer`") || strings.Contains(got, "pc/") || strings.Contains(got, "· pc") {
			t.Fatalf("help for jane (%q) = %q", text, got)
		}
		if got = slash(t, b, "UALEX", text); !strings.Contains(got, "`pc/other-bbbbbb` · pc") {
			t.Fatalf("help for alex (%q) = %q", text, got)
		}
	}
	if got := slash(t, b, "UBOB", "flyer: hi"); got != "" {
		t.Fatalf("stranger = %q", got)
	}
	if got := slash(t, b, "UJANE", "nobody: hi"); !strings.HasPrefix(got, "No agent called `nobody`.") || strings.Contains(got, "pc/") {
		t.Fatalf("unknown agent = %q", got)
	}
	if got := slash(t, b, "UJANE", "just words"); !strings.Contains(got, "/clanker name: message") {
		t.Fatalf("untagged = %q", got)
	}
	drainJobs(t, b)
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("delivered")
	}
	if n := len(f.callsTo("chat.postMessage")); n != 0 {
		t.Fatalf("posts = %d", n)
	}
}

func TestClankerCommandByOwner(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	bus.SetModVersion(sidA, agentbus.MinCommandModVersion)
	if got := slash(t, b, "UJANE", "flyer: !compact"); got != ownersOnlyCommands {
		t.Fatalf("non-owner ack = %q", got)
	}
	drainJobs(t, b)
	if bus.Pending(sidA) {
		t.Fatalf("a non-owner's command was delivered: %+v", bus.Claim(sidA))
	}
	if got := slash(t, b, "UALEX", "flyer: !compact now"); got != "Working… the result arrives in your DM with @clanker-bro." {
		t.Fatalf("owner ack = %q", got)
	}
	drainJobs(t, b)
	m := claimOne(t, bus, sidA)
	if m.Command == nil || m.Command.Command != "compact" || m.Command.Args != "now" || m.Via != agentbus.ViaSlash || m.SlackUserID != "UALEX" {
		t.Fatalf("command = %+v", m)
	}
	// Its report goes to the owner's DM.
	sendReply(t, b, bus, sidA, "✅ !compact: done", m.ID)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["thread_ts"] != "" {
		t.Fatalf("report = %+v", got)
	}
	// !commands needs no agent.
	slash(t, b, "UALEX", "!commands")
	drainJobs(t, b)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || !strings.Contains(got["text"], "registry commands") {
		t.Fatalf("!commands = %+v", got)
	}
}

func TestSlashEnvelopeAckCarriesTheAnswer(t *testing.T) {
	b, f, _ := newTestBridge(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.runSocket(ctx)
	}()
	f.push(`{"type":"slash_commands","envelope_id":"env-c1","payload":{"command":"/clanker","text":"flyer: hi","user_id":"UALEX","channel_id":"D0HUMAN"}}`)
	waitAck(t, f, "env-c1")
	cancel()
	<-done
	if got := gjson.Get(f.ackPayload("env-c1"), "text").String(); got != "Sent to flyer — answer arrives in your DM with @clanker-bro." {
		t.Fatalf("ack payload = %s", f.ackPayload("env-c1"))
	}
}

// Task 9: a non-allowed /clanker is silent too: an empty ack (Slack shows
// nothing) and nothing posted.
func TestClankerRefusesNonAllowedSilently(t *testing.T) {
	b, f, _ := newTestBridge(t)
	if got := slash(t, b, "UBOB", "flyer: hi"); got != "" {
		t.Fatalf("ack = %q", got)
	}
	drainJobs(t, b)
	if n := len(f.callsTo("chat.postEphemeral")) + len(f.callsTo("chat.postMessage")) + len(f.callsTo("views.open")); n != 0 {
		t.Fatalf("Slack calls = %d", n)
	}
}

func TestViewsOpenFailureTellsTheUser(t *testing.T) {
	b, f, _ := newTestBridge(t)
	f.setFail("views.open", "expired_trigger_id")
	b.handleInteractive(json.RawMessage(shortcutPayload("UALEX", "hi")))
	b.viewsWG.Wait()
	drainJobs(t, b)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["text"] != dialogFailed {
		t.Fatalf("post = %+v", got)
	}
}

func TestClankerIgnoresOtherCommands(t *testing.T) {
	b, _, bus := newTestBridge(t)
	data, _ := json.Marshal(map[string]any{"command": "/other", "text": "flyer: hi", "user_id": "UALEX"})
	if ack := b.handleSlash(json.RawMessage(data)); ack != nil {
		t.Fatalf("ack = %+v", ack)
	}
	drainJobs(t, b)
	if bus.Pending(sidA) {
		t.Fatal("delivered")
	}
}

func TestClankerMoveAcksWorking(t *testing.T) {
	b, _, _ := newTestBridge(t)
	if got := slash(t, b, "UALEX", "flyer: !dm"); !strings.HasPrefix(got, "Working…") {
		t.Fatalf("ack = %q", got)
	}
}
