# agentbus: cross-machine messaging between Claude Code sessions

Date: 2026-10-02. Status: design, awaiting review.

## Goal

Let Claude Code sessions on any machine of Alex's tailnet find each other and
coordinate work (hand off tasks, ask questions, report status), the way
`ListAgents` and `SendMessage` work between sessions on one machine. Remote
Control, which normally carries that across machines, is unavailable because
every session sends inference to this proxy (`ANTHROPIC_BASE_URL`).

## Decisions already made (with Alex)

- Purpose: coordinate work between sessions on different machines.
- An idle session that receives a message wakes up and acts on it. Verified by a
  spike on 2026-10-02: a `SessionStart` command hook with `asyncRewake: true`
  that exits 2 wakes a session idle at its prompt and its stderr reaches the
  model ("Stop hook feedback" label, model replied without user input).
- Addressing: every session gets an automatic address, and can also claim a
  friendly name.
- The bus lives inside the CLIProxyAPI fork. Tailnet only; nothing is published
  outside it. Upstream drift is acceptable.
- Machines set themselves up when a session on them first talks to the proxy:
  the proxy tells the agent how, and the agent runs a setup script the proxy
  serves. Approving one permission prompt per machine is acceptable.
- Interface preference: MCP tools, falling back to plain CLI calls. This first
  version ships the CLI form (`curl` against the proxy); an MCP server is a
  later, optional layer over the same endpoints.

## Overview

```
 Claude Code session (any machine)
   |  every request: X-Claude-Code-Session-Id, optional X-Claude-Code-Agent-Id
   v
 CLIProxyAPI fork  ── agentbus ──  sessions registry + inboxes (persisted)
   ^      ^                         |
   |      |  long-poll /wait        | inject note + pending messages into the
   |      |  (wake hook)            | next main-thread request of the session
   |  curl /v1/agentbus/send ...    v
```

Three ways a message reaches a session, all claiming from one inbox so each
message is delivered exactly once:

1. Wake hook (set-up machines): a background hook long-polls `/wait`; when a
   message arrives it prints it and exits 2, which wakes the session.
2. Request injection (any machine): pending messages are appended to the
   session's next main-thread request.
3. Explicit read: `GET /v1/agentbus/inbox` (for an agent that wants to check).

## Components

### 1. Registry (backend)

A session record, keyed by Claude Code session ID:

- `session_id`, `address`, `name` (optional, unique), `machine`, `cwd`
- `last_request_at`, `inflight` (count of main-thread requests in progress)
- `waiter_connected_at` (last time a wake hook held `/wait`)
- `setup_hinted` (the setup note was already injected)

Sources:

- Every `/v1/messages` request with `X-Claude-Code-Session-Id` creates or
  refreshes the record (`last_request_at`, `inflight` around the request).
  Requests that also carry `X-Claude-Code-Agent-Id` are subagent turns: they
  refresh `last_request_at` but never receive injections.
