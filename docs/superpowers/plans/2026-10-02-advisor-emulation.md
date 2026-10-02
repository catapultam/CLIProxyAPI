# Advisor emulation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Working advisor for GPT sessions through the proxy (spec: `docs/superpowers/specs/2026-10-02-advisor-emulation-design.md`).

**Architecture:** Translator exposes the advisor as a function when the handler marks the body; a Claude SSE stitcher in `sdk/api/handlers/claude` suppresses the call, gets advice via `ExecuteModel`, and stitches an optional continuation stream into the same response.

## Global Constraints

- Only streaming `/v1/messages` with an `advisor_*` tool and a non-Claude model.
- Without the marker the translator keeps dropping server tools (37d6a63).
- No em or en dashes; no AI attribution in commits; build and test on cakebox; canary before production.

## Review Focus

1. A turn with advisor plus other tool calls must still end with stop reason `tool_use` and every non-advisor tool_use block intact.
2. The client must see exactly one `message_start` and one `message_stop`, and strictly increasing block indices, across both streams.
3. An advisor failure must not leave the response unterminated.
4. Requests to Claude models must never carry the marker upstream.
5. Chunks that split an SSE event across two reads must parse correctly.

### Task 1: Translator mapping behind the marker
Files: `internal/translator/codex/claude/codex_claude_request.go`, test file `codex_claude_server_tools_test.go`.
Tests: with `cpa_advisor_emulation:true` the advisor becomes `{"type":"function","name":"advisor","parameters":{"type":"object","properties":{}}}` and the system section is kept; without it the existing tests still pass.

### Task 2: SSE stitcher
Files: `sdk/api/handlers/claude/advisor_stitch.go`, `advisor_stitch_test.go`.
Interface: `newAdvisorStitcher(write func([]byte))`, `(*advisorStitcher) Feed(chunk []byte)`, `FinishFirst() (advisorTurn, bool)` where `advisorTurn{Called bool; ToolUseID string; Blocks []json.RawMessage; OtherTools bool}`, `EmitText(text string)`, `BeginContinuation()`, `Close()` (writes the held or continuation terminal events).
Tests per Review Focus 1, 2, 5.

### Task 3: Advice call and transcript
Files: `sdk/api/handlers/claude/advisor_call.go`, `advisor_call_test.go`.
Interface: `renderAdvisorTranscript(original []byte, turn []json.RawMessage, limit int) string`, `resolveAdvisorModel(declared string) string`, `buildAdvisorRequest(model, transcript string) []byte`, `extractClaudeText(body []byte) string`.
Tests: rendering of text, tool_use, tool_result, images; truncation keeps the tail; alias mapping; request shape.

### Task 4: Handler wiring
Files: `sdk/api/handlers/claude/code_handlers.go`, `advisor_emulation.go`, test.
Behavior per spec; `shouldEmulateAdvisor(rawJSON)`; continuation body builder `buildAdvisorContinuation(original []byte, turn advisorTurn, advice string) []byte`.
Tests: trigger conditions; continuation body shape (advisor tool removed, marker removed, two messages appended); an end-to-end handler test with a fake executor is optional if the units cover the flow.

### Task 5: Deploy and verify
Canary on 8328 with a real `gpt-6.1-sol` request declaring the advisor and a prompt that asks it to consult the advisor first; check one stitched response containing `Advisor (claude-opus-5-5):` and a continuation. Promote, remove canary.
