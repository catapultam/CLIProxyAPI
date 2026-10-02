# agentbus channel plugin

Date: 2026-10-02. Branch: `next-reset` (catapultam fork only).

## Goal

Let Claude Code sessions on different machines (this workstation, Shoggoth, future cakebox VMs) see and
message each other, and wake an idle session when a message arrives, without the `wait.sh`
SessionStart/Stop hook. The hook long-polls for up to a day, which keeps `claude -p` from ever exiting.

## Non-goals

- Appearing in Claude Code's own `ListAgents`/`/list-agents`. That requires Remote Control (claude.ai
  login, blocked by our gateway auth) or writing Claude Code's session registry, which we do not do.
- Changing how the proxy authenticates clients.

## Findings this design relies on

- Custom channels work through the proxy with gateway auth in interactive sessions when started with
  `--dangerously-load-development-channels` (verified with a probe server: the event reached Claude).
- Claude Code passes MCP servers `CLAUDE_CODE_SESSION_ID`, `ANTHROPIC_BASE_URL`,
  `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_PROJECT_DIR`, and `CLAUDE_CODE_SESSION_ATTENDED` (verified by
  recording env var names).
- The proxy already sees the same session id on every `/v1/messages` call
  (`X-Claude-Code-Session-Id`) and already delivers pending agentbus messages by injecting them into the
  next main-thread request, claiming them exactly once (`internal/agentbus/inject.go`).
- A channel notification is silently dropped when the session did not load the server as a channel.

## Architecture

```
 session (claude + plugin)                cakebox: cli-proxy-api (compose stack cliproxyapi)
 ┌────────────────────────┐   /v1/messages (+ injected messages)   ┌──────────────────────────┐
 │ Claude Code ───────────┼───────────────────────────────────────▶│ inject middleware        │
 │   ▲ channel event      │                                         │ agentbus store           │
 │   │ (wake ping)        │   /v1/agentbus/hello, /signal (long-poll)│  peers / send / name     │
 │ agentbus channel server┼───────────────────────────────────────▶│  signal (new)            │
 │   tools: peers, send,  │                                         └──────────────────────────┘
 │          rename        │
 └────────────────────────┘
```

### The wake-ping model (why the channel never carries message text)

The channel server never claims messages. It long-polls a new **signal** endpoint that reports "this
session has messages it has not been told about" without claiming them. On a signal it pushes a short
channel event ("N new agentbus message(s); they arrive with your next turn"). The event starts a turn in
an idle session; that turn's `/v1/messages` request carries the messages through the existing injection,
which claims them exactly once.

Consequences:
- No loss: if the session was not started as a channel, the event is dropped, but the messages stay in
  the inbox and are injected on the session's next request, exactly as today.
- No double delivery: only injection claims.
- `claude -p` sessions are unaffected: channels never register there, and nothing blocks exit.

## Components

### 1. Proxy backend (Go, `internal/agentbus`)

- **New `GET /v1/agentbus/signal?session=&machine=&cwd=`**: calls `Hello`, then long-polls up to 50 s.
  Returns `200 {"pending": n, "ids": [...]}` when the inbox holds messages not yet signaled to this
  session, marking them signaled; `204` on timeout; `409` when superseded by a newer signal poll for
  the same session (same generation mechanism as `/wait`). Never claims.
- **Store**: per-session `Signaled` set (message ids), dropped when a message is claimed; persisted with
  the session.
- **Injection note**: when a session has used `/signal` within `idleWithin`, the note lists peers and
  names the plugin tools (`agentbus` MCP tools) instead of curl recipes, and the setup hint is never
  sent. Sessions without the plugin keep today's text.
- **Setup migration**: `GET /v1/agentbus/setup` gains `?uninstall=1`, which removes the `wait.sh`
  hook entries (matching command path only) and the waiter file, keeping unrelated hooks.
- Existing routes (`/peers`, `/send`, `/name`, `/inbox`, `/hello`, `/wait`) are unchanged.

### 2. Plugin `agentbus` (new repo `catapultam/claude-plugins`, public)

