// Package usageanalytics is a passive, metadata-only PostgreSQL sink for
// subscription capacity analytics. It reads usage records and usage poll
// results that the proxy already produces; it never calls a provider.
package usageanalytics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

// maxErrorBodyScan bounds the failure body bytes parsed for error.type/code.
const maxErrorBodyScan = 64 << 10

// maxUsedPercent matches the schema CHECK; larger values are dropped as implausible.
const maxUsedPercent = 1000

// item is one row for exactly one table. It is also the spool line format.
type item struct {
	A *attemptRow     `json:"a,omitempty"`
	O *observationRow `json:"o,omitempty"`
	H *heartbeatRow   `json:"h,omitempty"`
}

type attemptRow struct {
	EventID                  string     `json:"event_id"`
	RunID                    string     `json:"run_id"`
	ReceivedAt               time.Time  `json:"received_at"`
	TraceID                  *string    `json:"trace_id"`
	AttemptID                *string    `json:"attempt_id"`
	Provider                 string     `json:"provider"`
	ExecutorType             *string    `json:"executor_type"`
	AuthKind                 *string    `json:"auth_kind"`
	AccountHash              *string    `json:"account_hash"`
	ClientKeyHash            *string    `json:"client_key_hash"`
	RequestedModel           *string    `json:"requested_model"`
	UpstreamModel            *string    `json:"upstream_model"`
	ServedModel              *string    `json:"served_model"`
	RequestedEffort          *string    `json:"requested_effort"`
	UpstreamEffort           *string    `json:"upstream_effort"`
	RequestedTier            *string    `json:"requested_tier"`
	ServedTier               *string    `json:"served_tier"`
	Stream                   bool       `json:"stream"`
	StartedAt                *time.Time `json:"started_at"`
	EndedAt                  *time.Time `json:"ended_at"`
	LatencyMs                *int64     `json:"latency_ms"`
	TTFTMs                   *int64     `json:"ttft_ms"`
	Failed                   bool       `json:"failed"`
	Origin                   string     `json:"origin"`
	HTTPStatus               *int64     `json:"http_status"`
	ErrorType                *string    `json:"error_type"`
	ErrorCode                *string    `json:"error_code"`
	FailureClass             *string    `json:"failure_class"`
	ClassifierVersion        *int64     `json:"classifier_version"`
	UsageKnown               bool       `json:"usage_known"`
	RawInput                 *int64     `json:"raw_input"`
	RawOutput                *int64     `json:"raw_output"`
	RawReasoning             *int64     `json:"raw_reasoning"`
	RawCached                *int64     `json:"raw_cached"`
	RawCacheRead             *int64     `json:"raw_cache_read"`
	RawCacheCreation         *int64     `json:"raw_cache_creation"`
	RawCacheCreation5m       *int64     `json:"raw_cache_creation_5m"`
	RawCacheCreation1h       *int64     `json:"raw_cache_creation_1h"`
	RawTotal                 *int64     `json:"raw_total"`
	AccountingVersion        *int64     `json:"accounting_version"`
	AccountingQuality        *string    `json:"accounting_quality"`
	TotalTokens              *int64     `json:"total_tokens"`
	InputUncachedTokens      *int64     `json:"input_uncached"`
	InputCacheReadTokens     *int64     `json:"input_cache_read"`
	InputCacheWriteTokens    *int64     `json:"input_cache_write"`
	OutputNonReasoningTokens *int64     `json:"output_non_reasoning"`
	OutputReasoningTokens    *int64     `json:"output_reasoning"`
	UnclassifiedTokens       *int64     `json:"unclassified"`
}

const attemptColumns = `event_id, collector_run_id, received_at, trace_id, attempt_id, provider, executor_type,
auth_kind, account_hash, client_key_hash, requested_model, upstream_model, served_model, requested_effort,
upstream_effort, requested_tier, served_tier, stream, started_at, ended_at, latency_ms, ttft_ms, failed, origin,
http_status, error_type, error_code, failure_class, classifier_version, usage_known, raw_input_tokens,
raw_output_tokens, raw_reasoning_tokens, raw_cached_tokens, raw_cache_read_tokens, raw_cache_creation_tokens,
raw_cache_creation_5m_tokens, raw_cache_creation_1h_tokens, raw_total_tokens, accounting_version,
accounting_quality, total_tokens, input_uncached_tokens, input_cache_read_tokens, input_cache_write_tokens,
output_non_reasoning_tokens, output_reasoning_tokens, unclassified_tokens`

