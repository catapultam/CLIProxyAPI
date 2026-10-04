package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func runSafeguardStream(t *testing.T, payload, streamData string) (upstreamBody, client string) {
	t.Helper()
	bodies := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies <- string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamData))
	}))
	defer server.Close()

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-123", "base_url": server.URL}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(payload)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var b strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		b.Write(chunk.Payload)
	}
	return <-bodies, b.String()
}

const safeguardStreamWithoutResults = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-opus-5","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01ABC","name":"Bash","input":{}}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func messageDeltaPayload(t *testing.T, client string) string {
	t.Helper()
	for _, line := range strings.Split(client, "\n") {
		if strings.HasPrefix(line, "data:") && strings.Contains(line, `"message_delta"`) {
			return strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	t.Fatalf("no message_delta in client stream:\n%s", client)
	return ""
}

func TestClaudeExecutor_ExecuteStream_FillsMissingSafeguardResults(t *testing.T) {
	payload := `{"model":"claude-opus-5","safeguards":[{"type":"dangerous_tool_use"}],"messages":[{"role":"user","content":"hi"}],"stream":true}`
	upstream, client := runSafeguardStream(t, payload, safeguardStreamWithoutResults)
	if !gjson.Get(upstream, "safeguards").Exists() {
		t.Fatalf("safeguards must reach the upstream: %s", upstream)
	}
	delta := messageDeltaPayload(t, client)
	call := gjson.Get(delta, "delta.safeguard_results.0.status.tool_uses.toolu_01ABC")
	if call.Get("type").String() != "unavailable" || call.Get("reason").String() != "error" {
		t.Fatalf("want an unavailable/error result for the tool call, got %s", delta)
	}
	if !strings.Contains(client, `"toolu_01ABC"`) || !strings.Contains(client, "message_stop") {
		t.Fatalf("stream content changed:\n%s", client)
	}
}

func TestClaudeExecutor_ExecuteStream_PassesUpstreamSafeguardResults(t *testing.T) {
	payload := `{"model":"claude-opus-5","safeguards":[{"type":"dangerous_tool_use"}],"messages":[{"role":"user","content":"hi"}],"stream":true}`
	withResults := strings.Replace(safeguardStreamWithoutResults,
		`"delta":{"stop_reason":"tool_use"}`,
		`"delta":{"stop_reason":"tool_use","safeguard_results":[{"type":"dangerous_tool_use","status":{"type":"available","tool_uses":{"toolu_01ABC":{"type":"evaluated","outcome":"not_flagged"}}}}]}`, 1)
	_, client := runSafeguardStream(t, payload, withResults)
	delta := messageDeltaPayload(t, client)
	if got := gjson.Get(delta, "delta.safeguard_results.0.status.tool_uses.toolu_01ABC.outcome").String(); got != "not_flagged" {
		t.Fatalf("upstream verdict must pass through unchanged, got %s", delta)
	}
}

func TestClaudeExecutor_ExecuteStream_NoSafeguardsNoFill(t *testing.T) {
	payload := `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":true}`
	_, client := runSafeguardStream(t, payload, safeguardStreamWithoutResults)
	if strings.Contains(client, "safeguard_results") {
		t.Fatalf("no fill without safeguards in the request:\n%s", client)
	}
}
