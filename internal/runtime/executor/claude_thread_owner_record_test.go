package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// newClaudeThreadOwnerUpstream answers like Anthropic with the given message id,
// as JSON or as an SSE stream depending on the request's stream flag, and
// reports whether the thread object reached it.
func newClaudeThreadOwnerUpstream(t *testing.T, messageID string, sawThread *bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*sawThread = gjson.GetBytes(body, "thread").IsObject()
		if gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, strings.Join([]string{
				`event: message_start`,
				`data: {"type":"message_start","message":{"id":"` + messageID + `","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
				``,
				`event: content_block_start`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				``,
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
				``,
				`event: content_block_stop`,
				`data: {"type":"content_block_stop","index":0}`,
				``,
				`event: message_delta`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				``,
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				``,
				``,
			}, "\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"`+messageID+`","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func runClaudeThreadOwnerRequest(t *testing.T, stream bool, auth *cliproxyauth.Auth, payload string) {
	t.Helper()
	executor := NewClaudeExecutor(&config.Config{})
	req := cliproxyexecutor.Request{Model: "claude-3-5-sonnet-20241022", Payload: []byte(payload)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: []byte(payload), Stream: stream}
	if !stream {
		if _, errExecute := executor.Execute(context.Background(), auth, req, opts); errExecute != nil {
			t.Fatalf("execute: %v", errExecute)
		}
		return
	}
	result, errStream := executor.ExecuteStream(context.Background(), auth, req, opts)
	if errStream != nil {
		t.Fatalf("execute stream: %v", errStream)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}
}

// A successful thread response records its message id against the credential
// that served it, for both upstream response shapes.
func TestClaudeExecutorRecordsThreadOwner(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := map[bool]string{false: "non-streaming", true: "streaming"}[stream]
		t.Run(name, func(t *testing.T) {
			messageID := "msg_thread_owner_record_" + strings.ReplaceAll(name, "-", "_")
			sawThread := false
			server := newClaudeThreadOwnerUpstream(t, messageID, &sawThread)
			auth := &cliproxyauth.Auth{ID: "claude-thread-owner-record-" + name, Provider: "claude", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
			payload := `{"model":"claude-3-5-sonnet-20241022","max_tokens":16,"stream":` + map[bool]string{false: "false", true: "true"}[stream] +
				`,"thread":{"type":"create"},"messages":[{"role":"user","content":"hi"}]}`

			runClaudeThreadOwnerRequest(t, stream, auth, payload)

			if !sawThread {
				t.Fatal("thread object did not reach the upstream")
			}
			if got := cliproxyauth.ClaudeThreadOwner(messageID); got != auth.ID {
				t.Fatalf("owner(%s) = %q, want %q", messageID, got, auth.ID)
			}
		})
	}
}

// Responses to requests without a thread are not recorded: nothing can continue
// from them, so they would only grow the store.
func TestClaudeExecutorSkipsThreadOwnerWithoutThread(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := map[bool]string{false: "non-streaming", true: "streaming"}[stream]
		t.Run(name, func(t *testing.T) {
			messageID := "msg_no_thread_" + strings.ReplaceAll(name, "-", "_")
			sawThread := false
			server := newClaudeThreadOwnerUpstream(t, messageID, &sawThread)
			auth := &cliproxyauth.Auth{ID: "claude-no-thread-" + name, Provider: "claude", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
			payload := `{"model":"claude-3-5-sonnet-20241022","max_tokens":16,"stream":` + map[bool]string{false: "false", true: "true"}[stream] +
				`,"messages":[{"role":"user","content":"hi"}]}`

			runClaudeThreadOwnerRequest(t, stream, auth, payload)

			if got := cliproxyauth.ClaudeThreadOwner(messageID); got != "" {
				t.Fatalf("owner(%s) = %q, want none for a request without a thread", messageID, got)
			}
		})
	}
}
