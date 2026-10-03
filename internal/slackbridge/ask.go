package slackbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
)

// "Ask an agent": reaching an agent from anywhere in Slack, including 1:1
// DMs between people, which the bot can't read.
//   - The message shortcut (callback askCallbackID) opens a modal to pick an
//     agent and add a note; on submit the message goes to that agent, quoted.
//   - The /clanker slash command sends "name: message" to an agent.
//
// Only allowed users may use either. The agent's answer goes to the user's
// DM with the bot, at the top level; nothing is ever posted where the
// message was. Both come over Socket Mode and are acked at once: the
// handlers only touch memory and queue jobs for the Slack calls.

const (
	askCallbackID     = "ask_agent"
	askViewCallbackID = "ask_agent_submit"
	askAgentBlock     = "agent"
	askNoteBlock      = "note"
	// maxAskText caps the message text the shortcut passes on.
	maxAskText = 4000
	// maxAskNote caps the note the modal takes.
	maxAskNote = 1000
	// askTTL is how long an open modal's message is kept for its submit.
	askTTL = time.Hour
	// maxAsks caps the open modals remembered; the oldest go first.
	maxAsks = 200
	// maxAskOptions caps the agents the modal offers (Slack's static_select
	// limit).
	maxAskOptions = 100
	// maxOptionText caps an option's text (Slack's limit).
	maxOptionText = 75

	notAllowedHere = "You're not allowed to use this."
	askDefaultNote = "Please look at this message."
	askExpired     = "This request expired. Run Ask an agent on the message again."
	askChoose      = "Choose an agent from the list."
	noAgentsOnline = "No agents are online right now."
	clankerUsage   = "Write `/clanker name: message` (or `/clanker @name message`) to message an agent."
)

// askKey names an open modal: who opened it on which message.
type askKey struct{ user, channel, ts string }

// askEntry is the message an open modal is about, and the agents it offers
// (option i is sids[i]).
type askEntry struct {
	text        string
	hash        string
	author      string
	channelName string
	sids        []string
	at          time.Time
}

// askMeta is the modal's private_metadata: the message, and a hash of its
// text, so a submit matches the shortcut it came from.
type askMeta struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	Hash    string `json:"hash"`
}

