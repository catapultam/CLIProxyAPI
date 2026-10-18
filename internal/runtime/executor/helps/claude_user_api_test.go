package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeClaudeUserAPISubagentSystem(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{"SDK identity string", `{"system":"You are a Claude agent, built on Anthropic's Claude Agent SDK."}`, ""},
		{"subagent instructions string", `{"system":"You are an agent for Claude Code, Anthropic's official CLI for Claude. Keep the user's task."}`, "You are a coding assistant. Keep the user's task."},
		{"combined main instructions", `{"system":"You are Claude Code, Anthropic's official CLI for Claude.\nKeep the user's task."}`, "You are a coding assistant.\nKeep the user's task."},
		{"ordinary mention", `{"system":"Explain how Claude Code works."}`, "Explain how Claude Code works."},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := NormalizeClaudeUserAPIBody([]byte(test.body))
			if text := gjson.GetBytes(got, "system").String(); text != test.want {
				t.Fatalf("system = %q, want %q", text, test.want)
			}
		})
	}
	body := []byte(`{"system":[{"type":"text","text":"You are a Claude agent, built on Anthropic's Claude Agent SDK."},{"type":"text","text":"You are an agent for Claude Code, Anthropic's official CLI for Claude. Keep the user's task.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"Explain Claude Code."}],"tools":[{"name":"read_file"}]}`)
	got := NormalizeClaudeUserAPIBody(body)
	if gjson.GetBytes(got, "system.#").Int() != 1 || gjson.GetBytes(got, "system.0.text").String() != "You are a coding assistant. Keep the user's task." {
		t.Fatalf("subagent system was not normalized: %s", got)
	}
	if gjson.GetBytes(got, "system.0.cache_control.type").String() != "ephemeral" || gjson.GetBytes(got, "messages").Raw != gjson.GetBytes(body, "messages").Raw || gjson.GetBytes(got, "tools").Raw != gjson.GetBytes(body, "tools").Raw {
		t.Fatal("cache controls, conversation, or tools were changed")
	}
}
