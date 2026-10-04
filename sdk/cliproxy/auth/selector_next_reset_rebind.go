package auth

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

// next-reset rebind: while the routing strategy is next-reset with session
// affinity on, a session bound to credential B moves to the credential A that
// next-reset would pick right now when A resets first, the move is affordable
// and no guard applies. See
// docs/superpowers/specs/2026-10-03-next-reset-rebind-design.md.

const (
	// nextResetMoveCooldown is the minimum time between two moves of one
	// session, which prevents flapping.
	nextResetMoveCooldown = time.Hour
	// Prompt cache lifetimes for the two cache_control TTLs.
	nextResetCacheTTL5m = 5 * time.Minute
	nextResetCacheTTL1h = time.Hour
	// Cache price multipliers relative to input tokens.
	nextResetCacheWrite5mMult = 1.25
	nextResetCacheWrite1hMult = 2.0
	nextResetCacheReadMult    = 0.1
	nextResetOutputMult       = 5.0
	// nextResetLearnerAlpha is the EWMA weight of a new tokens-per-% sample.
	nextResetLearnerAlpha = 0.3
	// nextResetLearnerMinSamples is how many samples make an estimate valid.
	nextResetLearnerMinSamples = 3
	// nextResetRebindSessionIdle evicts session entries idle this long. It is
	// longer than both the 1h cache lifetime and the move cooldown, so an
	// evicted session has nothing left worth remembering.
	nextResetRebindSessionIdle = 2 * time.Hour
	// nextResetRebindMaxSessions caps tracked session entries.
	nextResetRebindMaxSessions = 10000
	// nextResetRebindSweepEvery bounds how often idle entries are swept.
	nextResetRebindSweepEvery = 5 * time.Minute

	nextResetWeeklyUtilizationHeader = "Anthropic-Ratelimit-Unified-7d-Utilization"
	nextResetContextCompactedHeader  = "X-Claude-Code-Context-Compacted"
)

// nextResetSessionState is what the tracker knows about one session and model.
type nextResetSessionState struct {
	// hasUsage reports that a response for this session has been observed, so
	// contextTokens and lastSeen are meaningful.
	hasUsage bool
	// contextTokens is the last observed request size: input + cache read +
	// cache creation tokens.
	contextTokens int64
	// lastSeen is when the last request with observed usage was sent.
	lastSeen time.Time
	// movedAt is when the session was last moved by next-reset.
	movedAt time.Time
	// compacted is the compaction header value of the previous request, so
	// only the first request after a compaction counts as a cold cache.
	compacted bool
	// touched drives idle eviction.
	touched time.Time
}

// nextResetTokenLearner estimates tokens per 1% of a credential's weekly quota.
type nextResetTokenLearner struct {
	accumulated float64
	lastUtil    float64
	haveUtil    bool
	estimate    float64
	samples     int
}

// nextResetUsageObservation is one response, as seen by the tracker.
type nextResetUsageObservation struct {
	sessionID       string
	models          []string
	authID          string
	input           int64
	cacheRead       int64
	cacheCreation   int64
	cacheCreation5m int64
	cacheCreation1h int64
	output          int64
	util7d          float64
	haveUtil7d      bool
	at              time.Time
}

// weightedTokens prices a response in input-token equivalents. When the
// cache-creation TTL split is unknown, all cache writes count at the 5m rate.
func (o nextResetUsageObservation) weightedTokens() float64 {
	creation := nextResetCacheWrite5mMult * float64(o.cacheCreation)
	if split := o.cacheCreation5m + o.cacheCreation1h; split > 0 && split == o.cacheCreation {
		creation = nextResetCacheWrite5mMult*float64(o.cacheCreation5m) + nextResetCacheWrite1hMult*float64(o.cacheCreation1h)
	}
	return float64(o.input) + creation + nextResetCacheReadMult*float64(o.cacheRead) + nextResetOutputMult*float64(o.output)
}

// nextResetRebindTracker holds per-session context sizes and per-credential
// tokens-per-% estimates, in memory and bounded.
type nextResetRebindTracker struct {
	mu          sync.Mutex
	now         func() time.Time
	sessions    map[string]*nextResetSessionState
	creds       map[string]*nextResetTokenLearner
	maxSessions int
	lastSweep   time.Time
}

func newNextResetRebindTracker(now func() time.Time) *nextResetRebindTracker {
	if now == nil {
		now = time.Now
	}
	return &nextResetRebindTracker{
		now:         now,
		sessions:    make(map[string]*nextResetSessionState),
		creds:       make(map[string]*nextResetTokenLearner),
		maxSessions: nextResetRebindMaxSessions,
	}
}