func (r *attemptRow) args() []any {
	return []any{r.EventID, r.RunID, r.ReceivedAt, r.TraceID, r.AttemptID, r.Provider, r.ExecutorType,
		r.AuthKind, r.AccountHash, r.ClientKeyHash, r.RequestedModel, r.UpstreamModel, r.ServedModel, r.RequestedEffort,
		r.UpstreamEffort, r.RequestedTier, r.ServedTier, r.Stream, r.StartedAt, r.EndedAt, r.LatencyMs, r.TTFTMs, r.Failed, r.Origin,
		r.HTTPStatus, r.ErrorType, r.ErrorCode, r.FailureClass, r.ClassifierVersion, r.UsageKnown, r.RawInput,
		r.RawOutput, r.RawReasoning, r.RawCached, r.RawCacheRead, r.RawCacheCreation,
		r.RawCacheCreation5m, r.RawCacheCreation1h, r.RawTotal, r.AccountingVersion,
		r.AccountingQuality, r.TotalTokens, r.InputUncachedTokens, r.InputCacheReadTokens, r.InputCacheWriteTokens,
		r.OutputNonReasoningTokens, r.OutputReasoningTokens, r.UnclassifiedTokens}
}

type observationRow struct {
	ObservationID     string     `json:"observation_id"`
	RunID             string     `json:"run_id"`
	ReceivedAt        time.Time  `json:"received_at"`
	Source            string     `json:"source"`
	AttemptEventID    *string    `json:"attempt_event_id"`
	ObservedAt        time.Time  `json:"observed_at"`
	ObservedAtQuality string     `json:"observed_at_quality"`
	ObservedNotBefore *time.Time `json:"observed_not_before"`
	Provider          string     `json:"provider"`
	AuthKind          *string    `json:"auth_kind"`
	AccountHash       string     `json:"account_hash"`
	WindowKey         string     `json:"window_key"`
	RawWindow         string     `json:"raw_window"`
	WindowMinutes     *int64     `json:"window_minutes"`
	UsedPercent       float64    `json:"used_percent"`
	RawValue          string     `json:"raw_value"`
	RawScale          string     `json:"raw_scale"`
	RawDecimals       *int64     `json:"raw_decimals"`
	ResetAt           *time.Time `json:"reset_at"`
	ResetKind         *string    `json:"reset_kind"`
	Status            *string    `json:"status"`
}

const observationColumns = `observation_id, collector_run_id, received_at, source, attempt_event_id, observed_at,
observed_at_quality, observed_not_before, provider, auth_kind, account_hash, window_key, raw_window, window_minutes,
used_percent, raw_value, raw_scale, raw_decimals, reset_at, reset_kind, status`

func (r *observationRow) args() []any {
	return []any{r.ObservationID, r.RunID, r.ReceivedAt, r.Source, r.AttemptEventID, r.ObservedAt,
		r.ObservedAtQuality, r.ObservedNotBefore, r.Provider, r.AuthKind, r.AccountHash, r.WindowKey, r.RawWindow, r.WindowMinutes,
		r.UsedPercent, r.RawValue, r.RawScale, r.RawDecimals, r.ResetAt, r.ResetKind, r.Status}
}

type heartbeatRow struct {
	RunID        string    `json:"run_id"`
	Seq          int64     `json:"seq"`
	At           time.Time `json:"at"`
	Kind         string    `json:"kind"`
	QueueDropped int64     `json:"queue_dropped"`
	SpoolDropped int64     `json:"spool_dropped"`
	Spooled      int64     `json:"spooled"`
	SpoolBytes   int64     `json:"spool_bytes"`
	WriteErrors  int64     `json:"write_errors"`
}

const heartbeatColumns = `run_id, seq, at, kind, queue_dropped, spool_dropped, spooled, spool_bytes, write_errors`

func (r *heartbeatRow) args() []any {
	return []any{r.RunID, r.Seq, r.At, r.Kind, r.QueueDropped, r.SpoolDropped, r.Spooled, r.SpoolBytes, r.WriteErrors}
}

