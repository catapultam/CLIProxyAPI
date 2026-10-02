package auth

import (
	"context"
	"sort"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	// nextResetStaleAfter is how old weekly data may be and still drive ranking.
	nextResetStaleAfter = 6 * time.Hour
	// nextResetMinHours floors the hours-to-reset divisor so a window about to
	// reset does not produce an unbounded score.
	nextResetMinHours = 0.5
)

type nextResetTier int

const (
	nextResetReady nextResetTier = iota
	nextResetTrial
	nextResetOther
	nextResetUnavailable
)

// nextResetAssessment is one credential's standing for a single pick.
type nextResetAssessment struct {
	auth           *Auth
	tier           nextResetTier
	score          float64
	weeklyResetsAt time.Time
	weeklyUsedPct  float64
}

// NextResetSelector spends the quota that is closest to being lost first. For
// Claude and Codex OAuth credentials it ranks by remaining weekly percent per
// hour until the weekly reset, skips credentials whose short or weekly window
// (or, for Fable models, the Fable sub-limit) is exhausted, and rotates
// credentials it has no data for. Each pick ranks only the candidates CPA
// offers, which are already scoped to the providers serving the model.
// With session affinity on it is the fallback selector, so it decides cold
// bindings and failover while affinity keeps sessions on their credential.
type NextResetSelector struct {
	rotation RoundRobinSelector
	polled   *nextResetPolledStore
	now      func() time.Time
}

// NewNextResetSelector returns a selector that reads the shared poll store.
func NewNextResetSelector() *NextResetSelector {
	return &NextResetSelector{polled: nextResetPolled}
}

func (s *NextResetSelector) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Pick selects the credential with the most weekly quota at risk.
func (s *NextResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := s.clock()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	nextResetMarkActive(now)

	ranked := make([]nextResetAssessment, 0, len(available))
	for _, auth := range available {
		ranked = append(ranked, s.assess(auth, model, now))
	}
	sortNextReset(ranked)

	if ranked[0].tier == nextResetReady {
		return ranked[0].auth, nil
	}
	// No credential has usable weekly data: rotate through the best remaining
	// tier so idle credentials get traffic and report their quota. When every
	// candidate looks exhausted, rotate through all of them and let the
	// upstream answer, rather than failing a pick CPA considers eligible.
	tier := ranked[0].tier
	pool := make([]*Auth, 0, len(ranked))
	for _, a := range ranked {
		if a.tier == tier {
			pool = append(pool, a.auth)
		}
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].ID < pool[j].ID })
	return s.rotation.Pick(withPrevalidatedAuthCandidates(ctx), provider, model, opts, pool)
}

func (s *NextResetSelector) assess(auth *Auth, model string, now time.Time) nextResetAssessment {
	a := nextResetAssessment{auth: auth, tier: nextResetOther}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider != "claude" && provider != "codex" {
		return a
	}
	if strings.EqualFold(auth.Attributes["auth_kind"], "apikey") {
		// API-key credentials have no plan windows and bill per token.
		return a
	}
	snap, ok := nextResetView(auth, s.polled)
	if !ok {
		a.tier = nextResetTrial
		return a
	}
	if nextResetExhausted(snap.Short, now) || nextResetExhausted(snap.Weekly, now) {
		a.tier = nextResetUnavailable
		return a
	}
	if provider == "claude" && strings.Contains(strings.ToLower(model), "fable") && nextResetExhausted(snap.Fable, now) {
		a.tier = nextResetUnavailable
		return a
	}
	w := snap.Weekly
	if !w.Known || !w.ResetsAt.After(now) || now.Sub(snap.ObservedAt) > nextResetStaleAfter {
		a.tier = nextResetTrial
		return a
	}
	hours := w.ResetsAt.Sub(now).Hours()
	if hours < nextResetMinHours {
		hours = nextResetMinHours
	}
	remaining := 100 - w.UsedPct
	if remaining < 0 {
		remaining = 0
	}
	a.tier = nextResetReady
	a.score = remaining / hours
	a.weeklyResetsAt = w.ResetsAt
	a.weeklyUsedPct = w.UsedPct
	return a
}

// nextResetExhausted reports a window that blocks use right now. It is derived
// from the stored reset time on every pick, so an idle credential returns to
// rotation once its reset passes.
func nextResetExhausted(w nextResetWindow, now time.Time) bool {
	if !w.Known || !w.ResetsAt.After(now) {
		return false
	}
	return w.Rejected || w.UsedPct >= 100
}

// sortNextReset orders by tier, then for Ready credentials by score (desc),
// earlier weekly reset, less used, and finally by ID.
func sortNextReset(as []nextResetAssessment) {
	sort.SliceStable(as, func(i, j int) bool {
		x, y := as[i], as[j]
		if x.tier != y.tier {
			return x.tier < y.tier
		}
		if x.tier == nextResetReady {
			if x.score != y.score {
				return x.score > y.score
			}
			if !x.weeklyResetsAt.Equal(y.weeklyResetsAt) {
				return x.weeklyResetsAt.Before(y.weeklyResetsAt)
			}
			if x.weeklyUsedPct != y.weeklyUsedPct {
				return x.weeklyUsedPct < y.weeklyUsedPct
			}
		}
		return x.auth.ID < y.auth.ID
	})
}