// nextResetRebindState is shared by every NextResetSelector so a config reload
// that rebuilds the selector keeps what was learned.
var nextResetRebindState = newNextResetRebindTracker(nil)

var nextResetRebindPluginOnce sync.Once

// registerNextResetRebindUsagePlugin feeds every usage record to the shared
// tracker. Usage records carry the canonical session ID (the executor context
// is synced from CanonicalSessionIDMetadataKey), the credential, the response
// token usage and the upstream response headers.
func registerNextResetRebindUsagePlugin() {
	nextResetRebindPluginOnce.Do(func() {
		coreusage.RegisterNamedPlugin("next-reset-rebind", nextResetRebindState)
	})
}

// nextResetRebindKey scopes session state like the affinity cache: per
// session and per model, so side requests on another model (e.g. a small
// helper model under the same session ID) do not overwrite the context size.
func nextResetRebindKey(sessionID, model string) string {
	return sessionID + "\x00" + canonicalModelKey(model)
}

// HandleUsage implements coreusage.Plugin.
func (t *nextResetRebindTracker) HandleUsage(_ context.Context, record coreusage.Record) {
	if t == nil || record.Failed || strings.TrimSpace(record.AuthID) == "" {
		return
	}
	if record.Generate != nil && !*record.Generate {
		return
	}
	obs := nextResetUsageObservation{
		sessionID:       strings.TrimSpace(record.SessionID),
		authID:          record.AuthID,
		input:           record.Detail.InputTokens,
		cacheRead:       record.Detail.CacheReadTokens,
		cacheCreation:   record.Detail.CacheCreationTokens,
		cacheCreation5m: record.Detail.CacheCreation5mTokens,
		cacheCreation1h: record.Detail.CacheCreation1hTokens,
		output:          record.Detail.OutputTokens,
		at:              record.RequestedAt,
	}
	for _, m := range []string{record.Alias, record.Model} {
		if key := canonicalModelKey(m); key != "" && (len(obs.models) == 0 || obs.models[0] != key) {
			obs.models = append(obs.models, key)
		}
	}
	if raw := strings.TrimSpace(record.ResponseHeaders.Get(nextResetWeeklyUtilizationHeader)); raw != "" {
		if f, errParse := strconv.ParseFloat(raw, 64); errParse == nil && f >= 0 {
			obs.util7d, obs.haveUtil7d = f, true
		}
	}
	t.observeUsage(obs)
}

