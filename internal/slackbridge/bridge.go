package slackbridge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
)

const (
	jobQueueSize     = 256
	commandQueueSize = 64
	maxBackoff       = 5 * time.Minute
	seenEvents       = 512
)

// Config is what the bridge needs from config.yaml plus where to keep state.
type Config struct {
	BotToken      string
	AppToken      string
	Channel       string
	AllowedEmails []string
	// Home is where sessions' threads open: "channel" (the default) or "dm",
	// the first allowed user's DM with the bot. With "dm", Channel is
	// optional.
	Home      string
	StatePath string
	// CommandsDir holds the agent command registry (<name>.yaml files).
	// Empty means no registry: only harness commands and !commands work.
	CommandsDir string
	// APIBase overrides https://slack.com/api/ (tests).
	APIBase string
}

// parseHome reads a home setting, case-insensitively: homeDM or homeChannel
// (also for an empty value). known is false for anything else, which falls
// back to homeChannel.
func parseHome(s string) (home string, known bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case homeDM:
		return homeDM, true
	case homeChannel, "":
		return homeChannel, true
	}
	return homeChannel, false
}

func (c Config) complete() bool {
	if strings.TrimSpace(c.BotToken) == "" || strings.TrimSpace(c.AppToken) == "" || len(c.AllowedEmails) == 0 {
		return false
	}
	home, _ := parseHome(c.Home)
	return home == homeDM || strings.TrimSpace(c.Channel) != ""
}

type job func(ctx context.Context) error

var (
	_ agentbus.Bridge       = (*Bridge)(nil)
	_ agentbus.ImagePoster  = (*Bridge)(nil)
	_ agentbus.OwnerChecker = (*Bridge)(nil)
	_ agentbus.OwnerLister  = (*Bridge)(nil)
)

// Bridge links the agentbus to one Slack channel. It implements
// agentbus.Bridge.
type Bridge struct {
	cfg    Config
	bus    *agentbus.Store
	api    *api
	dialer *websocket.Dialer
	state  *state
	// cmdRegistry is the agent command registry; nil when CommandsDir is empty.
	cmdRegistry *registry
	jobs        chan job
	// commands holds allow jobs and command replies. runJobs drains it
	// first, and enqueue's drop-oldest never touches it.
	commands   chan job
	backoff    func(attempt int) time.Duration
	retryDelay time.Duration

	// home is the config's home (homeChannel or homeDM), set by New.
	home string

	// Set by resolve before the bridge is attached or the socket runs.
	// channelID is empty when no channel is configured (home dm only).
	// homeChannelID is where new sessions' threads open: channelID, or the
	// first owner's DM with the bot (ownerID's).
	channelID     string
	botUserID     string
	homeChannelID string
	ownerID       string

	seenMu   sync.Mutex
	seen     map[string]bool
	seenRing []string

	// cmdSeq counts allow/remove commands per target user, so a queued allow
	// applies only if no later command for that user came in. cmdMu also
	// makes checking the count and changing the allowlist one step.
	cmdMu  sync.Mutex
	cmdSeq map[string]uint64

	// opening holds one gate per session whose thread is being picked, so
	// the job goroutine and concurrent image uploads open a session's thread
	// once. openMu guards the map; neither is held while calling the Store.
	openMu  sync.Mutex
	opening map[string]*openGate

	// dmChannels caches user ID -> the bot's DM channel with them, learned
	// from conversations.open or an inbound DM. dmMu guards it and is never
	// held across a Slack call or a Store call.
	dmMu       sync.Mutex
	dmChannels map[string]string

	// guestNames caches guest user ID -> their sanitized Slack display name.
	// guestQueued counts each guest's messages waiting on the job queue for
	// a users.info lookup; while any wait, later ones queue behind them, so
	// a guest's messages stay in order. guestMu guards both and is never
	// held across a Slack call or a Store call.
	guestMu     sync.Mutex
	guestNames  map[string]string
	guestQueued map[string]int

	// botName is the bot's display name from Slack (see BotName);
	// botNameMu guards it and is never held across a Slack call.
	botNameMu sync.Mutex
	botName   string

	// memberLists caches conversations' member lists (see members);
	// membersMu guards it and is never held across a Slack call.
	membersMu   sync.Mutex
	memberLists map[string]memberList

	// floods holds each linked conversation's guest rate-limit bucket.
	// floodMu guards it and is never held across a Slack or Store call.
	floodMu sync.Mutex
	floods  map[string]*guestBucket

	cancel context.CancelFunc
	done   chan struct{}
	jobsWG sync.WaitGroup
}

