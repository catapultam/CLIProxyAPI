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
	// maxEarlyReceipts caps the receipts kept for ids not recorded yet.
	maxEarlyReceipts = 256
)

var _ agentbus.Receipts = (*Bridge)(nil)

// receiptRank orders receipt reactions; anything else is 0.
func receiptRank(reaction string) int {
	switch reaction {
	case reactionQueued, reactionCommand:
		return 1
	case reactionReceived:
		return 2
	case reactionRead:
		return 3
	}
	return 0
}

// Received marks messages the agent's mod claimed through /wait. It
// implements agentbus.Receipts.
func (b *Bridge) Received(ids []string) { b.advanceReceipts(ids, reactionReceived) }

// Read marks messages the model got. It implements agentbus.Receipts.
func (b *Bridge) Read(ids []string) { b.advanceReceipts(ids, reactionRead) }

// advanceReceipts queues the reaction changes for ids moving to reaction:
// the new reaction is added first, then the one it replaces is removed, so
// the message always shows one.
func (b *Bridge) advanceReceipts(ids []string, reaction string) {
	for _, c := range b.state.advanceReceipts(ids, reaction) {
		b.enqueue(func(ctx context.Context) error {
			if err := b.api.addReaction(ctx, b.cfg.BotToken, c.channel, c.ts, c.add); err != nil {
				return err
			}
			if c.remove == "" {
				return nil
			}
			return b.api.removeReaction(ctx, b.cfg.BotToken, c.channel, c.ts, c.remove)
		})
	}
}