// hashIdentity returns a stable SHA-256 of a secret or personal identifier,
// separated by domain so equal values in different roles do not match.
func hashIdentity(domain, value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	sum := sha256.Sum256([]byte("cliproxy-analytics/v1/" + domain + "\x00" + value))
	out := hex.EncodeToString(sum[:])
	return &out
}

// cleanText makes text safe for PostgreSQL: valid UTF-8 and no NUL bytes.
func cleanText(value string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(value, "\uFFFD"), "\x00", "")
}

func optString(value string) *string {
	value = strings.TrimSpace(cleanText(value))
	if value == "" {
		return nil
	}
	return &value
}

func optInt(value int64) *int64 { return &value }

// optCount keeps a non-negative count; a negative one is stored as unknown.
func optCount(value int64) *int64 {
	if value < 0 {
		return nil
	}
	return &value
}

func optTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

// sanitizeCode keeps short machine-readable identifiers and drops free text.
func sanitizeCode(value string) *string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > 64 {
		return nil
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
			return nil
		}
	}
	return &value
}

func errorTypeAndCode(body string) (*string, *string) {
	if body == "" || len(body) > maxErrorBodyScan || !gjson.Valid(body) {
		return nil, nil
	}
	pick := func(paths ...string) *string {
		for _, path := range paths {
			if v := sanitizeCode(gjson.Get(body, path).String()); v != nil {
				return v
			}
		}
		return nil
	}
	return pick("error.type", "type"), pick("error.code", "code", "error.status")
}

// buildAttempt converts a usage record into an attempt row and its header
// quota observations. The record body, raw keys, emails, auth ID, base URL,
// session IDs and headers outside the allowlist are never copied.
func buildAttempt(ctx context.Context, rec usage.Record, runID string, now time.Time) (*attemptRow, []*observationRow) {
	provider := strings.TrimSpace(cleanText(rec.Provider))
	if provider == "" {
		provider = "unknown"
	}
	row := &attemptRow{
		EventID:         uuid.NewString(),
		RunID:           runID,
		ReceivedAt:      now,
		TraceID:         optString(rec.TraceID),
		AttemptID:       optString(rec.RequestID),
		Provider:        provider,
		ExecutorType:    optString(rec.ExecutorType),
		AuthKind:        optString(rec.AuthType),
		AccountHash:     hashIdentity("auth", rec.AuthID),
		ClientKeyHash:   hashIdentity("client-key", rec.APIKey),
		RequestedModel:  optString(rec.Alias),
		UpstreamModel:   optString(rec.Model),
		ServedModel:     optString(rec.ResponseModel),
		RequestedEffort: optString(usage.ReasoningEffortFromContext(ctx)),
		UpstreamEffort:  optString(rec.ReasoningEffort),
		RequestedTier:   optString(rec.ServiceTier),
		ServedTier:      optString(rec.ResponseServiceTier),
		Stream:          rec.Stream || usage.StreamFromContext(ctx),
		Failed:          rec.Failed,
		Origin:          "unmarked",
	}
	if row.RequestedTier == nil {
		row.RequestedTier = optString(rec.RequestServiceTier)
	}
	if row.ServedTier == nil {
		row.ServedTier = optString(rec.Detail.ResponseServiceTier)
	}
	// Origin is evidence only: "unmarked" does not prove the proxy made the failure.
	if len(rec.ResponseHeaders) > 0 {
		row.Origin = "upstream_response"
	} else if cliproxyexecutor.UpstreamAttempted(ctx) {
		row.Origin = "upstream_no_response"
	}
	if !rec.RequestedAt.IsZero() {
		row.StartedAt = optTime(rec.RequestedAt)
		row.EndedAt = optTime(rec.RequestedAt.Add(rec.Latency))
		row.LatencyMs = optInt(rec.Latency.Milliseconds())
	}
	if rec.TTFT > 0 {
		row.TTFTMs = optInt(rec.TTFT.Milliseconds())
	}
	if rec.Failed {
		if rec.Fail.StatusCode > 0 {
			row.HTTPStatus = optInt(int64(rec.Fail.StatusCode))
		}
		row.ErrorType, row.ErrorCode = errorTypeAndCode(rec.Fail.Body)
		row.FailureClass = optString(rec.Fail.Class)
		row.ClassifierVersion = optInt(cliproxyauth.UsageFailureClassVersion)
	}
	fillTokens(row, rec)

	var observations []*observationRow
	if row.AccountHash != nil {
		observedAt, quality := rec.ResponseHeadersAt, "header_received"
		if observedAt.IsZero() {
			observedAt, quality = now, "attempt_end"
			if row.EndedAt != nil {
				observedAt = *row.EndedAt
			}
		}
		for _, obs := range headerObservations(rec.ResponseHeaders, observedAt) {
			obs.ObservationID = uuid.NewString()
			obs.RunID = runID
			obs.ReceivedAt = now
			obs.Source = "response_header"
			obs.AttemptEventID = &row.EventID
			obs.ObservedAtQuality = quality
			obs.ObservedNotBefore = row.StartedAt
			obs.Provider = provider
			obs.AuthKind = row.AuthKind
			obs.AccountHash = *row.AccountHash
			observations = append(observations, obs)
		}
	}
	return row, observations
}

