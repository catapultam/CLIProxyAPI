package slackbridge

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

// Receipt reactions on a user's message, in order. Each replaces the one
// before it, so a message shows one at a time and never moves back.
const (
	// reactionQueued: queued on the bus.
	reactionQueued = "inbox_tray"
	// reactionCommand: a command queued on the bus (the queued state of a
	// command message).
	reactionCommand = "gear"
	// reactionReceived: the agent's mod claimed it through /wait.
	reactionReceived = "envelope_with_arrow"
	// reactionRead: the model got it.
	reactionRead = "eyes"
	// receiptDismissed is not a reaction: the agent dismissed the message
	// (it wasn't meant for it), so it shows none, for good.
	receiptDismissed = "dismissed"
	// maxEarlyReceipts caps the receipts kept for ids not recorded yet.
	maxEarlyReceipts = 256
)

// receiptReactions are every reaction a receipt can show.
var receiptReactions = []string{reactionQueued, reactionCommand, reactionReceived, reactionRead}

var (
	_ agentbus.Receipts  = (*Bridge)(nil)
	_ agentbus.Dismisser = (*Bridge)(nil)
)

// receiptRank orders receipt reactions; anything else is 0. Dismissed is
// last, so no later receipt replaces it.
func receiptRank(reaction string) int {
	switch reaction {
	case reactionQueued, reactionCommand:
		return 1
	case reactionReceived:
		return 2
	case reactionRead:
		return 3
	case receiptDismissed:
		return 4
	}
	return 0
}

// Received marks messages the agent's mod claimed through /wait. It
// implements agentbus.Receipts.
func (b *Bridge) Received(ids []string) { b.advanceReceipts(ids, reactionReceived) }

// Read marks messages the model got. It implements agentbus.Receipts.
func (b *Bridge) Read(ids []string) { b.advanceReceipts(ids, reactionRead) }

// Dismissed clears and keeps clear the receipts of the ids delivered to
// sessionID (or a session it took over, or that took it over). It
// implements agentbus.Dismisser and returns how many it dismissed.
func (b *Bridge) Dismissed(sessionID string, ids []string) int {
	dismissed := b.state.dismissReceipts(sessionID, ids)
	for _, id := range dismissed {
		b.syncReceipt(id)
	}
	return len(dismissed)
}

// advanceReceipts moves ids to reaction and queues a sync of each one that
// moved.
func (b *Bridge) advanceReceipts(ids []string, reaction string) {
	for _, id := range b.state.advanceReceipts(ids, reaction) {
		b.syncReceipt(id)
	}
}

// syncReceipt queues a job that makes message id's reactions match its
// receipt as the state has it when the job runs, not when it was queued, so
// jobs that run late or out of order (a retry, a full queue) never leave a
// stale reaction: the new reaction is added first, then the one shown
// before is removed, so the message always shows one. A dismissed message
// loses every receipt reaction.
func (b *Bridge) syncReceipt(id string) {
	b.enqueue(func(ctx context.Context) error {
		r, ok := b.state.receiptOf(id)
		if !ok || r.TS == "" || r.Receipt == r.Shown {
			return nil
		}
		if r.Receipt == receiptDismissed {
			for _, name := range receiptReactions {
				if err := b.api.removeReaction(ctx, b.cfg.BotToken, r.Channel, r.TS, name); err != nil {
					return err
				}
			}
			b.state.setShown(id, "")
			return nil
		}
		if err := b.api.addReaction(ctx, b.cfg.BotToken, r.Channel, r.TS, r.Receipt); err != nil {
			return err
		}
		if r.Shown != "" {
			if err := b.api.removeReaction(ctx, b.cfg.BotToken, r.Channel, r.TS, r.Shown); err != nil {
				return err
			}
		}
		b.state.setShown(id, r.Receipt)
		return nil
	})
}

// advanceReceipts moves each recorded message in ids to receipt reaction
// when that is further along than its current one, and returns the ids that
// moved. A receipt never moves back, and never off dismissed. An id with no
// record yet is kept in early for record; an expired one, or one recorded
// before receipts (no TS), is ignored.
func (st *state) advanceReceipts(ids []string, reaction string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	rank := receiptRank(reaction)
	var out []string
	for _, id := range ids {
		if id == "" {
			continue
		}
		i, ok := st.replyIndexLocked(id)
		if !ok {
			if !st.knownLocked(id) {
				st.noteEarlyLocked(id, reaction)
			}
			continue
		}
		r := &st.replies[i]
		if r.TS == "" || rank <= receiptRank(r.Receipt) {
			continue
		}
		r.Receipt = reaction
		out = append(out, id)
	}
	if len(out) > 0 {
		st.dirty = true
	}
	return out
}

// dismissReceipts marks the unexpired records of ids delivered to sid (as
// one session across handoffs) dismissed, and returns those ids.
func (st *state) dismissReceipts(sid string, ids []string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for _, id := range ids {
		i, ok := st.replyIndexLocked(id)
		if !ok || !st.sameSessionLocked(st.replies[i].Session, sid) {
			continue
		}
		st.replies[i].Receipt = receiptDismissed
		out = append(out, id)
	}
	if len(out) > 0 {
		st.dirty = true
	}
	return out
}

// receiptOf returns message id's record (even an expired one, so a late
// sync still finishes).
func (st *state) receiptOf(id string) (replyRecord, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := len(st.replies) - 1; i >= 0; i-- {
		if st.replies[i].ID == id {
			return st.replies[i], true
		}
	}
	return replyRecord{}, false
}

// setShown records that message id now shows shown (empty for none). A
// receipt that moved on meanwhile has its own sync queued.
func (st *state) setShown(id, shown string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := len(st.replies) - 1; i >= 0; i-- {
		if st.replies[i].ID == id {
			st.replies[i].Shown = shown
			st.dirty = true
			return
		}
	}
}
