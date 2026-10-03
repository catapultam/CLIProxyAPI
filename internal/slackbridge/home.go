package slackbridge

import (
	"context"
	"errors"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
)

// Moving a session's home thread: an owner says "!channel" or "!dm" (or
// "take it to the channel" / "take it to my dm") in the agent's thread, or
// "name: !channel" at the top level. The bridge opens a new home thread for
// the agent there; the old thread keeps routing replies to it.

const (
	moveChannelName = "channel"
	moveDMName      = "dm"

	// ownersOnlyMoves refuses a move from a non-owner.
	ownersOnlyMoves = "Only people set in config.yaml (allowed-emails) can move an agent's thread."
	howToMove       = "To move an agent's thread, say `!channel` or `!dm` in its thread (or \"take it to the channel\" / \"take it to my DM\"), or `name: !channel` at the top level."
	noChannel       = "No channel is configured."
	alreadyThere    = "Already there."
)

// movePhrases are the plain-language forms of !channel and !dm, compared
// lowercased with runs of spaces collapsed and trailing punctuation dropped.
var movePhrases = map[string]string{
	"take it to the channel": homeChannel,
	"move it to the channel": homeChannel,
	"move to the channel":    homeChannel,
	"take it to dm":          homeDM,
	"take it to my dm":       homeDM,
	"move it to dm":          homeDM,
	"move to dm":             homeDM,
}

// parseMove reports whether text asks to move an agent's home thread, and
// where to (homeChannel or homeDM). "!channel" and "!dm" are bridge commands
// whatever follows them, so they never reach an agent as a slash command.
func parseMove(text string) (string, bool) {
	s := strings.Join(strings.Fields(strings.ToLower(text)), " ")
	if word, ok := strings.CutPrefix(s, "!"); ok {
		word, _, _ = strings.Cut(word, " ")
		switch strings.TrimRight(word, ".!?") {
		case moveChannelName:
			return homeChannel, true
		case moveDMName:
			return homeDM, true
		}
		return "", false
	}
	home, ok := movePhrases[strings.TrimRight(s, ".!?,;: ")]
	return home, ok
}

// move runs a move of target's home thread (a session id, name or address;
// empty when the message named no agent) to home, asked by user. Only owners
// may; a refusal is logged. notFound is the reply when target is unknown.
// Opening the new thread is a network call, so the rest runs as a queued
// command job (applyMove); a retry after a failed reply only re-sends the
// reply.
func (b *Bridge) move(ev messageEvent, user allowedUser, target, home, notFound string) {
	if !user.config {
		log.Infof("slack: refused !%s from non-owner %s", home, user.ID)
		b.replyCommand(ev, ownersOnlyMoves)
		return
	}
	if target == "" {
		b.replyCommand(ev, howToMove)
		return
	}
	o, ok := b.sessionOutbound(target)
	if !ok {
		b.replyCommand(ev, notFound)
		return
	}
	var reply string
	applied := false
	b.enqueueCommand(func(ctx context.Context) error {
		if !applied {
			reply, applied = b.applyMove(ctx, ev, user, o, home), true
		}
		if reply == "" {
			return nil
		}
		return b.replyNow(ctx, ev, reply)
	})
}

// sessionOutbound finds the session target names (an id, name or address)
// and what its header needs.
func (b *Bridge) sessionOutbound(target string) (agentbus.Outbound, bool) {
	if o, err := b.bus.SessionOutbound(target); err == nil {
		return o, true
	}
	sid, found := b.bus.Resolve(target)
	if !found {
		return agentbus.Outbound{}, false
	}
	o, err := b.bus.SessionOutbound(sid)
	return o, err == nil
}

