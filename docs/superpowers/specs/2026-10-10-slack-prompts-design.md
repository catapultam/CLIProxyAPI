# Slack prompts: answer blocking Claude Code prompts from Slack

Date: 2026-10-10. Branch: `slack-prompts`, off `catapultam`. Status: design,
revised after an adversarial review.

## Goal

Anything that blocks a Claude Code session on the person can be answered from
Slack, so an agent can be driven with no access to its console. Each blocking
prompt is posted to the owner's Slack DM as it opens, and the terminal dialog
stays up beside it. Whichever side answers first wins.

In scope:

- permission dialogs (tool approval, with "always allow" where the dialog
  offers rules), for the main loop and for subagents;
- `AskUserQuestion` (choice, multi-select, free text, number);
- `ExitPlanMode` plan approval;
- denial-prompt's "Allow once" question after an auto-mode denial;
- an idle ping when a turn ends in auto mode.

Out of scope, and so not covered by the "no console" promise:

- MCP elicitation forms. `ClassicResultFields` has no `Elicitation` entry, so a
  mod cannot answer one.
- Teammates. Each runs in its own process with its own copy of the mod, and
  gets this feature as its own session.
- Sandbox and network-access prompts, unless they raise
  `classic.PermissionRequest`. Not verified (guess G5).

Decided by Alex, 2026-10-10:

- Only owners (the `allowed-emails` people) answer any prompt.
- The idle ping fires after every main-loop turn that ends in auto mode.
- Unknowns are settled by best guesses (below), not more probes. A wrong guess
  must fail safe: the terminal dialog always stays up and still works.

## What the probes showed (Claude Code 2.1.296)

A throwaway mod forced an ask on a marked Bash command and logged timestamps.

1. The dialog draws while a `classic.PermissionRequest` hook is pending, and a
   keyboard answer wins at once. The hook's late answer is ignored.