- `/wait` and `/hello` (sent by the wake hook) set `machine` (the machine's
  `hostname`) and `cwd` (from the hook's stdin).

Address: `<machine>/<cwd basename>-<first 6 of session id>`, lowercased, with
characters outside `[a-z0-9._-]` replaced by `-`. Before the machine is known
(no wake hook yet) the machine part is `unknown`. A name, once claimed, can be
used anywhere an address is accepted.

Status shown to peers:

- `busy`: `inflight > 0`
- `idle`: not busy, and a wake hook polled within the last 2 minutes
- `away`: not busy, no wake hook, last request within 30 minutes
- `offline`: older than that. Offline sessions stay listed for 24 hours.

### 2. Messages (backend)

```json
{
  "id": "m_<ulid>",
  "from": "<address of sender>",
  "to": "<address or name>",
  "body": "<text, at most 16 KiB>",
  "reply_to": "<message id, optional>",
  "created_at": "<RFC3339>"
}
```

- `to` resolves to exactly one session at send time; unknown targets are a 404.
  No broadcast in this version.
- Inbox per session, FIFO. A message is removed when claimed by one of the three
  delivery paths. Unclaimed messages expire after 7 days.
- Sending to an `offline` session is allowed: it is delivered when that session
  resumes (a resumed conversation keeps its session ID).
- Registry and inboxes are persisted to `agentbus-state.json` next to
  `next-reset-state.json` (WRITABLE_PATH), written atomically on change, loaded
  at startup.

### 3. HTTP endpoints (backend)

All under `/v1/agentbus`, behind the existing client API key middleware.

| Method and path | Purpose |
| --- | --- |
| `GET /peers` | All sessions with address, name, machine, cwd, status, last seen |
| `POST /send` | `{from_session, to, body, reply_to?}` → `{id}` |
| `POST /name` | `{session, name}` → claim or clear (empty) a name |
| `GET /inbox?session=` | Claim and return all pending messages |
| `POST /hello` | `{session, machine, cwd}` from the wake hook at session start |
| `GET /wait?session=&machine=&cwd=` | Long-poll up to 50 s; returns claimed messages, or 204 on timeout; a newer waiter for the same session makes older ones return 409 |
| `GET /setup` | Shell script that installs the wake hook on the calling machine |

`from_session` is the sender's session ID, which the injected note already
fills in for the agent. Peers only see addresses, never raw session IDs.

### 4. Request injection (backend)

In `ClaudeMessages`, after the raw body is read and before routing, for
main-thread requests (session header present, agent header absent):

- First request of a session: inject the bus note (below).
- Pending messages: claim them and inject them.
- No machine wake hook seen for this session within 2 minutes of its first
  request, and `setup_hinted` not set: add the setup line to the next note and
  set `setup_hinted`.

Injection appends one text block to the content of the last `user` message
(converting string content to a one-element block array first), so the
`system` array and the cached prompt prefix are untouched. If the forwarded
request fails (status >= 400 or transport error), claimed messages go back to
the front of the inbox so they are not lost.

The injected text is wrapped as:

```
<agentbus>
You are <address> on the agentbus, which links Claude Code sessions across
Alex's machines. Peers online: <address (name) status>, ...
Send:  curl -s -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" \
         "$ANTHROPIC_BASE_URL/v1/agentbus/send" -d '{"from_session":"<id>","to":"<addr>","body":"..."}'
Peers: curl -s -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" "$ANTHROPIC_BASE_URL/v1/agentbus/peers"
Name:  ... /v1/agentbus/name -d '{"session":"<id>","name":"..."}'
[setup line, when hinted]
[messages: "Message m_.. from <addr>: <body> (reply with reply_to m_..)"]
</agentbus>
```

The full note goes on the session's first request and when the peer list
changed since the last note; otherwise only messages are injected.

### 5. Wake hook and setup script (client side, served by the proxy)

`GET /v1/agentbus/setup` returns a POSIX `sh` script that:

1. Finds `python3` or `python` (present on the PC, the Fedora box, the Steam
   Deck) and uses it to merge two hook entries into `~/.claude/settings.json`,
   idempotently and keeping everything else:
   - `SessionStart`: `{type: command, command: "<waiter>", asyncRewake: true}`
   - `Stop`: the same command, so the waiter is re-armed after every turn.
2. Writes the waiter script to `~/.claude/hooks/agentbus/wait.sh`.
3. Posts `/hello` for the current session so the machine name appears at once.

The waiter (`wait.sh`): reads the hook's stdin JSON (`session_id`, `cwd`),
loops `curl /wait` (50 s per call). On 200 it prints the messages to stderr and
exits 2 (wake). On 409 (a newer waiter took over) or when the base URL or token
is missing it exits 0 silently. On network errors it retries with backoff up to
a few minutes, then exits 0.

### 6. Telling every session (no publishing)

- Bus note injected by the proxy (section 4) on each session's first request.
- Setup line injected once per session until the machine has a wake hook.
- Woken sessions see the messages in the wake text (stderr of the waiter).

## Error handling

- Malformed bodies, oversized messages, unknown targets: 4xx JSON errors, no
  injection side effects.
- Injection never breaks a request: any failure to parse or rewrite the body
  logs at debug and forwards the original body unchanged.
- Persistence failures log a warning and retry on the next change.
- Hooks never block a session: async, silent on failure.

## Testing

- Backend unit tests: address formatting and name resolution; status
  transitions; inbox claim/return/expiry; `/wait` takeover (409) and timeout;
  injection into string and block content, skipping subagent requests,
  returning messages on failed requests; persistence round trip.
- Setup script: run against a temp HOME on the build container; check the
  settings merge is idempotent and preserves existing hooks.
- Live, on the tailnet: two sessions on this PC exchange messages; then one
  session on the Fedora box sets itself up from the injected hint and is woken
  by a message from the PC.
- First implementation task re-checks that a `Stop` hook with `asyncRewake`
  re-arms the waiter (the spike only verified `SessionStart`).

## Out of scope for this version

Broadcast and group messages, attachments, message history and search, an MCP
server, a UI page in the Management Center.
