package helps

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const safeguardsRequest = `{"model":"claude-opus-5-5","safeguards":[{"type":"dangerous_tool_use"}],"messages":[]}`

func feedLines(t *testing.T, f *ClaudeSafeguardFiller, lines []string) []string {
	t.Helper()
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, string(f.FillStreamLine([]byte(l))))
	}
	return out
}

func toolUsesOf(t *testing.T, dataLine string) gjson.Result {
	t.Helper()
	payload := strings.TrimSpace(strings.TrimPrefix(dataLine, "data:"))
	res := gjson.Get(payload, "delta.safeguard_results")
	if !res.Exists() {
		t.Fatalf("no safeguard_results in %s", dataLine)
	}
	if n := len(res.Array()); n != 1 {
		t.Fatalf("want 1 safeguard result, got %d", n)
	}
	if got := res.Get("0.type").String(); got != "dangerous_tool_use" {
		t.Fatalf("type = %q", got)
	}
	if got := res.Get("0.status.type").String(); got != "available" {
		t.Fatalf("status.type = %q", got)
	}
	return res.Get("0.status.tool_uses")
}

func TestSafeguardFillerStreamMissingResults(t *testing.T) {
	f := NewClaudeSafeguardFiller([]byte(safeguardsRequest))
	out := feedLines(t, f, []string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5-5","content":[]}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_A","name":"Bash","input":{}}}`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_B","name":"Read","input":{}}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":5}}`,
		`data: {"type":"message_stop"}`,
	})
	if !f.Filled() {
		t.Fatal("expected the filler to fill")
	}
	tu := toolUsesOf(t, out[7])
	for _, id := range []string{"toolu_A", "toolu_B"} {
		v := tu.Get(id)
		if v.Get("type").String() != "unavailable" || v.Get("reason").String() != "error" {
			t.Fatalf("%s = %s, want unavailable/error", id, v.Raw)
		}
	}
	if n := len(tu.Map()); n != 2 {
		t.Fatalf("want 2 tool uses, got %d", n)
	}
	// The filled result never marks a call as evaluated or allowed.
	if strings.Contains(out[7], "evaluated") || strings.Contains(out[7], "not_flagged") {
		t.Fatalf("filled result must not carry a verdict: %s", out[7])
	}
	// Everything else is unchanged.
	if gjson.Get(strings.TrimPrefix(out[7], "data: "), "usage.output_tokens").Int() != 5 {
		t.Fatalf("usage lost: %s", out[7])
	}
	for i, l := range out {
		if i != 7 && l != []string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5-5","content":[]}}`,
			``,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_A","name":"Bash","input":{}}}`,
			`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_B","name":"Read","input":{}}}`,
			`event: message_delta`,
			``,
			`data: {"type":"message_stop"}`,
		}[i] {
			t.Fatalf("line %d changed: %s", i, l)
		}
	}
}

func TestSafeguardFillerStreamNoToolUses(t *testing.T) {
	f := NewClaudeSafeguardFiller([]byte(safeguardsRequest))
	out := feedLines(t, f, []string{
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
	})
	tu := toolUsesOf(t, out[0])
	if len(tu.Map()) != 0 || tu.Raw != "{}" {
		t.Fatalf("want empty tool_uses object, got %s", tu.Raw)
	}
}

func TestSafeguardFillerStreamKeepsUpstreamResults(t *testing.T) {
	f := NewClaudeSafeguardFiller([]byte(safeguardsRequest))
	line := `data: {"type":"message_delta","delta":{"stop_reason":"tool_use","safeguard_results":[{"type":"dangerous_tool_use","status":{"type":"available","tool_uses":{"toolu_A":{"type":"evaluated","outcome":"not_flagged"}}}}]}}`
	out := feedLines(t, f, []string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_A","name":"Bash","input":{}}}`,
		line,
	})
	if out[1] != line || f.Filled() {
		t.Fatalf("upstream results must pass through untouched: %s", out[1])
	}
}

func TestSafeguardFillerStreamKeepsUnsupported(t *testing.T) {
	f := NewClaudeSafeguardFiller([]byte(safeguardsRequest))
	line := `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","safeguard_results":[{"type":"dangerous_tool_use","status":{"type":"unsupported"}}]}}`
	if out := feedLines(t, f, []string{line}); out[0] != line || f.Filled() {
		t.Fatalf("a real server answer must pass through: %s", out[0])
	}
}

func TestSafeguardFillerStreamOnlyFirstStopDelta(t *testing.T) {
	f := NewClaudeSafeguardFiller([]byte(safeguardsRequest))
	out := feedLines(t, f, []string{
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":null},"usage":{"output_tokens":9}}`,
	})
	toolUsesOf(t, out[0])
	if strings.Contains(out[1], "safeguard_results") {
		t.Fatalf("a later usage-only delta must stay untouched: %s", out[1])
	}
}

func TestSafeguardFillerInactiveWithoutSafeguards(t *testing.T) {
	f := NewClaudeSafeguardFiller([]byte(`{"model":"claude-opus-5-5","messages":[]}`))
	line := `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`
	if out := feedLines(t, f, []string{line}); out[0] != line || f.Filled() {
		t.Fatalf("requests without safeguards must not be touched: %s", out[0])
	}
}

func TestSafeguardFillerCRLFAndEventLines(t *testing.T) {
	f := NewClaudeSafeguardFiller([]byte(safeguardsRequest))
	out := feedLines(t, f, []string{"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\r"})
	if !strings.HasSuffix(out[0], "\r") {
		t.Fatalf("line ending lost: %q", out[0])
	}
	toolUsesOf(t, strings.TrimSuffix(out[0], "\r"))
}

func TestFillClaudeSafeguardResultsNonStream(t *testing.T) {
	resp := `{"id":"msg_1","type":"message","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"toolu_X","name":"Bash","input":{}}],"stop_reason":"tool_use","usage":{"output_tokens":3}}`
	out, filled := FillClaudeSafeguardResults([]byte(safeguardsRequest), []byte(resp))
	if !filled {
		t.Fatal("expected fill")
	}
	res := gjson.GetBytes(out, "safeguard_results")
	if res.Get("0.status.tool_uses.toolu_X.type").String() != "unavailable" || res.Get("0.status.tool_uses.toolu_X.reason").String() != "error" {
		t.Fatalf("bad fill: %s", res.Raw)
	}
	if gjson.GetBytes(out, "usage.output_tokens").Int() != 3 {
		t.Fatalf("body changed: %s", out)
	}

	present := `{"id":"msg_1","content":[],"safeguard_results":[{"type":"dangerous_tool_use","status":{"type":"available","tool_uses":{}}}]}`
	if out, filled := FillClaudeSafeguardResults([]byte(safeguardsRequest), []byte(present)); filled || string(out) != present {
		t.Fatalf("existing results must pass through: %s", out)
	}
	if out, filled := FillClaudeSafeguardResults([]byte(`{"messages":[]}`), []byte(resp)); filled || string(out) != resp {
		t.Fatalf("requests without safeguards must not be touched: %s", out)
	}
	if out, filled := FillClaudeSafeguardResults([]byte(safeguardsRequest), []byte(`{"type":"error"}`)); filled || string(out) != `{"type":"error"}` {
		t.Fatalf("error bodies must not be touched: %s", out)
	}
}
