package auth

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// PooledUsageWindow is one pooled window in the shape Claude Code's status
// line uses for rate_limits: percent used and the reset as unix seconds.
type PooledUsageWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at,omitempty"`
}

// PooledUsage is the usage of every credential that can serve one model, or
// of every credential of one provider.
type PooledUsage struct {
	Model    string             `json:"model,omitempty"`
	Provider string             `json:"provider,omitempty"`
	Accounts int                `json:"accounts"`
	Known    int                `json:"known"`
	FiveHour *PooledUsageWindow `json:"five_hour,omitempty"`
	SevenDay *PooledUsageWindow `json:"seven_day,omitempty"`
	// SevenDayByProvider lists the weekly pool of every provider with
	// credentials, so one status-line request can show all of them. The
	// provider serving the requested model uses that model's pool.
	SevenDayByProvider []ProviderWeeklyUsage `json:"seven_day_by_provider,omitempty"`
}

// ProviderWeeklyUsage is one provider's pooled weekly window. Provider is the
// display name: "claude" or "openai" (Codex credentials).
type ProviderWeeklyUsage struct {
	Provider string             `json:"provider"`
	Accounts int                `json:"accounts"`
	SevenDay *PooledUsageWindow `json:"seven_day,omitempty"`
}

// PooledUsageReport is the full status-line view for a model: the model's own
// pool plus the weekly pool of every provider with credentials.
func (m *Manager) PooledUsageReport(model string, now time.Time) PooledUsage {
	out := m.PooledUsageForModel(model, now)
	for _, provider := range []string{"claude", "codex"} {
		pool := m.PooledUsageForProvider(provider, now)
		if pool.Accounts == 0 {
			continue
		}
		entry := ProviderWeeklyUsage{Provider: provider, Accounts: pool.Accounts, SevenDay: pool.SevenDay}
		if provider == "codex" {
			entry.Provider = "openai"
		}
		if out.Provider == provider {
			entry.Accounts, entry.SevenDay = out.Accounts, out.SevenDay
		}
		out.SevenDayByProvider = append(out.SevenDayByProvider, entry)
	}
	return out
}

// PooledUsageForModel reports pooled 5h and weekly usage across the Claude and
// Codex OAuth credentials that serve model. Each window's used_percentage is
// the share of the pool's combined capacity that is used: when every
// credential counted in Accounts has a known size (see authSize) it is the
// size-weighted mean of each credential's percent, so a larger account's
// usage counts for more of the pool; otherwise it is the plain mean, as
// before. Either way a full or latched credential counts as 100. resets_at
// is the earliest reset among credentials with a known window, unaffected by
// weighting. For Fable models a credential's weekly figure is its Fable
// sub-limit when that is higher. Credentials without data are counted in
// accounts but not known.
func (m *Manager) PooledUsageForModel(model string, now time.Time) PooledUsage {
	reg := registry.GetGlobalRegistry()
	fable := strings.Contains(strings.ToLower(model), "fable")
	providers := make(map[string]struct{}, 2)
	out := m.pooledUsage(now, fable, func(auth *Auth) bool {
		if !m.authSupportsRouteModel(reg, auth, model) {
			return false
		}
		providers[strings.ToLower(auth.Provider)] = struct{}{}
		return true
	})
	out.Model = model
	if len(providers) == 1 {
		for p := range providers {
			out.Provider = p
		}
	}
	return out
}

// PooledUsageForProvider pools every Claude or Codex OAuth credential of one
// provider, whatever model it serves.
func (m *Manager) PooledUsageForProvider(provider string, now time.Time) PooledUsage {
	provider = strings.ToLower(strings.TrimSpace(provider))
	out := m.pooledUsage(now, false, func(auth *Auth) bool { return strings.EqualFold(auth.Provider, provider) })
	out.Provider = provider
	return out
}

func (m *Manager) pooledUsage(now time.Time, fable bool, include func(*Auth) bool) PooledUsage {
	var out PooledUsage
	if m == nil {
		return out
	}
	var short, weekly pooledAccumulator
	// sizesKnown tracks whether every credential counted in out.Accounts has
	// a known size, including ones with no usage snapshot yet (they never
	// reach add() below but still decide whether the pool as a whole may be
	// weighted). It can only go from true to false as the pool is scanned.
	sizesKnown := true
	for _, auth := range m.List() {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled || !nextResetTracked(auth) {
			continue
		}
		if !include(auth) {
			continue
		}
		out.Accounts++
		size, sizeOK := authSize(auth)
		if !sizeOK {
			sizesKnown = false
		}
		snap, ok := nextResetView(auth, nextResetPolled)
		if !ok {
			continue
		}
		out.Known++
		latched := nextResetIsLatched(auth.ID)
		short.add(snap.Short, latched, now, size)
		w := snap.Weekly
		if fable && snap.Fable.Known && snap.Fable.UsedPct > w.UsedPct {
			w = snap.Fable
		}
		weekly.add(w, latched, now, size)
	}
	out.FiveHour = short.window(sizesKnown)
	out.SevenDay = weekly.window(sizesKnown)
	return out
}

// pooledAccumulator folds one window's reading from every pooled credential
// into both a plain mean (sum/count) and a size-weighted mean
// (weightedSum/sizeSum). window() picks between them: the weighted mean is
// only meaningful when every credential add() saw had a known size, which
// the caller (pooledUsage) tracks across the whole pool and passes in.
type pooledAccumulator struct {
	sum         float64
	count       int
	weightedSum float64
	sizeSum     float64
	earliest    time.Time
}

// add folds one credential's window into the accumulator. size is that
// credential's hand-set size (0 when unknown); it only matters when window()
// is later asked for the weighted mean.
func (a *pooledAccumulator) add(w nextResetWindow, latched bool, now time.Time, size float64) {
	if !w.Known {
		return
	}
	used := w.UsedPct
	if w.ResetsAt.After(now) || w.ResetsAt.IsZero() {
		if w.Rejected {
			used = 100
		}
	} else if !latched {
		// The window rolled over since it was observed and nothing says the
		// credential is still full, so its capacity is back.
		used = 0
	}
	if latched && used < 100 && nextResetWindowFull(w) {
		used = 100
	}
	if used > 100 {
		used = 100
	}
	if used < 0 {
		used = 0
	}
	a.sum += used
	a.count++
	a.weightedSum += used * size
	a.sizeSum += size
	if w.ResetsAt.After(now) && (a.earliest.IsZero() || w.ResetsAt.Before(a.earliest)) {
		a.earliest = w.ResetsAt
	}
}

// window reports the pooled window, or nil when nothing was added. weighted
// requests the size-weighted mean; it is only honored when sizeSum is
// positive, i.e. at least one add() carried a known size (the caller only
// passes true when every counted credential's size was known, so sizeSum is
// then necessarily positive whenever count is).
func (a *pooledAccumulator) window(weighted bool) *PooledUsageWindow {
	if a.count == 0 {
		return nil
	}
	pct := a.sum / float64(a.count)
	if weighted && a.sizeSum > 0 {
		pct = a.weightedSum / a.sizeSum
	}
	w := &PooledUsageWindow{UsedPercentage: pct}
	if !a.earliest.IsZero() {
		w.ResetsAt = a.earliest.Unix()
	}
	return w
}
