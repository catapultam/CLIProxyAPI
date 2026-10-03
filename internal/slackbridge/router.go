package slackbridge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
)

const (
	howToAddress = "To reach an agent, reply in its thread, or post `name: message` at the top level (the name or address from its thread header)."
	commandHelp  = "Commands (for people set in config.yaml): `@agents allow @person` lets someone instruct agents; `@agents remove @person` takes that back."
	ownersOnly   = "Only people set in config.yaml (allowed-emails) can allow or remove users."
	notSavedNote = " (not saved; this reverts when the proxy restarts)"
	onlineLimit  = 10
)

type messageEvent struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	Channel  string `json:"channel"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
}

// relayedSubtypes are the message subtypes that are a person writing:
// a plain message, a thread reply also sent to the channel, a message with a file.
var relayedSubtypes = map[string]bool{"": true, "thread_broadcast": true, "file_share": true}

// handleEvent routes one Slack message. It only touches memory, the state
// file and the job queues, never the network, so the socket loop can ack as
// soon as it returns.
func (b *Bridge) handleEvent(eventID string, ev messageEvent) {
	if ev.Type != "message" || !relayedSubtypes[ev.Subtype] || ev.BotID != "" ||
		ev.Channel != b.channelID || ev.User == "" || ev.User == b.botUserID {
		return
	}
	user, ok := b.state.user(ev.User)
	if !ok {
		return
	}
	// Dedup after the allowlist so strangers can't flush the ring. Without an
	// event id, fall back to the message identity.
	key := eventID
	if key == "" {
		key = ev.Channel + ":" + ev.TS
	}
	if b.alreadySeen(key) {
		return
	}
	if verb, target, isCommand := parseCommand(ev.Text, b.botUserID); isCommand {
		b.command(ev, user, verb, target)
		return
	}
	text := plainText(ev.Text, b.state.idLabels())
	if ev.ThreadTS != "" && ev.ThreadTS != ev.TS {
		sid, linked := b.state.session(ev.ThreadTS)
		if !linked {
			// Answer an unlinked thread once, not on every message in it.
			if !b.alreadySeen("unlinked:" + ev.ThreadTS) {
				b.reply(ev, "This thread isn't linked to an agent. "+howToAddress)
			}
			return
		}
		b.deliver(ev, sid, text, user, "That agent's session has ended, so this wasn't delivered.")
		return
	}
	target, body, addressedOK := parseAddressed(text)
	if !addressedOK {
		b.reply(ev, howToAddress)
		return
	}
	notFound := fmt.Sprintf("No agent called `%s`. %s", escape(target), b.onlineHint())
	if sid, delivered := b.deliver(ev, target, body, user, notFound); delivered {
		b.state.setThread(sid, ev.TS)
	}
}

func (b *Bridge) alreadySeen(eventID string) bool {
	if eventID == "" {
		return false
	}
	b.seenMu.Lock()
	defer b.seenMu.Unlock()
	if b.seen[eventID] {
		return true
	}
	b.seen[eventID] = true
	b.seenRing = append(b.seenRing, eventID)
	if len(b.seenRing) > seenEvents {
		delete(b.seen, b.seenRing[0])
		b.seenRing = b.seenRing[1:]
	}
	return false
}

// deliver queues body for target; notFound is the reply when the target is
// unknown. A delivered message's thread is recorded, so the session can answer
// there.
func (b *Bridge) deliver(ev messageEvent, target, body string, user allowedUser, notFound string) (string, bool) {
	sid, msgID, err := b.bus.Deliver(target, body, user.Label)
	switch {
	case err == nil:
		b.state.recordReply(msgID, ev.Channel, replyThread(ev), sid)
		b.react(ev, "inbox_tray")
		return sid, true
	case errors.Is(err, agentbus.ErrUnknownTarget):
		b.reply(ev, notFound)
	case errors.Is(err, agentbus.ErrBodyTooLarge):
		b.reply(ev, "That message is over the 16 KiB agentbus limit and was not delivered.")
	default:
		b.reply(ev, "Not delivered: "+escape(err.Error()))
	}
	return "", false
}

func (b *Bridge) onlineHint() string {
	var names []string
	for _, p := range b.bus.Peers() {
		if p.Address == agentbus.SlackAddress || p.Status == agentbus.StatusOffline {
			continue
		}
		label := p.Address
		if p.Name != "" {
			label = p.Name + " (" + p.Address + ")"
		}
		names = append(names, "`"+escape(label)+"`")
		if len(names) == onlineLimit {
			break
		}
	}
	if len(names) == 0 {
		return "No agents are online."
	}
	return "Online: " + strings.Join(names, ", ")
}

// command runs allow/remove. Only users seeded from config (owners) may run
// them, so access granted from Slack can't chain. The check happens before
// any lookup or enqueue, so a non-owner's attempt never calls users.info.
//
// remove applies at once, so revoking access never waits behind queued
// posts. allow needs a users.info lookup, so it is a queued command job; it
// records the target's command count now and applies only if no later
// allow/remove for the same user came in meanwhile.
func (b *Bridge) command(ev messageEvent, user allowedUser, verb, target string) {
	if (verb == "allow" || verb == "remove") && !user.config {
		log.Infof("slack: refused %s of %s by non-owner %s", verb, target, user.ID)
		b.replyCommand(ev, ownersOnly)
		return
	}
	switch verb {
	case "allow":
		b.cmdMu.Lock()
		b.cmdSeq[target]++
		seq := b.cmdSeq[target]
		b.cmdMu.Unlock()
		// The outcome is decided once; a retry after a failed reply only
		// re-sends the reply.
		var confirm string
		b.enqueueCommand(func(ctx context.Context) error {
			if confirm == "" {
				confirm = b.applyAllow(ctx, user, target, seq)
			}
			return b.replyNow(ctx, ev, confirm)
		})
	case "remove":
		b.cmdMu.Lock()
		b.cmdSeq[target]++
		confirm := b.applyRemove(user, target)
		b.cmdMu.Unlock()
		b.replyCommand(ev, confirm)
	default:
		b.replyCommand(ev, commandHelp)
	}
}

// applyAllow looks target up and allows them unless a later command for
// target superseded this one (seq is no longer current). It returns the
// reply text.
func (b *Bridge) applyAllow(ctx context.Context, user allowedUser, target string, seq uint64) string {
	label, isBot, err := b.api.userInfo(ctx, b.cfg.BotToken, target)
	if err != nil {
		log.Infof("slack: %s tried to allow %s: lookup failed", user.ID, target)
		return "Couldn't look that user up: " + escape(err.Error())
	}
	if isBot {
		log.Infof("slack: %s tried to allow %s: refused, it is a bot", user.ID, target)
		return "Bots can't be allowed."
	}
	b.cmdMu.Lock()
	if b.cmdSeq[target] != seq {
		b.cmdMu.Unlock()
		log.Infof("slack: %s's allow of %s superseded by a later command", user.ID, target)
		return fmt.Sprintf("A later command for <@%s> superseded this allow, so it wasn't applied.", target)
	}
	u, added, errAllow := b.state.allow(target, label)
	b.cmdMu.Unlock()
	if !added {
		log.Infof("slack: %s tried to allow %s: already allowed", user.ID, target)
		return fmt.Sprintf("<@%s> is already allowed, as @%s.", u.ID, u.Label)
	}
	confirm := fmt.Sprintf("<@%s> can now instruct agents. Agents know them as @%s.", u.ID, u.Label)
	if errAllow != nil {
		logSaveError(errAllow)
		confirm += notSavedNote
	}
	log.Infof("slack: %s allowed %s as @%s", user.ID, u.ID, u.Label)
	return confirm
}

// applyRemove removes target and returns the reply text.
func (b *Bridge) applyRemove(user allowedUser, target string) string {
	u, _ := b.state.user(target)
	switch err := b.state.remove(target); {
	case errors.Is(err, errConfigUser):
		log.Infof("slack: %s tried to remove %s: set in config.yaml", user.ID, target)
		return fmt.Sprintf("@%s is set in config.yaml (allowed-emails) and can't be removed from Slack.", u.Label)
	case errors.Is(err, errNotAllowed):
		log.Infof("slack: %s tried to remove %s: not on the list", user.ID, target)
		return fmt.Sprintf("<@%s> isn't on the list.", target)
	default:
		confirm := fmt.Sprintf("<@%s> can no longer instruct agents.", target)
		if err != nil {
			logSaveError(err)
			confirm += notSavedNote
		}
		log.Infof("slack: %s removed %s", user.ID, target)
		return confirm
	}
}

// replyThread is the thread a reply to ev belongs in.
func replyThread(ev messageEvent) string {
	if ev.ThreadTS != "" {
		return ev.ThreadTS
	}
	return ev.TS
}

func (b *Bridge) reply(ev messageEvent, text string) {
	b.enqueue(func(ctx context.Context) error { return b.replyNow(ctx, ev, text) })
}

// replyCommand queues a reply to a command on the command queue.
func (b *Bridge) replyCommand(ev messageEvent, text string) {
	b.enqueueCommand(func(ctx context.Context) error { return b.replyNow(ctx, ev, text) })
}

func (b *Bridge) replyNow(ctx context.Context, ev messageEvent, text string) error {
	_, err := b.api.postMessage(ctx, b.cfg.BotToken, b.channelID, text, replyThread(ev))
	return err
}

func (b *Bridge) react(ev messageEvent, name string) {
	b.enqueue(func(ctx context.Context) error {
		return b.api.addReaction(ctx, b.cfg.BotToken, b.channelID, ev.TS, name)
	})
}
