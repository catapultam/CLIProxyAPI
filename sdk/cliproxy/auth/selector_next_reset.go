package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
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
	// nextResetShortWindowNearFullPct is the short-window (5h for Claude,
	// primary for Codex) usage floor at and above which a Ready credential is
	// also treated as near full, even when its weekly usage has headroom. This
	// keeps the selector from herding every request onto one account whose
	// short window is about to run dry just because its weekly reset is close.
	nextResetShortWindowNearFullPct = 90.0
	// nextResetSizeBiasShift is how much earlier a smaller account's weekly
	// reset is treated as happening, for ranking and rebind purposes only
	// (see nextResetEffectiveReset). It is flat regardless of how much
	// smaller the account is.
	nextResetSizeBiasShift = 24 * time.Hour
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
	shortUsedPct   float64
	blockedUntil   time.Time
	// effectiveWeeklyResetsAt is weeklyResetsAt shifted 24h earlier when this
	// credential's size is known and smaller than the largest known size
	// among the Ready credentials ranked alongside it (see
	// nextResetApplyEffectiveResets). It drives ranking step (b) and the
	// rebind "which account resets first" comparison; real time-to-reset math
	// (the move cost allowance) must keep using weeklyResetsAt. Populated by
	// sortNextReset; zero until then.
	effectiveWeeklyResetsAt time.Time
}

// NextResetSelector spends the quota that is closest to being lost first. For
// Claude and Codex OAuth credentials it is earliest-deadline-first: among Ready
// credentials, the one whose weekly window resets soonest is picked, except
// that a credential is pushed to the back of the line, and used last rather
// than first, when it is near full: at or above nextResetNearFullPct weekly
// usage, or at or above nextResetShortWindowNearFullPct usage on its short
// window (5h for Claude, primary for Codex). The short-window floor keeps the
// selector from herding every request onto one account just because its
// weekly reset is close, when that account's short window is nearly spent.
// It skips credentials whose short or weekly window (or, for Fable models,
// the Fable sub-limit) is exhausted until that window resets, and rotates
// credentials it has no data for. Each pick ranks only the candidates CPA
// offers, which are already scoped to the providers serving the model. With
// session affinity on it is the fallback selector, so it decides cold
// bindings and failover while affinity keeps sessions on their credential.
type NextResetSelector struct {
	rotation RoundRobinSelector
	polled   *nextResetPolledStore
	now      func() time.Time
	// rebind tracks sessions and credentials so that session affinity can move
	// a bound session toward the credential that resets first. Nil disables
	// moves (see selector_next_reset_rebind.go).
	rebind *nextResetRebindTracker
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

// nextResetAuthIdentity returns an opaque, stable identifier for logging: the
// first 12 hex characters of the SHA-256 hash of the auth ID. File-based
// credential IDs and file names routinely embed the account's email address
// (e.g. "claude-12345678-alice@example.com.json"; see
// internal/watcher/synthesizer/file.go), and so does Auth.Label. None of
// those may ever appear in a log line, so this intentionally logs neither
// the ID, the file name, nor Label. The hash is stable for a given
// credential, so separate log lines for the same account can still be
// correlated without revealing which account it is.
func nextResetAuthIdentity(auth *Auth) string {
	if auth == nil {
		return ""
	}
	id := strings.TrimSpace(auth.ID)
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:6])
}

