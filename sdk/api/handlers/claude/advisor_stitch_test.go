package claude

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func sse(event, data string) string { return "event: " + event + "\ndata: " + data + "\n\n" }

const (
	evMessageStart = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"gpt-6.1-sol","usage":{"input_tokens":10,"output_tokens":0}}}`
	evPing         = `{"type":"ping"}`
)

func textBlock(idx int, text string) string {
	i := itoa(idx)
	return sse("content_block_start", `{"type":"content_block_start","index":`+i+`,"content_block":{"type":"text","text":""}}`) +
		sse("content_block_delta", `{"type":"content_block_delta","index":`+i+`,"delta":{"type":"text_delta","text":"`+text+`"}}`) +
		sse("content_block_stop", `{"type":"content_block_stop","index":`+i+`}`)
}

func toolBlock(idx int, id, name, json string) string {
	i := itoa(idx)
	return sse("content_block_start", `{"type":"content_block_start","index":`+i+`,"content_block":{"type":"tool_use","id":"`+id+`","name":"`+name+`","input":{}}}`) +
		sse("content_block_delta", `{"type":"content_block_delta","index":`+i+`,"delta":{"type":"input_json_delta","partial_json":`+jsonString(json)+`}}`) +
		sse("content_block_stop", `{"type":"content_block_stop","index":`+i+`}`)
}

func terminal(stop string, out int) string {
	return sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stop+`","stop_sequence":null},"usage":{"output_tokens":`+itoa(out)+`}}`) +
		sse("message_stop", `{"type":"message_stop"}`)
}

func itoa(i int) string { return strconv.Itoa(i) }

