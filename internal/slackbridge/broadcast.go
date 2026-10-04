package slackbridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
)

// Broadcasts: an owner's "all: message" (or "@all message"), written in the
// main channel, a DM with the bot, a message that mentions the bot, or with
// /clanker, goes to every session that isn't offline, as their instruction
// to each, marked as a broadcast (agentbus.Message.Broadcast).
// Every agent answers where the broadcast was written. Its Slack message
// shows one receipt for all of them (state.groupReceipt). "all: !cmd" sends
// a command to every session that can run commands, except shell commands.

const (
	ownersOnlyBroadcast = "Only owners can message all agents."
	shellNoBroadcast    = "Run shell commands per agent."
	moveNoBroadcast     = "Move agents' threads one agent at a time."
)

// broadcastPlan is who a broadcast goes to and what: body, or cmd for a
// command. addrs is parallel to sids: each recipient's canonical bus
// address, for BroadcastTo (never a name, which can repeat once an offline
// session's name is reused). skipped counts the sessions left out because
// they can't run commands.
type broadcastPlan struct {
	sids    []string
	addrs   []string
	body    string
	cmd     *agentbus.Command
	skipped int
}

// broadcastBody returns the message of an "all: message" or "@all message"
// post (raw is its text before plainText, text after).
func broadcastBody(raw, text string) (string, bool) {
	name, body, ok := parseAddressed(text)
	if !ok {
		name, body, ok = parseAtTagged(raw, text)
	}
	if !ok || !strings.EqualFold(name, agentbus.BroadcastName) {
		return "", false
	}
	return body, true
}

// broadcast sends body from user to every live session and replies once,
// where ev was written, with who got it.
func (b *Bridge) broadcast(ev messageEvent, user allowedUser, body string) {
	plan, refusal := b.planBroadcast(user, body)
	if refusal != "" {
		b.reply(ev, refusal)
		return
	}
	b.runBroadcast(ev, user, plan)
	b.reply(ev, b.broadcastReply(ev, plan))
}

// planBroadcast decides who body from user goes to, or returns the reply
// that refuses it. Only owners may broadcast. It only touches memory.
func (b *Bridge) planBroadcast(user allowedUser, body string) (broadcastPlan, string) {
	if !user.config {
		log.Infof("slack: refused a broadcast from non-owner %s", user.ID)
		return broadcastPlan{}, ownersOnlyBroadcast
	}
	plan := broadcastPlan{body: body}
	if _, isMove := parseMove(body); isMove {
		return broadcastPlan{}, moveNoBroadcast
	}
	if isBang(body) {
		name, rest, errParse := parseBang(body)
		if errParse != nil {
			return broadcastPlan{}, errParse.Error()
		}
		if name == listCommandsName {
			return broadcastPlan{}, b.commandList()
		}
		cmd, refusal := b.commandFor(user, name, rest)
		if refusal != "" {
			return broadcastPlan{}, refusal
		}
		if cmd.Kind == agentbus.CommandShell {
			log.Infof("slack: refused a broadcast of !%s from %s: a shell command", name, user.ID)
			return broadcastPlan{}, shellNoBroadcast
		}
		plan.cmd = &cmd
	} else if len(body) > agentbus.MaxBodyBytes {
		return broadcastPlan{}, tooLarge
	}
	for _, r := range b.liveSessions() {
		if plan.cmd != nil {
			if _, capable, errCapable := b.bus.CommandCapable(r.sid); errCapable != nil || !capable {
				plan.skipped++
				continue
			}
		}
		plan.sids = append(plan.sids, r.sid)
		plan.addrs = append(plan.addrs, r.address)
	}
	return plan, ""
}

// liveRecipient is one session a broadcast can reach: its id and its
// canonical bus address, for another recipient's BroadcastTo. The address,
// not the name, since a name can be reused once the session that had it
// goes offline, while the address stays unique.
type liveRecipient struct {
	sid, address string
}

// liveSessions lists the sessions that aren't offline, most recent first,
// as help shows them (the bridge's own peer left out), paired with each
// one's bus address.
func (b *Bridge) liveSessions() []liveRecipient {
	var out []liveRecipient
	for _, p := range b.bus.Peers() {
		if p.Address == agentbus.SlackAddress || p.Status == agentbus.StatusOffline {
			continue
		}
		sid, ok := b.bus.Resolve(p.Address)
		if !ok {
			continue
		}
		out = append(out, liveRecipient{sid: sid, address: p.Address})
	}
	return out
}