// interactivePayload is the part of an interactive payload the bridge
// reads: a message shortcut (message_action) or a modal submit
// (view_submission).
type interactivePayload struct {
	Type       string `json:"type"`
	CallbackID string `json:"callback_id"`
	TriggerID  string `json:"trigger_id"`
	User       struct {
		ID string `json:"id"`
	} `json:"user"`
	Channel struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"channel"`
	Message struct {
		User  string `json:"user"`
		BotID string `json:"bot_id"`
		Text  string `json:"text"`
		TS    string `json:"ts"`
	} `json:"message"`
	View struct {
		CallbackID      string `json:"callback_id"`
		PrivateMetadata string `json:"private_metadata"`
		State           struct {
			Values map[string]map[string]viewValue `json:"values"`
		} `json:"state"`
	} `json:"view"`
}

// viewValue is one input's value in a submitted modal.
type viewValue struct {
	Value          string `json:"value"`
	SelectedOption *struct {
		Value string `json:"value"`
	} `json:"selected_option"`
}

// slashPayload is the part of a slash command payload the bridge reads.
type slashPayload struct {
	Command string `json:"command"`
	Text    string `json:"text"`
	UserID  string `json:"user_id"`
}

// agentChoice is an agent the modal offers.
type agentChoice struct{ sid, label string }

// handleInteractive handles an interactive envelope's payload and returns
// what its ack carries (nil for nothing).
func (b *Bridge) handleInteractive(raw json.RawMessage) map[string]any {
	var p interactivePayload
	if errJSON := json.Unmarshal(raw, &p); errJSON != nil {
		log.Debugf("slack: bad interactive payload: %v", errJSON)
		return nil
	}
	switch {
	case p.Type == "message_action" && p.CallbackID == askCallbackID:
		b.askShortcut(p)
	case p.Type == "view_submission" && p.View.CallbackID == askViewCallbackID:
		return b.askSubmit(p)
	}
	return nil
}

// askShortcut handles "Ask an agent" on a message: for an allowed user it
// remembers the message and opens the modal; anyone else is told, privately,
// that they can't use it.
func (b *Bridge) askShortcut(p interactivePayload) {
	user, ok := b.state.user(p.User.ID)
	if !ok {
		log.Infof("slack: refused Ask an agent from %s: not an allowed user", p.User.ID)
		b.ephemeralInDM(p.User.ID, notAllowedHere)
		return
	}
	choices := b.agentChoices(user.config)
	if len(choices) == 0 {
		b.ephemeralInDM(user.ID, noAgentsOnline)
		return
	}
	text := firstRunes(p.Message.Text, maxAskText)
	sum := sha256.Sum256([]byte(text))
	entry := askEntry{text: text, hash: hex.EncodeToString(sum[:8]), author: p.Message.User, channelName: p.Channel.Name, at: b.state.now()}
	if p.Message.BotID != "" {
		entry.author = ""
	}
	labels := make([]string, 0, len(choices))
	for _, c := range choices {
		entry.sids = append(entry.sids, c.sid)
		labels = append(labels, c.label)
	}
	b.rememberAsk(askKey{user.ID, p.Channel.ID, p.Message.TS}, entry)
	meta, errMeta := json.Marshal(askMeta{Channel: p.Channel.ID, TS: p.Message.TS, Hash: entry.hash})
	view, errView := askView(labels, string(meta))
	if errMeta != nil || errView != nil {
		log.Warnf("slack: Ask an agent: can't build the modal")
		return
	}
	trigger := p.TriggerID
	b.enqueueCommand(func(ctx context.Context) error {
		return b.api.openView(ctx, b.cfg.BotToken, trigger, view)
	})
}

// askSubmit handles the modal's submit. A valid one is delivered (in a
// command job) and closes the modal; otherwise the ack shows the error in
// the modal.
func (b *Bridge) askSubmit(p interactivePayload) map[string]any {
	user, ok := b.state.user(p.User.ID)
	if !ok {
		log.Infof("slack: refused an Ask an agent submit from %s: not an allowed user", p.User.ID)
		return askError(notAllowedHere)
	}
	var meta askMeta
	if errJSON := json.Unmarshal([]byte(p.View.PrivateMetadata), &meta); errJSON != nil {
		return askError(askExpired)
	}
	choice := -1
	if sel := p.View.State.Values[askAgentBlock][askAgentBlock].SelectedOption; sel != nil {
		if n, errAtoi := strconv.Atoi(sel.Value); errAtoi == nil {
			choice = n
		}
	}
	entry, errText := b.takeAsk(askKey{user.ID, meta.Channel, meta.TS}, meta.Hash, choice)
	if errText != "" {
		return askError(errText)
	}
	note := strings.TrimSpace(p.View.State.Values[askNoteBlock][askNoteBlock].Value)
	b.enqueueCommand(b.askJob(user, entry.sids[choice], note, meta.Channel, entry))
	return nil
}

// askError is the ack that shows text under the agent picker.
func askError(text string) map[string]any {
	return map[string]any{"response_action": "errors", "errors": map[string]string{askAgentBlock: text}}
}

// askJob delivers an asked-about message to sid as user's instruction,
// with the answer going to user's DM with the bot, then posts there what
// was sent. Delivery happens once; a retry only re-posts the line.
func (b *Bridge) askJob(user allowedUser, sid, note, channel string, entry askEntry) job {
	delivered := false
	var dm, confirm string
	return func(ctx context.Context) error {
		if !delivered {
			ch, err := b.dmChannel(ctx, user.ID)
			if err != nil {
				return err
			}
			delivered, dm = true, ch
			if note == "" {
				note = askDefaultNote
			}
			plain := plainText(entry.text, b.state.idLabels())
			body := fmt.Sprintf("%s\n\nQuoted message from %s in %s:\n%s", note, b.askAuthor(ctx, entry.author), b.askWhere(channel, entry.channelName), quoteLines(plain))
			ev := messageEvent{Type: "message", Channel: dm, ChannelType: "im", User: user.ID, via: agentbus.ViaShortcut}
			nsid, ok := b.deliver(ev, sid, body, user, sessionEnded, false)
			if !ok {
				return nil
			}
			log.Infof("slack: %s asked %s about a message (Ask an agent)", user.ID, b.bus.Address(nsid))
			confirm = "Sent to " + escape(b.agentLabel(ev, nsid)) + " — they'll answer here.\n" + quoteLines(escape(plain))
		}
		if confirm == "" {
			return nil
		}
		_, err := b.api.postMessage(ctx, b.cfg.BotToken, dm, confirm, "")
		return err
	}
}

// askAuthor names a message's author for the agent: an allowed user's
// label, a guest's label, or "someone" (a bot, or a failed lookup).
func (b *Bridge) askAuthor(ctx context.Context, userID string) string {
	if userID == "" {
		return "someone"
	}
	if u, ok := b.state.user(userID); ok {
		return u.Label
	}
	name := b.guestName(ctx, userID)
	if strings.TrimSpace(name) == "" {
		return "someone"
	}
	return b.guestLabel(name, userID)
}

// askWhere names the conversation a message was in, from its id and name.
func (b *Bridge) askWhere(channel, name string) string {
	switch {
	case strings.HasPrefix(channel, "D"):
		return "a DM"
	case strings.HasPrefix(name, "mpdm-"):
		return "a group DM"
	case name != "":
		return "#" + name
	}
	return "a channel"
}

// quoteLines prefixes every line of text with "> ".
func quoteLines(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}

// agentChoices lists the online agents the modal offers, as help does: for
// an owner (full) by name or address with the machine, else named agents
// by name only.
func (b *Bridge) agentChoices(full bool) []agentChoice {
	var out []agentChoice
	for _, p := range b.bus.Peers() {
		if p.Address == agentbus.SlackAddress || p.Status == agentbus.StatusOffline {
			continue
		}
		label := p.Name
		switch {
		case full:
			if label == "" {
				label = p.Address
			}
			label += " · " + p.Machine
		case label == "":
			continue
		}
		sid, ok := b.bus.Resolve(p.Address)
		if !ok {
			continue
		}
		out = append(out, agentChoice{sid: sid, label: firstRunes(label, maxOptionText)})
		if len(out) == maxAskOptions {
			break
		}
	}
	return out
}

// askView is the modal: a required agent picker (option i is labels[i]) and
// an optional note.
func askView(labels []string, meta string) (string, error) {
	plain := func(text string) map[string]any { return map[string]any{"type": "plain_text", "text": text} }
	options := make([]map[string]any, 0, len(labels))
	for i, label := range labels {
		options = append(options, map[string]any{"text": plain(label), "value": strconv.Itoa(i)})
	}
	view := map[string]any{
		"type":             "modal",
		"callback_id":      askViewCallbackID,
		"private_metadata": meta,
		"title":            plain("Ask an agent"),
		"submit":           plain("Send"),
		"close":            plain("Cancel"),
		"blocks": []map[string]any{
			{"type": "input", "block_id": askAgentBlock, "label": plain("Agent"),
				"element": map[string]any{"type": "static_select", "action_id": askAgentBlock, "placeholder": plain("Choose an agent"), "options": options}},
			{"type": "input", "block_id": askNoteBlock, "optional": true, "label": plain("Note"),
				"element": map[string]any{"type": "plain_text_input", "action_id": askNoteBlock, "multiline": true, "max_length": maxAskNote, "placeholder": plain(askDefaultNote)}},
		},
	}
	data, err := json.Marshal(view)
	return string(data), err
}

// rememberAsk keeps an open modal's message, dropping expired ones and the
// oldest past maxAsks.
func (b *Bridge) rememberAsk(key askKey, entry askEntry) {
	b.askMu.Lock()
	defer b.askMu.Unlock()
	if b.asks == nil {
		b.asks = map[askKey]askEntry{}
	}
	cutoff := entry.at.Add(-askTTL)
	for k, e := range b.asks {
		if !e.at.After(cutoff) {
			delete(b.asks, k)
		}
	}
	b.asks[key] = entry
	for len(b.asks) > maxAsks {
		var oldest askKey
		first := true
		for k, e := range b.asks {
			if first || e.at.Before(b.asks[oldest].at) {
				oldest, first = k, false
			}
		}
		delete(b.asks, oldest)
	}
}

// takeAsk returns and forgets the open modal key when hash matches its
// message and choice is one of its agents; else it keeps it and returns
// the error to show.
func (b *Bridge) takeAsk(key askKey, hash string, choice int) (askEntry, string) {
	b.askMu.Lock()
	defer b.askMu.Unlock()
	e, ok := b.asks[key]
	if !ok || e.hash != hash || !e.at.After(b.state.now().Add(-askTTL)) {
		return askEntry{}, askExpired
	}
	if choice < 0 || choice >= len(e.sids) {
		return askEntry{}, askChoose
	}
	delete(b.asks, key)
	return e, ""
}

// ephemeralInDM tells userID text privately, in their DM with the bot.
func (b *Bridge) ephemeralInDM(userID, text string) {
	b.enqueueCommand(func(ctx context.Context) error {
		channel, err := b.dmChannel(ctx, userID)
		if err != nil {
			return err
		}
		return b.api.postEphemeral(ctx, b.cfg.BotToken, channel, userID, text)
	})
}

// handleSlash handles a slash command envelope's payload and returns its
// ack, which carries the user's (ephemeral) answer.
func (b *Bridge) handleSlash(raw json.RawMessage) map[string]any {
	var p slashPayload
	if errJSON := json.Unmarshal(raw, &p); errJSON != nil {
		log.Debugf("slack: bad slash command payload: %v", errJSON)
		return nil
	}
	return map[string]any{"text": b.clanker(p)}
}

// clanker runs /clanker for an allowed user: "name: message" (or "@name
// message") goes to that agent, as from them, with the answer in their DM
// with the bot; "!commands" follow the usual owner rule; empty text or
// "help" lists the agents they may see. It returns the ephemeral answer.
func (b *Bridge) clanker(p slashPayload) string {
	user, ok := b.state.user(p.UserID)
	if !ok {
		log.Infof("slack: refused /clanker from %s: not an allowed user", p.UserID)
		return notAllowedHere
	}
	// Its answers go to the user's DM, so it is shown as there.
	ev := messageEvent{Type: "message", ChannelType: "im", User: user.ID, via: agentbus.ViaSlash}
	raw := strings.TrimSpace(p.Text)
	if raw == "" || strings.EqualFold(raw, "help") {
		return b.help(ev)
	}
	inDM := fmt.Sprintf("The answer arrives in your DM with %s.", b.botMention())
	text := plainText(raw, b.state.idLabels())
	if isBang(text) {
		// Only !commands needs no agent; runCommand explains the rest.
		if !user.config {
			log.Infof("slack: refused a /clanker command from non-owner %s", user.ID)
			return ownersOnlyCommands
		}
		b.enqueueCommand(b.slashJob(user, "", text))
		return inDM
	}
	target, body, tagged := parseAddressed(text)
	if !tagged {
		target, body, tagged = parseAtTagged(raw, text)
	}
	if !tagged {
		return clankerUsage + "\n" + b.help(ev)
	}
	sid, found := b.bus.Resolve(target)
	if !found {
		return b.notFoundReply(ev, target)
	}
	if isBang(body) && !user.config {
		log.Infof("slack: refused a /clanker command from non-owner %s", user.ID)
		return ownersOnlyCommands
	}
	b.enqueueCommand(b.slashJob(user, sid, body))
	label := escape(b.agentLabel(ev, sid))
	if b.bus.SessionStatus(sid) == agentbus.StatusOffline {
		return fmt.Sprintf("%s is offline; it gets this when it's back. %s", label, inDM)
	}
	return fmt.Sprintf("Sent to %s — answer arrives in your DM with %s.", label, b.botMention())
}

// slashJob carries out /clanker text for user to sid (empty for a command
// that names no agent) from the user's DM with the bot, where any reply
// goes. Opening that DM is the only Slack call before delivery, so a retry
// never delivers twice.
func (b *Bridge) slashJob(user allowedUser, sid, text string) job {
	return func(ctx context.Context) error {
		channel, err := b.dmChannel(ctx, user.ID)
		if err != nil {
			return err
		}
		ev := messageEvent{Type: "message", Channel: channel, ChannelType: "im", User: user.ID, via: agentbus.ViaSlash}
		if sid == "" {
			b.runCommand(ev, user, "", text, sessionEnded, false)
			return nil
		}
		b.send(ev, user, sid, text, false)
		return nil
	}
}
