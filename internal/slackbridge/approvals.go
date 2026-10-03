package slackbridge

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
)

// Approvals: in a linked conversation anyone may talk with the agent, but
// only allowed users' messages are instructions. When a guest asks for an
// action, the agent asks for approval by posting "confirm: <what it will
// do>"; an allowed user's 👍 on that post approves it, and the agent gets
// an approval message (agentbus.Message.Approval).

const (
	// approvalTTL is how long an approval request can be approved.
	approvalTTL = 24 * time.Hour
	// maxApprovals caps the remembered approval requests; the oldest go first.
	maxApprovals = 500
	// approvalTextLimit caps the request text an approval repeats.
	approvalTextLimit = 200
	// approvalNote follows an approval request's text in Slack.
	approvalNote = "_Needs approval: an allowed user reacts 👍 to approve._"
	// reactionApproved marks an approved request.
	reactionApproved = "white_check_mark"
	// confirmPrefix starts an agent's approval request (case-insensitive).
	confirmPrefix = "confirm:"
)

// pendingApproval is an approval request the bot posted for a session.
type pendingApproval struct {
	// Channel and TS are the bot's post.
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	// Thread is the thread the post is in; empty at the top level.
	Thread  string `json:"thread,omitempty"`
	Session string `json:"session"`
	// Request is the bus id of the session's "confirm:" message.
	Request string `json:"request"`
	// Text is the request's text, cut to approvalTextLimit characters.
	Text string `json:"text"`
	// Link marks a post that answered a message which reached the session
	// only through the conversation's link (see replyRecord.Link).
	Link    bool      `json:"link,omitempty"`
	Created time.Time `json:"created"`
	// Done marks a request that was approved; it can't be approved again.
	Done bool `json:"done,omitempty"`
}

// confirmRequest returns the request text when body asks for approval:
// "confirm:" (any case, leading space allowed) and a non-empty text.
func confirmRequest(body string) (string, bool) {
	t := strings.TrimSpace(body)
	if len(t) < len(confirmPrefix) || !strings.EqualFold(t[:len(confirmPrefix)], confirmPrefix) {
		return "", false
	}
	rest := strings.TrimSpace(t[len(confirmPrefix):])
	return rest, rest != ""
}

// isApprovalReaction reports whether a reaction is a 👍: +1 or thumbsup,
// plain or with a skin tone (skin-tone-2 to skin-tone-6).
func isApprovalReaction(name string) bool {
	base, tone, toned := strings.Cut(name, "::")
	if base != "+1" && base != "thumbsup" {
		return false
	}
	if !toned {
		return true
	}
	n, ok := strings.CutPrefix(tone, "skin-tone-")
	return ok && len(n) == 1 && n[0] >= '2' && n[0] <= '6'
}

// firstRunes cuts s to at most n characters.
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// noteApproval records the bot's post ts of o's approval request (request,
// the text after "confirm:") at t, so an allowed user's 👍 on it approves it.
func (b *Bridge) noteApproval(o agentbus.Outbound, request string, t postTarget, ts string) {
	if ts == "" || o.ID == "" {
		return
	}
	b.state.addApproval(pendingApproval{Channel: t.channel, TS: ts, Thread: t.threadTS, Session: o.SessionID, Request: o.ID, Text: firstRunes(request, approvalTextLimit), Link: t.link})
}

// handleReaction handles a reaction_added event: an allowed user's 👍 on an
// open approval request delivers the approval to its session (once), and
// the bot marks the post ✅. Anything else is ignored; a 👍 from someone who
// isn't allowed is logged at debug level only. It touches only memory, the
// state and the job queue.
func (b *Bridge) handleReaction(ev messageEvent) {
	if !isApprovalReaction(ev.Reaction) || ev.Item.Type != "message" || ev.User == "" || ev.User == b.botUserID {
		return
	}
	if p, ok := b.state.approval(ev.Item.Channel, ev.Item.TS); !ok || p.Done {
		return
	}
	user, allowed := b.state.user(ev.User)
	if !allowed {
		log.Debugf("slack: ignored a 👍 by %s on an approval request in %s: not an allowed user", ev.User, ev.Item.Channel)
		return
	}
	p, ok := b.state.takeApproval(ev.Item.Channel, ev.Item.TS)
	if !ok {
		return
	}
	sid := b.state.current(p.Session)
	nsid, msgID, err := b.bus.DeliverApproval(sid, p.Request, "approved: "+p.Text, user.Label)
	if err != nil {
		log.Warnf("slack: %s's approval of request %s not delivered: %v", user.ID, p.Request, err)
		return
	}
	// The agent answers the approval where the request was.
	r := replyRecord{ID: msgID, Channel: p.Channel, ThreadTS: p.Thread, Session: nsid, Link: p.Link, TopLevel: p.Thread == ""}
	if strings.HasPrefix(p.Channel, "D") {
		r.DMUser = user.ID
	}
	b.state.record(r)
	log.Infof("slack: %s approved request %s of %s", user.ID, p.Request, b.bus.Address(nsid))
	b.enqueue(func(ctx context.Context) error {
		return b.api.addReaction(ctx, b.cfg.BotToken, p.Channel, p.TS, reactionApproved)
	})
}

// addApproval remembers p (stamped now), dropping expired requests and the
// oldest past maxApprovals.
func (st *state) addApproval(p pendingApproval) {
	st.mu.Lock()
	defer st.mu.Unlock()
	p.Created = st.now()
	st.pruneApprovalsLocked()
	st.approvals = append(st.approvals, p)
	if extra := len(st.approvals) - maxApprovals; extra > 0 {
		st.approvals = st.approvals[:copy(st.approvals, st.approvals[extra:])]
	}
	st.dirty = true
}

// approvalIndexLocked finds the unexpired request posted at ts in channel.
// The caller holds st.mu.
func (st *state) approvalIndexLocked(channel, ts string) (int, bool) {
	cutoff := st.now().Add(-approvalTTL)
	for i := len(st.approvals) - 1; i >= 0; i-- {
		if p := st.approvals[i]; p.Channel == channel && p.TS == ts {
			return i, p.Created.After(cutoff)
		}
	}
	return 0, false
}

// approval returns the unexpired request posted at ts in channel.
func (st *state) approval(channel, ts string) (pendingApproval, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	i, ok := st.approvalIndexLocked(channel, ts)
	if !ok {
		return pendingApproval{}, false
	}
	return st.approvals[i], true
}

// takeApproval marks the unexpired, not yet approved request posted at ts in
// channel approved and returns it; false when there is none.
func (st *state) takeApproval(channel, ts string) (pendingApproval, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	i, ok := st.approvalIndexLocked(channel, ts)
	if !ok || st.approvals[i].Done {
		return pendingApproval{}, false
	}
	st.approvals[i].Done = true
	st.dirty = true
	return st.approvals[i], true
}

// pruneApprovals drops expired approval requests.
func (st *state) pruneApprovals() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.pruneApprovalsLocked()
}

// pruneApprovalsLocked drops expired approval requests. The caller holds
// st.mu.
func (st *state) pruneApprovalsLocked() {
	cutoff := st.now().Add(-approvalTTL)
	kept := st.approvals[:0]
	for _, p := range st.approvals {
		if p.Created.After(cutoff) {
			kept = append(kept, p)
		}
	}
	if len(kept) != len(st.approvals) {
		st.dirty = true
	}
	st.approvals = kept
}