// fillTokens keeps token absence unknown: an all-zero detail is stored as NULL.
func fillTokens(row *attemptRow, rec usage.Record) {
	d := usage.EnsureTokenBreakdownForProvider(rec.Detail, rec.Provider, rec.ExecutorType)
	b := d.TokenBreakdown
	known := d.InputTokens != 0 || d.OutputTokens != 0 || d.ReasoningTokens != 0 || d.CachedTokens != 0 ||
		d.CacheReadTokens != 0 || d.CacheCreationTokens != 0 || d.TotalTokens != 0 || b.TotalTokens != 0
	row.UsageKnown = known
	if !known {
		return
	}
	row.RawInput, row.RawOutput, row.RawReasoning = optCount(d.InputTokens), optCount(d.OutputTokens), optCount(d.ReasoningTokens)
	row.RawCached, row.RawCacheRead, row.RawCacheCreation = optCount(d.CachedTokens), optCount(d.CacheReadTokens), optCount(d.CacheCreationTokens)
	row.RawTotal = optCount(d.TotalTokens)
	// The TTL split is zero when the upstream did not report it.
	if d.CacheCreation5mTokens != 0 || d.CacheCreation1hTokens != 0 || d.CacheCreationTokens == 0 {
		row.RawCacheCreation5m, row.RawCacheCreation1h = optCount(d.CacheCreation5mTokens), optCount(d.CacheCreation1hTokens)
	}
	if b.Valid() {
		row.AccountingVersion = optCount(int64(b.SchemaVersion))
		row.AccountingQuality = optString(string(b.Quality))
		row.TotalTokens = optCount(b.TotalTokens)
		row.InputUncachedTokens = optCount(b.Input.UncachedTokens)
		row.InputCacheReadTokens = optCount(b.Input.CacheReadTokens)
		row.InputCacheWriteTokens = optCount(b.Input.CacheWriteTokens)
		row.OutputNonReasoningTokens = optCount(b.Output.NonReasoningTokens)
		row.OutputReasoningTokens = optCount(b.Output.ReasoningTokens)
		row.UnclassifiedTokens = optCount(b.UnclassifiedTokens)
	}
}

// parsePercent parses a reported usage value. It returns the normalized
// 0..100 value and the digits after the decimal point in the raw text.
func parsePercent(raw string, fraction bool) (float64, int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 32 {
		return 0, 0, false
	}
	f, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, 0, false
	}
	if fraction {
		f *= 100
	}
	if f > maxUsedPercent {
		return 0, 0, false
	}
	var decimals int64
	if dot := strings.IndexByte(raw, '.'); dot >= 0 {
		decimals = int64(len(raw) - dot - 1)
	}
	return f, decimals, true
}

