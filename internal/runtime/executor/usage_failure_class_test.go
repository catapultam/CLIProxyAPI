package executor

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// recordingPlugin captures records from the shared usage manager.
type recordingPlugin struct{ records chan usage.Record }

func (p *recordingPlugin) HandleUsage(_ context.Context, rec usage.Record) {
	select {
	case p.records <- rec:
	default:
	}
}

// The analytics failure class comes from the existing auth classifiers applied
// to the real executor error types.
func TestUsageFailureClassOnExecutorErrors(t *testing.T) {
	rejected5h := http.Header{}
	rejected5h.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
	codexStreamed, _, okStreamed := codexTerminalFailureErr([]byte(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"limit"}}}`))
	if !okStreamed {
		t.Fatal("codex terminal event not parsed")
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"claude 429 with 5h rejection", classifyClaudeUpstreamError(429, rejected5h, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"x"}}`)), "credential_quota"},
		{"claude plain 429", classifyClaudeUpstreamError(429, http.Header{}, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"x"}}`)), "rate_limit_unscoped"},
		{"claude 429 fast mode credits", classifyClaudeUpstreamError(429, http.Header{}, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for fast mode"}}`)), "request_scoped"},
		{"claude 400 billing", classifyClaudeUpstreamError(400, http.Header{}, []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low"}}`)), "billing"},
		{"codex 429 usage limit", newCodexStatusErr(429, []byte(`{"error":{"type":"usage_limit_reached","message":"x"}}`)), "credential_quota"},
		{"codex 429 rate limit", newCodexStatusErr(429, []byte(`{"error":{"type":"rate_limit_exceeded","message":"x"}}`)), "rate_limit_unscoped"},
		{"codex HTTP 200 streamed usage limit", codexStreamed, "credential_quota"},
		{"server error", newCodexStatusErr(503, []byte(`{"error":{"message":"busy"}}`)), ""},
		{"transport", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, "transient_transport"},
		{"canceled", context.Canceled, ""},
	}
	for _, tc := range cases {
		if got := cliproxyauth.UsageFailureClass(tc.err); got != tc.want {
			t.Errorf("%s: class %q, want %q", tc.name, got, tc.want)
		}
	}
}

// PublishFailure carries the class into the usage record seen by plugins.
func TestPublishFailureCarriesClass(t *testing.T) {
	plugin := &recordingPlugin{records: make(chan usage.Record, 1)}
	usage.RegisterNamedPlugin("test-usage-failure-class", plugin)
	reporter := helps.NewUsageReporter(context.Background(), "codex", "gpt-x", nil)
	reporter.PublishFailure(context.Background(), newCodexStatusErr(429, []byte(`{"error":{"type":"usage_limit_reached"}}`)))
	var rec usage.Record
	select {
	case rec = <-plugin.records:
	case <-time.After(10 * time.Second):
		t.Fatal("no usage record delivered")
	}
	if rec.Fail.Class != "credential_quota" || rec.Fail.StatusCode != 429 {
		t.Fatalf("record failure %+v", rec.Fail)
	}
}
