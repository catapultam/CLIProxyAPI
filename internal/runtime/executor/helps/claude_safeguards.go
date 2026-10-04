package helps

import (
	"bytes"
	"encoding/json"
	"net/http"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// LogClaudeSafeguardFill records that the proxy filled missing safeguard
// results, with the upstream request id so the cause can be traced.
func LogClaudeSafeguardFill(headers http.Header, toolUses int) {
	log.WithFields(log.Fields{
		"upstream_request_id": HeaderValueCaseInsensitive(headers, "request-id"),
		"tool_uses":           toolUses,
	}).Warn("claude: upstream response had no safeguard_results for an auto-mode request; filled them as unavailable (retryable)")
}

// Claude Code in auto mode sends a `safeguards` request field and reads the
// server's verdicts from `safeguard_results`. A response that completes without
// them makes Claude Code fall back to its own (billed) classifier for the rest
// of the session. When an upstream response omits them, the proxy fills in a
// result that marks every tool call of the response as "unavailable" with
// reason "error": Claude Code then treats the check as a transient failure for
// those calls and lets the model retry them, instead of latching the fallback.
// The filled result never carries a verdict, so nothing is ever approved by it.

const claudeSafeguardType = "dangerous_tool_use"

type claudeSafeguardCall struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

type claudeSafeguardStatus struct {
	Type     string                         `json:"type"`
	ToolUses map[string]claudeSafeguardCall `json:"tool_uses"`
}

type claudeSafeguardResult struct {
	Type   string                `json:"type"`
	Status claudeSafeguardStatus `json:"status"`
}

// claudeSafeguardUnavailable builds a safeguard_results value that reports the
// check as unavailable (retryable) for each listed tool call.
func claudeSafeguardUnavailable(toolUseIDs []string) []byte {
	calls := make(map[string]claudeSafeguardCall, len(toolUseIDs))
	for _, id := range toolUseIDs {
		calls[id] = claudeSafeguardCall{Type: "unavailable", Reason: "error"}
	}
	raw, _ := json.Marshal([]claudeSafeguardResult{{
		Type:   claudeSafeguardType,
		Status: claudeSafeguardStatus{Type: "available", ToolUses: calls},
	}})
	return raw
}

// ClaudeRequestWantsSafeguards reports whether the request asked the server
// for safeguard results.
func ClaudeRequestWantsSafeguards(requestBody []byte) bool {
	return gjson.GetBytes(requestBody, "safeguards").Exists()
}

// ClaudeSafeguardFiller fills missing safeguard_results into one Claude SSE
// response, line by line. It is inactive for requests without `safeguards`.
type ClaudeSafeguardFiller struct {
	active     bool
	done       bool
	filled     bool
	toolUseIDs []string
}

// NewClaudeSafeguardFiller returns a filler for the response to requestBody.
func NewClaudeSafeguardFiller(requestBody []byte) *ClaudeSafeguardFiller {
	return &ClaudeSafeguardFiller{active: ClaudeRequestWantsSafeguards(requestBody)}
}

// Filled reports whether the filler added safeguard_results to the response.
func (f *ClaudeSafeguardFiller) Filled() bool { return f != nil && f.filled }

// ToolUses reports how many tool calls the filled result covers.
func (f *ClaudeSafeguardFiller) ToolUses() int {
	if f == nil {
		return 0
	}
	return len(f.toolUseIDs)
}

// FillStreamLine returns line unchanged, except for the response's first
// message_delta carrying a stop_reason when no safeguard_results were seen:
// that line gets an "unavailable" result for every tool call seen so far.
func (f *ClaudeSafeguardFiller) FillStreamLine(line []byte) []byte {
	if f == nil || !f.active || f.done {
		return line
	}
	trimmed := bytes.TrimRight(line, "\r")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if !gjson.ValidBytes(payload) {
		return line
	}
	root := gjson.ParseBytes(payload)
	if root.Get("message.safeguard_results").Exists() || root.Get("delta.safeguard_results").Exists() || root.Get("safeguard_results").Exists() {
		f.done = true
		return line
	}
	switch root.Get("type").String() {
	case "content_block_start":
		if root.Get("content_block.type").String() == "tool_use" {
			if id := root.Get("content_block.id").String(); id != "" {
				f.toolUseIDs = append(f.toolUseIDs, id)
			}
		}
	case "message_delta":
		if root.Get("delta.stop_reason").String() == "" {
			return line
		}
		updated, err := sjson.SetRawBytes(payload, "delta.safeguard_results", claudeSafeguardUnavailable(f.toolUseIDs))
		if err != nil {
			return line
		}
		f.done, f.filled = true, true
		out := make([]byte, 0, len(updated)+8)
		out = append(out, "data: "...)
		out = append(out, updated...)
		if len(trimmed) != len(line) {
			out = append(out, '\r')
		}
		return out
	}
	return line
}

// FillClaudeSafeguardResults fills a non-streaming Claude message response
// that lacks safeguard_results, for a request that asked for them. Anything
// that isn't a message (an error body, a missing results field being present)
// is returned unchanged.
func FillClaudeSafeguardResults(requestBody, responseBody []byte) ([]byte, bool) {
	if !ClaudeRequestWantsSafeguards(requestBody) || !gjson.ValidBytes(responseBody) {
		return responseBody, false
	}
	root := gjson.ParseBytes(responseBody)
	if root.Get("safeguard_results").Exists() || !root.Get("content").IsArray() {
		return responseBody, false
	}
	var ids []string
	for _, block := range root.Get("content").Array() {
		if block.Get("type").String() == "tool_use" {
			if id := block.Get("id").String(); id != "" {
				ids = append(ids, id)
			}
		}
	}
	updated, err := sjson.SetRawBytes(responseBody, "safeguard_results", claudeSafeguardUnavailable(ids))
	if err != nil {
		return responseBody, false
	}
	return updated, true
}
