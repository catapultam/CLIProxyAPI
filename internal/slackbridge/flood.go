package slackbridge

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

// Guest flood control: each linked conversation gets a token bucket for
// its guests' messages.
const (
	// guestPerMinute is how many guest messages a minute a conversation
	// refills; guestBurst is how many it can send at once.
	guestPerMinute = 10
	guestBurst     = 10
	// slowNoticeEvery is how often an over-limit conversation is told.
	slowNoticeEvery = time.Minute
	// maxGuestBuckets caps the buckets kept; full ones are dropped first.
	maxGuestBuckets = 1000
	// slowingDown is the one reply an over-limit conversation gets a minute.
	slowingDown = "Slowing down: messages are being dropped for a minute."
)

// guestBucket is one conversation's token bucket.
type guestBucket struct {
	tokens  float64
	last    time.Time
	noticed time.Time
}

// allowGuest takes a token from channel's bucket. When there is none, it
// reports whether the conversation should be told (once a slowNoticeEvery).
// It uses the state's clock.
func (b *Bridge) allowGuest(channel string) (ok, notify bool) {
	now := b.state.now()
	b.floodMu.Lock()
	defer b.floodMu.Unlock()
	if b.floods == nil {
		b.floods = map[string]*guestBucket{}
	}
	bk := b.floods[channel]
	if bk == nil {
		if len(b.floods) >= maxGuestBuckets {
			b.dropFullBucketsLocked(now)
		}
		bk = &guestBucket{tokens: guestBurst, last: now}
		b.floods[channel] = bk
	}
	if elapsed := now.Sub(bk.last); elapsed > 0 {
		bk.tokens = min(guestBurst, bk.tokens+elapsed.Minutes()*guestPerMinute)
	}
	bk.last = now
	if bk.tokens >= 1 {
		bk.tokens--
		return true, false
	}
	if bk.noticed.IsZero() || now.Sub(bk.noticed) >= slowNoticeEvery {
		bk.noticed = now
		return false, true
	}
	return false, false
}

// dropFullBucketsLocked forgets buckets that have refilled. The caller holds
// floodMu.
func (b *Bridge) dropFullBucketsLocked(now time.Time) {
	for channel, bk := range b.floods {
		if bk.tokens+now.Sub(bk.last).Minutes()*guestPerMinute >= guestBurst {
			delete(b.floods, channel)
		}
	}
}

// guestFlooded applies the rate limit to a guest's message in ev's
// conversation and reports whether it must be dropped. The first drop in a
// minute gets the slowing-down reply; later ones are dropped silently.
func (b *Bridge) guestFlooded(ev messageEvent) bool {
	ok, notify := b.allowGuest(ev.Channel)
	if ok {
		return false
	}
	if notify {
		log.Infof("slack: guests in %s are over the rate limit; dropping their messages for a minute", ev.Channel)
		b.enqueue(func(ctx context.Context) error {
			_, err := b.api.postMessage(ctx, b.cfg.BotToken, ev.Channel, slowingDown, "")
			return err
		})
	}
	return true
}
