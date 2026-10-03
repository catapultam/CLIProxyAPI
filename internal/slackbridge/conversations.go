package slackbridge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
)

// Guest conversations: an owner links a conversation other than the main
// channel (a group DM, another channel, a DM) to an agent. Everything
// written there reaches that agent; allowed users' messages as their
// instructions, everyone else's as guest input (agentbus.Message.Guest).

const (
	mainNotLinkable = "The main channel can't be linked or unlinked: it reaches every agent already."
	notLinked       = "This conversation isn't linked to an agent."
	needSomeone     = "Name at least one other person: `@agents chat @person with <agent>`."
	tooLarge        = "That message is over the 16 KiB agentbus limit and was not delivered."
)

// conversationLink returns channel's link while its session hasn't been
// absent from the bus for more than linkAbsentTTL, first noting when the
// session was last seen there.
func (b *Bridge) conversationLink(channel string) (convLink, bool) {
	l, ok := b.state.conversation(channel)
	if !ok {
		return convLink{}, false
	}
	if at, known := b.bus.SessionSeen(l.Session); known && at.After(l.Seen) {
		b.state.touchConversations(map[string]time.Time{l.Session: at})
	}
	return l, true
}

// refreshLinks notes when each linked session was last on the bus and
// drops the links of sessions absent for more than linkAbsentTTL. New runs
// it after loading the state.
func (b *Bridge) refreshLinks() {
	if b.bus == nil {
		return
	}
	seen := map[string]time.Time{}
	for _, sid := range b.state.conversationSessions() {
		if at, ok := b.bus.SessionSeen(sid); ok {
			seen[sid] = at
		}
	}
	b.state.touchConversations(seen)
}

// conversationKind names the kind of conversation ev was written in.
func conversationKind(ev messageEvent) string {
	switch {
	case isDM(ev):
		return "direct message"
	case ev.ChannelType == "mpim":
		return "group DM"
	}
	return "channel"
}

// notFoundAgent is the reply when agent doesn't resolve to a session.
func (b *Bridge) notFoundAgent(agent string) string {
	return fmt.Sprintf("No agent called `%s`. %s", escape(agent), b.onlineHint())
}

// openLinked runs "chat @a [@b …] with <agent>" (a group DM of the owner
// and those people) and "dm @a with <agent>" (the bot's DM with that
// person, without the owner). Opening the conversation is a network call,
// so the rest happens in a queued command job (applyOpen); a retry after a
// failed reply only re-sends the reply.
func (b *Bridge) openLinked(ev messageEvent, owner allowedUser, cmd botCommand) {
	sid, found := b.bus.Resolve(cmd.agent)
	if !found {
		b.replyCommand(ev, b.notFoundAgent(cmd.agent))
		return
	}
	var members []string
	if cmd.verb == "chat" {
		members = []string{owner.ID}
	}
	for _, id := range cmd.users {
		if id != b.botUserID && !slices.Contains(members, id) {
			members = append(members, id)
		}
	}
	if (cmd.verb == "chat" && len(members) < 2) || len(members) == 0 {
		b.replyCommand(ev, needSomeone)
		return
	}
	var confirm string
	b.enqueueCommand(func(ctx context.Context) error {
		if confirm == "" {
			confirm = b.applyOpen(ctx, owner, cmd.verb, members, sid, cmd.agent)
		}
		return b.replyNow(ctx, ev, confirm)
	})
}

// applyOpen opens the conversation of the bot with members, links it to
// sid, posts the intro there and tells the agent. It returns the reply for
// the owner.
func (b *Bridge) applyOpen(ctx context.Context, owner allowedUser, verb string, members []string, sid, agent string) string {
	kind := "group DM"
	if verb == "dm" {
		kind = "direct message"
	}
	channel, err := b.api.openConversation(ctx, b.cfg.BotToken, members)
	if err != nil {
		log.Infof("slack: %s couldn't open a %s: %v", owner.ID, kind, err)
		return "Couldn't open the conversation: " + escape(err.Error())
	}
	// Everyone in it is known, so the allowed ones are listed (even none).
	names, instructors, instructorIDs := []string{}, []string{}, []string{}
	for _, id := range members {
		if u, ok := b.state.user(id); ok {
			names = append(names, "@"+u.Label)
			instructors = append(instructors, "@"+u.Label)
			instructorIDs = append(instructorIDs, "<@"+u.ID+">")
			continue
		}
		names = append(names, "@"+b.guestLabel(b.guestName(ctx, id), id))
	}
	prev, errSave := b.state.linkConversation(channel, sid, owner.ID)
	if verb == "dm" {
		if u, ok := b.state.user(members[0]); ok {
			// Their plain messages there go to the linked agent from now on.
			b.state.setDMLast(u.ID, sid)
			b.rememberDM(u.ID, channel)
		}
	}
	intro := fmt.Sprintf("Linked to *%s*. Messages here go to that agent. %s", escape(agent), whoInstructs(instructorIDs))
	if _, errPost := b.api.postMessage(ctx, b.cfg.BotToken, channel, intro, ""); errPost != nil {
		log.Warnf("slack: intro for %s %s: %v", kind, channel, errPost)
	}
	b.noticeLinked(channel, sid, fmt.Sprintf("You were linked to a Slack %s with %s (opened by @%s). %s To post there, reply to this notice.",
		kind, strings.Join(names, ", "), owner.Label, whoInstructsAgent(instructors)))
	log.Infof("slack: %s opened %s %s, linked to %s", owner.ID, kind, channel, b.bus.Address(sid))
	confirm := fmt.Sprintf("Opened a %s with %s, linked to *%s*.", kind, mentionList(members), escape(agent)) + b.relinked(prev, sid, owner)
	if errSave != nil {
		logSaveError(errSave)
		confirm += notSavedNote
	}
	return confirm
}

