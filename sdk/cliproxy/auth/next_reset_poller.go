package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	claudeUsageURL       = "https://api.anthropic.com/api/oauth/usage"
	claudeUsageUserAgent = "claude-cli/2.1.280 (external, cli)"

	nextResetPollEvery      = time.Minute
	nextResetPollStaleAfter = 15 * time.Minute
	// nextResetPollIdleAfter stops polling when the strategy has made no pick
	// for this long, so an unused proxy does not keep calling Anthropic.
	nextResetPollIdleAfter = 2 * time.Hour
	nextResetPollTimeout   = 30 * time.Second

	nextResetBackoffAuth    = 30 * time.Minute
	nextResetBackoffLimited = 15 * time.Minute
	nextResetBackoffOther   = 5 * time.Minute
)

// nextResetLastPick is the unix-nano time of the last next-reset pick.
var nextResetLastPick atomic.Int64

func nextResetMarkActive(now time.Time) { nextResetLastPick.Store(now.UnixNano()) }

func withPrevalidatedAuthCandidates(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, prevalidatedAuthCandidatesKey{}, true)
}

// NextResetHTTPDoer sends a request on behalf of one credential, applying that
// credential's proxy settings.
type NextResetHTTPDoer func(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error)

// nextResetPoller reads the Claude usage endpoint for OAuth credentials whose
// poll is older than nextResetPollStaleAfter, one credential per tick. It is
// read-only: it never refreshes or writes credentials.
type nextResetPoller struct {
	list   func() []*Auth
	do     NextResetHTTPDoer
	store  *nextResetPolledStore
	active func() bool

	mu       sync.Mutex
	backoff  map[string]time.Time
	lastPoll map[string]time.Time
}

func (p *nextResetPoller) runOnce(ctx context.Context, now time.Time) {
	if p.active != nil && !p.active() {
		return
	}
	if now.Sub(time.Unix(0, nextResetLastPick.Load())) > nextResetPollIdleAfter {
		return
	}
	auths := p.list()
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	for _, auth := range auths {
		if auth == nil || auth.Disabled || !strings.EqualFold(auth.Provider, "claude") {
			continue
		}
		if strings.EqualFold(auth.Attributes["auth_kind"], "apikey") || authAccessToken(auth) == "" {
			continue
		}
		p.mu.Lock()
		until, polled := p.backoff[auth.ID], p.lastPoll[auth.ID]
		p.mu.Unlock()
		if now.Before(until) || now.Sub(polled) < nextResetPollStaleAfter {
			continue
		}
		p.fetch(ctx, auth, now)
		return
	}
}

func (p *nextResetPoller) fetch(ctx context.Context, auth *Auth, now time.Time) {
	setBackoff := func(d time.Duration, reason string) {
		p.mu.Lock()
		p.backoff[auth.ID] = now.Add(d)
		p.mu.Unlock()
		log.Debugf("next-reset: usage poll for %s backing off %s: %s", auth.ID, d, reason)
	}
	p.mu.Lock()
	p.lastPoll[auth.ID] = now
	p.mu.Unlock()

	reqCtx, cancel := context.WithTimeout(ctx, nextResetPollTimeout)
	defer cancel()
	req, errReq := http.NewRequestWithContext(reqCtx, http.MethodGet, claudeUsageURL, nil)
	if errReq != nil {
		setBackoff(nextResetBackoffOther, errReq.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+authAccessToken(auth))
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", claudeUsageUserAgent)
	req.Header.Set("Content-Type", "application/json")
	resp, errDo := p.do(reqCtx, auth, req)
	if errDo != nil {
		setBackoff(nextResetBackoffOther, errDo.Error())
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("next-reset: close usage response: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		setBackoff(nextResetBackoffOther, errRead.Error())
		return
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		snap, errParse := parseClaudeUsageEndpoint(body, now)
		if errParse != nil {
			setBackoff(nextResetBackoffOther, "parse: "+errParse.Error())
			return
		}
		p.store.set(auth.ID, snap)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		setBackoff(nextResetBackoffAuth, fmt.Sprintf("http %d", resp.StatusCode))
	case resp.StatusCode == http.StatusTooManyRequests:
		d := nextResetBackoffLimited
		if secs, errAtoi := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); errAtoi == nil && time.Duration(secs)*time.Second > d {
			d = time.Duration(secs) * time.Second
		}
		setBackoff(d, "http 429")
	default:
		setBackoff(nextResetBackoffOther, fmt.Sprintf("http %d", resp.StatusCode))
	}
}

// StartNextResetPoller polls the Claude usage endpoint while the next-reset
// strategy is the active selector (directly or as the session-affinity
// fallback). It stops when ctx is cancelled.
func (m *Manager) StartNextResetPoller(ctx context.Context, do NextResetHTTPDoer) {
	if m == nil || do == nil {
		return
	}
	p := &nextResetPoller{
		list:     m.List,
		do:       do,
		store:    nextResetPolled,
		active:   func() bool { return selectorUsesNextReset(m.Selector()) },
		backoff:  make(map[string]time.Time),
		lastPoll: make(map[string]time.Time),
	}
	go func() {
		ticker := time.NewTicker(nextResetPollEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Errorf("next-reset: usage poller recovered from panic: %v", r)
						}
					}()
					p.runOnce(ctx, time.Now())
				}()
			}
		}
	}()
}

func selectorUsesNextReset(selector Selector) bool {
	switch s := selector.(type) {
	case *NextResetSelector:
		return true
	case *SessionAffinitySelector:
		_, ok := s.fallback.(*NextResetSelector)
		return ok
	default:
		return false
	}
}
