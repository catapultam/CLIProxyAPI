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
	commandHelp  = "Commands (for people set in config.yaml): `@agents allow @person` lets someone instruct agents; `@agents remove @person` takes that back. " +
		"`@agents chat @person [@person …] with <agent>` opens a group DM linked to an agent; `@agents dm @person with <agent>` opens the bot's DM with that person, linked to an agent. " +
		"`@agents link <agent>` links the conversation it's posted in; `@agents unlink` undoes that. From the main channel or your DM with the bot, `@agents links` lists linked conversations, and `@agents unlink @person` or `@agents unlink <conversation id>` unlinks them. In a linked conversation, everyone who isn't allowed is a guest: the agent gets their messages as input, not instructions."
	ownersOnly = "Only people set in config.yaml (allowed-emails) can allow or remove users."
	// ownersOnlyLinks refuses chat, dm, link and unlink from a non-owner.
	ownersOnlyLinks = "Only people set in config.yaml (allowed-emails) can open, list, link or unlink conversations."
	notSavedNote    = " (not saved; this reverts when the proxy restarts)"
	// ownersOnlyCommands refuses a "!" command from a non-owner.
	ownersOnlyCommands = "Only owners can run commands."
	howToCommand       = "Run a command in an agent's thread (`!compact`), or at the top level as `name: !compact`. `!commands` lists them."
	sessionEnded       = "That agent's session has ended, so this wasn't delivered."
	// howToDM answers a DM the bridge can't route.
	howToDM     = "To reach an agent here, write `name: message` (the name or address from its posts). After that, plain messages go to the agent you last talked to here, and a reply in a thread goes to the agent whose message started it."
	onlineLimit = 10
)

type messageEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Channel string `json:"channel"`
	// ChannelType is "im" in a direct message with the bot.
	ChannelType string `json:"channel_type"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
}

// relayedSubtypes are the message subtypes that are a person writing:
// a plain message, a thread reply also sent to the channel, a message with a file.
var relayedSubtypes = map[string]bool{"": true, "thread_broadcast": true, "file_share": true}

// isDM reports whether ev is in a user's direct message with the bot (Slack
// gives a DM's channel id a "D" prefix).
func isDM(ev messageEvent) bool {
	return ev.ChannelType == "im" || strings.HasPrefix(ev.Channel, "D")
}

// handleEvent routes one Slack message: from the channel, a DM with the bot,
// or another conversation the bot is in (see routeForeign). It only touches
// memory, the state file and the job queues, never the network, so the
// socket loop can ack as soon as it returns.
//
// Anywhere, a message that tags an agent ("name: message" or "@name
// message", where name resolves to a session) reaches that agent, and its
// answer lands where the message was (see sendTagged). A leading mention of
// the bot that isn't a command is dropped first, so "@agents name: …" tags
// too.
//
// Only allowed users get through, except in a conversation linked to an
// agent, where anyone else is a guest (see routeGuest).
func (b *Bridge) handleEvent(eventID string, ev messageEvent) {
	dm := isDM(ev)
	if ev.Type != "message" || !relayedSubtypes[ev.Subtype] || ev.BotID != "" ||
		ev.Channel == "" || ev.User == "" || ev.User == b.botUserID {
		return
	}
	main := !dm && ev.Channel == b.channelID
	user, allowed := b.state.user(ev.User)
	var link convLink
	linked := false
	if !main {
		link, linked = b.conversationLink(ev.Channel)
	}
	if !allowed && !linked {
		return
	}
	// Dedup after the allowlist (and the link check) so strangers can't
	// flush the ring. Without an event id, fall back to the message identity.
	key := eventID
	if key == "" {
		key = ev.Channel + ":" + ev.TS
	}
	if b.alreadySeen(key) {
		return
	}
	if linked {
		// So an owner can later unlink every conversation this person is in.
		b.state.noteMember(ev.Channel, ev.User)
	}
	if !allowed {
		b.routeGuest(ev, link)
		return
	}
	active := main || dm || linked
	cmd, rest, mentioned := parseCommand(ev.Text, b.botUserID)
	if cmd.verb != "" {
		// Where the bot was only added, it answers nothing but the commands
		// that link it.
		if active || cmd.opensLink() {
			b.command(ev, user, cmd)
		}
		return
	}
	if mentioned {
		ev.Text = rest
		if _, _, tagOK := b.tagged(ev, plainText(rest, b.state.idLabels())); !tagOK {
			if active {
				b.replyCommand(ev, commandHelp)
			}
			return
		}
	}
	text := plainText(ev.Text, b.state.idLabels())
	if !main && !dm {
		b.routeForeign(ev, user, text, link, linked)
		return
	}
	if dm {
		b.routeDM(ev, user, text, link, linked)
		return
	}
	if ev.ThreadTS != "" && ev.ThreadTS != ev.TS {
		sid, linked := b.state.session(ev.ThreadTS)
		if b.sendTagged(ev, user, text, sid) {
			return
		}
		if !linked {
			if home, isMove := parseMove(text); isMove {
				b.move(ev, user, "", home, "")
				return
			}
			// Answer an unlinked thread once, not on every message in it.
			if !b.alreadySeen("unlinked:" + ev.ThreadTS) {
				b.reply(ev, "This thread isn't linked to an agent. "+howToAddress)
			}
			return
		}
		b.send(ev, user, sid, text, false)
		return
	}
	target, body, addressedOK := parseAddressed(text)
	if !addressedOK {
		if sid, tagBody, tagOK := b.tagged(ev, text); tagOK {
			b.send(ev, user, sid, tagBody, true)
			return
		}
		if home, isMove := parseMove(text); isMove {
			b.move(ev, user, "", home, "")
			return
		}
		if isBang(text) {
			// Only !commands needs no target; runCommand explains the rest.
			b.runCommand(ev, user, "", text, "", false)
			return
		}
		b.reply(ev, howToAddress)
		return
	}
	notFound := fmt.Sprintf("No agent called `%s`. %s", escape(target), b.onlineHint())
	if home, isMove := parseMove(body); isMove {
		b.move(ev, user, target, home, notFound)
		return
	}
	if isBang(body) {
		b.runCommand(ev, user, target, body, notFound, true)
		return
	}
	b.deliver(ev, target, body, user, notFound, true)
}

// routeForeign handles a message from an allowed user in a conversation
// that is neither the channel nor their DM with the bot: a group DM or
// another channel the bot was added to. The answer lands in that
// conversation and thread. With the usual "!" command rule:
//   - a message that tags an agent goes to that agent;
//   - else a reply in a thread whose first message (or, failing that, a
//     later one) went to an agent, or under an agent's own post there, goes
//     to that agent;
//   - else, when the conversation is linked, the message goes to its agent.
//
// Anything else is ignored without a reply, so the bot stays quiet where
// it was only added.
func (b *Bridge) routeForeign(ev messageEvent, user allowedUser, text string, link convLink, linked bool) {
	own, ownOK := "", false
	if ev.ThreadTS != "" && ev.ThreadTS != ev.TS {
		own, ownOK = b.foreignThreadSession(ev)
	}
	if !ownOK && linked {
		own, ownOK = link.Session, true
	}
	if b.sendTagged(ev, user, text, own) || !ownOK {
		return
	}
	b.send(ev, user, own, text, false)
}

// foreignThreadSession is the session a thread reply in a conversation
// other than the main channel and DMs goes to: the agent whose own
// top-level post started the thread, else the one its messages went to
// (state.threadSession).
func (b *Bridge) foreignThreadSession(ev messageEvent) (string, bool) {
	if sid, ok := b.state.dmSession(ev.Channel, ev.ThreadTS); ok {
		return sid, true
	}
	return b.state.threadSession(ev.Channel, ev.ThreadTS)
}

// tagged finds the agent text tags: "name: message" or "@name message" (see
// parseAtTagged), where name resolves to a bus session (never "slack"). It
// returns that session's id and the message.
func (b *Bridge) tagged(ev messageEvent, text string) (string, string, bool) {
	name, body, ok := parseAddressed(text)
	if !ok {
		name, body, ok = parseAtTagged(ev.Text, text)
	}
	if !ok {
		return "", "", false
	}
	sid, found := b.bus.Resolve(name)
	if !found {
		return "", "", false
	}
	return sid, body, true
}

// sendTagged sends the message in text to the agent it tags, unless that is
// own (the session this conversation or thread already goes to; empty for
// none), and reports whether it did. The delivery adopts nothing: the reply
// map sends the agent's answer back to this conversation and thread, which
// stay own's. A name that doesn't resolve is no tag ("note: …"), and a tag
// of own itself leaves text whole for own.
func (b *Bridge) sendTagged(ev messageEvent, user allowedUser, text, own string) bool {
	sid, body, ok := b.tagged(ev, text)
	if !ok || sid == own {
		return false
	}
	b.send(ev, user, sid, body, false)
	return true
}

// send delivers text to the session sid, or runs it there when it is a "!"
// command, or moves sid's home thread when it asks for that (parseMove).
// adopt is as for deliver.
func (b *Bridge) send(ev messageEvent, user allowedUser, sid, text string, adopt bool) {
	if home, isMove := parseMove(text); isMove {
		b.move(ev, user, sid, home, sessionEnded)
		return
	}
	if isBang(text) {
		b.runCommand(ev, user, sid, text, sessionEnded, adopt)
		return
	}
	b.deliver(ev, sid, text, user, sessionEnded, adopt)
}

// routeDM routes a message from an allowed user in their DM with the bot:
//   - a reply in a thread goes to the agent the thread's first message is
//     tied to, unless it tags another agent;
//   - a top-level "name: …" or "@name …" goes to that agent;
//   - any other top-level message goes to the agent the user last talked to
//     in the DM (dmLast, while it hasn't expired), else to the agent the DM
//     is linked to, or gets a help reply.
//
// "!" commands take the same routes, with the same owner rule. Every
// delivery makes its agent the user's dmLast.
func (b *Bridge) routeDM(ev messageEvent, user allowedUser, text string, link convLink, linkedDM bool) {
	b.rememberDM(ev.User, ev.Channel)
	if ev.ThreadTS != "" && ev.ThreadTS != ev.TS {
		sid, linked := b.dmThreadSession(ev)
		if !linked && linkedDM {
			sid, linked = link.Session, true
		}
		if b.sendTagged(ev, user, text, sid) {
			return
		}
		if !linked {
			if home, isMove := parseMove(text); isMove {
				b.move(ev, user, "", home, "")
				return
			}
			if !b.alreadySeen("unlinked:" + ev.Channel + ":" + ev.ThreadTS) {
				b.reply(ev, "This thread isn't linked to an agent. "+howToDM)
			}
			return
		}
		b.send(ev, user, sid, text, false)
		return
	}
	if target, body, addressedOK := parseAddressed(text); addressedOK {
		notFound := fmt.Sprintf("No agent called `%s`. %s", escape(target), b.onlineHint())
		if home, isMove := parseMove(body); isMove {
			b.move(ev, user, target, home, notFound)
			return
		}
		if isBang(body) {
			b.runCommand(ev, user, target, body, notFound, true)
			return
		}
		b.deliver(ev, target, body, user, notFound, true)
		return
	}
	if sid, body, ok := b.tagged(ev, text); ok {
		b.send(ev, user, sid, body, true)
		return
	}
	if home, isMove := parseMove(text); isMove {
		// A move names its agent by thread or "name:", never by dmLast.
		b.move(ev, user, "", home, "")
		return
	}
	sid, ok := b.state.dmLast(ev.User)
	if !ok && linkedDM {
		sid, ok = link.Session, true
	}
	if isBang(text) {
		// Without a dmLast the target is empty: runCommand answers !commands
		// and explains the rest.
		b.runCommand(ev, user, sid, text, sessionEnded, ok)
		return
	}
	if !ok {
		b.reply(ev, howToDM+" "+b.onlineHint())
		return
	}
	b.deliver(ev, sid, text, user, sessionEnded, true)
}

// dmThreadSession is the session a DM thread reply goes to: the one its
// thread's first message is tied to, or, failing that, a session thread
// opened there.
func (b *Bridge) dmThreadSession(ev messageEvent) (string, bool) {
	if sid, ok := b.state.dmSession(ev.Channel, ev.ThreadTS); ok {
		return sid, true
	}
	return b.state.session(ev.ThreadTS)
}

// runCommand handles "!name rest" from user for target (a session id, name
// or address; empty at the top level without "name:"). Only owners may run
// commands. !commands is answered here; anything else resolves against the
// registry (falling through to the Claude Code slash command of that name)
// and is delivered as a command message the target's mod runs. adopt is as
// for deliver (a top-level post). notFound is the reply when target is
// unknown. Replies go on the command queue, so a flood of agent posts can't
// drop them.
func (b *Bridge) runCommand(ev messageEvent, user allowedUser, target, text, notFound string, adopt bool) {
	if !user.config {
		log.Infof("slack: refused a command from non-owner %s", user.ID)
		b.replyCommand(ev, ownersOnlyCommands)
		return
	}
	name, rest, errParse := parseBang(text)
	if errParse != nil {
		b.replyCommand(ev, errParse.Error())
		return
	}
	if name == listCommandsName {
		b.replyCommand(ev, b.commandList())
		return
	}
	if target == "" {
		b.replyCommand(ev, howToCommand)
		return
	}
	cmd := agentbus.Command{Name: name, Kind: agentbus.CommandSlash, Command: name, Args: rest}
	if entry, found := b.cmdRegistry.lookup(name); found {
		if entry.err != nil {
			b.replyCommand(ev, fmt.Sprintf("`!%s` is misconfigured.", name))
			return
		}
		if errArgs := entry.spec.checkArgs(rest); errArgs != nil {
			log.Infof("slack: refused !%s from %s: arguments not accepted", name, user.ID)
			b.replyCommand(ev, errArgs.Error())
			return
		}
		cmd = entry.spec.command(rest)
	}
	display := target
	if adopt {
		display = escape(target)
	}
	sid, capable, errCapable := b.bus.CommandCapable(target)
	// Name the agent as the user did, unless target is a session id.
	if errCapable == nil && (!adopt || target == sid) {
		display = escape(b.bus.Address(sid))
	}
	if errCapable == nil && !capable {
		errCapable = agentbus.ErrCommandUnsupported
	}
	var msgID string
	if errCapable == nil {
		sid, msgID, errCapable = b.bus.DeliverCommand(sid, cmd, user.Label, user.ID)
	}
	switch {
	case errCapable == nil:
	case errors.Is(errCapable, agentbus.ErrUnknownTarget):
		b.replyCommand(ev, notFound)
		return
	case errors.Is(errCapable, agentbus.ErrCommandUnsupported):
		b.replyCommand(ev, fmt.Sprintf("`%s` can't run commands (agentbus plugin %s+ required)", display, agentbus.MinCommandModVersion))
		return
	default:
		b.replyCommand(ev, "Not delivered: "+escape(errCapable.Error()))
		return
	}
	b.react(ev, b.recordDelivery(ev, msgID, sid, adopt, reactionCommand))
	log.Infof("slack: %s sent !%s (%s) to %s", user.ID, cmd.Name, cmd.Kind, b.bus.Address(sid))
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

// viaOf is where ev was written, as agentbus marks it: a DM, a group
// conversation (a group DM or another channel), or the main channel ("").
func (b *Bridge) viaOf(ev messageEvent) string {
	switch {
	case isDM(ev):
		return agentbus.ViaDM
	case ev.Channel != b.channelID:
		return agentbus.ViaGroup
	}
	return ""
}

// deliver queues body for target; notFound is the reply when the target is
// unknown. A message from a DM or a group conversation is marked as such.
// Where it came from is recorded (recordDelivery), so the session can
// answer there; adopt is true for a top-level post.
func (b *Bridge) deliver(ev messageEvent, target, body string, user allowedUser, notFound string, adopt bool) {
	sid, msgID, err := b.bus.DeliverVia(target, body, user.Label, b.viaOf(ev))
	switch {
	case err == nil:
		b.react(ev, b.recordDelivery(ev, msgID, sid, adopt, reactionQueued))
	case errors.Is(err, agentbus.ErrUnknownTarget):
		b.reply(ev, notFound)
	case errors.Is(err, agentbus.ErrBodyTooLarge):
		b.reply(ev, "That message is over the 16 KiB agentbus limit and was not delivered.")
	default:
		b.reply(ev, "Not delivered: "+escape(err.Error()))
	}
}

// recordDelivery remembers that msgID, delivered to sid, came from ev, so sid
// can answer in that conversation and thread. With adopt (a top-level post),
// later messages there reach sid too: in the channel the post's thread
// becomes one of sid's threads (its home thread when it has none and its
// home is the channel); in a DM the post is linked to sid, so thread
// replies under it reach sid. Every delivery from a DM also makes sid the
// user's dmLast.
//
// queued is the receipt reaction the message starts with; recordDelivery
// returns the one to put on ev now, which is a later receipt when one beat
// the record (see state.record).
func (b *Bridge) recordDelivery(ev messageEvent, msgID, sid string, adopt bool, queued string) string {
	r := replyRecord{ID: msgID, Channel: ev.Channel, ThreadTS: replyThread(ev), Session: sid, TS: ev.TS, Receipt: queued}
	if !isDM(ev) {
		reaction := b.state.record(r)
		if adopt {
			// The post's thread becomes sid's home thread only when sid has
			// none and its home is the channel; else it is just linked.
			b.state.setThread(sid, ev.Channel, ev.TS, b.homeOf(sid) == homeChannel)
		}
		return reaction
	}
	r.DMUser = ev.User
	reaction := b.state.record(r)
	b.state.setDMLast(ev.User, sid)
	if adopt {
		b.state.linkDM(ev.Channel, ev.TS, sid, false)
	}
	return reaction
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

// command runs an "@bot <verb>" command: allow/remove, or chat, dm, link
// and unlink (see conversations.go). Only users seeded from config (owners)
// may run them, so access granted from Slack can't chain. The check happens
// before any lookup or enqueue, so a non-owner's attempt never calls
// users.info.
//
// remove applies at once, so revoking access never waits behind queued
// posts. allow needs a users.info lookup, so it is a queued command job; it
// records the target's command count now and applies only if no later
// allow/remove for the same user came in meanwhile.
func (b *Bridge) command(ev messageEvent, user allowedUser, cmd botCommand) {
	if !user.config {
		log.Infof("slack: refused %s by non-owner %s", cmd.verb, user.ID)
		if cmd.opensLink() || cmd.verb == "links" {
			b.replyCommand(ev, ownersOnlyLinks)
		} else {
			b.replyCommand(ev, ownersOnly)
		}
		return
	}
	if !cmd.ok {
		b.replyCommand(ev, commandHelp)
		return
	}
	var target string
	if len(cmd.users) == 1 {
		target = cmd.users[0]
	}
	switch cmd.verb {
	case "chat", "dm":
		b.openLinked(ev, user, cmd)
	case "link":
		b.linkHere(ev, user, cmd.agent)
	case "unlink":
		switch {
		case target != "":
			b.unlinkPerson(ev, user, target)
		case cmd.conv != "":
			b.unlinkByID(ev, user, cmd.conv)
		default:
			b.unlinkHere(ev, user)
		}
	case "links":
		b.listLinks(ev)
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

// replyThread is the thread a reply to ev belongs in: ev's thread, or for a
// top-level post a new thread under it, except in a DM, where a reply to a
// top-level message stays at the top level ("").
func replyThread(ev messageEvent) string {
	switch {
	case ev.ThreadTS != "" && ev.ThreadTS != ev.TS:
		return ev.ThreadTS
	case isDM(ev):
		return ""
	default:
		return ev.TS
	}
}

func (b *Bridge) reply(ev messageEvent, text string) {
	b.enqueue(func(ctx context.Context) error { return b.replyNow(ctx, ev, text) })
}

// replyCommand queues a reply to a command on the command queue.
func (b *Bridge) replyCommand(ev messageEvent, text string) {
	b.enqueueCommand(func(ctx context.Context) error { return b.replyNow(ctx, ev, text) })
}

func (b *Bridge) replyNow(ctx context.Context, ev messageEvent, text string) error {
	_, err := b.api.postMessage(ctx, b.cfg.BotToken, ev.Channel, text, replyThread(ev))
	return err
}

func (b *Bridge) react(ev messageEvent, name string) {
	b.enqueue(func(ctx context.Context) error {
		return b.api.addReaction(ctx, b.cfg.BotToken, ev.Channel, ev.TS, name)
	})
}