// New builds a bridge, or returns nil, nil when cfg is incomplete (Slack off).
func New(cfg Config, bus *agentbus.Store) (*Bridge, error) {
	home, known := parseHome(cfg.Home)
	if !known {
		log.Warnf("slack: unknown home %q (want channel or dm); using channel", strings.TrimSpace(cfg.Home))
	}
	if !cfg.complete() {
		return nil, nil
	}
	st, errLoad := loadState(cfg.StatePath)
	if errLoad != nil {
		log.Warnf("slack: load state (starting empty): %v", errLoad)
	}
	b := &Bridge{
		cfg:         cfg,
		home:        home,
		bus:         bus,
		api:         newAPI(cfg.APIBase),
		dialer:      websocket.DefaultDialer,
		state:       st,
		cmdRegistry: newRegistry(cfg.CommandsDir),
		jobs:        make(chan job, jobQueueSize),
		commands:    make(chan job, commandQueueSize),
		backoff:     defaultBackoff,
		retryDelay:  2 * time.Second,
		seen:        map[string]bool{},
		cmdSeq:      map[string]uint64{},
		opening:     map[string]*openGate{},
		dmChannels:  map[string]string{},
		guestNames:  map[string]string{},
		guestQueued: map[string]int{},
	}
	b.refreshLinks()
	return b, nil
}

func defaultBackoff(attempt int) time.Duration {
	d := time.Second << min(attempt, 9)
	return min(d, maxBackoff)
}

func logSaveError(err error) { log.Warnf("slack: save state: %v", err) }

// Users lists the allowed users' labels.
func (b *Bridge) Users() []string { return b.state.labels() }

// Owners lists the labels of the allowed users seeded from config. It
// implements agentbus.OwnerLister.
func (b *Bridge) Owners() []string { return b.state.ownerLabels() }

// IsOwner reports whether userID is an allowed user seeded from config. It
// implements agentbus.OwnerChecker.
func (b *Bridge) IsOwner(userID string) bool {
	u, ok := b.state.user(userID)
	return ok && u.config
}

// errDMUserGone is a DM whose label is no longer an allowed user's.
var errDMUserGone = errors.New("not an allowed user")

// Post queues a session's message for its thread, or for an allowed user's
// DM when o.DM is set. It never blocks.
func (b *Bridge) Post(o agentbus.Outbound) {
	b.logCommandOutcome(o)
	b.enqueue(func(ctx context.Context) error { return b.postOutbound(ctx, o) })
}

// logCommandOutcome logs a command's outcome when o is the agentbus mod's
// report on it: an answer to a command message that starts with ✅ or ❌.
// Only the outcome, the command's name and the machine are logged, never
// the report's text or the command's output.
func (b *Bridge) logCommandOutcome(o agentbus.Outbound) {
	if o.ReplyTo == "" || o.DM != "" {
		return
	}
	r, ok := b.state.replyTarget(o.ReplyTo, o.SessionID)
	if !ok || r.Command == "" {
		return
	}
	outcome := ""
	switch {
	case strings.HasPrefix(o.Body, "✅"):
		outcome = "✅ ran"
	case strings.HasPrefix(o.Body, "❌"):
		outcome = "❌ failed"
	default:
		return
	}
	log.Infof("slack: %s !%s on %s", outcome, r.Command, o.Machine)
}

