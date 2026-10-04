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
	// nextResetNearFullPct is the weekly-usage floor at and above which a Ready
	// credential is treated as near full. Near-full credentials are ranked after
	// every credential that still has headroom, so an account that is almost out
	// of weekly quota is spent last instead of being picked first merely because
	// its reset happens to land soonest.
	nextResetNearFullPct = 98.0
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
	weeklyResetsAt time.Time
	weeklyUsedPct  float64
	blockedUntil   time.Time
}

// NextResetSelector spends the quota that is closest to being lost first. For
// Claude and Codex OAuth credentials it is earliest-deadline-first: among Ready
// credentials, the one whose weekly window resets soonest is picked, except
// that a credential at or above nextResetNearFullPct weekly usage is pushed to
// the back of the line so a near-exhausted account is spent last rather than
// first. It skips credentials whose short or weekly window (or, for Fable
// models, the Fable sub-limit) is exhausted until that window resets, and
// rotates credentials it has no data for. Each pick ranks only the candidates
// CPA offers, which are already scoped to the providers serving the model.
// With session affinity on it is the fallback selector, so it decides cold
// bindings and failover while affinity keeps sessions on their credential.
type NextResetSelector struct {
	rotation RoundRobinSelector
	polled   *nextResetPolledStore
	now      func() time.Time
}

// nextResetColdPickKey marks a context as originating from a cold session
// binding or a failover reselect, as opposed to a per-request pick made for a
// session with no affinity (which would log on every request). Only picks
// made under this marker are logged at Info; see withNextResetColdPick.
type nextResetColdPickKey struct{}

// withNextResetColdPick marks ctx so NextResetSelector logs the pick it makes.
// Callers should only apply this to picks that happen once per cold binding or
// failover, never to picks that can repeat on every request for the same
// session, or the resulting log line becomes per-request noise.
func withNextResetColdPick(ctx context.Context) context.Context {
	return context.WithValue(ctx, nextResetColdPickKey{}, true)
}

func isNextResetColdPick(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	marked, _ := ctx.Value(nextResetColdPickKey{}).(bool)
	return marked
}

// nextResetAuthLabel returns the auth's human readable label for logging, or
// its ID when no label is set.
func nextResetAuthLabel(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if label := strings.TrimSpace(auth.Label); label != "" {
		return label
	}
	return auth.ID
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

// Pick selects the credential whose weekly quota is closest to being lost,
// i.e. earliest-deadline-first among credentials with headroom (see
// sortNextReset).
func (s *NextResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := s.clock()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	ranked := make([]nextResetAssessment, 0, len(available))
	for _, auth := range available {
		ranked = append(ranked, s.assess(auth, model, now))
	}
	sortNextReset(ranked)

	if ranked[0].tier == nextResetReady {
		picked := ranked[0]
		if isNextResetColdPick(ctx) {
			readyCount := 0
			for _, a := range ranked {
				if a.tier == nextResetReady {
					readyCount++
				}
			}
			selectorLogEntry(ctx).Infof(
				"next-reset: cold pick | auth=%s weekly_used=%.1f%% weekly_reset=%s ready=%d",
				nextResetAuthLabel(picked.auth), picked.weeklyUsedPct, picked.weeklyResetsAt.Format(time.RFC3339), readyCount,
			)
		}
		return picked.auth, nil
	}
	tier := ranked[0].tier
	if tier == nextResetUnavailable {
		// Every candidate is exhausted for this request. Answer with a
		// cooldown instead of sending requests the upstream will reject.
		return nil, newModelCooldownError(model, provider, nextResetSoonestReset(ranked, now).Sub(now))
	}
	// No credential has usable weekly data: rotate through the best remaining
	// tier so idle credentials get traffic and report their quota.
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
	for _, w := range []nextResetWindow{snap.Short, snap.Weekly} {
		if nextResetExhausted(w, now) {
			a.tier, a.blockedUntil = nextResetUnavailable, w.ResetsAt
			return a
		}
	}
	if provider == "claude" && strings.Contains(strings.ToLower(model), "fable") && nextResetExhausted(snap.Fable, now) {
		a.tier, a.blockedUntil = nextResetUnavailable, snap.Fable.ResetsAt
		return a
	}
	w := snap.Weekly
	if !w.Known || !w.ResetsAt.After(now) || now.Sub(snap.ObservedAt) > nextResetStaleAfter {
		a.tier = nextResetTrial
		return a
	}
	a.tier = nextResetReady
	a.weeklyResetsAt = w.ResetsAt
	a.weeklyUsedPct = w.UsedPct
	return a
}

// nextResetSoonestReset is the earliest reset among exhausted windows, or a
// short retry when none is known.
func nextResetSoonestReset(ranked []nextResetAssessment, now time.Time) time.Time {
	soonest := time.Time{}
	for _, a := range ranked {
		if !a.blockedUntil.After(now) {
			continue
		}
		if soonest.IsZero() || a.blockedUntil.Before(soonest) {
			soonest = a.blockedUntil
		}
	}
	if soonest.IsZero() {
		return now.Add(nextResetLatchRetry)
	}
	return soonest
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

// sortNextReset orders by tier, then for Ready credentials by: (a) headroom,
// credentials below nextResetNearFullPct weekly usage before those at or
// above it, so a near-full account is used last rather than first; (b)
// earlier weekly reset first, spending the quota closest to being lost
// first; (c) lower weekly used percent first; and finally (d) auth ID, for a
// deterministic order.
func sortNextReset(as []nextResetAssessment) {
	sort.SliceStable(as, func(i, j int) bool {
		x, y := as[i], as[j]
		if x.tier != y.tier {
			return x.tier < y.tier
		}
		if x.tier == nextResetReady {
			xNearFull := x.weeklyUsedPct >= nextResetNearFullPct
			yNearFull := y.weeklyUsedPct >= nextResetNearFullPct
			if xNearFull != yNearFull {
				return !xNearFull
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