// otherAddrs returns addrs without the entry at i, as a fresh slice, so
// each broadcast recipient's BroadcastTo names only the others, never
// itself.
func otherAddrs(addrs []string, i int) []string {
	if len(addrs) <= 1 {
		return nil
	}
	out := make([]string, 0, len(addrs)-1)
	for j, a := range addrs {
		if j != i {
			out = append(out, a)
		}
	}
	return out
}

// runBroadcast delivers plan from user to each of its sessions and records
// where each came from (ev), as one group for the receipt. It adopts nothing
// and leaves dm_last alone: a broadcast is no agent's conversation.
func (b *Bridge) runBroadcast(ev messageEvent, user allowedUser, plan broadcastPlan) {
	group := ""
	if ev.TS != "" {
		group = ev.Channel + "/" + ev.TS
	}
	var first string
	delivered := 0
	total := len(plan.sids)
	for i, sid := range plan.sids {
		others := otherAddrs(plan.addrs, i)
		var nsid, msgID, queued, command string
		var err error
		if plan.cmd != nil {
			nsid, msgID, err = b.bus.DeliverCommandBroadcast(sid, *plan.cmd, user.Label, user.ID, b.viaOf(ev), others, total)
			queued, command = reactionCommand, plan.cmd.Name
		} else {
			nsid, msgID, err = b.bus.DeliverBroadcast(sid, plan.body, user.Label, b.viaOf(ev), others, total)
			queued = reactionQueued
		}
		if err != nil {
			log.Infof("slack: broadcast from %s not delivered to %s: %v", user.ID, b.bus.Address(sid), err)
			continue
		}
		r := b.deliveryRecord(ev, msgID, nsid, queued, command)
		r.Group = group
		b.state.record(r)
		if first == "" {
			first = msgID
		}
		delivered++
	}
	if first != "" {
		b.syncReceipt(first)
	}
	log.Infof("slack: %s broadcast to %d agents", user.ID, delivered)
}

// broadcastReply says who plan went to, in ev's conversation: at most
// helpLimit agents by agentLabel (public names outside owner-only places),
// then how many more, and how many were skipped for lack of commands.
func (b *Bridge) broadcastReply(ev messageEvent, plan broadcastPlan) string {
	if len(plan.sids) == 0 && plan.skipped == 0 {
		return noAgentsOnline
	}
	what := ""
	if plan.cmd != nil {
		what = " `!" + escape(plan.cmd.Name) + "`"
	}
	n := len(plan.sids)
	unit := "agents"
	if n == 1 {
		unit = "agent"
	}
	text := fmt.Sprintf("→ sent%s to %d %s", what, n, unit)
	var names []string
	for _, sid := range plan.sids {
		if len(names) == helpLimit {
			break
		}
		names = append(names, "`"+escape(b.agentLabel(ev, sid))+"`")
	}
	if len(names) > 0 {
		text += ": " + strings.Join(names, ", ")
	}
	if extra := n - len(names); extra > 0 {
		text += fmt.Sprintf(" +%d more", extra)
	}
	if plan.skipped > 0 {
		text += fmt.Sprintf(" (skipped: %d without plugin %s+)", plan.skipped, agentbus.MinCommandModVersion)
	}
	return text
}

// clankerBroadcast runs "/clanker all: body" for user (ev is their DM with
// the bot, its channel not known yet). Its answer is the ack; the deliveries
// run in a job once the DM is open, so the agents answer there.
func (b *Bridge) clankerBroadcast(ev messageEvent, user allowedUser, body string) string {
	plan, refusal := b.planBroadcast(user, body)
	if refusal != "" {
		return refusal
	}
	reply := b.broadcastReply(ev, plan)
	if len(plan.sids) == 0 {
		return reply
	}
	b.enqueueCommand(func(ctx context.Context) error {
		channel, err := b.dmChannel(ctx, user.ID)
		if err != nil {
			return err
		}
		dm := ev
		dm.Channel = channel
		b.runBroadcast(dm, user, plan)
		return nil
	})
	return reply + ". Answers arrive in your DM with " + b.botMention() + "."
}
