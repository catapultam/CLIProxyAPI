package auth

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
)

// nextResetWindow is one rate-limit window. UsedPct is percent used, 0..100.
type nextResetWindow struct {
	Known    bool
	UsedPct  float64
	ResetsAt time.Time
	Rejected bool
}

// nextResetSnapshot is what the next-reset strategy knows about one credential.
// Weekly is the long window that drives ranking, Short the 5h (Claude) or
// short (Codex) window, and Fable the Claude Fable weekly sub-limit, which only
// the usage endpoint reports.
type nextResetSnapshot struct {
	Short      nextResetWindow
	Weekly     nextResetWindow
	Fable      nextResetWindow
	ObservedAt time.Time
}

// nextResetFromSignals reads the passive snapshot CPA stores from every
// upstream response (QuotaState.Signals). Header keys are canonical MIME form.
func nextResetFromSignals(provider string, q QuotaState) (nextResetSnapshot, bool) {
	if len(q.Signals) == 0 {
		return nextResetSnapshot{}, false
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return claudeSignalsSnapshot(q.Signals, q.ObservedAt)
	case "codex":
		return codexSignalsSnapshot(q.Signals, q.ObservedAt)
	default:
		return nextResetSnapshot{}, false
	}
}

func signalValue(signals map[string]string, name string) (string, bool) {
	if v, ok := signals[name]; ok {
		return strings.TrimSpace(v), true
	}
	for k, v := range signals {
		if strings.EqualFold(k, name) {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// claudeSignalsSnapshot reads anthropic-ratelimit-unified-* signals: utilization
// is a 0..1 fraction and reset is unix seconds.
func claudeSignalsSnapshot(signals map[string]string, observedAt time.Time) (nextResetSnapshot, bool) {
	window := func(name string) nextResetWindow {
		var w nextResetWindow
		prefix := "Anthropic-Ratelimit-Unified-" + name + "-"
		if raw, ok := signalValue(signals, prefix+"Utilization"); ok {
			if f, errParse := strconv.ParseFloat(raw, 64); errParse == nil {
				w.UsedPct, w.Known = f*100, true
			}
		}
		if raw, ok := signalValue(signals, prefix+"Reset"); ok {
			if secs, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil && secs > 0 {
				w.ResetsAt, w.Known = time.Unix(secs, 0), true
			}
		}
		if raw, ok := signalValue(signals, prefix+"Status"); ok && raw != "" {
			w.Rejected, w.Known = strings.EqualFold(raw, "rejected"), true
		}
		return w
	}
	snap := nextResetSnapshot{Short: window("5h"), Weekly: window("7d"), ObservedAt: observedAt}
	return snap, snap.Short.Known || snap.Weekly.Known
}

// codexSignalsSnapshot reads x-codex-{primary,secondary}-* signals. used-percent
// is 0..100. The long window is whichever reports more window minutes, so the
// mapping does not assume secondary is weekly.
func codexSignalsSnapshot(signals map[string]string, observedAt time.Time) (nextResetSnapshot, bool) {
	type codexWindow struct {
		w       nextResetWindow
		minutes float64
	}
	window := func(name string) codexWindow {
		var cw codexWindow
		prefix := "X-Codex-" + name + "-"
		if raw, ok := signalValue(signals, prefix+"Used-Percent"); ok {
			if f, errParse := strconv.ParseFloat(raw, 64); errParse == nil {
				cw.w.UsedPct, cw.w.Known = f, true
			}
		}
		if raw, ok := signalValue(signals, prefix+"Window-Minutes"); ok {
			if f, errParse := strconv.ParseFloat(raw, 64); errParse == nil {
				cw.minutes = f
			}
		}
		if raw, ok := signalValue(signals, prefix+"Reset-At"); ok {
			if secs, errParse := strconv.ParseFloat(raw, 64); errParse == nil && secs > 0 {
				cw.w.ResetsAt, cw.w.Known = time.Unix(int64(secs), 0), true
			}
		} else if raw, ok := signalValue(signals, prefix+"Reset-After-Seconds"); ok && !observedAt.IsZero() {
			if secs, errParse := strconv.ParseFloat(raw, 64); errParse == nil && secs >= 0 {
				cw.w.ResetsAt, cw.w.Known = observedAt.Add(time.Duration(secs*float64(time.Second))), true
			}
		}
		return cw
	}
	primary, secondary := window("Primary"), window("Secondary")
	short, weekly := primary, secondary
	if primary.minutes > secondary.minutes {
		short, weekly = secondary, primary
	}
	if !weekly.w.Known && short.w.Known && short.minutes >= 7*24*60 {
		short, weekly = weekly, short
	}
	if raw, ok := signalValue(signals, "X-Codex-Limit-Reached"); ok && strings.EqualFold(raw, "true") {
		// The flag does not say which window tripped; the used-percent values do.
		if weekly.w.UsedPct >= 100 {
			weekly.w.Rejected = true
		} else {
			short.w.Rejected, short.w.Known = true, true
		}
	}
	snap := nextResetSnapshot{Short: short.w, Weekly: weekly.w, ObservedAt: observedAt}
	return snap, snap.Short.Known || snap.Weekly.Known
}

type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type claudeUsageLimit struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"`
	ResetsAt *string  `json:"resets_at"`
	IsActive *bool    `json:"is_active"`
	Scope    struct {
		Model struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

type claudeUsageBody struct {
	FiveHour *claudeUsageWindow `json:"five_hour"`
	SevenDay *claudeUsageWindow `json:"seven_day"`
	// IguanaNecktie is the legacy field for the Fable weekly sub-limit.
	IguanaNecktie *claudeUsageWindow `json:"iguana_necktie"`
	Limits        []claudeUsageLimit `json:"limits"`
}

// parseClaudeUsageEndpoint reads GET https://api.anthropic.com/api/oauth/usage.
// Utilization there is already 0..100 and resets_at is RFC3339 or null.
func parseClaudeUsageEndpoint(body []byte, now time.Time) (nextResetSnapshot, error) {
	var b claudeUsageBody
	if errUnmarshal := json.Unmarshal(body, &b); errUnmarshal != nil {
		return nextResetSnapshot{}, errUnmarshal
	}
	return nextResetSnapshot{
		Short:      claudeUsageToWindow(b.FiveHour),
		Weekly:     claudeUsageToWindow(b.SevenDay),
		Fable:      claudeFableWindow(b),
		ObservedAt: now,
	}, nil
}

func claudeUsageToWindow(w *claudeUsageWindow) nextResetWindow {
	if w == nil || w.Utilization == nil {
		return nextResetWindow{}
	}
	out := nextResetWindow{Known: true, UsedPct: *w.Utilization}
	if w.ResetsAt != nil {
		if t, errParse := time.Parse(time.RFC3339, *w.ResetsAt); errParse == nil {
			out.ResetsAt = t
		}
	}
	return out
}

// claudeFableWindow mirrors the Management Center: an active weekly_scoped
// limit scoped to model "Fable" or "Fable 5", else the first such limit, else
// the legacy iguana_necktie field.
func claudeFableWindow(b claudeUsageBody) nextResetWindow {
	var pick *claudeUsageLimit
	for i := range b.Limits {
		l := &b.Limits[i]
		name := strings.ToLower(strings.TrimSpace(l.Scope.Model.DisplayName))
		if !strings.EqualFold(strings.TrimSpace(l.Kind), "weekly_scoped") || (name != "fable" && name != "fable 5") || l.Percent == nil {
			continue
		}
		if l.IsActive != nil && *l.IsActive {
			pick = l
			break
		}
		if pick == nil {
			pick = l
		}
	}
	if pick != nil {
		return claudeUsageToWindow(&claudeUsageWindow{Utilization: pick.Percent, ResetsAt: pick.ResetsAt})
	}
	return claudeUsageToWindow(b.IguanaNecktie)
}

// nextResetPolledStore holds usage-endpoint snapshots keyed by auth ID. Response
// signals are fresher for the 5h and weekly windows; polls fill credentials
// that have seen no traffic and carry the Fable sub-limit headers never report.
type nextResetPolledStore struct {
	mu   sync.RWMutex
	data map[string]nextResetSnapshot
}

func newNextResetPolledStore() *nextResetPolledStore {
	return &nextResetPolledStore{data: make(map[string]nextResetSnapshot)}
}

func (p *nextResetPolledStore) set(authID string, snap nextResetSnapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.data[authID] = snap
}

func (p *nextResetPolledStore) get(authID string) (nextResetSnapshot, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	snap, ok := p.data[authID]
	return snap, ok
}

// nextResetPolled is shared by every NextResetSelector so a config reload that
// rebuilds the selector keeps polled data.
var nextResetPolled = newNextResetPolledStore()

// nextResetView merges response signals and the last poll for one credential.
// The newer source wins for the short and weekly windows; Fable always comes
// from the poll.
func nextResetView(auth *Auth, polled *nextResetPolledStore) (nextResetSnapshot, bool) {
	signals, haveSignals := nextResetFromSignals(auth.Provider, auth.Quota)
	var poll nextResetSnapshot
	havePoll := false
	if polled != nil {
		poll, havePoll = polled.get(auth.ID)
	}
	switch {
	case haveSignals && havePoll:
		merged := signals
		if poll.ObservedAt.After(signals.ObservedAt) {
			merged.Short, merged.Weekly, merged.ObservedAt = poll.Short, poll.Weekly, poll.ObservedAt
		}
		merged.Fable = poll.Fable
		return merged, true
	case haveSignals:
		return signals, true
	case havePoll:
		return poll, true
	default:
		return nextResetSnapshot{}, false
	}
}
