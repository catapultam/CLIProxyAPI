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

// PooledUsage is the usage of every credential that can serve one model.
type PooledUsage struct {
	Model    string             `json:"model"`
	Accounts int                `json:"accounts"`
	Known    int                `json:"known"`
	FiveHour *PooledUsageWindow `json:"five_hour,omitempty"`
	SevenDay *PooledUsageWindow `json:"seven_day,omitempty"`
}

// PooledUsageForModel reports pooled 5h and weekly usage across the Claude and
// Codex OAuth credentials that serve model. Each window's used_percentage is
// the share of the pool's combined capacity that is used (the mean of each
// credential's percent, counting a full or latched credential as 100), and
// resets_at is the earliest reset among credentials with a known window. For
// Fable models a credential's weekly figure is its Fable sub-limit when that
// is higher. Credentials without data are counted in accounts but not known.
func (m *Manager) PooledUsageForModel(model string, now time.Time) PooledUsage {
	out := PooledUsage{Model: model}
	if m == nil {
		return out
	}
	reg := registry.GetGlobalRegistry()
	fable := strings.Contains(strings.ToLower(model), "fable")
	var short, weekly pooledAccumulator
	for _, auth := range m.List() {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled || !nextResetTracked(auth) {
			continue
		}
		if !m.authSupportsRouteModel(reg, auth, model) {
			continue
		}
		out.Accounts++
		snap, ok := nextResetView(auth, nextResetPolled)
		if !ok {
			continue
		}
		out.Known++
		latched := nextResetIsLatched(auth.ID)
		short.add(snap.Short, latched, now)
		w := snap.Weekly
		if fable && snap.Fable.Known && snap.Fable.UsedPct > w.UsedPct {
			w = snap.Fable
		}
		weekly.add(w, latched, now)
	}
	out.FiveHour = short.window()
	out.SevenDay = weekly.window()
	return out
}

type pooledAccumulator struct {
	sum      float64
	count    int
	earliest time.Time
}

func (a *pooledAccumulator) add(w nextResetWindow, latched bool, now time.Time) {
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
	if w.ResetsAt.After(now) && (a.earliest.IsZero() || w.ResetsAt.Before(a.earliest)) {
		a.earliest = w.ResetsAt
	}
}

func (a *pooledAccumulator) window() *PooledUsageWindow {
	if a.count == 0 {
		return nil
	}
	w := &PooledUsageWindow{UsedPercentage: a.sum / float64(a.count)}
	if !a.earliest.IsZero() {
		w.ResetsAt = a.earliest.Unix()
	}
	return w
}
