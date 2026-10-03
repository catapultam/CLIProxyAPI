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
	// reactionWorking: a turn handling it is still running, 15s after it
	// started.
	reactionWorking = "hourglass_flowing_sand"
	// reactionRead: the model got it.
	reactionRead = "eyes"
	// reactionDone: the agent believes it fully answered the message. Final,
	// like dismissed: it never moves back, but a later dismiss still clears
	// it, and a later done still replaces a dismiss.
	reactionDone = "white_check_mark"
	// receiptDismissed is not a reaction: the agent dismissed the message
	// (it wasn't meant for it), so it shows none, for good (unless a later
	// done replaces it).
	receiptDismissed = "dismissed"
	// maxEarlyReceipts caps the receipts kept for ids not recorded yet.
	maxEarlyReceipts = 256
)

// receiptReactions are every reaction a receipt can show.
var receiptReactions = []string{reactionQueued, reactionCommand, reactionReceived, reactionWorking, reactionRead, reactionDone}

var (
	_ agentbus.Receipts  = (*Bridge)(nil)
	_ agentbus.Dismisser = (*Bridge)(nil)
	_ agentbus.Doner     = (*Bridge)(nil)
	_ agentbus.Worker    = (*Bridge)(nil)
)

// receiptRank orders receipt reactions; anything else is 0. Done and
// dismissed are above every reaction advanceReceipts sets (received,
// working, read), so neither moves back through the normal flow; between
// themselves, Dismissed and Done each overwrite unconditionally, so the
// later of the two always wins regardless of rank.
func receiptRank(reaction string) int {
	switch reaction {
	case reactionQueued, reactionCommand:
		return 1
	case reactionReceived:
		return 2
	case reactionWorking:
		return 3
	case reactionRead:
		return 4
	case reactionDone:
		return 5
	case receiptDismissed:
		return 6
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

// Done marks the ids delivered to sessionID (or a session it took over)
// done, replacing any earlier receipt, including dismissed. It implements
// agentbus.Doner and returns how many it marked.
func (b *Bridge) Done(sessionID string, ids []string) int {
	done := b.state.doneReceipts(sessionID, ids)
	for _, id := range done {
		b.syncReceipt(id)
	}
	return len(done)
}

// Working marks the ids delivered to sessionID (or a session it took over)
// working, when that is further along than their current receipt. It
// implements agentbus.Worker and returns how many it marked.
func (b *Bridge) Working(sessionID string, ids []string) int {
	working := b.state.workReceipts(sessionID, ids)
	for _, id := range working {
		b.syncReceipt(id)
	}
	return len(working)
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
// loses every receipt reaction. A broadcast's message shows its group's
// receipt (groupReceipt).
func (b *Bridge) syncReceipt(id string) {
	b.enqueue(func(ctx context.Context) error {
		r, ok := b.state.receiptOf(id)
		if !ok || r.TS == "" {
			return nil
		}
		if r.Group != "" {
			r.Receipt, r.Shown = b.state.groupReceipt(r.Group)
		}
		if r.Receipt == r.Shown {
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
// one session across handoffs) dismissed, and returns those ids. It
// overwrites any state, including done: a later dismiss always wins.
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

// doneReceipts marks the unexpired records of ids delivered to sid (as one
// session across handoffs) done, and returns those ids. It overwrites any
// state, including dismissed: a later done always wins.
func (st *state) doneReceipts(sid string, ids []string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for _, id := range ids {
		i, ok := st.replyIndexLocked(id)
		if !ok || !st.sameSessionLocked(st.replies[i].Session, sid) {
			continue
		}
		st.replies[i].Receipt = reactionDone
		out = append(out, id)
	}
	if len(out) > 0 {
		st.dirty = true
	}
	return out
}

// workReceipts marks the unexpired records of ids delivered to sid (as one
// session across handoffs) working, when that is further along than their
// current receipt, and returns those ids. Unlike dismissReceipts and
// doneReceipts, this never moves a receipt back: it is part of the normal
// advancing flow (queued/command -> received -> working -> read), not a
// final action either of those can override.
func (st *state) workReceipts(sid string, ids []string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	rank := receiptRank(reactionWorking)
	var out []string
	for _, id := range ids {
		i, ok := st.replyIndexLocked(id)
		if !ok || !st.sameSessionLocked(st.replies[i].Session, sid) {
			continue
		}
		if st.replies[i].TS == "" || rank <= receiptRank(st.replies[i].Receipt) {
			continue
		}
		st.replies[i].Receipt = reactionWorking
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

// setShown records that message id now shows shown (empty for none), on
// every record of its broadcast group when it has one. A receipt that moved
// on meanwhile has its own sync queued.
func (st *state) setShown(id, shown string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := len(st.replies) - 1; i >= 0; i-- {
		if st.replies[i].ID != id {
			continue
		}
		group := st.replies[i].Group
		if group == "" {
			st.replies[i].Shown = shown
		} else {
			for j := range st.replies {
				if st.replies[j].Group == group {
					st.replies[j].Shown = shown
				}
			}
		}
		st.dirty = true
		return
	}
}

// groupReceipt is the receipt a broadcast's message shows (want) and the one
// it shows now (shown): ✅ once every recipient has marked it done or
// dismissed it, with at least one done; no reaction once every recipient
// dismissed it with none done; ⏳ while any recipient is working and none is
// done yet; 👀 once all read it, where a dismissal counts as read; 📨 once
// any recipient received it; until then the queued reaction (⚙️ for a
// command).
func (st *state) groupReceipt(group string) (want, shown string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	queued, allRead, anyReceived, anyWorking, anyDone, allDoneOrDismissed, first := reactionQueued, true, false, false, false, true, true
	for _, r := range st.replies {
		if r.Group != group {
			continue
		}
		if first {
			shown, first = r.Shown, false
		}
		switch r.Receipt {
		case reactionDone:
			anyReceived, anyDone = true, true
		case reactionRead, receiptDismissed:
			anyReceived = true
		case reactionWorking:
			anyReceived, allRead, anyWorking = true, false, true
		case reactionReceived:
			anyReceived, allRead = true, false
		default:
			allRead = false
			if r.Receipt == reactionCommand {
				queued = reactionCommand
			}
		}
		if r.Receipt != reactionDone && r.Receipt != receiptDismissed {
			allDoneOrDismissed = false
		}
	}
	switch {
	case first:
		return shown, shown
	case allDoneOrDismissed && anyDone:
		return reactionDone, shown
	case allDoneOrDismissed:
		// Every recipient dismissed it, and none marked it done: no reaction.
		return receiptDismissed, shown
	case anyWorking && !anyDone:
		return reactionWorking, shown
	case allRead:
		return reactionRead, shown
	case anyReceived:
		return reactionReceived, shown
	}
	return queued, shown
}