// observeUsage records one response for its session and credential.
func (t *nextResetRebindTracker) observeUsage(o nextResetUsageObservation) {
	if t == nil {
		return
	}
	now := t.now()
	at := o.at
	if at.IsZero() {
		at = now
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	if o.authID != "" {
		l := t.creds[o.authID]
		if l == nil {
			l = &nextResetTokenLearner{}
			t.creds[o.authID] = l
		}
		l.observe(o.weightedTokens(), o.util7d, o.haveUtil7d)
	}

	contextTokens := o.input + o.cacheRead + o.cacheCreation
	if o.sessionID == "" || contextTokens <= 0 {
		return
	}
	for _, model := range o.models {
		st := t.sessionLocked(nextResetRebindKey(o.sessionID, model), now)
		if st.hasUsage && st.lastSeen.After(at) {
			// An older request finished after a newer one; keep the newer.
			continue
		}
		st.hasUsage, st.contextTokens, st.lastSeen = true, contextTokens, at
	}
}

// observe folds one response into the estimate. Tokens before the first
// utilization reading are discarded; an increase closes a sample; a decrease
// (a weekly reset) clears the accumulator but keeps the estimate.
func (l *nextResetTokenLearner) observe(weighted, util float64, haveUtil bool) {
	l.accumulated += weighted
	if !haveUtil {
		return
	}
	switch {
	case !l.haveUtil:
		l.accumulated = 0
	case util > l.lastUtil:
		sample := l.accumulated / ((util - l.lastUtil) * 100)
		if l.samples == 0 {
			l.estimate = sample
		} else {
			l.estimate = nextResetLearnerAlpha*sample + (1-nextResetLearnerAlpha)*l.estimate
		}
		l.samples++
		l.accumulated = 0
	case util < l.lastUtil:
		l.accumulated = 0
	}
	l.lastUtil, l.haveUtil = util, true
}

// tokensPerPct returns the credential's estimate once it is valid.
func (t *nextResetRebindTracker) tokensPerPct(authID string) (float64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.creds[authID]
	if l == nil || l.samples < nextResetLearnerMinSamples || l.estimate <= 0 {
		return 0, false
	}
	return l.estimate, true
}

// session returns a copy of the session state.
func (t *nextResetRebindTracker) session(key string) (nextResetSessionState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.sessions[key]
	if !ok {
		return nextResetSessionState{}, false
	}
	return *st, true
}

// noteRequest records the compaction flag of a request and reports whether
// it is the first flagged request since an unflagged one.
func (t *nextResetRebindTracker) noteRequest(key string, compacted bool, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.sessionLocked(key, now)
	first := compacted && !st.compacted
	st.compacted = compacted
	return first
}

func (t *nextResetRebindTracker) markMoved(key string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessionLocked(key, now).movedAt = now
}

// sessionLocked returns the entry for key, creating it and enforcing the
// idle and count bounds. t.mu must be held.
func (t *nextResetRebindTracker) sessionLocked(key string, now time.Time) *nextResetSessionState {
	st, ok := t.sessions[key]
	if !ok {
		t.evictLocked(now)
		st = &nextResetSessionState{}
		t.sessions[key] = st
	}
	if now.After(st.touched) {
		st.touched = now
	}
	return st
}

// evictLocked drops idle entries every few minutes or when the map is full,
// then the least recently touched entries while it is still full.
func (t *nextResetRebindTracker) evictLocked(now time.Time) {
	full := t.maxSessions > 0 && len(t.sessions) >= t.maxSessions
	if !full && now.Sub(t.lastSweep) < nextResetRebindSweepEvery {
		return
	}
	t.lastSweep = now
	for key, st := range t.sessions {
		if now.Sub(st.touched) > nextResetRebindSessionIdle {
			delete(t.sessions, key)
		}
	}
	for t.maxSessions > 0 && len(t.sessions) >= t.maxSessions {
		oldestKey, oldest := "", time.Time{}
		for key, st := range t.sessions {
			if oldestKey == "" || st.touched.Before(oldest) {
				oldestKey, oldest = key, st.touched
			}
		}
		delete(t.sessions, oldestKey)
	}
}

// nextResetRequestFacts are the request-body facts a move depends on.
type nextResetRequestFacts struct {
	oneHourTTL bool
	thread     bool
	advisor    bool
}

// nextResetRequestFactsFrom reads the original request body. Byte scans gate
// the JSON walks, so ordinary requests pay little.
func nextResetRequestFactsFrom(body []byte) nextResetRequestFacts {
	var f nextResetRequestFacts
	if len(body) == 0 {
		return f
	}
	if bytes.Contains(body, []byte("previous_message_id")) {
		thread := gjson.GetBytes(body, "thread")
		f.thread = thread.IsObject() && strings.TrimSpace(thread.Get("previous_message_id").String()) != ""
	}
	if bytes.Contains(body, []byte(`"1h"`)) {
		f.oneHourTTL = nextResetBodyUsesOneHourTTL(body)
	}
	if bytes.Contains(body, []byte("advisor_")) {
		f.advisor = nextResetBodyHasAdvisorResult(body)
	}
	return f
}

func nextResetBodyUsesOneHourTTL(body []byte) bool {
	isOneHour := func(v gjson.Result) bool { return v.Get("cache_control.ttl").String() == "1h" }
	if isOneHour(gjson.ParseBytes(body)) {
		return true
	}
	for _, path := range []string{"system", "tools"} {
		for _, block := range gjson.GetBytes(body, path).Array() {
			if isOneHour(block) {
				return true
			}
		}
	}
	for _, msg := range gjson.GetBytes(body, "messages").Array() {
		for _, block := range msg.Get("content").Array() {
			if isOneHour(block) {
				return true
			}
		}
	}
	return false
}

// nextResetBodyHasAdvisorResult mirrors the executor's advisor detection
// (claudeHistoryHasAdvisorCallOrResult) for result blocks: advisor results are
// bound to the account that produced them.
func nextResetBodyHasAdvisorResult(body []byte) bool {
	isResult := func(t string) bool { return t == "advisor_tool_result" || t == "advisor_redacted_result" }
	for _, msg := range gjson.GetBytes(body, "messages").Array() {
		content := msg.Get("content")
		if content.IsObject() {
			if isResult(content.Get("type").String()) {
				return true
			}
			continue
		}
		for _, block := range content.Array() {
			if isResult(block.Get("type").String()) {
				return true
			}
			if block.Get("type").String() != "tool_result" {
				continue
			}
			inner := block.Get("content")
			if inner.IsObject() && inner.Get("type").String() == "advisor_redacted_result" {
				return true
			}
			for _, b := range inner.Array() {
				if b.Get("type").String() == "advisor_redacted_result" {
					return true
				}
			}
		}
	}
	return false
}

// nextResetMetadataCompacted reports the proxy's own compaction detection
// (IsCompactionMetadataKey), which marks the first request after a compaction.
func nextResetMetadataCompacted(metadata map[string]any) bool {
	flag, _ := metadata[cliproxyexecutor.IsCompactionMetadataKey].(bool)
	return flag
}

// nextResetHeaderCompacted reports a truthy x-claude-code-context-compacted
// header. The header may stay set on later requests, so callers pass it
// through nextResetRebindTracker.noteRequest to keep only the first.
func nextResetHeaderCompacted(headers http.Header) bool {
	raw := strings.ToLower(strings.TrimSpace(headers.Get(nextResetContextCompactedHeader)))
	return raw != "" && raw != "false" && raw != "0"
}

// nextResetMoveCostTokens is the cache rewrite cost of moving a warm session
// of contextTokens, in input-token equivalents.
func nextResetMoveCostTokens(contextTokens int64, oneHourTTL bool) float64 {
	write := nextResetCacheWrite5mMult
	if oneHourTTL {
		write = nextResetCacheWrite1hMult
	}
	return float64(contextTokens) * (write - nextResetCacheReadMult)
}

// nextResetMoveAllowance is the share of A's remaining weekly quota a move may
// spend: remaining / max(1, hours until A's reset). In A's final hour it is
// all of it.
func nextResetMoveAllowance(a nextResetAssessment, now time.Time) float64 {
	remaining := 100 - a.weeklyUsedPct
	if remaining < 0 {
		remaining = 0
	}
	hours := a.weeklyResetsAt.Sub(now).Hours()
	if hours < 1 {
		hours = 1
	}
	return remaining / hours
}

// nextResetMove decides whether the session bound to bound should move, and
// returns the credential to move to, or nil to stay. candidates are the same
// credentials a cold pick would rank (the highest available priority tier).
func (s *SessionAffinitySelector) nextResetMove(ctx context.Context, provider, model, sessionID string, opts cliproxyexecutor.Options, candidates []*Auth, bound *Auth) *Auth {
	nr, ok := s.fallback.(*NextResetSelector)
	if !ok || nr == nil || nr.rebind == nil || bound == nil || sessionID == "" {
		return nil
	}
	tracker := nr.rebind
	now := nr.clock()
	key := nextResetRebindKey(sessionID, model)
	// Note the header on every bound request, before any early return, so a
	// sticky header only counts on the first request after a compaction.
	firstAfterCompaction := tracker.noteRequest(key, nextResetHeaderCompacted(opts.Headers), now) ||
		nextResetMetadataCompacted(opts.Metadata)

	st, ok := tracker.session(key)
	if !ok || !st.hasUsage {
		// The cost is unknown until a response for this session is seen.
		return nil
	}
	if !st.movedAt.IsZero() && now.Sub(st.movedAt) < nextResetMoveCooldown {
		return nil
	}
	ranked, err := nr.rank(ctx, provider, model, candidates, now)
	if err != nil || len(ranked) == 0 {
		return nil
	}
	a := ranked[0]
	// A is next-reset's pick: Ready and not near full (the 5h near-full floor
	// is part of nextResetIsNearFull), and it must be a different credential.
	if a.tier != nextResetReady || nextResetIsNearFull(a) || a.auth.ID == bound.ID {
		return nil
	}
	b := nr.assess(bound, model, now)
	if b.tier != nextResetReady || !a.weeklyResetsAt.Before(b.weeklyResetsAt) {
		return nil
	}

	facts := nextResetRequestFactsFrom(opts.OriginalRequest)
	if facts.thread || facts.advisor {
		return nil
	}

	ttl := nextResetCacheTTL5m
	if facts.oneHourTTL {
		ttl = nextResetCacheTTL1h
	}
	cold := firstAfterCompaction || now.Sub(st.lastSeen) > ttl
	cost := 0.0
	if !cold {
		tokensPerPct, okEstimate := tracker.tokensPerPct(a.auth.ID)
		if !okEstimate {
			return nil
		}
		cost = nextResetMoveCostTokens(st.contextTokens, facts.oneHourTTL) / tokensPerPct
	}
	allowance := nextResetMoveAllowance(a, now)
	if cost > allowance {
		return nil
	}

	tracker.markMoved(key, now)
	selectorLogEntry(ctx).Infof(
		"next-reset: moved session | session=%s from=%s to=%s cost=%.3f%% allowance=%.3f%% a_reset=%s b_reset=%s cold=%t",
		truncateSessionID(sessionID), nextResetAuthIdentity(bound), nextResetAuthIdentity(a.auth), cost, allowance,
		a.weeklyResetsAt.Format(time.RFC3339), b.weeklyResetsAt.Format(time.RFC3339), cold,
	)
	return a.auth
}
