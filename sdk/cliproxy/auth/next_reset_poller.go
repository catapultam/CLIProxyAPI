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
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	claudeUsageURL       = "https://api.anthropic.com/api/oauth/usage"
	claudeUsageUserAgent = "claude-cli/2.1.280 (external, cli)"
	codexUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	codexUsageUserAgent  = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"

	nextResetPollEvery = time.Minute
	// nextResetPollRoutine refreshes every credential at least this often.
	nextResetPollRoutine = 3 * time.Hour
	nextResetPollTimeout = 30 * time.Second

	nextResetBackoffAuth    = 30 * time.Minute
	nextResetBackoffLimited = 15 * time.Minute
	nextResetBackoffOther   = 5 * time.Minute
)

func withPrevalidatedAuthCandidates(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, prevalidatedAuthCandidatesKey{}, true)
}

// NextResetHTTPDoer sends a request on behalf of one credential, applying that
// credential's proxy settings.
type NextResetHTTPDoer func(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error)

// nextResetPoller reads the Claude and Codex usage endpoints proactively, with
// or without traffic: every credential at startup and every three hours, and
// any credential as soon as one of its windows reaches its reset time. A
// latched credential whose reset has passed is polled every two minutes until
// the endpoint confirms room. The poller is read-only: it never refreshes or
// writes credentials.
type nextResetPoller struct {
	list   func() []*Auth
	do     NextResetHTTPDoer
	store  *nextResetPolledStore
	active func() bool

	mu       sync.Mutex
	backoff  map[string]time.Time
	lastPoll map[string]time.Time
}

// due reports whether auth should be polled now, and its priority (lower
// first): 0 for a latched credential past its reset, 1 for a credential with a
// window that reset since the last poll, 2 for the routine refresh.
func (p *nextResetPoller) due(auth *Auth, now time.Time) (int, bool) {
	p.mu.Lock()
	until, polled := p.backoff[auth.ID], p.lastPoll[auth.ID]
	p.mu.Unlock()
	if now.Before(until) {
		return 0, false
	}
	since := now.Sub(polled)
	if nextResetIsLatched(auth.ID) {
		if _, expected := nextResetBlocked(auth, now); !expected.After(now.Add(nextResetLatchRetry)) {
			return 0, since >= nextResetLatchRetry
		}
	}
	if snap, ok := nextResetView(auth, p.store); ok {
		for _, w := range []nextResetWindow{snap.Short, snap.Weekly, snap.Fable} {
			if w.Known && !w.ResetsAt.IsZero() && w.ResetsAt.After(polled) && !w.ResetsAt.After(now) {
				return 1, true
			}
		}
	}
	return 2, since >= nextResetPollRoutine
}

func (p *nextResetPoller) runOnce(ctx context.Context, now time.Time) {
	if p.active != nil && !p.active() {
		return
	}
	type dueAuth struct {
		auth *Auth
		rank int
	}
	var due []dueAuth
	for _, auth := range p.list() {
		if auth == nil || auth.Disabled || !nextResetTracked(auth) || authAccessToken(auth) == "" {
			continue
		}
		if rank, ok := p.due(auth, now); ok {
			due = append(due, dueAuth{auth: auth, rank: rank})
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].rank != due[j].rank {
			return due[i].rank < due[j].rank
		}
		return due[i].auth.ID < due[j].auth.ID
	})
	for _, d := range due {
		if ctx.Err() != nil {
			return
		}
		p.fetch(ctx, d.auth, now)
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

// StartNextResetPoller restores saved usage state from statePath, then polls
// the usage endpoints while the next-reset strategy is the active selector
// (directly or as the session-affinity fallback): at startup, then checking
// every minute what is due, and saving state whenever it changed. It stops
// when ctx is cancelled. An empty statePath disables persistence.
func (m *Manager) StartNextResetPoller(ctx context.Context, do NextResetHTTPDoer, statePath string) {
	if m == nil || do == nil {
		return
	}
	loadNextResetState(statePath, time.Now())
	p := &nextResetPoller{
		list:     m.List,
		do:       do,
		store:    nextResetPolled,
		active:   func() bool { return selectorUsesNextReset(m.Selector()) },
		backoff:  make(map[string]time.Time),
		lastPoll: make(map[string]time.Time),
	}
	save := func() {
		if !nextResetStateDirty.Load() {
			return
		}
		if errSave := saveNextResetState(statePath); errSave != nil {
			log.Warnf("next-reset: save state %s: %v", statePath, errSave)
		}
	}
	tick := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("next-reset: usage poller recovered from panic: %v", r)
			}
		}()
		p.runOnce(ctx, time.Now())
		save()
	}
	go func() {
		tick()
		ticker := time.NewTicker(nextResetPollEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				save()
				return
			case <-ticker.C:
				tick()
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