func (b *Bridge) postOutbound(ctx context.Context, o agentbus.Outbound) error {
	text := withMentions(o.Body, b.state.mentionIDs())
	t, opened, err := b.threadFor(ctx, o, text)
	if errors.Is(err, errDMUserGone) {
		log.Warnf("slack: dropped a DM from %s to @%s: not an allowed user", o.Address, o.DM)
		return nil
	}
	if errors.Is(err, errReplyUnreachable) {
		b.noticeUnreachable(o)
		return nil
	}
	if err != nil || opened {
		return err
	}
	ts, err := b.api.postMessage(ctx, b.cfg.BotToken, t.channel, text, t.threadTS)
	if err == nil && t.threadTS == "" {
		b.state.linkDM(t.channel, ts, o.SessionID, true)
	}
	return err
}

// postTarget is where a post goes: a conversation and a thread in it. An
// empty threadTS is the conversation's top level, where only DM posts go.
type postTarget struct{ channel, threadTS string }

// threadFor picks where a session's post goes:
//   - with o.DM, the top level of that allowed user's DM (errDMUserGone when
//     the label isn't an allowed user's any more);
//   - else the conversation and thread o.ReplyTo came from, when that message
//     was delivered to this session (a top-level DM is answered at the DM's
//     top level);
//   - else the session's home thread, wherever it is (threads stay put when
//     the home setting changes). When it has none, threadFor opens one by
//     posting the session header, followed by text when text isn't empty,
//     at the top level of its home conversation (homeChannelFor).
//
// A session's first top-level post in a DM also starts with its header (see
// dmTop). opened reports that text already went out with a header, so the
// caller doesn't post it again. Text and image posts both choose here. Only
// one caller per session checks and opens at a time, so a session never gets
// two header posts in one place.
func (b *Bridge) threadFor(ctx context.Context, o agentbus.Outbound, text string) (t postTarget, opened bool, err error) {
	if o.DM != "" {
		channel, errDM := b.dmChannelFor(ctx, o)
		if errDM != nil {
			return postTarget{}, false, errDM
		}
		return b.dmTop(ctx, o, channel, text)
	}
	t, ok, errGone := b.repliedThread(o)
	if errGone != nil {
		return postTarget{}, false, errGone
	}
	if ok {
		if t.threadTS == "" {
			return b.dmTop(ctx, o, t.channel, text)
		}
		return t, false, nil
	}
	unlock, errLock := b.lockOpening(ctx, o.SessionID)
	if errLock != nil {
		return postTarget{}, false, errLock
	}
	defer unlock()
	if t, ok := b.ownThread(o.SessionID); ok {
		return t, false, nil
	}
	channel, err := b.homeChannelFor(ctx, o.SessionID)
	if err != nil {
		return postTarget{}, false, err
	}
	first := sessionHeader(o)
	if text != "" {
		first += "\n" + text
	}
	ts, err := b.api.postMessage(ctx, b.cfg.BotToken, channel, first, "")
	if err != nil {
		return postTarget{}, false, err
	}
	b.state.moveThread(o.SessionID, channel, ts, "")
	b.noteHomeHeader(channel, ts, o.SessionID)
	return postTarget{channel: channel, threadTS: ts}, true, nil
}

// noteHomeHeader records a home thread's header post in a DM as the
// session's own top-level post there, so a later DM post from the session
// knows its header is the newest there (dmHeaded).
func (b *Bridge) noteHomeHeader(channel, ts, sid string) {
	if channel != "" && channel != b.channelID {
		b.state.linkDM(channel, ts, sid, true)
	}
}

// errReplyUnreachable is an answer (reply_to) to a message from a private
// or linked conversation the session can't post in any more: the DM's user
// was removed, or the conversation was unlinked, relinked or pruned.
var errReplyUnreachable = errors.New("conversation no longer reachable")

