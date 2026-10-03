package slackbridge

import (
	"context"
	"net/url"
	"time"

	log "github.com/sirupsen/logrus"
)

// Who is in a conversation decides whether guests read there (shell and
// image output is refused) and whether it is owner-only (full headers with
// address and machine). It comes from Slack's member list, never from who
// has written.
const (
	// memberCacheTTL is how long a conversation's member list is reused.
	// member_joined_channel and member_left_channel events drop it sooner
	// when the app is subscribed to them.
	memberCacheTTL = 5 * time.Minute
	// maxMemberPages bounds one conversations.members lookup.
	maxMemberPages = 50
)

// memberList is a cached conversations.members answer.
type memberList struct {
	ids []string
	at  time.Time
}

// conversationMembers lists the user IDs in channel (bots included),
// following the pagination cursor.
func (a *api) conversationMembers(ctx context.Context, token, channel string) ([]string, error) {
	var ids []string
	cursor := ""
	for page := 0; page < maxMemberPages; page++ {
		params := url.Values{"channel": {channel}, "limit": {"200"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		body, err := a.call(ctx, token, "conversations.members", params)
		if err != nil {
			return nil, err
		}
		for _, id := range body.Get("members").Array() {
			ids = append(ids, id.String())
		}
		if cursor = body.Get("response_metadata.next_cursor").String(); cursor == "" {
			break
		}
	}
	return ids, nil
}

// members returns channel's members other than the bot, from the cache when
// it is fresh. It calls Slack, so it runs in jobs or callers' goroutines,
// never in the socket's event handler; no lock is held across the call. A
// failed lookup isn't cached.
func (b *Bridge) members(ctx context.Context, channel string) ([]string, error) {
	now := b.state.now()
	b.membersMu.Lock()
	cached, ok := b.memberLists[channel]
	b.membersMu.Unlock()
	if ok && now.Sub(cached.at) < memberCacheTTL {
		return cached.ids, nil
	}
	all, err := b.api.conversationMembers(ctx, b.cfg.BotToken, channel)
	if err != nil {
		log.Infof("slack: members of %s: lookup failed: %v", channel, err)
		return nil, err
	}
	ids := make([]string, 0, len(all))
	for _, id := range all {
		if id != "" && id != b.botUserID {
			ids = append(ids, id)
		}
	}
	b.membersMu.Lock()
	if b.memberLists == nil {
		b.memberLists = map[string]memberList{}
	}
	b.memberLists[channel] = memberList{ids: ids, at: now}
	b.membersMu.Unlock()
	return ids, nil
}

// forgetMembers drops channel's cached member list (someone joined or left).
func (b *Bridge) forgetMembers(channel string) {
	b.membersMu.Lock()
	defer b.membersMu.Unlock()
	delete(b.memberLists, channel)
}

// hasGuests reports whether someone in channel isn't an allowed user. A
// failed lookup counts as yes (fail closed).
func (b *Bridge) hasGuests(ctx context.Context, channel string) bool {
	ids, err := b.members(ctx, channel)
	if err != nil {
		return true
	}
	for _, id := range ids {
		if _, allowed := b.state.user(id); !allowed {
			return true
		}
	}
	return false
}

// ownerOnlyChannel reports whether only owners read conversation channel:
// the main channel, or a conversation whose members (besides the bot) are
// all config owners. A failed lookup counts as no (fail closed).
func (b *Bridge) ownerOnlyChannel(ctx context.Context, channel string) bool {
	if channel == "" {
		return false
	}
	if channel == b.channelID {
		return true
	}
	ids, err := b.members(ctx, channel)
	if err != nil {
		return false
	}
	for _, id := range ids {
		if !b.IsOwner(id) {
			return false
		}
	}
	return true
}