func jsonString(s string) string {
	var b bytes.Buffer
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

type sseEvent struct{ name, data string }

func parseOut(t *testing.T, out string) []sseEvent {
	t.Helper()
	var evs []sseEvent
	for _, block := range strings.Split(strings.TrimSpace(out), "\n\n") {
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
		if ev.name != "" {
			evs = append(evs, ev)
		}
	}
	return evs
}

func count(evs []sseEvent, name string) int {
	n := 0
	for _, e := range evs {
		if e.name == name {
			n++
		}
	}
	return n
}

func checkIndices(t *testing.T, evs []sseEvent) {
	t.Helper()
	next := 0
	for _, e := range evs {
		if e.name != "content_block_start" {
			continue
		}
		if got := int(gjson.Get(e.data, "index").Int()); got != next {
			t.Fatalf("block index %d, want %d", got, next)
		}
		next++
	}
}

func TestStitcherPassesThroughWithoutAdvisor(t *testing.T) {
	var out bytes.Buffer
	st := newAdvisorStitcher(func(b []byte) { out.Write(b) })
	in := sse("message_start", evMessageStart) + textBlock(0, "hi") + toolBlock(1, "t1", "Bash", `{"command":"ls"}`) + terminal("tool_use", 7)
	st.Feed([]byte(in))
	turn := st.FinishFirst()
	if turn.Called {
		t.Fatal("advisor reported without a call")
	}
	st.Close()
	evs := parseOut(t, out.String())
	if count(evs, "message_start") != 1 || count(evs, "message_stop") != 1 || count(evs, "content_block_start") != 2 {
		t.Fatalf("events = %v", evs)
	}
	if !strings.Contains(out.String(), `"stop_reason":"tool_use"`) {
		t.Fatal("stop reason changed")
	}
}

func TestStitcherSuppressesAdvisorAndStitchesContinuation(t *testing.T) {
	var out bytes.Buffer
	st := newAdvisorStitcher(func(b []byte) { out.Write(b) })
	first := sse("message_start", evMessageStart) + sse("ping", evPing) + textBlock(0, "let me ask") + toolBlock(1, "call_adv", "advisor", `{}`) + terminal("tool_use", 5)
	// Split mid-event to exercise buffering.
	st.Feed([]byte(first[:37]))
	st.Feed([]byte(first[37:]))
	turn := st.FinishFirst()
	if !turn.Called || turn.ToolUseID != "call_adv" || turn.OtherTools {
		t.Fatalf("turn = %+v", turn)
	}
	if len(turn.Blocks) != 1 || gjson.GetBytes(turn.Blocks[0], "text").String() != "let me ask" {
		t.Fatalf("turn blocks = %s", turn.Blocks)
	}
	if strings.Contains(out.String(), "advisor") {
		t.Fatalf("advisor call leaked: %s", out.String())
	}
	st.EmitText("Advisor (claude-opus-5-5): check the tests")
	st.BeginContinuation()
	st.Feed([]byte(sse("message_start", evMessageStart) + textBlock(0, "done") + terminal("end_turn", 9)))
	st.Close()

	evs := parseOut(t, out.String())
	if count(evs, "message_start") != 1 || count(evs, "message_stop") != 1 || count(evs, "message_delta") != 1 {
		t.Fatalf("framing wrong: %v", evs)
	}
	checkIndices(t, evs)
	if count(evs, "content_block_start") != 3 {
		t.Fatalf("blocks = %d", count(evs, "content_block_start"))
	}
	delta := ""
	for _, e := range evs {
		if e.name == "message_delta" {
			delta = e.data
		}
	}
	if gjson.Get(delta, "delta.stop_reason").String() != "end_turn" || gjson.Get(delta, "usage.output_tokens").Int() != 14 {
		t.Fatalf("final delta = %s", delta)
	}
	if !strings.Contains(out.String(), "check the tests") {
		t.Fatal("advice text missing")
	}
}

func TestStitcherAdvisorWithOtherToolsKeepsToolUse(t *testing.T) {
	var out bytes.Buffer
	st := newAdvisorStitcher(func(b []byte) { out.Write(b) })
	st.Feed([]byte(sse("message_start", evMessageStart) + toolBlock(0, "call_adv", "advisor", `{}`) + toolBlock(1, "t2", "Bash", `{"command":"go test"}`) + terminal("tool_use", 6)))
	turn := st.FinishFirst()
	if !turn.Called || !turn.OtherTools {
		t.Fatalf("turn = %+v", turn)
	}
	st.EmitText("Advisor (x): run the race detector")
	st.Close()
	evs := parseOut(t, out.String())
	checkIndices(t, evs)
	if !strings.Contains(out.String(), `"name":"Bash"`) || !strings.Contains(out.String(), `"stop_reason":"tool_use"`) {
		t.Fatalf("other tool or stop reason lost: %s", out.String())
	}
	if count(evs, "message_stop") != 1 {
		t.Fatal("message_stop count")
	}
}

func TestStitcherCloseWithoutContinuationEndsTurn(t *testing.T) {
	var out bytes.Buffer
	st := newAdvisorStitcher(func(b []byte) { out.Write(b) })
	st.Feed([]byte(sse("message_start", evMessageStart) + toolBlock(0, "call_adv", "advisor", `{}`) + terminal("tool_use", 3)))
	st.FinishFirst()
	st.EmitText("Advisor (x) unavailable: boom")
	st.Close()
	if !strings.Contains(out.String(), `"stop_reason":"end_turn"`) || strings.Count(out.String(), "event: message_stop") != 1 {
		t.Fatalf("out = %s", out.String())
	}
}

func TestStitcherRecordsToolInputAndThinking(t *testing.T) {
	var out bytes.Buffer
	st := newAdvisorStitcher(func(b []byte) { out.Write(b) })
	thinking := sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`) +
		sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`) +
		sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig1"}}`) +
		sse("content_block_stop", `{"type":"content_block_stop","index":0}`)
	st.Feed([]byte(sse("message_start", evMessageStart) + thinking + toolBlock(1, "call_adv", "advisor", `{}`) + terminal("tool_use", 2)))
	turn := st.FinishFirst()
	if len(turn.Blocks) != 1 || gjson.GetBytes(turn.Blocks[0], "type").String() != "thinking" ||
		gjson.GetBytes(turn.Blocks[0], "thinking").String() != "hmm" || gjson.GetBytes(turn.Blocks[0], "signature").String() != "sig1" {
		t.Fatalf("blocks = %s", turn.Blocks)
	}
}
