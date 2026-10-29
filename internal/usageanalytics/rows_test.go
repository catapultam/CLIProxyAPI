package usageanalytics

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestBuildAttemptKeepsSecretsOut(t *testing.T) {
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rec := usage.Record{
		RequestID: "attempt-1", TraceID: "0192f000-0000-7000-8000-000000000001",
		Provider: "claude", ExecutorType: "ClaudeExecutor", Model: "claude-x", Alias: "alias-x",
		APIKey: "sk-client-SECRET", AuthID: "claude-user@example.com.json", AuthIndex: "idx",
		AccessTokenSHA256: "tokhash", Source: "user@example.com", BaseURL: "https://secret.example.com/v1",
		SessionID: "sess-SECRET", AuthType: "oauth", RequestedAt: start, Latency: 3 * time.Second,
		Failed: true,
		Fail: usage.Failure{StatusCode: 429, Class: "credential_quota",
			Body: `{"error":{"type":"rate_limit_error","code":"usage_limit_reached","message":"user@example.com SECRET text"}}`},
		ResponseHeaders: http.Header{"Authorization": {"Bearer SECRET"}, "X-Request-Id": {"SECRET-REQ"}},
	}
	row, _ := buildAttempt(context.Background(), rec, "run", start)
	raw, errMarshal := json.Marshal(item{A: row})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	text := string(raw)
	for _, secret := range []string{"SECRET", "example.com", "claude-user", "tokhash", "secret.example"} {
		if strings.Contains(text, secret) {
			t.Fatalf("row leaks %q: %s", secret, text)
		}
	}
	if row.AccountHash == nil || len(*row.AccountHash) != 64 || row.ClientKeyHash == nil || *row.AccountHash == *row.ClientKeyHash {
		t.Fatalf("hashes: %v %v", row.AccountHash, row.ClientKeyHash)
	}
	if *row.ErrorType != "rate_limit_error" || *row.ErrorCode != "usage_limit_reached" || *row.FailureClass != "credential_quota" {
		t.Fatalf("error fields: %v %v %v", *row.ErrorType, *row.ErrorCode, *row.FailureClass)
	}
	if row.Origin != "upstream_response" || *row.LatencyMs != 3000 || !row.EndedAt.Equal(start.Add(3*time.Second)) {
		t.Fatalf("origin/timing: %s %v %v", row.Origin, *row.LatencyMs, row.EndedAt)
	}
}

func TestErrorTypeRejectsFreeText(t *testing.T) {
	typ, code := errorTypeAndCode(`{"error":{"type":"Bad Thing happened","code":"x@y"}}`)
	if typ != nil || code != nil {
		t.Fatalf("free text kept: %v %v", typ, code)
	}
	typ, code = errorTypeAndCode("not json at all")
	if typ != nil || code != nil {
		t.Fatal("non-JSON body produced fields")
	}
}

func TestUnknownTokensStayNull(t *testing.T) {
	row, _ := buildAttempt(context.Background(), usage.Record{Provider: "codex", AuthID: "a"}, "run", time.Now())
	if row.UsageKnown || row.RawInput != nil || row.TotalTokens != nil || row.AccountingQuality != nil {
		t.Fatalf("unknown usage stored as values: %+v", row)
	}

	rec := usage.Record{Provider: "claude", Detail: usage.Detail{InputTokens: 10, OutputTokens: 5, CacheCreationTokens: 4}}
	row, _ = buildAttempt(context.Background(), rec, "run", time.Now())
	if !row.UsageKnown || *row.TotalTokens != 19 || *row.InputCacheWriteTokens != 4 {
		t.Fatalf("known usage: %+v", row)
	}
	if row.RawCacheCreation5m != nil || row.RawCacheCreation1h != nil {
		t.Fatal("unreported cache TTL split must stay unknown")
	}
	rec.Detail.CacheCreation1hTokens = 4
	row, _ = buildAttempt(context.Background(), rec, "run", time.Now())
	if row.RawCacheCreation1h == nil || *row.RawCacheCreation1h != 4 || *row.RawCacheCreation5m != 0 {
		t.Fatal("reported TTL split lost")
	}
}

