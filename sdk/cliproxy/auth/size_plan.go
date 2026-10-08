package auth

import (
	"encoding/json"
	"strings"
	"sync"
)

// codexPlanSizes maps a Codex plan_type (see internal/watcher/synthesizer/file.go
// and internal/runtime/executor/codex_executor_auth.go for where the proxy
// populates it) to the size the owner assigned each tier, matched
// case-insensitively. Plans absent here (pro, free, enterprise, business,
// go, and anything unrecognized) have no derived size: authSize falls
// through to "unknown" for them exactly as it did before plan-derived sizing
// existed.
var codexPlanSizes = map[string]float64{
	"plus":    1,
	"team":    1,
	"prolite": 5,
}

// codexPlanSize derives auth's size from its stored Codex plan_type: the
// routing attribute first, then auth.Metadata as a fallback, mirroring how
// plan_type itself is populated onto the credential.
func codexPlanSize(auth *Auth) (float64, bool) {
	if auth == nil {
		return 0, false
	}
	planType := strings.TrimSpace(auth.Attributes["plan_type"])
	if planType == "" {
		planType = authMetadataString(auth, "plan_type")
	}
	if planType == "" {
		return 0, false
	}
	size, ok := codexPlanSizes[strings.ToLower(planType)]
	return size, ok
}

// claudeProfileBody is the subset of GET
// https://api.anthropic.com/api/oauth/profile that the plan-size mapping
// needs. The live endpoint returns far more (including account identifiers),
// none of which this proxy may read, store, or log; keep this struct limited
// to exactly these fields.
type claudeProfileBody struct {
	Account struct {
		HasClaudePro bool `json:"has_claude_pro"`
	} `json:"account"`
	Organization struct {
		OrganizationType string `json:"organization_type"`
		RateLimitTier    string `json:"rate_limit_tier"`
	} `json:"organization"`
}

// parseClaudeProfileSize maps a profile response body to a plan size, using
// the same owner-provided scale Codex uses (see codexPlanSizes): a
// rate_limit_tier naming the 20x or 5x multiplier wins outright; otherwise a
// Pro or Team organization -- by organization_type or account.has_claude_pro
// -- is size 1; everything else (free, enterprise, a missing or
// unrecognized tier) is unknown. A malformed body is reported as an error so
// the caller backs off instead of mistaking it for a known "unknown" plan.
func parseClaudeProfileSize(body []byte) (float64, bool, error) {
	var b claudeProfileBody
	if errUnmarshal := json.Unmarshal(body, &b); errUnmarshal != nil {
		return 0, false, errUnmarshal
	}
	tier := strings.ToLower(strings.TrimSpace(b.Organization.RateLimitTier))
	switch {
	case strings.Contains(tier, "max_20x"):
		return 20, true, nil
	case strings.Contains(tier, "max_5x"):
		return 5, true, nil
	}
	orgType := strings.ToLower(strings.TrimSpace(b.Organization.OrganizationType))
	if orgType == "claude_pro" || orgType == "claude_team" || b.Account.HasClaudePro {
		return 1, true, nil
	}
	return 0, false, nil
}

// nextResetPlanSizeEntry is one Claude credential's plan size as last
// derived from the profile endpoint (see parseClaudeProfileSize and
// nextResetPoller.maybeFetchClaudePlanSize). Known is false when the last
// successful fetch mapped to no known tier (free, enterprise, ...); a failed
// fetch leaves the previous entry in place rather than writing a new one.
type nextResetPlanSizeEntry struct {
	Size  float64 `json:"size,omitempty"`
	Known bool    `json:"known,omitempty"`
}

// nextResetPlanSizeStore holds derived Claude plan sizes keyed by auth ID,
// the same shape nextResetPolledStore uses for usage snapshots (see
// next_reset_quota.go). Codex needs no such store: codexPlanSize reads the
// plan_type the credential already carries, with nothing to fetch or cache.
type nextResetPlanSizeStore struct {
	mu   sync.RWMutex
	data map[string]nextResetPlanSizeEntry
}

func newNextResetPlanSizeStore() *nextResetPlanSizeStore {
	return &nextResetPlanSizeStore{data: make(map[string]nextResetPlanSizeEntry)}
}

func (s *nextResetPlanSizeStore) get(authID string) (nextResetPlanSizeEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data[authID]
	return e, ok
}

func (s *nextResetPlanSizeStore) set(authID string, entry nextResetPlanSizeEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[authID] = entry
	nextResetStateDirty.Store(true)
}

func (s *nextResetPlanSizeStore) snapshot() map[string]nextResetPlanSizeEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]nextResetPlanSizeEntry, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}

// prune drops any entry whose auth ID is not in keep, in memory and (via
// nextResetStateDirty) in the next persisted state file, so a credential no
// longer tracked by the manager does not keep its derived plan size forever.
func (s *nextResetPlanSizeStore) prune(keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.data {
		if !keep[id] {
			delete(s.data, id)
			nextResetStateDirty.Store(true)
		}
	}
}

// nextResetPlanSizes is shared by every poller/selector so a config reload
// that rebuilds the selector keeps derived sizes, mirroring nextResetPolled.
var nextResetPlanSizes = newNextResetPlanSizeStore()

// planDerivedSize is authSize's third precedence step (see
// authSizeWithSource): Codex from its stored plan_type, Claude from
// whatever nextResetPoller.maybeFetchClaudePlanSize last derived from the
// profile endpoint. Unknown for every other provider, or before the first
// successful fetch for a Claude credential.
func planDerivedSize(auth *Auth) (float64, bool) {
	if auth == nil {
		return 0, false
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "codex":
		return codexPlanSize(auth)
	case "claude":
		entry, ok := nextResetPlanSizes.get(auth.ID)
		if !ok || !entry.Known {
			return 0, false
		}
		return entry.Size, true
	default:
		return 0, false
	}
}
