package helps

import (
	"bytes"
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

func TestFillClaudeSafeguardResultsNonStreamTreatsNullAndEmptyAsMissing(t *testing.T) {
	for _, existing := range []string{`null`, `[]`} {
		resp := `{"id":"msg_1","content":[{"type":"tool_use","id":"toolu_X","name":"Bash","input":{}}],"safeguard_results":` + existing + `}`
		out, filled := FillClaudeSafeguardResults([]byte(safeguardsRequest), []byte(resp))
		if !filled {
			t.Fatalf("existing=%s: expected fill", existing)
		}
		res := gjson.GetBytes(out, "safeguard_results")
		if res.Get("0.status.tool_uses.toolu_X.type").String() != "unavailable" {
			t.Fatalf("existing=%s: bad fill: %s", existing, res.Raw)
		}
	}
}

// TestSafeguardFillerStreamReplacesUnusableResults covers the three shapes
// Claude Code's client parser treats as server_no_result: a null value, an
// empty array, and an array without a dangerous_tool_use entry. Each must be
// overwritten by the stop-carrying message_delta, not left alone.
func TestSafeguardFillerStreamReplacesUnusableResults(t *testing.T) {
	for _, existing := range []string{`null`, `[]`, `[{"type":"other"}]`} {
		f := NewClaudeSafeguardFiller([]byte(safeguardsRequest))
		line := `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","safeguard_results":` + existing + `}}`
		out := feedLines(t, f, []string{line})
		if !f.Filled() {
			t.Fatalf("existing=%s: expected fill", existing)
		}
		toolUsesOf(t, out[0])
	}
}

// TestSafeguardFillerStreamMessageStartNullDoesNotDisarm verifies that a
// message_start carrying "safeguard_results":null does not stop the filler
// from watching: the later stop-carrying message_delta must still be filled.
func TestSafeguardFillerStreamMessageStartNullDoesNotDisarm(t *testing.T) {
	f := NewClaudeSafeguardFiller([]byte(safeguardsRequest))
	out := feedLines(t, f, []string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5-5","content":[],"safeguard_results":null}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_A","name":"Bash","input":{}}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
	})
	if !f.Filled() {
		t.Fatal("expected the later stop delta to still be filled")
	}
	toolUsesOf(t, out[2])
}

// TestSafeguardFillerStreamKeepsUsableUnsupportedReportsStatus checks that a
// real "unsupported" answer passes through untouched (as in
// TestSafeguardFillerStreamKeepsUnsupported) and that UpstreamStatus reports
// it for diagnostics.
func TestSafeguardFillerStreamKeepsUsableUnsupportedReportsStatus(t *testing.T) {
	f := NewClaudeSafeguardFiller([]byte(safeguardsRequest))
	line := `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","safeguard_results":[{"type":"dangerous_tool_use","status":{"type":"unsupported","reason":"not_entitled"}}]}}`
	if out := feedLines(t, f, []string{line}); out[0] != line || f.Filled() {
		t.Fatalf("a real server answer must pass through: %s", out[0])
	}
	typ, reason := f.UpstreamStatus()
	if typ != "unsupported" || reason != "not_entitled" {
		t.Fatalf("UpstreamStatus() = (%q, %q), want (unsupported, not_entitled)", typ, reason)
	}
}

func TestSafeguardFillerUpstreamStatusEmptyWhenFilled(t *testing.T) {
	f := NewClaudeSafeguardFiller([]byte(safeguardsRequest))
	feedLines(t, f, []string{`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`})
	if !f.Filled() {
		t.Fatal("expected fill")
	}
	if typ, reason := f.UpstreamStatus(); typ != "" || reason != "" {
		t.Fatalf("UpstreamStatus() = (%q, %q), want empty when the filler filled it itself", typ, reason)
	}
}

func TestSafeguardFillerFillChunkMultiLine(t *testing.T) {
	f := NewClaudeSafeguardFiller([]byte(safeguardsRequest))
	chunk := []byte("event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_A","name":"Bash","input":{}}}` + "\n\n")
	out := f.FillChunk(chunk)
	if !bytes.Equal(out, chunk) {
		t.Fatalf("non-stop chunk must be byte-identical: got %q, want %q", out, chunk)
	}

	stopChunk := []byte("event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}` + "\n\n")
	filled := f.FillChunk(stopChunk)
	if !f.Filled() {
		t.Fatal("expected the stop chunk to be filled")
	}
	lines := strings.Split(string(filled), "\n")
	if lines[0] != "event: message_delta" {
		t.Fatalf("event line changed: %q", lines[0])
	}
	if !strings.Contains(lines[1], `"toolu_A"`) || !strings.Contains(lines[1], "unavailable") {
		t.Fatalf("data line not filled: %q", lines[1])
	}
	// Trailing blank lines (the SSE event terminator) must be preserved.
	if lines[2] != "" || lines[3] != "" {
		t.Fatalf("trailing blank lines lost: %#v", lines)
	}

	// Once done, FillChunk must stop touching chunks (fast path).
	again := f.FillChunk([]byte(`data: {"type":"message_stop"}` + "\n\n"))
	if string(again) != `data: {"type":"message_stop"}`+"\n\n" {
		t.Fatalf("filler kept mutating after done: %q", again)
	}
}

func TestSafeguardFillerFillChunkInactive(t *testing.T) {
	f := NewClaudeSafeguardFiller([]byte(`{"model":"claude-opus-5-5","messages":[]}`))
	chunk := []byte("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
	if out := f.FillChunk(chunk); !bytes.Equal(out, chunk) {
		t.Fatalf("inactive filler must not touch chunks: %q", out)
	}
	if out := (*ClaudeSafeguardFiller)(nil).FillChunk(chunk); !bytes.Equal(out, chunk) {
		t.Fatalf("nil filler must not touch chunks: %q", out)
	}
}