// linkHere runs "link <agent>" in the conversation it was posted in.
func (b *Bridge) linkHere(ev messageEvent, owner allowedUser, agent string) {
	if !isDM(ev) && ev.Channel == b.channelID {
		b.replyCommand(ev, mainNotLinkable)
		return
	}
	sid, found := b.bus.Resolve(agent)
	if !found {
		b.replyCommand(ev, b.notFoundAgent(agent))
		return
	}
	prev, errSave := b.state.linkConversation(ev.Channel, sid, owner.ID)
	if isDM(ev) {
		// The owner's own DM: plain messages go to the linked agent now.
		b.state.setDMLast(ev.User, sid)
	}
	kind := conversationKind(ev)
	b.noticeLinked(ev.Channel, sid, fmt.Sprintf("You were linked to a Slack %s by @%s. %s To post there, reply to this notice.",
		kind, owner.Label, whoInstructsAgent(nil)))
	log.Infof("slack: %s linked %s %s to %s", owner.ID, kind, ev.Channel, b.bus.Address(sid))
	confirm := fmt.Sprintf("Linked this conversation to *%s*. Messages here go to that agent. %s", escape(agent), whoInstructs(nil)) + b.relinked(prev, sid, owner)
	if errSave != nil {
		logSaveError(errSave)
		confirm += notSavedNote
	}
	b.replyCommand(ev, confirm)
}

// unlinkHere runs "unlink" in the conversation it was posted in.
func (b *Bridge) unlinkHere(ev messageEvent, owner allowedUser) {
	if !isDM(ev) && ev.Channel == b.channelID {
		b.replyCommand(ev, mainNotLinkable)
		return
	}
	prev, ok, errSave := b.state.unlinkConversation(ev.Channel)
	if !ok {
		b.replyCommand(ev, notLinked)
		return
	}
	b.noticeUnlinked(prev, owner)
	log.Infof("slack: %s unlinked %s from %s", owner.ID, ev.Channel, b.bus.Address(prev))
	confirm := fmt.Sprintf("Unlinked this conversation from `%s`. Messages here no longer reach it; tag an agent (`name: message`) to reach one.", escape(b.bus.Address(prev)))
	if errSave != nil {
		logSaveError(errSave)
		confirm += notSavedNote
	}
	b.replyCommand(ev, confirm)
}

// relinked is what the confirmation adds when the conversation was linked
// before (to prev); a session that lost the link is told so.
func (b *Bridge) relinked(prev, sid string, owner allowedUser) string {
	switch prev {
	case "":
		return ""
	case sid:
		return " (It already was.)"
	}
	b.noticeUnlinked(prev, owner)
	return fmt.Sprintf(" It was linked to `%s` before; that link is replaced.", escape(b.bus.Address(prev)))
}

// whoInstructs is the intro's line on whose messages are instructions:
// mentions lists the allowed people in the conversation; nil means it isn't
// known who is there.
func whoInstructs(mentions []string) string {
	switch {
	case mentions == nil:
		return "Allowed users' messages are instructions; everyone else's are guest input."
	case len(mentions) == 0:
		return "Everyone's messages here are guest input, not instructions."
	case len(mentions) == 1:
		return mentions[0] + "'s messages are instructions; everyone else's are guest input."
	}
	return "Messages from " + strings.Join(mentions, ", ") + " are instructions; everyone else's are guest input."
}

// whoInstructsAgent is whoInstructs for the agent's notice, with labels.
func whoInstructsAgent(labels []string) string {
	switch {
	case labels == nil:
		return "Messages from there reach you: allowed users' are instructions, everyone else's are guest input."
	case len(labels) == 0:
		return "Messages from there reach you as guest input, not instructions."
	}
	return "Messages from there reach you: those from " + strings.Join(labels, ", ") + " are instructions, everyone else's are guest input."
}

// mentionList renders user IDs as Slack mentions.
func mentionList(ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, "<@"+id+">")
	}
	return strings.Join(out, ", ")
}