// unreachableNotice is what the agent is told when its answer was dropped.
const unreachableNotice = "That conversation is no longer reachable; your message was not posted."

// noticeUnreachable tells o's session its answer was dropped. A session that
// has left the bus is skipped.
func (b *Bridge) noticeUnreachable(o agentbus.Outbound) {
	if _, _, err := b.bus.DeliverNotice(o.SessionID, unreachableNotice); err != nil && !errors.Is(err, agentbus.ErrUnknownTarget) {
		log.Warnf("slack: unreachable notice for %s not delivered: %v", o.Address, err)
	}
}

// ownThread returns session sid's home thread. A thread with no channel
// (from an old state file, when no channel is configured to place it in)
// counts as none.
func (b *Bridge) ownThread(sid string) (postTarget, bool) {
	ref, ok := b.state.homeThread(sid)
	if !ok {
		return postTarget{}, false
	}
	if ref.Channel == "" {
		ref.Channel = b.channelID
	}
	return postTarget{channel: ref.Channel, threadTS: ref.TS}, ref.Channel != ""
}

// homeOf is where session sid's home thread belongs: where an owner moved
// it, else the config's home.
func (b *Bridge) homeOf(sid string) string {
	if home := b.state.home(sid); home != "" {
		return home
	}
	return b.home
}

// homeChannelFor is the conversation a new home thread for session sid
// opens in: the channel, or the first owner's DM, by homeOf. A session moved
// to the channel opens in the home conversation when no channel is
// configured any more.
func (b *Bridge) homeChannelFor(ctx context.Context, sid string) (string, error) {
	switch b.homeOf(sid) {
	case homeChannel:
		if b.channelID != "" {
			return b.channelID, nil
		}
	case homeDM:
		// The DM of the owner who moved it there, while they are an owner.
		owner := b.state.homeOwner(sid)
		if owner == "" || !b.IsOwner(owner) {
			owner = b.ownerID
		}
		return b.dmChannel(ctx, owner)
	}
	return b.homeChannelID, nil
}

// dmTop prepares a top-level post by o's session in DM channel. The session's
// first post there starts with its header line: dmTop posts the header,
// followed by text when text isn't empty, links it to the session and
// reports opened. Otherwise it posts nothing, and the caller posts (and, for
// text, links) itself.
func (b *Bridge) dmTop(ctx context.Context, o agentbus.Outbound, channel, text string) (postTarget, bool, error) {
	t := postTarget{channel: channel}
	unlock, errLock := b.lockOpening(ctx, o.SessionID)
	if errLock != nil {
		return t, false, errLock
	}
	defer unlock()
	if b.state.dmHeaded(channel, o.SessionID) {
		return t, false, nil
	}
	first := b.headerFor(ctx, o, channel)
	if text != "" {
		first += "\n" + text
	}
	ts, err := b.api.postMessage(ctx, b.cfg.BotToken, channel, first, "")
	if err != nil {
		return t, false, err
	}
	b.state.linkDM(channel, ts, o.SessionID, true)
	return t, true, nil
}

// headerFor is the header line of o's session for a top-level post in
// conversation channel. The full header (name, address, machine) goes only
// where owners alone read it (ownerOnlyChannel: the main channel, or a
// conversation whose members are all owners). Anywhere else, or when the
// member lookup fails, it is the agent's name, or "an agent", so the setup
// isn't disclosed. It may call Slack, so callers run in jobs.
func (b *Bridge) headerFor(ctx context.Context, o agentbus.Outbound, channel string) string {
	if b.ownerOnlyChannel(ctx, channel) {
		return sessionHeader(o)
	}
	name := o.Name
	if name == "" {
		name = "an agent"
	}
	return "*" + escape(name) + "*"
}