Public because the plugin contains no secrets and private repos need GitHub credentials on every VM.

```
claude-plugins/
  .claude-plugin/marketplace.json      # marketplace "catapultam"
  plugins/agentbus/
    .claude-plugin/plugin.json
    .mcp.json                           # server "agentbus": python ${CLAUDE_PLUGIN_ROOT}/server.py
    server.py                           # stdlib only, Python 3.10+
    tests/                              # unittest against a fake HTTP bus
```

`server.py` is a stdio MCP server:
- Capabilities: `experimental['claude/channel']: {}`, `tools: {}`. No permission relay.
- `instructions`: explains wake pings, that message text arrives in the next turn inside
  `<agentbus>`, and when to use each tool.
- Identity from env: `CLAUDE_CODE_SESSION_ID`, `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`; machine
  from `socket.gethostname()`, cwd from `CLAUDE_PROJECT_DIR`. Missing values: tools return an error
  explaining what is missing; no polling.
- Signal loop (background thread): only when `CLAUDE_CODE_SESSION_ATTENDED` is truthy. Long-poll
  `/signal`; `200` emits one `notifications/claude/channel` event with meta `{count, ids}`; `204`
  repeats; `409` stops; network errors back off 5, 10, 20, 40, 60 s and retry indefinitely.
- Tools:
  - `list_peers()` → formatted `/peers` (address, name, machine, status).
  - `send_message(to, body, reply_to?)` → `/send` with `from_session` = own id.
  - `rename(name)` → `/name`.
  - `read_messages()` → `/inbox` (claims; for sessions that need content without a new turn).
- Token never logged; requests use the `Authorization` header only.

### 3. Launch and allowlist

- Default: the provisioning playbook and a one-time local setup add a `claude` wrapper (PowerShell
  profile function and bash alias) that runs
  `claude --dangerously-load-development-channels plugin:agentbus@catapultam`. The development-channel
  warning shows once per session start.
- Optional, pending a test the auto-mode classifier currently blocks: a managed-settings allowlist
  (`channelsEnabled: true`, `allowedChannelPlugins: [{marketplace: "catapultam", plugin: "agentbus"}]`)
  so the wrapper can use `--channels plugin:agentbus@catapultam` with no warning. Ships only if the test
  shows the warning gone under gateway auth.

### 4. Provisioning

The `provisioning-cakebox-windows-vms` skill replaces its `agentbus` step:
- add the marketplace and enable the plugin in user settings (`extraKnownMarketplaces`,
  `enabledPlugins`);
- run the setup uninstall to remove the `wait.sh` hooks;
- install the `claude` wrapper;
- verify: plugin listed, `list_peers` reachable (direct HTTP to `/peers`), no `wait.sh` hook present.
Python stays a prerequisite (the server is Python); Git Bash is no longer needed for agentbus.

## Error handling

- Proxy unreachable or 401: tools return the HTTP status and a one-line cause; the signal loop backs off
  and keeps retrying while the session lives.
- Unknown sender on `/send` (session never hit the proxy): the server calls `/hello` once at startup so
  its id is always known.
- Superseded (`409`): a second server instance for the same session (e.g. `/mcp` reconnect) takes over;
  the old loop exits quietly.

## Testing

- Go: table tests for `/signal` (pending vs signaled, claim clears signaled ids, generation takeover,
  timeout 204), injection note variant, setup uninstall output. `go test ./internal/agentbus/...`.
- Plugin: `python -m unittest` with a fake bus HTTP server covering hello-at-startup, signal → channel
  notification framing, 204 loop, 409 exit, backoff, tool request shapes, missing-env errors.
- End to end (interactive, both machines): send from this workstation to a Shoggoth session that is
  idle; the Shoggoth session wakes and shows the message; reply comes back; `claude -p` on both exits
  normally.

## Rollout

1. Backend changes on `next-reset`, deployed with the usual compose rebuild on cakebox.
2. Plugin repo published; installed on this workstation by hand-off command and on Shoggoth via the
   playbook.
3. Old hook removed by the setup uninstall on both machines.
4. Allowlist only after its test passes.