// noticeLinked tells sid it was linked to channel, and records the notice
// as a reply target there: the agent posts in channel by answering it.
func (b *Bridge) noticeLinked(channel, sid, body string) {
	nsid, msgID, err := b.bus.DeliverNotice(sid, body)
	if err != nil {
		log.Warnf("slack: link notice for %s not delivered: %v", b.bus.Address(sid), err)
		return
	}
	b.state.record(replyRecord{ID: msgID, Channel: channel, Session: nsid, Link: true})
}

// noticeUnlinked tells sid that a conversation it was linked to no longer
// is. A session that has left the bus is skipped.
func (b *Bridge) noticeUnlinked(sid string, owner allowedUser) {
	body := fmt.Sprintf("@%s unlinked a Slack conversation you were linked to. Messages from there no longer reach you, and answers to its guests go to your own thread.", owner.Label)
	if _, _, err := b.bus.DeliverNotice(sid, body); err != nil && !errors.Is(err, agentbus.ErrUnknownTarget) {
		log.Warnf("slack: unlink notice for %s not delivered: %v", b.bus.Address(sid), err)
	}
}

// routeGuest handles a message from someone who isn't allowed, in a
// conversation linked to link.Session: it reaches that agent as guest
// input, never as an instruction, and never another agent (no tags). A
// guest can't run commands. The guest's label needs a users.info lookup
// the first time, which runs as a job; while one of a guest's messages
// waits for it, the later ones queue behind it.
func (b *Bridge) routeGuest(ev messageEvent, link convLink) {
	cmd, rest, mentioned := parseCommand(ev.Text, b.botUserID)
	if cmd.verb != "" {
		log.Infof("slack: refused %s by guest %s in %s", cmd.verb, ev.User, ev.Channel)
		if cmd.opensLink() {
			b.reply(ev, ownersOnlyLinks)
		} else {
			b.reply(ev, ownersOnly)
		}
		return
	}
	raw := ev.Text
	if mentioned {
		raw = rest
	}
	text := plainText(raw, b.state.idLabels())
	if isBang(text) {
		log.Infof("slack: refused a command from guest %s in %s", ev.User, ev.Channel)
		b.reply(ev, ownersOnlyCommands)
		return
	}
	b.guestMu.Lock()
	name, cached := b.guestNames[ev.User]
	inline := cached && b.guestQueued[ev.User] == 0
	if !inline {
		b.guestQueued[ev.User]++
	}
	b.guestMu.Unlock()
	if inline {
		b.deliverGuest(ev, link.Session, text, name)
		return
	}
	// A dropped job (queue overflow) leaves the count up, so that guest's
	// later messages keep going through the queue; they still arrive.
	b.enqueue(func(ctx context.Context) error {
		b.deliverGuest(ev, link.Session, text, b.guestName(ctx, ev.User))
		b.guestMu.Lock()
		if b.guestQueued[ev.User]--; b.guestQueued[ev.User] <= 0 {
			delete(b.guestQueued, ev.User)
		}
		b.guestMu.Unlock()
		return nil
	})
}

// deliverGuest delivers a guest's text to sid and records where it came
// from, so the agent can answer there while the link lasts.
func (b *Bridge) deliverGuest(ev messageEvent, sid, text, name string) {
	nsid, msgID, err := b.bus.DeliverGuest(sid, text, b.guestLabel(name, ev.User), b.viaOf(ev))
	switch {
	case err == nil:
		r := replyRecord{ID: msgID, Channel: ev.Channel, ThreadTS: replyThread(ev), Session: nsid, TS: ev.TS, Receipt: reactionQueued, Link: true}
		b.react(ev, b.state.record(r))
	case errors.Is(err, agentbus.ErrUnknownTarget):
		b.reply(ev, sessionEnded)
	case errors.Is(err, agentbus.ErrBodyTooLarge):
		b.reply(ev, tooLarge)
	default:
		b.reply(ev, "Not delivered: "+escape(err.Error()))
	}
}

// guestName returns a guest's Slack display name, looking it up (and
// caching it) the first time. A failed lookup returns "" and isn't cached.
func (b *Bridge) guestName(ctx context.Context, userID string) string {
	b.guestMu.Lock()
	name, ok := b.guestNames[userID]
	b.guestMu.Unlock()
	if ok {
		return name
	}
	display, _, err := b.api.userInfo(ctx, b.cfg.BotToken, userID)
	if err != nil {
		log.Infof("slack: guest %s: lookup failed: %v", userID, err)
		return ""
	}
	b.guestMu.Lock()
	b.guestNames[userID] = display
	b.guestMu.Unlock()
	return display
}

// guestLabel is how agents know a guest: their sanitized display name (the
// user ID when unknown), with "-guest" added while it is an allowed user's
// label, so a guest never passes for an allowed user.
func (b *Bridge) guestLabel(name, userID string) string {
	label := sanitizeLabel(name)
	if strings.TrimSpace(name) == "" {
		label = sanitizeLabel(userID)
	}
	for {
		if _, taken := b.state.userByLabel(label); !taken {
			return label
		}
		label += "-guest"
	}
}
