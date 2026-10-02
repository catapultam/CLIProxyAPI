package claude

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// advisorTurn is what the first upstream stream did.
type advisorTurn struct {
	// Called reports that the model called the advisor.
	Called bool
	// ToolUseID is the id of the advisor tool_use block.
	ToolUseID string
	// OtherTools reports tool calls besides the advisor in the same turn.
	OtherTools bool
	// Blocks are the turn's content blocks other than the advisor call, in
	// Claude Messages format, for the continuation request.
	Blocks []json.RawMessage
}

type blockAcc struct {
	kind      string
	id        string
	name      string
	text      strings.Builder
	thinking  strings.Builder
	signature strings.Builder
	input     strings.Builder
	data      string
}

func (b *blockAcc) raw() json.RawMessage {
	var out []byte
	switch b.kind {
	case "text":
		out, _ = json.Marshal(map[string]string{"type": "text", "text": b.text.String()})
	case "thinking":
		out, _ = json.Marshal(map[string]string{"type": "thinking", "thinking": b.thinking.String(), "signature": b.signature.String()})
	case "redacted_thinking":
		out, _ = json.Marshal(map[string]string{"type": "redacted_thinking", "data": b.data})
	case "tool_use":
		input := strings.TrimSpace(b.input.String())
		if input == "" || !json.Valid([]byte(input)) {
			input = "{}"
		}
		out, _ = json.Marshal(map[string]any{"type": "tool_use", "id": b.id, "name": b.name, "input": json.RawMessage(input)})
	default:
		return nil
	}
	return out
}

// advisorStitcher relays Claude Messages SSE from one or two upstream streams
// as a single response. It hides the advisor tool call, keeps block indices
// contiguous, and holds the terminal events until Close.
type advisorStitcher struct {
	write func([]byte)
	buf   []byte

	outIndex int
	indexMap map[int64]int
	suppress map[int64]bool

	continuation bool
	turn         advisorTurn
	blocks       map[int64]*blockAcc
	order        []int64

	firstDelta   string
	contDelta    string
	outputTokens int64
}

func newAdvisorStitcher(write func([]byte)) *advisorStitcher {
	return &advisorStitcher{
		write:    write,
		indexMap: make(map[int64]int),
		suppress: make(map[int64]bool),
		blocks:   make(map[int64]*blockAcc),
	}
}

// Feed accepts raw SSE bytes; events may be split across calls.
func (s *advisorStitcher) Feed(chunk []byte) {
	s.buf = append(s.buf, chunk...)
	for {
		i := bytes.Index(s.buf, []byte("\n\n"))
		if i < 0 {
			return
		}
		raw := string(s.buf[:i])
		s.buf = s.buf[i+2:]
		s.handle(raw)
	}
}

func (s *advisorStitcher) emit(event, data string) {
	s.write([]byte("event: " + event + "\ndata: " + data + "\n\n"))
}

func (s *advisorStitcher) handle(raw string) {
	var event, data string
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if event == "" {
		event = gjson.Get(data, "type").String()
	}
	if event == "" {
		if strings.TrimSpace(raw) != "" {
			s.write([]byte(raw + "\n\n"))
		}
		return
	}
	idx := gjson.Get(data, "index")
	switch event {
	case "message_start":
		if !s.continuation {
			s.emit(event, data)
		}
	case "content_block_start":
		block := gjson.Get(data, "content_block")
		if !s.continuation && block.Get("type").String() == "tool_use" && block.Get("name").String() == "advisor" {
			s.suppress[idx.Int()] = true
			s.turn.Called = true
			s.turn.ToolUseID = block.Get("id").String()
			return
		}
		out := s.outIndex
		s.outIndex++
		s.indexMap[idx.Int()] = out
		if !s.continuation {
			acc := &blockAcc{kind: block.Get("type").String(), id: block.Get("id").String(), name: block.Get("name").String()}
			acc.text.WriteString(block.Get("text").String())
			acc.thinking.WriteString(block.Get("thinking").String())
			acc.data = block.Get("data").String()
			if acc.kind == "tool_use" {
				s.turn.OtherTools = true
			}
			s.blocks[idx.Int()] = acc
			s.order = append(s.order, idx.Int())
		}
		s.emit(event, s.reindex(data, out))
	case "content_block_delta", "content_block_stop":
		if s.suppress[idx.Int()] {
			return
		}
		out, ok := s.indexMap[idx.Int()]
		if !ok {
			out = int(idx.Int())
		}
		if event == "content_block_delta" && !s.continuation {
			if acc := s.blocks[idx.Int()]; acc != nil {
				delta := gjson.Get(data, "delta")
				switch delta.Get("type").String() {
				case "text_delta":
					acc.text.WriteString(delta.Get("text").String())
				case "thinking_delta":
					acc.thinking.WriteString(delta.Get("thinking").String())
				case "signature_delta":
					acc.signature.WriteString(delta.Get("signature").String())
				case "input_json_delta":
					acc.input.WriteString(delta.Get("partial_json").String())
				}
			}
		}
		s.emit(event, s.reindex(data, out))
	case "message_delta":
		s.outputTokens += gjson.Get(data, "usage.output_tokens").Int()
		if s.continuation {
			s.contDelta = data
		} else {
			s.firstDelta = data
		}
	case "message_stop":
		// Held; Close writes the single message_stop.
	default:
		s.emit(event, data)
	}
}

func (s *advisorStitcher) reindex(data string, out int) string {
	updated, err := sjson.Set(data, "index", out)
	if err != nil {
		return data
	}
	return updated
}

// FinishFirst reports what the first stream did once it has ended.
func (s *advisorStitcher) FinishFirst() advisorTurn {
	turn := s.turn
	turn.Blocks = nil
	for _, idx := range s.order {
		if raw := s.blocks[idx].raw(); raw != nil {
			turn.Blocks = append(turn.Blocks, raw)
		}
	}
	return turn
}

// EmitText writes a complete text block at the next output index.
func (s *advisorStitcher) EmitText(text string) {
	out := s.outIndex
	s.outIndex++
	start, _ := sjson.Set(`{"type":"content_block_start","content_block":{"type":"text","text":""}}`, "index", out)
	delta, _ := sjson.Set(`{"type":"content_block_delta","delta":{"type":"text_delta"}}`, "index", out)
	delta, _ = sjson.Set(delta, "delta.text", text)
	stop, _ := sjson.Set(`{"type":"content_block_stop"}`, "index", out)
	s.emit("content_block_start", start)
	s.emit("content_block_delta", delta)
	s.emit("content_block_stop", stop)
}

// BeginContinuation switches to relaying the follow-up stream.
func (s *advisorStitcher) BeginContinuation() {
	s.continuation = true
	s.buf = nil
	s.indexMap = make(map[int64]int)
	s.suppress = make(map[int64]bool)
}

// Close writes the final message_delta and message_stop. The continuation's
// stop reason wins; without one, a turn whose only call was the advisor ends
// as end_turn. Output tokens are summed across both streams.
func (s *advisorStitcher) Close() {
	delta := s.contDelta
	if delta == "" {
		delta = s.firstDelta
		if delta != "" && s.turn.Called && !s.turn.OtherTools {
			delta, _ = sjson.Set(delta, "delta.stop_reason", "end_turn")
		}
	}
	if delta == "" {
		delta = `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{}}`
	}
	delta, _ = sjson.Set(delta, "usage.output_tokens", s.outputTokens)
	s.emit("message_delta", delta)
	s.emit("message_stop", `{"type":"message_stop"}`)
}