// NewNextResetSelector returns a selector that reads the shared poll store.
func NewNextResetSelector() *NextResetSelector {
	registerNextResetRebindUsagePlugin()
	return &NextResetSelector{polled: nextResetPolled, rebind: nextResetRebindState}
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
	ranked, err := s.rank(ctx, provider, model, auths, now)
	if err != nil {
		return nil, err
	}

	if ranked[0].tier == nextResetReady {
		picked := ranked[0]
		if isNextResetColdPick(ctx) {
			readyCount := 0
			for _, a := range ranked {
				if a.tier == nextResetReady {
					readyCount++
				}
			}
			sizeLog := "unknown"
			if size, ok := authSize(picked.auth); ok {
				sizeLog = strconv.FormatFloat(size, 'g', -1, 64)
			}
			selectorLogEntry(ctx).Infof(
				"next-reset: cold pick | auth=%s provider=%s weekly_used=%.1f%% weekly_reset=%s ready=%d size=%s effective_reset=%s",
				nextResetAuthIdentity(picked.auth), provider, picked.weeklyUsedPct, picked.weeklyResetsAt.Format(time.RFC3339), readyCount,
				sizeLog, picked.effectiveWeeklyResetsAt.Format(time.RFC3339),
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

// rank assesses the available candidates and orders them the way a pick
// does (see sortNextReset). The result is never empty when err is nil.
func (s *NextResetSelector) rank(ctx context.Context, provider, model string, auths []*Auth, now time.Time) ([]nextResetAssessment, error) {
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
	return ranked, nil
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
	// Only a short window that is actually still open can make this
	// credential near full on short-window grounds: a window whose ResetsAt
	// has passed (or is zero/missing) is stale and must not demote a
	// credential that has since rolled over. nextResetExhausted above already
	// handles the "still open but fully used" case; this guards the near-full
	// floor specifically.
	if snap.Short.Known && snap.Short.ResetsAt.After(now) {
		a.shortUsedPct = snap.Short.UsedPct
	}
	return a
}

// nextResetIsNearFull reports whether a Ready credential should be pushed to
// the back of the ranking: either its weekly usage is at or above
// nextResetNearFullPct, or its short window usage is at or above
// nextResetShortWindowNearFullPct.
func nextResetIsNearFull(a nextResetAssessment) bool {
	return a.weeklyUsedPct >= nextResetNearFullPct || a.shortUsedPct >= nextResetShortWindowNearFullPct
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

// nextResetMaxReadySize returns the largest known auth size among the Ready
// assessments in as, so callers can tell which credentials count as
// "smaller" for the effective-reset shift. It is computed once per ranking
// call, not once per comparison.
func nextResetMaxReadySize(as []nextResetAssessment) (maxSize float64, known bool) {
	for _, a := range as {
		if a.tier != nextResetReady {
			continue
		}
		if size, ok := authSize(a.auth); ok && (!known || size > maxSize) {
			maxSize, known = size, true
		}
	}
	return maxSize, known
}

// nextResetEffectiveReset biases smaller accounts to be spent first: it
// shifts resetsAt nextResetSizeBiasShift earlier when auth's size is known
// and strictly smaller than maxSize (the largest known size among the
// credentials being ranked together), and leaves resetsAt unchanged
// otherwise (unknown size, size equal to the largest, or no known size at
// all in this ranking). Every smaller account gets the same flat shift
// regardless of how much smaller it is.
func nextResetEffectiveReset(auth *Auth, resetsAt time.Time, maxSize float64, maxKnown bool) time.Time {
	if !maxKnown {
		return resetsAt
	}
	size, ok := authSize(auth)
	if !ok || size >= maxSize {
		return resetsAt
	}
	return resetsAt.Add(-nextResetSizeBiasShift)
}

// nextResetApplyEffectiveResets fills in effectiveWeeklyResetsAt for every
// Ready assessment in as, using one max-size pass shared by the whole slice
// (see nextResetMaxReadySize). Non-Ready assessments are left with their
// weekly reset unchanged since ranking never compares them on reset time.
func nextResetApplyEffectiveResets(as []nextResetAssessment) {
	maxSize, maxKnown := nextResetMaxReadySize(as)
	for i := range as {
		if as[i].tier != nextResetReady {
			as[i].effectiveWeeklyResetsAt = as[i].weeklyResetsAt
			continue
		}
		as[i].effectiveWeeklyResetsAt = nextResetEffectiveReset(as[i].auth, as[i].weeklyResetsAt, maxSize, maxKnown)
	}
}

// sortNextReset orders by tier, then for Ready credentials by: (a) headroom,
// credentials that are not near full (see nextResetIsNearFull) before those
// that are, so a near-full account is used last rather than first; (b)
// earlier *effective* weekly reset first, spending the quota closest to
// being lost first, where the effective reset is 24h earlier than the real
// one for a credential whose known size is smaller than the largest known
// size among the Ready candidates (see nextResetEffectiveReset) -- this is
// the smaller-accounts-first bias, layered on top of earliest-deadline-first
// ranking; (c) lower weekly used percent first; and finally (d) auth ID, for
// a deterministic order. Near-full credentials are only pushed to the back
// as a group: their relative order still follows (b)-(d).
func sortNextReset(as []nextResetAssessment) {
	nextResetApplyEffectiveResets(as)
	sort.SliceStable(as, func(i, j int) bool {
		x, y := as[i], as[j]
		if x.tier != y.tier {
			return x.tier < y.tier
		}
		if x.tier == nextResetReady {
			xNearFull := nextResetIsNearFull(x)
			yNearFull := nextResetIsNearFull(y)
			if xNearFull != yNearFull {
				return !xNearFull
			}
			if !x.effectiveWeeklyResetsAt.Equal(y.effectiveWeeklyResetsAt) {
				return x.effectiveWeeklyResetsAt.Before(y.effectiveWeeklyResetsAt)
			}
			if x.weeklyUsedPct != y.weeklyUsedPct {
				return x.weeklyUsedPct < y.weeklyUsedPct
			}
		}
		return x.auth.ID < y.auth.ID
	})
}
