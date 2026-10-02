package claude

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToCodexDropsUnsupportedServerTools(t *testing.T) {
	in := []byte(`{
		"model":"gpt-5.5",
		"system":[{"type":"text","text":"You are Claude Code.\n\n# Advisor Tool\n\nCall advisor() before substantive work.\n\n# Environment\nPlatform: linux"}],
		"tools":[
			{"name":"Bash","description":"run","input_schema":{"type":"object","properties":{}}},
			{"type":"advisor_20260301","name":"advisor","model":"claude-opus-5-5"},
			{"type":"web_search_20250305","name":"web_search"}
		],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	out := ConvertClaudeRequestToCodex("gpt-5.5", in, true)

	var names []string
	for _, tool := range gjson.GetBytes(out, "tools").Array() {
		names = append(names, tool.Get("type").String()+":"+tool.Get("name").String())
	}
	joined := strings.Join(names, ",")
	if strings.Contains(joined, "advisor") {
		t.Fatalf("advisor server tool forwarded to codex: %s", joined)
	}
	if !strings.Contains(joined, "function:Bash") || !strings.Contains(joined, "web_search:") {
		t.Fatalf("regular or web search tool lost: %s", joined)
	}
	sys := gjson.GetBytes(out, "input.0.content.0.text").String()
	if strings.Contains(sys, "Advisor Tool") || strings.Contains(sys, "advisor()") {
		t.Fatalf("advisor instructions kept: %q", sys)
	}
	if !strings.Contains(sys, "You are Claude Code.") || !strings.Contains(sys, "# Environment\nPlatform: linux") {
		t.Fatalf("other system text lost: %q", sys)
	}
}

func TestConvertClaudeRequestToCodexKeepsAdvisorTextWithoutAdvisorTool(t *testing.T) {
	in := []byte(`{"model":"gpt-5.5","system":"# Advisor Tool\nnotes","tools":[{"name":"Bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`)
	out := ConvertClaudeRequestToCodex("gpt-5.5", in, true)
	if !strings.Contains(gjson.GetBytes(out, "input.0.content.0.text").String(), "# Advisor Tool") {
		t.Fatal("system text changed although no advisor tool was declared")
	}
}

func TestConvertClaudeRequestToCodexExposesAdvisorWhenEmulated(t *testing.T) {
	in := []byte(`{
		"model":"gpt-6.1-sol",
		"cpa_advisor_emulation":true,
		"system":[{"type":"text","text":"Intro.\n\n# Advisor Tool\n\nCall advisor() first.\n\n# Env\nx"}],
		"tools":[{"type":"advisor_20260301","name":"advisor","model":"opus"},{"name":"Bash","input_schema":{"type":"object","properties":{}}}],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	out := ConvertClaudeRequestToCodex("gpt-6.1-sol", in, true)
	var advisor gjson.Result
	for _, tool := range gjson.GetBytes(out, "tools").Array() {
		if tool.Get("name").String() == "advisor" {
			advisor = tool
		}
	}
	if !advisor.Exists() || advisor.Get("type").String() != "function" || advisor.Get("parameters.type").String() != "object" {
		t.Fatalf("advisor not exposed as a function: %s", gjson.GetBytes(out, "tools").Raw)
	}
	if advisor.Get("model").Exists() {
		t.Fatalf("advisor function carries the server tool model field: %s", advisor.Raw)
	}
	if !strings.Contains(gjson.GetBytes(out, "input.0.content.0.text").String(), "# Advisor Tool") {
		t.Fatal("advisor instructions stripped although emulation is on")
	}
	if gjson.GetBytes(out, "cpa_advisor_emulation").Exists() {
		t.Fatal("marker leaked into the codex body")
	}
}
