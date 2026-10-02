# Advisor emulation for non-Claude models

Date: 2026-10-02. Status: approved in chat ("send it").

## Problem

Claude Code declares its advisor as a server tool
(`{"type":"advisor_20260301","name":"advisor","model":"<advisorModel>"}`) that
Anthropic's API runs. When a session's model is served by another provider
(Codex/OpenAI through this proxy), nobody runs it. Since commit 37d6a63 the
Codex translator drops the tool and its "# Advisor Tool" system section, so GPT
models simply have no advisor. Goal: give them a working one.

## Decisions (with Alex)

- Advice appears as a visible text block in the reply,
  `Advisor (<model>): <advice>`, so it stays in the conversation history.
- Streaming Claude Messages requests only; non-streaming keeps the drop.

## Design

Applies when a `POST /v1/messages` request is streaming, declares an
`advisor_*` tool, and its model resolves only to non-Claude providers
(`util.GetProviderName` without `claude`).

1. Exposure. The handler marks the body with `"cpa_advisor_emulation": true`.
   The Codex request translator, seeing the marker, maps the advisor tool to a
   function `advisor` with an empty-object schema and keeps the "# Advisor Tool"
   system section. Without the marker the 37d6a63 behavior (drop and strip)
   stays. The marker is not copied into the Codex body (the translator builds
   the body from known fields).
2. Interception. The handler relays the translated Claude SSE through a
   stitcher instead of writing it straight through. The stitcher parses SSE
   events, suppresses the content block whose `content_block_start` is a
   `tool_use` named `advisor` (start, deltas, stop), renumbers block indices,
   records the other blocks of the turn (text, thinking with signature,
   tool_use with input), and holds back `message_delta` and `message_stop`.
3. Advice. At the end of the stream, if the advisor was called, the handler
   renders the conversation (original messages plus this turn's blocks) as
   plain text, truncated to the most recent 120k characters, and calls the
   advisor model through `ExecuteModel` (claude in, claude out, non-streaming).
   The advisor model is the declaration's `model`; bare aliases map to current
   models (`opus` to `claude-opus-5-5`, `sonnet` to `claude-sonnet-5-5`,
   `fable` to `claude-fable-5-1`, `haiku` to `claude-haiku-4-5-20251001`).
   System prompt: review another coding agent's work, give concise, specific
   advice on its next steps (mistakes, risks, better approach), do not do the
   work, under 250 words. The advice is emitted as a text block.
4. Continuation. If the turn's only tool call was the advisor, the handler
   sends a second streaming request: the original body without the advisor
   tool and without the marker, plus an assistant message with the turn's
   blocks and a `tool_use` advisor block, plus a user message with the matching
   `tool_result` carrying the advice. Its events are stitched into the same
   client response: its `message_start` dropped, indices offset, its
   `message_delta` (stop reason) used as the final one with `output_tokens`
   summed, one `message_stop`.
5. Other tools. If the turn also called other tools, those pass through, the
   advice block is appended, and the held `message_delta` (stop reason
   `tool_use`) and `message_stop` close the response.
6. Failures. Advisor errors produce `Advisor (<model>) unavailable: <reason>`
   and the flow continues. A failing continuation closes the response with the
   held `message_delta` and `message_stop` from the first stream after logging.

## Testing

Unit tests on the stitcher with recorded Claude SSE: suppression and
renumbering, holding and releasing the terminal events, continuation stitching
(single message_start/stop, summed usage), advisor-plus-other-tools path,
transcript rendering and truncation, alias mapping, trigger conditions,
translator mapping with and without the marker. Live: canary with a GPT model
and a Claude advisor.