func TestOriginEvidence(t *testing.T) {
	row, _ := buildAttempt(context.Background(), usage.Record{Failed: true}, "run", time.Now())
	if row.Origin != "unmarked" {
		t.Fatalf("origin %s", row.Origin)
	}
	ctx := cliproxyexecutor.WithUpstreamAttemptTracker(context.Background())
	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	row, _ = buildAttempt(ctx, usage.Record{Failed: true}, "run", time.Now())
	if row.Origin != "upstream_no_response" {
		t.Fatalf("origin %s", row.Origin)
	}
}

func TestHeaderObservations(t *testing.T) {
	headersAt := time.Date(2026, 10, 1, 12, 0, 1, 0, time.UTC)
	rec := usage.Record{
		Provider: "claude", AuthID: "acct", RequestedAt: headersAt.Add(-time.Second), Latency: time.Minute,
		ResponseHeadersAt: headersAt,
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.42"},
			"Anthropic-Ratelimit-Unified-5h-Reset":       {"1790000000"},
			"Anthropic-Ratelimit-Unified-5h-Status":      {"allowed"},
			"Anthropic-Ratelimit-Unified-7d-Utilization": {"bogus"},
		},
	}
	row, obs := buildAttempt(context.Background(), rec, "run", headersAt.Add(time.Minute))
	if len(obs) != 1 {
		t.Fatalf("observations: %d", len(obs))
	}
	o := obs[0]
	if o.WindowKey != "short" || o.UsedPercent != 42 || o.RawValue != "0.42" || o.RawScale != "fraction" || *o.RawDecimals != 2 ||
		!o.ObservedAt.Equal(headersAt) || o.ObservedAtQuality != "header_received" || *o.AttemptEventID != row.EventID ||
		o.ResetAt.Unix() != 1790000000 || *o.Status != "allowed" || o.AccountHash != *row.AccountHash {
		t.Fatalf("claude observation: %+v", o)
	}

	// Codex: the longer window is weekly; receive time unknown falls back to attempt end.
	rec = usage.Record{Provider: "codex", AuthID: "acct", RequestedAt: headersAt, Latency: time.Second,
		ResponseHeaders: http.Header{
			"X-Codex-Primary-Used-Percent":           {"80"},
			"X-Codex-Primary-Window-Minutes":         {"10080"},
			"X-Codex-Primary-Reset-After-Seconds":    {"60"},
			"X-Codex-Secondary-Used-Percent":         {"5.5"},
			"X-Codex-Secondary-Window-Minutes":       {"300"},
			"X-Codex-Secondary-Reset-At":             {"1790000000"},
			"X-Codex-Bengalfox-Primary-Used-Percent": {"99"},
		},
	}
	_, obs = buildAttempt(context.Background(), rec, "run", time.Now())
	if len(obs) != 2 {
		t.Fatalf("codex observations: %d", len(obs))
	}
	byKey := map[string]*observationRow{obs[0].WindowKey: obs[0], obs[1].WindowKey: obs[1]}
	if byKey["weekly"].RawWindow != "primary" || byKey["weekly"].UsedPercent != 80 || *byKey["weekly"].ResetKind != "relative" ||
		byKey["short"].RawWindow != "secondary" || *byKey["short"].WindowMinutes != 300 || byKey["short"].ObservedAtQuality != "attempt_end" ||
		!byKey["short"].ObservedAt.Equal(headersAt.Add(time.Second)) {
		t.Fatalf("codex mapping: %+v %+v", byKey["weekly"], byKey["short"])
	}

	// No account: no observation can be joined, so none is kept.
	rec.AuthID = ""
	if _, obs = buildAttempt(context.Background(), rec, "run", time.Now()); len(obs) != 0 {
		t.Fatal("observation without account")
	}
}