// dmChannelFor finds the allowed user o.DM labels (errDMUserGone when there
// is none now), returns the bot's DM channel with them, and makes o's session
// the agent their plain DMs go to.
func (b *Bridge) dmChannelFor(ctx context.Context, o agentbus.Outbound) (string, error) {
	u, ok := b.state.userByLabel(o.DM)
	if !ok {
		return "", errDMUserGone
	}
	channel, err := b.dmChannel(ctx, u.ID)
	if err != nil {
		return "", err
	}
	b.state.setDMLast(u.ID, o.SessionID)
	return channel, nil
}

// dmChannel returns the bot's DM channel with userID, calling
// conversations.open only when it isn't cached yet.
func (b *Bridge) dmChannel(ctx context.Context, userID string) (string, error) {
	b.dmMu.Lock()
	channel, ok := b.dmChannels[userID]
	b.dmMu.Unlock()
	if ok {
		return channel, nil
	}
	channel, err := b.api.openDM(ctx, b.cfg.BotToken, userID)
	if err != nil {
		return "", err
	}
	b.rememberDM(userID, channel)
	return channel, nil
}

// rememberDM caches channel as the bot's DM with userID.
func (b *Bridge) rememberDM(userID, channel string) {
	b.dmMu.Lock()
	defer b.dmMu.Unlock()
	b.dmChannels[userID] = channel
}

// repliedThread returns where o.ReplyTo came from when that message was
// delivered to o's session. An id delivered to another session is logged
// (both addresses, never the body) and ignored. An answer to a message from
// the DM of a user who is no longer allowed, or from a conversation no
// longer linked to the session, is errReplyUnreachable: it is dropped, never
// posted in the home thread instead, where a private answer would leak. It
// runs without bridge locks held, since it may ask the Store.
func (b *Bridge) repliedThread(o agentbus.Outbound) (postTarget, bool, error) {
	if o.ReplyTo == "" {
		return postTarget{}, false, nil
	}
	if r, ok := b.state.replyTarget(o.ReplyTo, o.SessionID); ok {
		if r.DMUser != "" {
			if _, allowed := b.state.user(r.DMUser); !allowed {
				log.Warnf("slack: dropped %s's answer to message %s from the DM of %s, who is no longer allowed", o.Address, o.ReplyTo, r.DMUser)
				return postTarget{}, false, errReplyUnreachable
			}
		}
		// A guest's message or the link notice reached the session only
		// through the conversation's link; once that is gone, so is the
		// session's way in.
		if r.Link {
			if l, linked := b.conversationLink(r.Channel); !linked || !b.state.sameSession(l.Session, o.SessionID) {
				log.Warnf("slack: dropped %s's answer to message %s from conversation %s, which is no longer linked to it", o.Address, o.ReplyTo, r.Channel)
				return postTarget{}, false, errReplyUnreachable
			}
		}
		channel := r.Channel
		if channel == "" {
			// An old record from the main channel.
			channel = b.channelID
		}
		thread := r.ThreadTS
		if r.TopLevel {
			// Answer at the level the message was written at.
			thread = ""
		}
		return postTarget{channel: channel, threadTS: thread}, channel != "", nil
	}
	if owner, ok := b.state.replyOwner(o.ReplyTo); ok && owner != o.SessionID {
		other := b.bus.Address(owner)
		if other == "" {
			other = "a session that has left the bus"
		}
		log.Warnf("slack: %s answered message %s, which went to %s; posting in its own thread instead", o.Address, o.ReplyTo, other)
	}
	return postTarget{}, false, nil
}

// openGate is a per-session lock that a waiter can give up on. refs counts
// its holder and waiters, so the gate leaves the map with the last of them.
type openGate struct {
	held chan struct{}
	refs int
}