// headerObservations reads only the allowlisted rate-limit headers:
// anthropic-ratelimit-unified-{5h,7d}-{utilization,reset,status} and
// x-codex-{primary,secondary}-{used-percent,window-minutes,reset-at,reset-after-seconds}.
func headerObservations(h http.Header, observedAt time.Time) []*observationRow {
	if len(h) == 0 {
		return nil
	}
	var out []*observationRow
	for _, w := range []struct{ raw, key string }{{"5h", "short"}, {"7d", "weekly"}} {
		prefix := "Anthropic-Ratelimit-Unified-" + w.raw + "-"
		raw := h.Get(prefix + "Utilization")
		pct, decimals, ok := parsePercent(raw, true)
		if !ok {
			continue
		}
		obs := &observationRow{ObservedAt: observedAt, WindowKey: w.key, RawWindow: w.raw, UsedPercent: pct,
			RawValue: strings.TrimSpace(raw), RawScale: "fraction", RawDecimals: optInt(decimals), Status: sanitizeCode(h.Get(prefix + "Status"))}
		if secs, errParse := strconv.ParseInt(strings.TrimSpace(h.Get(prefix+"Reset")), 10, 64); errParse == nil && secs > 0 {
			obs.ResetAt, obs.ResetKind = optTime(time.Unix(secs, 0).UTC()), optString("absolute")
		}
		out = append(out, obs)
	}

	type codexWindow struct {
		obs     *observationRow
		minutes float64
	}
	codex := func(name string) *codexWindow {
		prefix := "X-Codex-" + name + "-"
		raw := h.Get(prefix + "Used-Percent")
		pct, decimals, ok := parsePercent(raw, false)
		if !ok {
			return nil
		}
		cw := &codexWindow{obs: &observationRow{ObservedAt: observedAt, RawWindow: strings.ToLower(name), UsedPercent: pct,
			RawValue: strings.TrimSpace(raw), RawScale: "percent", RawDecimals: optInt(decimals)}}
		if minutes, errParse := strconv.ParseFloat(strings.TrimSpace(h.Get(prefix+"Window-Minutes")), 64); errParse == nil && minutes > 0 && minutes < 1e7 {
			cw.minutes = minutes
			cw.obs.WindowMinutes = optInt(int64(minutes))
		}
		if secs, errParse := strconv.ParseFloat(strings.TrimSpace(h.Get(prefix+"Reset-At")), 64); errParse == nil && secs > 0 && secs < 1e11 {
			cw.obs.ResetAt, cw.obs.ResetKind = optTime(time.Unix(int64(secs), 0).UTC()), optString("absolute")
		} else if secs, errParse := strconv.ParseFloat(strings.TrimSpace(h.Get(prefix+"Reset-After-Seconds")), 64); errParse == nil && secs >= 0 && secs < 1e9 {
			cw.obs.ResetAt, cw.obs.ResetKind = optTime(observedAt.Add(time.Duration(secs*float64(time.Second))).UTC()), optString("relative")
		}
		return cw
	}
	// Same window mapping as the next-reset selector: the longer window is weekly.
	short, weekly := codex("Primary"), codex("Secondary")
	if short != nil && weekly != nil && short.minutes > weekly.minutes {
		short, weekly = weekly, short
	}
	if weekly == nil && short != nil && short.minutes >= 7*24*60 {
		short, weekly = nil, short
	}
	if short != nil {
		short.obs.WindowKey = "short"
		out = append(out, short.obs)
	}
	if weekly != nil {
		weekly.obs.WindowKey = "weekly"
		out = append(out, weekly.obs)
	}
	return out
}

// pollObservations converts a passive usage poll result into observation rows.
func pollObservations(obs cliproxyauth.QuotaPollObservation, runID string, now time.Time) []*observationRow {
	account := hashIdentity("auth", obs.AuthID)
	if account == nil || obs.ObservedAt.IsZero() {
		return nil
	}
	provider := strings.TrimSpace(cleanText(obs.Provider))
	if provider == "" {
		provider = "unknown"
	}
	var out []*observationRow
	for _, w := range obs.Windows {
		if math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0) || w.UsedPercent < 0 || w.UsedPercent > maxUsedPercent {
			continue
		}
		row := &observationRow{ObservationID: uuid.NewString(), RunID: runID, ReceivedAt: now, Source: "usage_poll",
			ObservedAt: obs.ObservedAt, ObservedAtQuality: "poll", Provider: provider, AuthKind: optString(obs.AuthKind),
			AccountHash: *account, WindowKey: w.Key, RawWindow: "poll", UsedPercent: w.UsedPercent,
			RawValue: strconv.FormatFloat(w.UsedPercent, 'f', -1, 64), RawScale: "percent"}
		if !w.ResetsAt.IsZero() {
			row.ResetAt, row.ResetKind = optTime(w.ResetsAt.UTC()), optString("absolute")
		}
		if w.Rejected {
			row.Status = optString("rejected")
		}
		out = append(out, row)
	}
	return out
}
