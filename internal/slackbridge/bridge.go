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
	jobQueueSize = 256
	maxBackoff   = 5 * time.Minute
	seenEvents   = 512
)

// Config is what the bridge needs from config.yaml plus where to keep state.
type Config struct {
	BotToken      string
	AppToken      string
	Channel       string
	AllowedEmails []string
	StatePath     string
	// APIBase overrides https://slack.com/api/ (tests).
	APIBase string
}

func (c Config) complete() bool {
	return strings.TrimSpace(c.BotToken) != "" && strings.TrimSpace(c.AppToken) != "" &&
		strings.TrimSpace(c.Channel) != "" && len(c.AllowedEmails) > 0
}

type job func(ctx context.Context) error

// Bridge links the agentbus to one Slack channel. It implements
// agentbus.Bridge.
type Bridge struct {
	cfg        Config
	bus        *agentbus.Store
	api        *api
	dialer     *websocket.Dialer
	state      *state
	jobs       chan job
	backoff    func(attempt int) time.Duration
	retryDelay time.Duration

	// Set by resolve before the bridge is attached or the socket runs.
	channelID string
	botUserID string

	seenMu   sync.Mutex
	seen     map[string]bool
	seenRing []string

	cancel context.CancelFunc
	done   chan struct{}
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
		cfg:        cfg,
		bus:        bus,
		api:        newAPI(cfg.APIBase),
		dialer:     websocket.DefaultDialer,
		state:      st,
		jobs:       make(chan job, jobQueueSize),
		backoff:    defaultBackoff,
		retryDelay: 2 * time.Second,
		seen:       map[string]bool{},
	}, nil
}

func defaultBackoff(attempt int) time.Duration {
	d := time.Second << min(attempt, 9)
	return min(d, maxBackoff)
}

func logSaveError(err error) { log.Warnf("slack: save state: %v", err) }

// Users lists the allowed users' labels.
func (b *Bridge) Users() []string { return b.state.labels() }

// Post queues a session's message for its thread. It never blocks.
func (b *Bridge) Post(o agentbus.Outbound) {
	b.enqueue(func(ctx context.Context) error { return b.postOutbound(ctx, o) })
}

func (b *Bridge) postOutbound(ctx context.Context, o agentbus.Outbound) error {
	text := withMentions(o.Body, b.state.mentionIDs())
	if ts, ok := b.state.thread(o.SessionID); ok {
		_, err := b.api.postMessage(ctx, b.cfg.BotToken, b.channelID, text, ts)
		return err
	}
	ts, err := b.api.postMessage(ctx, b.cfg.BotToken, b.channelID, sessionHeader(o)+"\n"+text, "")
	if err != nil {
		return err
	}
	b.state.setThread(o.SessionID, ts)
	return nil
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

// runJobs runs queued jobs one at a time, retrying a failed job once.
func (b *Bridge) runJobs(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-b.jobs:
			err := j(ctx)
			if err == nil || ctx.Err() != nil {
				continue
			}
			log.Warnf("slack: %v (retrying once)", err)
			if !b.sleep(ctx, b.retryDelay) {
				return
			}
			if errRetry := j(ctx); errRetry != nil && ctx.Err() == nil {
				log.Warnf("slack: %v (dropped)", errRetry)
			}
		}
	}
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
		go b.runJobs(ctx)
		b.runSocket(ctx)
	}()
}

// Stop detaches from the bus and closes the connection.
func (b *Bridge) Stop() {
	if b.cancel == nil {
		return
	}
	b.bus.SetBridge(nil)
	b.cancel()
	<-b.done
	b.cancel = nil
}