// lockOpening takes sid's gate, or returns ctx's error if ctx ends first.
// The returned func releases it.
func (b *Bridge) lockOpening(ctx context.Context, sid string) (func(), error) {
	b.openMu.Lock()
	g := b.opening[sid]
	if g == nil {
		g = &openGate{held: make(chan struct{}, 1)}
		b.opening[sid] = g
	}
	g.refs++
	b.openMu.Unlock()
	drop := func() {
		b.openMu.Lock()
		if g.refs--; g.refs == 0 {
			delete(b.opening, sid)
		}
		b.openMu.Unlock()
	}
	select {
	case g.held <- struct{}{}:
		return func() {
			<-g.held
			drop()
		}, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

// PostImage uploads an image where threadFor puts o (the DM o.DM names, the
// replied-to thread or the session's own), first posting the session header
// plus caption when that place needs one. It implements agentbus.ImagePoster
// and runs in the caller's goroutine, not on the job queue, so the caller
// gets Slack's answer. The returned error is only a short code (Slack's
// error code, user_not_allowed, conversation_unreachable, or
// request_failed); the details are logged
// without the upload URL or any token.
func (b *Bridge) PostImage(ctx context.Context, o agentbus.Outbound, filename string, data []byte) error {
	errPost := b.postImage(ctx, o, sanitizeFilename(filename), data)
	if errPost == nil {
		return nil
	}
	log.Warnf("slack: image from %s: %v", o.Address, errPost)
	if errors.Is(errPost, errDMUserGone) {
		return errors.New("user_not_allowed")
	}
	if errors.Is(errPost, errReplyUnreachable) {
		return errors.New("conversation_unreachable")
	}
	var apiErr *apiError
	if errors.As(errPost, &apiErr) && apiErr.code != "" {
		return errors.New(apiErr.code)
	}
	return errors.New("request_failed")
}

func (b *Bridge) postImage(ctx context.Context, o agentbus.Outbound, filename string, data []byte) error {
	caption := ""
	if strings.TrimSpace(o.Body) != "" {
		caption = withMentions(o.Body, b.state.mentionIDs())
	}
	t, opened, err := b.threadFor(ctx, o, caption)
	if err != nil {
		return err
	}
	if opened {
		// The caption went out with the header.
		caption = ""
	}
	uploadURL, fileID, err := b.api.getUploadURL(ctx, b.cfg.BotToken, filename, len(data))
	if err != nil {
		return err
	}
	if err = b.api.uploadFile(ctx, uploadURL, data); err != nil {
		return err
	}
	return b.api.completeUpload(ctx, b.cfg.BotToken, fileID, filename, t.channel, t.threadTS, caption)
}

// enqueue adds a job, dropping the oldest queued job when the queue is full.
func (b *Bridge) enqueue(j job) {
	for {
		select {
		case b.jobs <- j:
			return
		default:
		}
		select {
		case <-b.jobs:
			log.Warn("slack: outgoing queue full, dropped the oldest post")
		default:
		}
	}
}

// enqueueCommand adds a command job. It never drops a queued one: when even
// the command queue is full, the new job is dropped (a remove has already
// been applied by then; only its reply is lost).
func (b *Bridge) enqueueCommand(j job) {
	select {
	case b.commands <- j:
	default:
		log.Warn("slack: command queue full, dropped a command job")
	}
}

// runJobs runs queued jobs one at a time, command jobs first, retrying a
// failed job once.
func (b *Bridge) runJobs(ctx context.Context) {
	for {
		var j job
		select {
		case <-ctx.Done():
			return
		case j = <-b.commands:
		default:
			select {
			case <-ctx.Done():
				return
			case j = <-b.commands:
			case j = <-b.jobs:
			}
		}
		if !b.runJob(ctx, j) {
			return
		}
	}
}

// runJob runs j, retrying once on failure; it reports whether ctx is live.
func (b *Bridge) runJob(ctx context.Context, j job) bool {
	err := j(ctx)
	if err == nil || ctx.Err() != nil {
		return ctx.Err() == nil
	}
	log.Warnf("slack: %v (retrying once)", err)
	if !b.sleep(ctx, b.retryDelay) {
		return false
	}
	if errRetry := j(ctx); errRetry != nil && ctx.Err() == nil {
		log.Warnf("slack: %v (dropped)", errRetry)
	}
	return ctx.Err() == nil
}

// sleep waits d or until ctx ends; it reports whether ctx is still live.
func (b *Bridge) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// resolve finds the bot, the channel (when one is configured), the config
// users and the home conversation: the channel, or with home dm the first
// resolved owner's DM with the bot. Emails Slack doesn't know are skipped;
// other errors abort so Start retries.
func (b *Bridge) resolve(ctx context.Context) error {
	botID, err := b.api.authTest(ctx, b.cfg.BotToken)
	if err != nil {
		return err
	}
	channelID := ""
	if strings.TrimSpace(b.cfg.Channel) != "" {
		if channelID, err = b.api.findChannel(ctx, b.cfg.BotToken, b.cfg.Channel); err != nil {
			return err
		}
	}
	var users []allowedUser
	for _, email := range b.cfg.AllowedEmails {
		id, errLookup := b.api.lookupByEmail(ctx, b.cfg.BotToken, email)
		var apiErr *apiError
		if errors.As(errLookup, &apiErr) && apiErr.code == "users_not_found" {
			log.Warnf("slack: no Slack user with allowed email %s; skipped", email)
			continue
		}
		if errLookup != nil {
			return errLookup
		}
		users = append(users, allowedUser{ID: id, Label: emailLabel(email), config: true})
	}
	if len(users) == 0 {
		return errors.New("slack: none of allowed-emails matched a Slack user")
	}
	ownerID, homeChannelID := users[0].ID, channelID
	if b.home == homeDM {
		if homeChannelID, err = b.api.openDM(ctx, b.cfg.BotToken, ownerID); err != nil {
			return err
		}
		b.rememberDM(ownerID, homeChannelID)
	}
	b.botUserID, b.channelID, b.homeChannelID, b.ownerID = botID, channelID, homeChannelID, ownerID
	b.refreshBotName(ctx)
	b.state.seed(users)
	if channelID != "" {
		// Threads from an old state file are all in the channel.
		b.state.fillThreadChannels(channelID)
	}
	return nil
}

// homeDescription names the home conversation for the start-up log line.
func (b *Bridge) homeDescription() string {
	if b.home != homeDM {
		return "home channel " + b.cfg.Channel
	}
	owner := b.ownerID
	if u, ok := b.state.user(b.ownerID); ok {
		owner = u.Label
	}
	desc := "home dm (" + owner + ")"
	if b.channelID != "" {
		desc += ", channel " + b.cfg.Channel
	}
	return desc
}

// Start resolves the bot, channel and users in the background (retrying with
// backoff), then attaches to the bus and keeps the socket open until Stop.
func (b *Bridge) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.done = make(chan struct{})
	go func() {
		defer close(b.done)
		for attempt := 1; ; attempt++ {
			err := b.resolve(ctx)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return
			}
			log.Warnf("slack: bridge not ready: %v", err)
			if !b.sleep(ctx, b.backoff(attempt)) {
				return
			}
		}
		b.bus.SetBridge(b)
		log.Infof("slack: bridge on, %s, allowed users %s", b.homeDescription(), strings.Join(b.Users(), ", "))
		b.jobsWG.Add(2)
		go func() {
			defer b.jobsWG.Done()
			b.runJobs(ctx)
		}()
		go func() {
			defer b.jobsWG.Done()
			b.runMaintenance(ctx)
		}()
		b.runSocket(ctx)
	}()
}

// Stop closes the connection, waits for the bridge's goroutines, then
// detaches from the bus. Detaching last means a resolve that finishes while
// Stop runs can't re-attach the bridge afterwards. Pending state changes
// are written last, also when the bridge never started.
func (b *Bridge) Stop() {
	if b.cancel != nil {
		b.cancel()
		<-b.done
		b.jobsWG.Wait()
		b.bus.SetBridge(nil)
		b.cancel = nil
	}
	if errFlush := b.state.flush(); errFlush != nil {
		logSaveError(errFlush)
	}
}
