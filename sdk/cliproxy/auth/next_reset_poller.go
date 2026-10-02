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
	codexUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	codexUsageUserAgent  = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"

	nextResetPollEvery      = time.Minute
	nextResetPollStaleAfter = 15 * time.Minute
	// nextResetPollLatchedEarly re-checks a latched credential before its
	// expected reset, so an early reset is noticed.
	nextResetPollLatchedEarly = 30 * time.Minute
	// nextResetPollIdleAfter stops routine polling when the strategy has made
	// no pick for this long. Latched credentials are still confirmed.
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

// nextResetPoller reads the Claude and Codex usage endpoints, one credential
// per tick. Latched credentials come first: once their expected reset passes
// they are polled until the endpoint confirms room, and before that they are
// re-checked occasionally for early resets. Other credentials are refreshed
// when their poll is stale, only while the strategy is in use. The poller is
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

func (p *nextResetPoller) due(auth *Auth, now time.Time, idle bool) (int, bool) {
	p.mu.Lock()
	until, polled := p.backoff[auth.ID], p.lastPoll[auth.ID]
	p.mu.Unlock()
	if now.Before(until) {
		return 0, false
	}
	since := now.Sub(polled)
	if nextResetIsLatched(auth.ID) {
		_, expected := nextResetBlocked(auth, now)
		if !expected.After(now.Add(nextResetLatchRetry)) {
			return 0, since >= nextResetLatchRetry
		}
		return 1, since >= nextResetPollLatchedEarly
	}
	if idle {
		return 0, false
	}
	return 2, since >= nextResetPollStaleAfter
}

func (p *nextResetPoller) runOnce(ctx context.Context, now time.Time) {
	if p.active != nil && !p.active() {
		return
	}
	idle := now.Sub(time.Unix(0, nextResetLastPick.Load())) > nextResetPollIdleAfter
	auths := p.list()
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	var pick *Auth
	pickRank := 3
	for _, auth := range auths {
		if auth == nil || auth.Disabled || !nextResetTracked(auth) || authAccessToken(auth) == "" {
			continue
		}
		rank, ok := p.due(auth, now, idle)
		if ok && rank < pickRank {
			pick, pickRank = auth, rank
		}
	}
	if pick != nil {
		p.fetch(ctx, pick, now)
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
	codex := strings.EqualFold(auth.Provider, "codex")
	url, agent := claudeUsageURL, claudeUsageUserAgent
	if codex {
		url, agent = codexUsageURL, codexUsageUserAgent
	}
	req, errReq := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if errReq != nil {
		setBackoff(nextResetBackoffOther, errReq.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+authAccessToken(auth))
	req.Header.Set("User-Agent", agent)
	req.Header.Set("Content-Type", "application/json")
	if codex {
		if accountID := authMetadataString(auth, "account_id"); accountID != "" {
			req.Header.Set("Chatgpt-Account-Id", accountID)
		}
	} else {
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	}
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
		parse := parseClaudeUsageEndpoint
		if codex {
			parse = parseCodexUsageEndpoint
		}
		snap, errParse := parse(body, now)
		if errParse != nil {
			setBackoff(nextResetBackoffOther, "parse: "+errParse.Error())
			return
		}
		p.store.set(auth.ID, snap)
		// Re-evaluate now so a confirmed reset releases the latch at once.
		nextResetBlocked(auth, now)
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

// StartNextResetPoller polls the usage endpoints while the next-reset strategy
// is the active selector (directly or as the session-affinity fallback). It
// stops when ctx is cancelled.
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
