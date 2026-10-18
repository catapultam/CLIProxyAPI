package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

type claudeUserAPITransport struct {
	t      *testing.T
	called bool
}

func (tr *claudeUserAPITransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.called = true
	if req.Header.Get("X-Api-Key") != "sk-ant-usr-test" {
		tr.t.Error("user API credential was not preserved")
	}
	if strings.Contains(req.Header.Get("User-Agent"), "claude-cli/") || req.Header.Get("X-App") == "cli" || strings.Contains(req.Header.Get("Anthropic-Beta"), "claude-code-") {
		tr.t.Error("user API request retained Claude Code billing headers")
	}
	body, errRead := io.ReadAll(req.Body)
	if errRead != nil {
		return nil, errRead
	}
	if strings.Contains(string(body), "x-anthropic-billing-header:") || strings.Contains(string(body), claudeCodeCLIIdentity) || strings.Contains(string(body), "You are a Claude agent,") || strings.Contains(string(body), "You are an agent for Claude Code,") {
		tr.t.Error("user API request retained Claude Code billing system blocks")
	}
	if !strings.Contains(string(body), "Keep this application rule.") || !strings.Contains(string(body), "Hello.") {
		tr.t.Error("application instructions or conversation content were lost")
	}
	payload := `{"id":"msg_test","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"model":"claude-sonnet-5-5","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	contentType := "application/json"
	if strings.Contains(req.URL.Path, "count_tokens") {
		payload = `{"input_tokens":1}`
	} else if gjson.GetBytes(body, "stream").Bool() {
		payload = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		contentType = "text/event-stream"
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(payload)), Request: req}, nil
}

func TestClaudeUserAPINormalizesEveryRequestPath(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "count"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "sk-ant-usr-test", Cloak: &config.CloakConfig{StrictMode: false}}}}
			exec := NewClaudeExecutor(cfg)
			auth := &cliproxyauth.Auth{ID: "user-api", Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-usr-test", "auth_kind": "apikey", "config_index": "0"}}
			transport := &claudeUserAPITransport{t: t}
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
			req := cliproxyexecutor.Request{Model: "claude-sonnet-5-5", Payload: []byte(`{"model":"claude-sonnet-5-5","max_tokens":8,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.280;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"You are a Claude agent, built on Anthropic's Claude Agent SDK."},{"type":"text","text":"You are an agent for Claude Code, Anthropic's official CLI for Claude. Keep this application rule."}],"messages":[{"role":"user","content":"Hello."}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"User-Agent": []string{"claude-cli/2.1.280 (external, cli)"}, "X-App": []string{"cli"}, "Anthropic-Beta": []string{"claude-code-20250219,interleaved-thinking-2025-05-14"}}}
			var err error
			switch mode {
			case "execute":
				_, err = exec.Execute(ctx, auth, req, opts)
			case "count":
				_, err = exec.CountTokens(ctx, auth, req, opts)
			case "stream":
				var result *cliproxyexecutor.StreamResult
				result, err = exec.ExecuteStream(ctx, auth, req, opts)
				if err == nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if !transport.called {
				t.Fatal("upstream request was not made")
			}
		})
	}
}

func TestClaudeUserAPIPreservesExplicitCLIProfile(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-usr-test", "auth_kind": "apikey", "fingerprint_profile": "claude-code-cli"}}
	policy, _ := resolveClaudeWirePolicy(&config.Config{}, auth, "sk-ant-usr-test", false)
	if !policy.Cloak || !policy.ProfileClaudeCodeCLI {
		t.Fatal("explicit CLI fingerprint profile was not preserved")
	}
}
