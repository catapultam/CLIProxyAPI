package usageanalytics

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// TestPostgresIntegration needs a database with schema.sql, views.sql and
// grants.sql applied:
//
//	CLIPROXY_ANALYTICS_TEST_PG_DSN       writer role (INSERT only)
//	CLIPROXY_ANALYTICS_TEST_PG_READER_DSN reader role (SELECT)
func TestPostgresIntegration(t *testing.T) {
	writerDSN, readerDSN := os.Getenv("CLIPROXY_ANALYTICS_TEST_PG_DSN"), os.Getenv("CLIPROXY_ANALYTICS_TEST_PG_READER_DSN")
	if writerDSN == "" || readerDSN == "" {
		t.Skip("set CLIPROXY_ANALYTICS_TEST_PG_DSN and CLIPROXY_ANALYTICS_TEST_PG_READER_DSN")
	}
	writer, errOpen := sql.Open("pgx", writerDSN)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	defer func() { _ = writer.Close() }()
	reader, errOpen := sql.Open("pgx", readerDSN)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	defer func() { _ = reader.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	account := uuid.NewString()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(time.Now().UnixNano() % int64(24*time.Hour)))
	reset, nextReset := base.Add(5*time.Hour), base.Add(10*time.Hour)
	obsFor := func(acct, window string, at time.Time, pct float64, resetAt time.Time) item {
		return item{O: &observationRow{ObservationID: uuid.NewString(), RunID: "00000000-0000-0000-0000-000000000001",
			ReceivedAt: at, Source: "response_header", ObservedAt: at, ObservedAtQuality: "header_received",
			Provider: "claude", AccountHash: acct, WindowKey: window, RawWindow: "5h", UsedPercent: pct,
			RawValue: "x", RawScale: "fraction", RawDecimals: optInt(2), ResetAt: &resetAt}}
	}
	obs := func(at time.Time, pct float64, resetAt time.Time) item {
		return obsFor(account, "short", at, pct, resetAt)
	}
	attemptFor := func(acct string, end time.Time, tokens int64) item {
		start := end.Add(-time.Second)
		return item{A: &attemptRow{EventID: uuid.NewString(), RunID: "00000000-0000-0000-0000-000000000001",
			ReceivedAt: end, Provider: "claude", AccountHash: &acct, StartedAt: &start, EndedAt: &end,
			Origin: "upstream_response", UsageKnown: true, TotalTokens: optInt(tokens), InputUncachedTokens: optInt(tokens)}}
	}
	attempt := func(end time.Time, tokens int64) item { return attemptFor(account, end, tokens) }
	failed := attempt(base.Add(150*time.Second), 0)
	failed.A.Failed, failed.A.UsageKnown, failed.A.TotalTokens, failed.A.InputUncachedTokens = true, false, nil, nil
	jitter := uuid.NewString() // 42 -> 43 -> 42 -> 43 in one window: net +1, not +2
	fable := uuid.NewString()
	items := []item{
		obs(base, 10, reset),
		attempt(base.Add(1*time.Minute), 40), attempt(base.Add(2*time.Minute), 60),
		failed,
		obs(base.Add(3*time.Minute), 10, reset), // zero delta: traffic must still count
		attempt(base.Add(4*time.Minute), 50),
		obs(base.Add(5*time.Minute), 12, reset),
		attempt(base.Add(6*time.Minute), 999),
		obs(base.Add(7*time.Minute), 1, nextReset), // new window: not in the span
		obsFor(jitter, "short", base, 42, reset), attemptFor(jitter, base.Add(time.Minute), 10),
		obsFor(jitter, "short", base.Add(2*time.Minute), 43, reset), attemptFor(jitter, base.Add(3*time.Minute), 10),
		obsFor(jitter, "short", base.Add(4*time.Minute), 42, reset), attemptFor(jitter, base.Add(5*time.Minute), 10),
		obsFor(jitter, "short", base.Add(6*time.Minute), 43, reset),
		obsFor(fable, "fable_weekly", base, 1, reset), attemptFor(fable, base.Add(time.Minute), 10),
		obsFor(fable, "fable_weekly", base.Add(2*time.Minute), 2, reset),
		{H: &heartbeatRow{RunID: uuid.NewString(), Seq: 1, At: base, Kind: "start"}},
	}
	for i := 0; i < 2; i++ { // the second insert is a replay and must be a no-op
		if errInsert := insertItems(ctx, writer, items); errInsert != nil {
			t.Fatalf("insert %d: %v", i, errInsert)
		}
	}

	var attempts, successful, zeroIntervals int64
	var delta, tokensPerPP, successPerPP float64
	errScan := reader.QueryRowContext(ctx, `SELECT attempts, successful_attempts, zero_delta_intervals, delta_percent,
		total_tokens_per_pp, successful_attempts_per_pp
		FROM subscription_analytics.capacity_spans WHERE account_hash = $1`, account).
		Scan(&attempts, &successful, &zeroIntervals, &delta, &tokensPerPP, &successPerPP)
	if errScan != nil {
		t.Fatal(errScan)
	}
	if attempts != 4 || successful != 3 || zeroIntervals != 1 || delta != 2 || tokensPerPP != 75 || successPerPP != 1.5 {
		t.Fatalf("span: attempts %d ok %d zero %d delta %v tokens/pp %v ok/pp %v", attempts, successful, zeroIntervals, delta, tokensPerPP, successPerPP)
	}
	var negatives int64
	if errScan = reader.QueryRowContext(ctx, `SELECT delta_percent, negative_delta_intervals, total_tokens_per_pp
		FROM subscription_analytics.capacity_spans WHERE account_hash = $1`, jitter).Scan(&delta, &negatives, &tokensPerPP); errScan != nil {
		t.Fatal(errScan)
	}
	if delta != 1 || negatives != 1 || tokensPerPP != 30 {
		t.Fatalf("jitter span: delta %v negatives %d tokens/pp %v", delta, negatives, tokensPerPP)
	}
	var fableSpans int
	if errScan = reader.QueryRowContext(ctx, `SELECT count(*) FROM subscription_analytics.capacity_spans WHERE account_hash = $1`,
		fable).Scan(&fableSpans); errScan != nil || fableSpans != 0 {
		t.Fatalf("fable window must not form ratio spans: %d %v", fableSpans, errScan)
	}
	var pairing string
	if errScan = reader.QueryRowContext(ctx, `SELECT pairing FROM subscription_analytics.quota_intervals
		WHERE account_hash = $1 ORDER BY observed_at DESC LIMIT 1`, account).Scan(&pairing); errScan != nil || pairing != "different_reset" {
		t.Fatalf("last pairing %q %v", pairing, errScan)
	}
	for _, view := range []string{"usage_hourly", "usage_daily", "error_categories_daily", "collector_health", "collector_gaps", "capacity_daily", "capacity_span_model_mix"} {
		rows, errQuery := reader.QueryContext(ctx, "SELECT * FROM subscription_analytics."+view+" LIMIT 1")
		if errQuery != nil {
			t.Fatalf("%s: %v", view, errQuery)
		}
		_ = rows.Close()
	}
	// Full sink path: Open, Start, HandleUsage, Close.
	sink, errSink := Open(writerDSN, t.TempDir())
	if errSink != nil {
		t.Fatal(errSink)
	}
	sink.Start()
	sink.HandleUsage(context.Background(), usage.Record{Provider: "codex", AuthID: account})
	if !sink.Close(ctx) {
		t.Fatal("sink close did not finish")
	}
	var beats int
	if errScan = reader.QueryRowContext(ctx, `SELECT count(*) FROM subscription_analytics.collector_heartbeats WHERE run_id = $1`,
		sink.runID).Scan(&beats); errScan != nil || beats != 2 {
		t.Fatalf("heartbeats %d %v", beats, errScan)
	}

	// The writer role has no SELECT.
	if _, errSelect := writer.ExecContext(ctx, "SELECT 1 FROM subscription_analytics.attempts LIMIT 1"); errSelect == nil {
		t.Fatal("writer role can read")
	}
}