// applyMove moves o's session's home thread to home: the channel, or the
// owner's own DM with the bot. It opens a new thread there with the session
// header (noting where it moved from), makes it the home thread, points to
// it from the old one, and tells the agent. The old thread stays linked, so
// replies in it still reach the session. It returns the reply for ev's
// place, or "" when the pointer already went there.
func (b *Bridge) applyMove(ctx context.Context, ev messageEvent, owner allowedUser, o agentbus.Outbound, home string) string {
	sid := o.SessionID
	target := b.channelID
	if home == homeChannel && target == "" {
		return noChannel
	}
	if home == homeDM {
		channel, errDM := b.dmChannel(ctx, owner.ID)
		if errDM != nil {
			log.Infof("slack: %s couldn't open their DM to move %s: %v", owner.ID, o.Address, errDM)
			return "Couldn't open your DM with the bot: " + escape(errDM.Error())
		}
		target = channel
	}
	old, hasOld, ts, reply := b.openMovedThread(ctx, o, target, home)
	if reply != "" {
		return reply
	}
	pointer := "Moved to " + b.placeLink(target)
	link, errLink := b.api.permalink(ctx, b.cfg.BotToken, target, ts)
	if errLink != nil {
		log.Infof("slack: permalink for %s's new thread: %v", o.Address, errLink)
	} else if link != "" {
		pointer += " → " + link
	}
	if hasOld {
		if _, errPost := b.api.postMessage(ctx, b.cfg.BotToken, old.channel, pointer, old.threadTS); errPost != nil {
			log.Warnf("slack: pointer in %s's old thread: %v", o.Address, errPost)
		}
	}
	if _, _, errNotice := b.bus.DeliverNotice(sid, "Your Slack home thread moved to "+b.placeForAgent(target, owner)+". Your messages go there now."); errNotice != nil {
		log.Warnf("slack: move notice for %s not delivered: %v", o.Address, errNotice)
	}
	log.Infof("slack: %s moved %s's home thread to %s", owner.ID, o.Address, home)
	if hasOld && ev.Channel == old.channel && replyThread(ev) == old.threadTS {
		return ""
	}
	return pointer
}

// openMovedThread opens o's session's new home thread in conversation target
// and records it, holding the session's opening gate so no post opens
// another thread meanwhile. It returns the old home thread (hasOld false
// when there was none) and the new thread's ts, or a reply when it didn't
// move ("Already there." or an error).
func (b *Bridge) openMovedThread(ctx context.Context, o agentbus.Outbound, target, home string) (old postTarget, hasOld bool, ts, reply string) {
	unlock, errLock := b.lockOpening(ctx, o.SessionID)
	if errLock != nil {
		return postTarget{}, false, "", "Not moved: " + escape(errLock.Error())
	}
	defer unlock()
	old, hasOld = b.ownThread(o.SessionID)
	current := old.channel
	if !hasOld {
		channel, errHome := b.homeChannelFor(ctx, o.SessionID)
		if errHome != nil {
			return old, hasOld, "", "Not moved: " + escape(errHome.Error())
		}
		current = channel
	}
	if current == target {
		return old, hasOld, "", alreadyThere
	}
	header := sessionHeader(o)
	if hasOld {
		header += " (moved from " + b.placeLink(old.channel) + ")"
	}
	ts, errPost := b.api.postMessage(ctx, b.cfg.BotToken, target, header, "")
	if errPost != nil {
		var apiErr *apiError
		if errors.As(errPost, &apiErr) && apiErr.code != "" {
			return old, hasOld, "", "Couldn't open the new thread: " + escape(apiErr.code)
		}
		return old, hasOld, "", "Couldn't open the new thread."
	}
	b.state.moveThread(o.SessionID, target, ts, home)
	return old, hasOld, ts, ""
}

// placeLink names conversation channel in Slack text: the main channel as a
// channel link, anything else as "DM".
func (b *Bridge) placeLink(channel string) string {
	if channel != "" && channel == b.channelID {
		return "<#" + channel + ">"
	}
	return "DM"
}

// placeForAgent names conversation channel for the agent's notice.
func (b *Bridge) placeForAgent(channel string, owner allowedUser) string {
	if channel != "" && channel == b.channelID {
		return "the Slack channel #" + strings.TrimPrefix(strings.TrimSpace(b.cfg.Channel), "#")
	}
	return "@" + owner.Label + "'s DM with the bot"
}
