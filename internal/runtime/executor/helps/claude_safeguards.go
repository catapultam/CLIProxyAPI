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

// LogClaudeSafeguardHandlerFill records that a response reached the Claude
// handler without usable safeguard_results for an auto-mode request — for
// example a non-Claude provider served through the Claude endpoint — and the
// handler filled them as unavailable (retryable). That is expected for every
// non-Claude model, so it logs at debug level.
func LogClaudeSafeguardHandlerFill(model string, toolUses int) {
	log.WithFields(log.Fields{
		"model":     model,
		"tool_uses": toolUses,
	}).Debug("claude: response reached the Claude handler without safeguard_results for an auto-mode request; filled them as unavailable (retryable)")
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

// claudeSafeguardStatusOf returns the status type/reason of the first
// dangerous_tool_use entry in r that carries a non-empty status.type, if any.
func claudeSafeguardStatusOf(r gjson.Result) (typ, reason string, ok bool) {
	if !r.IsArray() {
		return "", "", false
	}
	for _, entry := range r.Array() {
		if entry.Get("type").String() != claudeSafeguardType {
			continue
		}
		if t := entry.Get("status.type").String(); t != "" {
			return t, entry.Get("status.reason").String(), true
		}
	}
	return "", "", false
}

// claudeSafeguardResultsUsable reports whether r is a safeguard_results value
// Claude Code's client parser accepts: an array containing at least one
// dangerous_tool_use entry with a non-empty status.type. `null`, `[]`, and an
// array without a dangerous_tool_use entry are not usable — Claude Code's
// parser treats all of them as server_no_result.
func claudeSafeguardResultsUsable(r gjson.Result) bool {
	_, _, ok := claudeSafeguardStatusOf(r)
	return ok
}

// ClaudeSafeguardResponseStatus reports the status type/reason of a
// non-streaming message response's usable safeguard_results, if any.
func ClaudeSafeguardResponseStatus(responseBody []byte) (typ, reason string, ok bool) {
	return claudeSafeguardStatusOf(gjson.GetBytes(responseBody, "safeguard_results"))
}

// ClaudeSafeguardFiller fills missing or unusable safeguard_results into one
// Claude SSE response, line by line. It is inactive for requests without
// `safeguards`.
type ClaudeSafeguardFiller struct {
	active               bool
	done                 bool
	filled               bool
	toolUseIDs           []string
	upstreamStatusType   string
	upstreamStatusReason string
}

// NewClaudeSafeguardFiller returns a filler for the response to requestBody.
func NewClaudeSafeguardFiller(requestBody []byte) *ClaudeSafeguardFiller {
	return &ClaudeSafeguardFiller{active: ClaudeRequestWantsSafeguards(requestBody)}
}

// Filled reports whether the filler added safeguard_results to the response.
func (f *ClaudeSafeguardFiller) Filled() bool { return f != nil && f.filled }

// UpstreamStatus reports the safeguard_results status type and reason the
// filler observed in a usable upstream result, if any. It is empty when the
// filler itself filled the response, or never saw a usable result.
func (f *ClaudeSafeguardFiller) UpstreamStatus() (typ, reason string) {
	if f == nil {
		return "", ""
	}
	return f.upstreamStatusType, f.upstreamStatusReason
}

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
	for _, key := range [...]string{"message.safeguard_results", "delta.safeguard_results", "safeguard_results"} {
		v := root.Get(key)
		if !v.Exists() {
			continue
		}
		if typ, reason, ok := claudeSafeguardStatusOf(v); ok {
			f.upstreamStatusType, f.upstreamStatusReason = typ, reason
			f.done = true
			return line
		}
		// Present but not usable (null, [], or no dangerous_tool_use entry):
		// Claude Code's parser treats this the same as missing, so don't
		// disarm. A stop-carrying message_delta below may still overwrite it.
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

// FillChunk fills a chunk that may hold several complete SSE lines, for use
// at the handler layer where a provider's stream chunks are whole SSE events
// rather than single lines. It returns chunk unchanged once the filler is
// nil, inactive, or already done, and never buffers partial lines across
// calls — callers at that layer only ever see whole events.
func (f *ClaudeSafeguardFiller) FillChunk(chunk []byte) []byte {
	if f == nil || !f.active || f.done {
		return chunk
	}
	lines := bytes.Split(chunk, []byte("\n"))
	for i, line := range lines {
		lines[i] = f.FillStreamLine(line)
	}
	return bytes.Join(lines, []byte("\n"))
}

// FillClaudeSafeguardResults fills a non-streaming Claude message response
// that lacks usable safeguard_results, for a request that asked for them.
// Anything that isn't a message (an error body) is returned unchanged, and a
// usable existing result passes through untouched.
func FillClaudeSafeguardResults(requestBody, responseBody []byte) ([]byte, bool) {
	if !ClaudeRequestWantsSafeguards(requestBody) || !gjson.ValidBytes(responseBody) {
		return responseBody, false
	}
	root := gjson.ParseBytes(responseBody)
	if existing := root.Get("safeguard_results"); existing.Exists() && claudeSafeguardResultsUsable(existing) {
		return responseBody, false
	}
	if !root.Get("content").IsArray() {
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