2. A hook's answer dismisses the open dialog: `{ decision: { behavior:
   'allow' } }` after 5 s ran the command 155 ms later with nobody touching it.
3. The 10 s hook budget runs while a hook awaits a plain promise. A
   `$.clock.after` wait of 15 s was cut off and its answer discarded. Time
   inside `$.http.fetch`, `$.process.run` or `next(e)` is free. So every Slack
   wait sits inside `$.http.fetch`, looped.
4. The event carries no `tool_use_id`. Its keys are `session_id`,
   `transcript_path`, `cwd`, `scratchpad_dir`, `prompt_id`,
   `permission_mode`, `agent_id`, `agent_type`, `effort`, `hook_event_name`,
   `tool_name`, `tool_input`, `permission_suggestions`.
5. The hook can be entered twice for one dialog.

The binary (`claude.exe`, Bun) shows the engine calls the PermissionRequest
executor with the call's `toolUseID` and an abort signal, but does not pass
the id into the hook input.

## Best guesses (not verified)

- **G1. A keyboard answer may not abort the hook's `next.signal`.** The design
  does not depend on it: a prompt closes when its call settles (see Mod,
  "Closing prompts"), and a late Slack answer is reported by outcome, not as
  if it had decided.
- **G2. `ExitPlanMode` raises `classic.PermissionRequest`.** An `allow` exits
  plan mode, `updatedPermissions` `setMode` picks the next mode, and `deny`
  keeps planning with the message as feedback. This matches how the Agent SDK's
  `canUseTool` handles it. If it does not fire, plan approval stays
  terminal-only and nothing breaks. The plan text is `tool_input.plan` when it
  is a string; otherwise the newest `*.md` under `~/.claude/plans` changed in
  the last 6 hours; otherwise "The plan text was not available; it is in the
  terminal."
- **G3. `AskUserQuestion` may also raise `classic.PermissionRequest`.** The
  PermissionRequest hook skips it, and `AskUserQuestion` is handled by the
  documented `tool.call` race instead, so it works either way.
- **G4. `tool_input` equals the call's arguments** as a `tool.call` hook sees
  them, less the envelope keys (`tool`, `tool_use_id`, `agentId`,
  `requestMeta`, `consent`). Used only to close prompts early; a mismatch
  leaves the prompt open until the turn ends, when it is closed anyway.
- **G5.** Sandbox and network prompts are unknown; see out of scope.

## Approach

The proxy's Slack bridge owns the Slack side: the single Socket Mode
connection, Block Kit buttons, modals, and message edits. Clicks reach the
proxy, never a per-session socket, so none are lost. The mod owns the Claude
Code side: noticing the prompt, racing it, answering it. They meet at three new
agentbus routes.

Prompts are answered only by **buttons and modals, never by thread replies**.
A casual "yes" in a thread can never approve anything, and thread replies keep
working exactly as today: they are delivered to the session as messages.

## Proxy

### Capability

`POST /v1/agentbus/hello` adds `"prompts": true` to its response when the
Slack bridge is on and connected, and an owner DM can be opened. Otherwise
`false`. The mod reads it on every hello, including re-hellos after `/clear`,
`/resume` and a proxy restart.

### Routes (under `/v1/agentbus`, same bearer auth as the rest)

`POST /prompt` opens a prompt and posts it.

```json
{
  "session": "<session id>",
  "key": "<sha-256 hex of agent|tool|stable JSON of input>",
  "kind": "permission | plan | question | denial",
  "title": "Bash wants to run",
  "detail": "the full command, plan text or description",
  "agent": "optional subagent label",
  "always": true,
  "questions": [
    {
      "id": "q1",
      "text": "Which library?",
      "header": "Library",
      "kind": "choice | text | number",
      "options": [{ "id": "o1", "label": "dayjs", "description": "small" }],
      "multi": false,
      "min": 1, "max": 10, "step": 1, "unit": "px"
    }
  ]
}
```

- `always` (permission only) offers "Always allow". `questions` is used only by
  `kind: question`: 1 to 4 questions, at most 10 options each (Slack's radio
  and checkbox limit; the mod sends no more than `AskUserQuestion` allows).
- **Idempotent on `(session, key)` only while the prompt is live**: while the
  prompt with that key is open, or answered but not yet resolved, a repeat
  returns the same `id` and posts nothing. This makes hook re-entry, and two
  identical calls in flight, share one prompt and one answer. Once the prompt
  is resolved, expired or closed for any reason, the key is free: a new open
  with the same key creates a new prompt with a new `id` and a new Slack post,
  and never replays the old answer. The closed record stays findable by `id`
  for 10 minutes (a waiter that holds the id still gets its answer, then
  `410`), and is forgotten after an hour; at most 1,000 closed records are
  kept, the oldest evicted first. Reservation is atomic under the store lock;
  concurrent opens get the same id; a failed Slack post leaves the prompt
  reserved and retries the post once, but only when Slack surely did not take
  it (a Slack error reply, or a connection failure before the request was
  sent). After an ambiguous failure the post is not retried, since it could
  double-post: the prompt closes and the route returns `502`.
- Returns `200 {"id": "p_<32 hex>"}`. `404 {"error": ...}` when prompts are
  off or the session is unknown; `400` on a malformed body.
- Ids are 128-bit random. Every prompt route checks that the `session` given
  owns the id. This keeps sessions apart only as far as the existing
  self-reported session id does (see Security).

`GET /prompt/wait?session=&id=` long-polls up to 25 s.

- `200 {"answer": {...}}` once answered (shape below). Every waiter gets it.
- `204` still open. `410` closed without an answer (or the answer was already
  consumed more than 10 minutes ago). `404` unknown id.

The answer:

```json
{
  "choice": "allow | always | deny | approve_accept_edits | approve | keep_planning | allow_once | submit",
  "reason": "optional text from a deny or keep-planning modal",
  "answers": { "q1": "dayjs", "q2": ["a", "b"], "q3": "free text", "q4": 7 },
  "by": "alex"
}
```

`POST /prompt/resolve {"session", "id", "outcome", "note"}` closes a prompt
from the mod's side. `outcome` is one of:

- `ran`: the call went ahead;
- `denied`: the call was refused;
- `answered`: a question was answered;
- `cancelled`: the call was abandoned or interrupted;
- `ended`: the session ended.

Idempotent; wakes waiters with `410`.

### Where prompts go

Always the first owner's DM with the bot, never the channel. If the session's
thread already lives in that DM, the prompt is posted in it. Otherwise the
bridge opens (once per session) a top-level DM message headed like a thread
header (`*name* · address · machine`) and posts prompts in its thread. The
header is a separate post made before the prompt; `threadFor`'s own text post
is not reused for the prompt itself. Guests never see prompts: an owner DM
has no guests.

### Rendering

`chat.postMessage` with `blocks` (new: `postBlocks` in `api.go`) and a plain
`text` fallback ("Claude needs you: <title>"). All agent-supplied text goes
through the existing agent-text escaping (no `<!channel>`, no forged
mentions) and the proxy-token redaction, then is cut to Slack's limits on an
escape boundary (redaction always runs before any cut, and a cut never splits
`&amp;`). The redaction covers the configured bot and app tokens, the proxy's
API keys and the caller's bearer (whole, or a run of 8 or more characters of
one that a client cut short, at the end of the text or before an ellipsis or
whitespace), common token shapes (`xox`, `xapp`, `sk-`, `gh[pousr]_`,
`github_pat_`, AWS key ids, private keys, `Bearer`), and the literal value of
any setting whose name ends in `token`, `secret`, `password`, `api_key`,
`access_key`, `private_key` or `credential` (`NAME=value`, `NAME: value`,
JSON `"name": "value"`, and flags such as `--password x`), except a value that
is a code expression (`token = tokens[i]`). Every run of three or more
backticks in a fenced detail is broken, so a detail cannot close the fence:

- header: title (≤150) and, for subagents, `agent`;
- detail: split into `section` blocks of ≤2,900 characters inside code fences
  for permission and denial, plain mrkdwn for plan. At most 40 such blocks;
  past that a line says how many characters were not shown. The budget is
  computed from the blocks already used, so a message never exceeds Slack's 50
  blocks (for example 4 questions of 10 options plus a huge detail). A long
  command is never silently cut mid-way: the cut is always said.
- actions (`action_id` `prompt:<id>:<choice>`, value the choice):

| kind | buttons |
| --- | --- |
| permission | `Allow` (primary), `Always allow` (when `always`), `Deny` (danger), `Deny with reason…` (opens a modal) |
| plan | `Approve, auto-accept edits` (primary), `Approve`, `Keep planning…` (opens a modal with optional feedback) |
| denial | `Allow once` (primary), `Deny` (danger) |
| question, one single-choice question | one button per option (label ≤75, cut with `…`), plus `Answer…` opening the modal for free text |
| question, anything else | `Answer…` opening a modal with one input block per question: `radio_buttons` for single choice, `checkboxes` for multi, `plain_text_input` for text, `number_input` for number (with min/max), and an optional "Other" text input under each choice question |

Modal `callback_id` is `prompt_modal`, `private_metadata` the prompt id. Its
submission is validated (a choice question needs a pick or Other text) and
answers with `choice: submit`.

### Interactivity

`interactivePayload` gains `Actions []struct{ActionID, Value}`,
`ResponseURL`, and `Container`. The interactive switch in `ask.go` gets:

- `block_actions` with an `action_id` starting `prompt:`;
- `view_submission` with `callback_id == prompt_modal`.

Every handler does memory work only and returns the ack at once. Slack calls
(`openViewNow` for modals, ephemeral refusals, edits) run in jobs or their own
goroutine, as `askShortcut` already does.

1. Authorize: `user.config` (owner) only. Anyone else gets an ephemeral "Only
   an owner can answer this." from a job, and nothing changes.
2. Transition atomically under the prompt lock. `open` becomes `answered` with
   the answer; a click on a prompt that is no longer `open` gets an ephemeral
   "Already answered." and changes nothing.
3. Wake the waiters and mark the prompt for redraw.

### Message edits

Redraws never use the 64-slot command queue that carries owner commands,
answer delivery and `/clanker` jobs. A prompt marked dirty goes into a set of
prompt ids that one dedicated goroutine (started and stopped with the bridge)
drains: for each, it renders the prompt's state at the moment it runs, so the
last state always wins and a stale edit cannot put the buttons back, and calls
`chat.update`. Marking is coalescing (a prompt is in the set once) and never
blocks the ack path. A permanent failure (`msg_too_long`, `invalid_arguments`,
`invalid_blocks`, `message_not_found`, `cant_update_message`,
`channel_not_found`, and the like) gives up at once. Once a closed prompt's
final state is drawn, its large `detail` and `questions` are dropped from
memory. A failed `chat.update` (new `updateMessage` in `api.go`;
`chat:write` covers it, no new scope) is retried with the bridge's usual
backoff.

States, shown as a context line replacing the buttons:

- answered: `alex chose Allow · waiting for the session`;
- then from `/prompt/resolve`:
  - `Allowed by alex · ran`;
  - `Denied by alex: <reason>`;
  - `Answered in the terminal · ran`;
  - `Cancelled`;
  - `Session ended`;
- an answer whose outcome disagrees is shown as both. For example, a Slack
  Deny reaching a call already approved at the keyboard shows
  `alex chose Deny · ran (answered in the terminal first)`. When the resolve
  carries the note `terminal` and a Slack answer exists, the status always
  says both, whatever the Slack choice was, and never claims the Slack answer
  decided the call.

### Lifetime

In memory, in a new `internal/slackbridge/prompts.go` (state on the bridge,
since only it can render). At most 20 open per session and 200 overall: over
that, `POST /prompt` returns `429` and the mod leaves the prompt to the
terminal. A prompt closes:

- through `/prompt/resolve`;
- 24 h after it opened;
- when its session ends (`/bye`) or goes offline (its lease lapses);
- on a session hand-off, for the old session's prompts.

A proxy restart loses them. The mod's next wait gets `404` and re-opens the
prompt with the same key, so an unattended session recovers. A click on an
old message from before the restart gets an ephemeral "This prompt is from
before a restart and can't be answered here; answer in the terminal.", and the
message is edited, best effort and through the redraw goroutine, to lose its
buttons. A modal submit for a prompt that is gone closes the modal and posts
an ephemeral "This prompt is no longer open.".

Prompt availability (`prompts` in `/hello`) also requires that the first
owner's DM can be opened; the opened channel is cached, and a failure makes it
false until the next successful open. Sweeping (every 5 s) is the only full
scan for expiry. A session's prompt thread is dropped on `/bye`, lease lapse
and hand-off.

## Mod (agentbus)

Runs only in interactive sessions whose last hello said `prompts: true`. Off
with `AGENTBUS_SLACK_PROMPTS=0` in the environment. Every hook below fails
open: on any error it falls back to the terminal dialog alone, and never runs
`next(e)` twice.

### Shared pieces

- `promptKey(agent, tool, input)`: sha-256 (`crypto.subtle`) of
  `agent|tool|stableJSON(input)`, where stable JSON sorts keys.
- `redact(text)`: before anything leaves the machine, replace the shapes below
  with `[redacted]`:
  - the proxy token;
  - `xox[abpr]-…`, `xapp-…`, `sk-…`, `sk-ant-…`, `ghp_…`, `gho_…`,
    `github_pat_…`;
  - `AKIA[0-9A-Z]{16}`;
  - `-----BEGIN … PRIVATE KEY-----` blocks;
  - `Bearer <token>`;
  - `password=`, `token=`, `secret=` values.
- `waitAnswer($, id, reopen)`: loops `GET /prompt/wait`. On `200` it returns
  the answer. On `204` it loops. On `410` it returns closed. On `404` it calls
  `reopen()` once per 404 streak and goes on. After 3 consecutive errors it
  returns failed. The loop does no `$.clock` waits: each iteration is one
  fetch, so the budget is frozen nearly all the time. It also stops when a
  `stopped()` callback says so.

### Closing prompts

The outermost `tool.call` hook (registered first, as today) records each
in-flight call by `promptKey` of its arguments (G4), **before** the existing
main-loop guards, so subagent and plugin-origin calls are recorded too. When a
call settles, every prompt the mod opened under that key is resolved with:

- `ran`: a result without `isError`;
- `denied`: a deny;
- `cancelled`: an abort or throw.

On `turn.complete` (main loop) every prompt still open is resolved
`cancelled`. On `session.end` every one is resolved `ended`.

### Permissions and plans: `classic.PermissionRequest`

1. Skip when prompts are off, and for `AskUserQuestion` (G3).
2. Build the key from `agent_id`, `tool_name`, `tool_input`, then
   `POST /prompt`:
   - `ExitPlanMode` is `kind: plan`, with the plan text (G2) as `detail`;
   - every other tool is `kind: permission`, with `detail` from the tool
     input: the command for Bash, the path plus a diff-ish preview for
     Edit/Write, the URL for WebFetch, otherwise pretty JSON;
   - `always` is set when `permission_suggestions` is non-empty;
   - `agent` is `agent_type` when `agent_id` is set.
3. `waitAnswer`, stopping when `next.signal` aborts.
4. On an answer, return:
   - allow: `{ decision: { behavior: 'allow' } }`;
   - always: the same, with `updatedPermissions: e.permission_suggestions`;
   - deny: `{ decision: { behavior: 'deny', message: 'Denied from Slack by <by>' + (reason ? ': ' + reason : '') } }`;
   - approve_accept_edits: allow, with `updatedPermissions: [{ type: 'setMode', mode: 'acceptEdits', destination: 'session' }]`;
   - approve: allow, with `setMode` `default`;
   - keep_planning: deny, with the feedback as `message` (or "Keep planning." when empty).
5. Closed, failed, aborted, or prompts unavailable: `return next(e)`, once.
   The hook's `.catch` also answers `next.called ? next(e) : next(e)`: it
   fails open either way, and the terminal dialog still needs a person.

### Questions: `tool.call` on `AskUserQuestion`

```ts
const local = next(e)                         // draws the dialog; called once
const remote = openAndWait(...)               // never throws; null on failure
const first = await Promise.race([local.then(r => ({ r })), remote.then(a => ({ a }))])
```

- Local first: set `stopped`, resolve the prompt `answered` with note
  `terminal`, and return `first.r`.
- Remote answer first: return `{ result: { questions: e.questions, answers } }`,
  where `answers` maps each question's text to the label, the comma-joined
  labels, the free text, or the number as a string. Returning with `next`
  pending aborts the dialog beneath (documented `tool.call` behaviour). Then
  resolve `answered`.
- Remote `null` (failed or closed): `return await local`. Never a second
  `next(e)`.

The budget is frozen throughout because `next(e)` is in flight.

### Idle ping: `classic.Stop`

On the main loop (no `agent_id`) with `permission_mode === 'auto'`, post one
`/send` to `slack` (the session's own thread, as agents do):

```
Waiting for you.
<redacted first 1,500 characters of last_assistant_message>
```

Then `return next(e)`. Never blocks the stop. A reply in that thread wakes
the session as today.

### Version

agentbus `0.5.0`. The proxy needs no minimum version: a mod that never calls
the prompt routes is unaffected.

## denial-prompt

Two fixes, released as `0.2.0` in their own commit so they can ship or roll
back apart from agentbus.

1. **No shared queue.** Each denial gets its own row in the band, keyed by
   its `tool_use_id`, with its own buttons. The `pending` state becomes a
   family keyed by id. A dispatch never awaits another dispatch, so no hook
   burns its budget behind another (the review's finding).
2. **Slack when available.** At the denial, the hook calls `/hello`-less
   `POST /v1/agentbus/prompt` with `kind: denial`, using its own small `bus()`
   helper and the same env as agentbus (`ANTHROPIC_BASE_URL`,
   `ANTHROPIC_AUTH_TOKEN`). The session id comes from `$.session.id()`.
   - If that succeeds, the window is **10 minutes**. The hook races its
     band's press against a `waitAnswer` loop. Budget is frozen while a fetch
     is in flight, and the loop always has one in flight. The time limit is
     checked with `$.clock.now()` between fetches.
   - If it fails, the window is **8 seconds**, under the budget, counted by
     `$.clock.sleep` as today.

   The countdown text shows the window. A press or a Slack answer resolves the
   prompt (`ran` or `denied` after the re-run settles).

## Security

- Answers are bridge-only: no route but a Slack click or modal can answer a
  prompt, and `/send` cannot touch prompt state.
- Session identity stays self-reported, as for the whole bus
  (`agentbus.md`). A client that knows another session's id **and** a
  128-bit prompt id could read that prompt's answer or resolve it. It cannot
  answer it. Accepted, as the rest of the bus accepts this boundary.
- Prompts go only to an owner's DM. Text is redacted (mod) and escaped
  (proxy) before it is posted.

## Testing

Proxy (`go test ./internal/slackbridge/... ./internal/agentbus/...`, fake
Slack):

- open, idempotent re-open (same key, concurrent), wait/answer by button and
  by modal (single, multi, text, number, Other), deny-with-reason and
  keep-planning modals;
- owner-only (an allowed non-owner and a guest are refused), late click on a
  closed prompt, resolve outcomes and the edited text;
- coalesced redraws (two transitions while queued produce one edit with the
  last state);
- expiry, caps (`429`), `/bye` and lease-lapse closing, hand-off closing,
  `404` after restart;
- ack returns without any Slack call; `detail` escaping, redaction, block
  splitting, label cutting;
- `hello` reports `prompts`.

Mod (`claude plugin test`):

- the PermissionRequest hook maps every choice;
- it falls back on 404-twice, 410, three errors and an abort, calling `next`
  once;
- key reuse on re-entry;
- closing on settle, `turn.complete` and `session.end`;
- the AskUserQuestion race both ways, and on remote failure with no second
  `next`;
- the idle ping in auto mode only, never for a subagent;
- redaction.

denial-prompt: two parallel denials each answerable; the Slack path with a
mocked proxy; the 8 s fallback.

## Ship (each step with Alex's go-ahead)

Merge to `catapultam`, bump versions, deploy the proxy, live-check one prompt
of each kind, update `slack.md` and `agentbus.md` in homelab-notes.
