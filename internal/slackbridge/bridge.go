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
	StatePath     string
	// CommandsDir holds the agent command registry (<name>.yaml files).
	// Empty means no registry: only harness commands and !commands work.
	CommandsDir string
	// APIBase overrides https://slack.com/api/ (tests).
	APIBase string
}

func (c Config) complete() bool {
	return strings.TrimSpace(c.BotToken) != "" && strings.TrimSpace(c.AppToken) != "" &&
		strings.TrimSpace(c.Channel) != "" && len(c.AllowedEmails) > 0
}

type job func(ctx context.Context) error

var (
	_ agentbus.Bridge       = (*Bridge)(nil)
	_ agentbus.ImagePoster  = (*Bridge)(nil)
	_ agentbus.OwnerChecker = (*Bridge)(nil)
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

	// Set by resolve before the bridge is attached or the socket runs.
	channelID string
	botUserID string

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

	cancel context.CancelFunc
	done   chan struct{}
	jobsWG sync.WaitGroup
}

// New builds a bridge, or returns nil, nil when cfg is incomplete (Slack off).
func New(cfg Config, bus *agentbus.Store) (*Bridge, error) {
	if !cfg.complete() {
		return nil, nil
	}
	st, errLoad := loadState(cfg.StatePath)
	if errLoad != nil {
		log.Warnf("slack: load state (starting empty): %v", errLoad)
	}
	return &Bridge{
		cfg:         cfg,
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
	}, nil
}

func defaultBackoff(attempt int) time.Duration {
	d := time.Second << min(attempt, 9)
	return min(d, maxBackoff)
}

func logSaveError(err error) { log.Warnf("slack: save state: %v", err) }

// Users lists the allowed users' labels.
func (b *Bridge) Users() []string { return b.state.labels() }

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
	b.enqueue(func(ctx context.Context) error { return b.postOutbound(ctx, o) })
}

func (b *Bridge) postOutbound(ctx context.Context, o agentbus.Outbound) error {
	text := withMentions(o.Body, b.state.mentionIDs())
	t, opened, err := b.threadFor(ctx, o, text)
	if errors.Is(err, errDMUserGone) {
		log.Warnf("slack: dropped a DM from %s to @%s: not an allowed user", o.Address, o.DM)
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
//   - else the session's own thread. When it has none, threadFor opens one
//     by posting the session header, followed by text when text isn't empty,
//     at the top level of the channel.
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
	if t, ok := b.repliedThread(o); ok {
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
	if ts, ok := b.state.thread(o.SessionID); ok {
		return postTarget{channel: b.channelID, threadTS: ts}, false, nil
	}
	first := sessionHeader(o)
	if text != "" {
		first += "\n" + text
	}
	ts, err := b.api.postMessage(ctx, b.cfg.BotToken, b.channelID, first, "")
	if err != nil {
		return postTarget{}, false, err
	}
	b.state.setThread(o.SessionID, ts)
	return postTarget{channel: b.channelID, threadTS: ts}, true, nil
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
	first := sessionHeader(o)
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
// (both addresses, never the body) and ignored, and so is one from the DM of
// a user who is no longer allowed. It runs without bridge locks held, since
// it may ask the Store for the other session's address.
func (b *Bridge) repliedThread(o agentbus.Outbound) (postTarget, bool) {
	if o.ReplyTo == "" {
		return postTarget{}, false
	}
	if r, ok := b.state.replyTarget(o.ReplyTo, o.SessionID); ok {
		if r.DMUser != "" {
			if _, allowed := b.state.user(r.DMUser); !allowed {
				log.Warnf("slack: %s answered message %s from the DM of %s, who is no longer allowed; posting in its own thread instead", o.Address, o.ReplyTo, r.DMUser)
				return postTarget{}, false
			}
		}
		channel := r.Channel
		if channel == "" {
			channel = b.channelID
		}
		return postTarget{channel: channel, threadTS: r.ThreadTS}, true
	}
	if owner, ok := b.state.replyOwner(o.ReplyTo); ok && owner != o.SessionID {
		other := b.bus.Address(owner)
		if other == "" {
			other = "a session that has left the bus"
		}
		log.Warnf("slack: %s answered message %s, which went to %s; posting in its own thread instead", o.Address, o.ReplyTo, other)
	}
	return postTarget{}, false
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
// error code, user_not_allowed, or request_failed); the details are logged
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

// resolve finds the bot, the channel and the config users. Emails Slack
// doesn't know are skipped; other errors abort so Start retries.
func (b *Bridge) resolve(ctx context.Context) error {
	botID, err := b.api.authTest(ctx, b.cfg.BotToken)
	if err != nil {
		return err
	}
	channelID, err := b.api.findChannel(ctx, b.cfg.BotToken, b.cfg.Channel)
	if err != nil {
		return err
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
	b.botUserID, b.channelID = botID, channelID
	b.state.seed(users)
	return nil
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
		log.Infof("slack: bridge on, channel %s, allowed users %s", b.cfg.Channel, strings.Join(b.Users(), ", "))
		b.jobsWG.Add(1)
		go func() {
			defer b.jobsWG.Done()
			b.runJobs(ctx)
		}()
		b.runSocket(ctx)
	}()
}

// Stop closes the connection, waits for the bridge's goroutines, then
// detaches from the bus. Detaching last means a resolve that finishes while
// Stop runs can't re-attach the bridge afterwards.
func (b *Bridge) Stop() {
	if b.cancel == nil {
		return
	}
	b.cancel()
	<-b.done
	b.jobsWG.Wait()
	b.bus.SetBridge(nil)
	b.cancel = nil
}
