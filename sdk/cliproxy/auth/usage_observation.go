package auth

import (
	"net/http"
	"sync/atomic"
	"time"
)

// UsageFailureClassVersion identifies the UsageFailureClass mapping.
const UsageFailureClassVersion = 1

// UsageFailureClass returns the verdict of the existing auth classifiers for an
// upstream attempt error, for usage sinks. It returns "" when no classifier matches.
func UsageFailureClass(err error) string {
	if err == nil {
		return ""
	}
	restriction := usageRestrictionStatusCode(err)
	switch {
	case isRequestScopedError(err):
		return "request_scoped"
	case restriction == http.StatusTooManyRequests && isCredentialScopedError(err):
		return "credential_quota"
	case restriction == http.StatusPaymentRequired || isBillingError(err):
		return "billing"
	case restriction == http.StatusTooManyRequests:
		return "rate_limit_unscoped"
	case isTransientTransportError(err):
		return "transient_transport"
	default:
		return ""
	}
}

// QuotaPollWindow is one window of a usage poll snapshot. UsedPercent is 0..100.
type QuotaPollWindow struct {
	Key         string // "short", "weekly" or "fable_weekly"
	UsedPercent float64
	ResetsAt    time.Time
	Rejected    bool
}

// QuotaPollObservation is a passive copy of one next-reset usage poll result.
// ObservedAt is when the poll response was read.
type QuotaPollObservation struct {
	AuthID     string
	Provider   string
	AuthKind   string
	ObservedAt time.Time
	Windows    []QuotaPollWindow
}

var quotaPollObserver atomic.Pointer[func(QuotaPollObservation)]

// SetQuotaPollObserver installs a passive observer of usage poll results.
// The observer must not block. Pass nil to remove it.
func SetQuotaPollObserver(fn func(QuotaPollObservation)) {
	if fn == nil {
		quotaPollObserver.Store(nil)
		return
	}
	quotaPollObserver.Store(&fn)
}

// completedAt is when the poll response was read; snap.ObservedAt is the poll start.
func notifyQuotaPollObserver(auth *Auth, snap nextResetSnapshot, completedAt time.Time) {
	fn := quotaPollObserver.Load()
	if fn == nil || auth == nil {
		return
	}
	obs := QuotaPollObservation{AuthID: auth.ID, Provider: auth.Provider, AuthKind: auth.AuthKind(), ObservedAt: completedAt}
	for _, w := range []struct {
		key string
		win nextResetWindow
	}{{"short", snap.Short}, {"weekly", snap.Weekly}, {"fable_weekly", snap.Fable}} {
		if w.win.Known {
			obs.Windows = append(obs.Windows, QuotaPollWindow{Key: w.key, UsedPercent: w.win.UsedPct, ResetsAt: w.win.ResetsAt, Rejected: w.win.Rejected})
		}
	}
	if len(obs.Windows) > 0 {
		(*fn)(obs)
	}
}
