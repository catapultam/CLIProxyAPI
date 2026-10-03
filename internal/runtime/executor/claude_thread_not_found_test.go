package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const claudeMissingThreadStateUpstreamBody = `{"type":"error","error":{"type":"not_found_error","message":"No thread state was found for the requested ` + "`previous_message_id`" + `. Replay the full conversation with ` + "`thread: {\\\"type\\\": \\\"create\\\"}`" + ` to start a new Thread."}}`

// assertClaudeThreadNotFoundError checks the error the executor hands back for
// Anthropic's missing-thread 404: still a request-scoped 404, with the original
// type and message, plus error.details.error_code "thread_not_found" so Claude
// Code replays the conversation instead of reporting a model error.
func assertClaudeThreadNotFoundError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var scoped cliproxyexecutor.RequestScopedError
	if !errors.As(err, &scoped) || !scoped.IsRequestScoped() {
		t.Fatalf("error %T is not request-scoped", err)
	}
	var status cliproxyexecutor.StatusError
	if !errors.As(err, &status) || status.StatusCode() != http.StatusNotFound {
		t.Fatalf("status was not preserved: %v", err)
	}
	body := err.Error()
	if got := gjson.Get(body, "error.details.error_code").String(); got != "thread_not_found" {
		t.Fatalf("error.details.error_code = %q, want thread_not_found; body %s", got, body)
	}
	if got, want := gjson.Get(body, "error.type").String(), "not_found_error"; got != want {
		t.Fatalf("error.type = %q, want %q", got, want)
	}
	if got, want := gjson.Get(body, "error.message").String(), gjson.Get(claudeMissingThreadStateUpstreamBody, "error.message").String(); got != want {
		t.Fatalf("error.message = %q, want %q", got, want)
	}
	if got := gjson.Get(body, "type").String(); got != "error" {
		t.Fatalf("type = %q, want error", got)
	}
}

func newClaudeMissingThreadStateServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(claudeMissingThreadStateUpstreamBody))
	}))
	t.Cleanup(server.Close)
	return server
}

func claudeThreadContinuePayload() []byte {
	return []byte(`{"thread":{"type":"continue","previous_message_id":"msg_previous"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
}

func TestClaudeExecutor_MissingThreadStateAddsThreadNotFoundCode_Execute(t *testing.T) {
	server := newClaudeMissingThreadStateServer(t)
	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "claude-thread-not-found", Provider: "claude", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: claudeThreadContinuePayload(),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})

	assertClaudeThreadNotFoundError(t, err)
}

func TestClaudeExecutor_MissingThreadStateAddsThreadNotFoundCode_ExecuteStream(t *testing.T) {
	server := newClaudeMissingThreadStateServer(t)
	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "claude-thread-not-found", Provider: "claude", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}

	// The 404 arrives before any stream is opened, so it is returned directly
	// rather than as a chunk.
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: claudeThreadContinuePayload(),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: true})
	if result != nil {
		t.Fatalf("expected no stream result, got %#v", result)
	}

	assertClaudeThreadNotFoundError(t, err)
}

func TestAnnotateClaudeMissingThreadStateLeavesOtherErrorsAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "model not found", status: http.StatusNotFound, body: `{"type":"error","error":{"type":"not_found_error","message":"model: claude-nonexistent-model"}}`},
		{name: "thread text on another status", status: http.StatusBadRequest, body: claudeMissingThreadStateUpstreamBody},
		{name: "non-object details", status: http.StatusNotFound, body: `{"type":"error","error":{"type":"not_found_error","message":"No thread state was found","details":"opaque"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(annotateClaudeMissingThreadState(tc.status, []byte(tc.body))); got != tc.body {
				t.Fatalf("body was rewritten:\n got  %s\n want %s", got, tc.body)
			}
		})
	}
}

func TestAnnotateClaudeMissingThreadStateKeepsExistingDetails(t *testing.T) {
	body := `{"type":"error","error":{"type":"not_found_error","message":"No thread state was found","details":{"thread_id":"thr_1"}}}`
	got := annotateClaudeMissingThreadState(http.StatusNotFound, []byte(body))
	if code := gjson.GetBytes(got, "error.details.error_code").String(); code != "thread_not_found" {
		t.Fatalf("error_code = %q, want thread_not_found: %s", code, got)
	}
	if id := gjson.GetBytes(got, "error.details.thread_id").String(); id != "thr_1" {
		t.Fatalf("existing details were lost: %s", got)
	}
}
