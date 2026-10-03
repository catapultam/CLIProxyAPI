# Slack bridge for agentbus — design

Date: 2026-10-02. Branch: `slack-bridge` (off `next-reset`).

## Goal

Alex's Claude Code agents can post status updates to Alex on Slack, and Alex can
reply on Slack to give them instructions. It must work for every session on the
proxy without per-client setup.

## Approach

Make Slack a peer on the agentbus instead of a separate MCP server. Every client
already runs the agentbus mod, so agents reach Slack with the `SendMessage` tool
they already have (`to: "agentbus:slack"`). Alex's Slack replies come back as
ordinary agentbus messages, which the bus already delivers by injecting them into
the next request and by waking idle sessions through the mod's `/wait` poll.

Rejected: a proxy-hosted Slack MCP server. It would need a new MCP endpoint, a
way to tell which session is calling (MCP requests carry no Claude session id),
and an `.mcp.json` shipped to every client. Inbound instructions would still have
to go through agentbus to wake an idle session.

## Slack side

- Existing workspace. A new Slack app "Agents", created from the manifest below,
  using Socket Mode. The proxy opens an outbound websocket to Slack, so cakebox
  needs no public URL.
- One channel (e.g. `#agents`) that Alex creates and invites the bot to.
- **Thread per session.** The first message from a session becomes a top-level
  post labelled with the session (`*name* · address · machine · cwd`). Later
  messages from that session go in its thread.
- **Alex replying in a thread** sends the reply to that thread's session.
- **Alex posting top-level** must start with a session name or address
  (`flyer: do X` or `@flyer do X`). Anything else gets a short bot reply in
  thread explaining how to address an agent.
- Only messages from user IDs in `allowed-users` are relayed. Bot messages and
  edits are ignored. Anyone else in the workspace cannot instruct agents.
- Delivery feedback: the bot adds an `:inbox_tray:` reaction when it has queued
  Alex's message on the bus, and replies in thread with an error if the target is
  unknown.

App manifest (Alex pastes this into api.slack.com → Create App → From manifest):

```yaml
display_information:
  name: Agents
features:
  bot_user:
    display_name: agents
    always_online: true
oauth_config:
  scopes:
    bot:
      - chat:write
      - channels:history
      - groups:history
      - reactions:write
settings:
  event_subscriptions:
    bot_events:
      - message.channels
      - message.groups
  socket_mode_enabled: true
  org_deploy_enabled: false
  token_rotation_enabled: false
```

After installing, Alex generates an app-level token with `connections:write`.

## Proxy side

New package `internal/slackbridge`:

- **Config** — a `slack:` block in `config.yaml`: `bot-token` (xoxb-),
  `app-token` (xapp-), `channel` (channel ID), `allowed-users` (Slack user IDs).
  The bridge is off unless all four are set. Read at startup; changing it needs a
  restart. Tokens live only in cakebox's `config.yaml` and are never logged.
- **Socket Mode client** — `apps.connections.open` with the app token, then a
  gorilla/websocket read loop: ack every envelope, handle `events_api` message
  events, reconnect with backoff on `disconnect` or read errors. No read
  deadlines (Slack pings; gorilla answers them).
- **Web API client** — `chat.postMessage` and `reactions.add` with the bot token.
  Outgoing posts go through a buffered queue drained by one goroutine, so an
  agentbus `Send` never blocks on Slack.
- **Thread map** — session id ↔ thread `ts`, persisted to `slack-state.json` next
  to `agentbus-state.json` (same save cadence and shutdown save), so threads
  survive restarts.

Changes in `internal/agentbus` (small, behind an optional hook):

- A reserved peer address `slack`. When a bridge is attached, `Resolve("slack")`
  succeeds and `Send` to it hands the message (sender session id, address, name,
  machine, cwd, body) to the bridge instead of an inbox. `Peers()` lists it as
  `address: slack, machine: slack, status: idle`, so the mod's `ListAgents` shows
  `agentbus:slack`.
- A `DeliverFromUser(targetSessionID, body, replyTo)` entry point that queues a
  message from `slack` with a new `from_user: true` field on `Message`.
- The injected agentbus note gains one line when the bridge is on: Alex is
  reachable as `slack`; post a short update when you finish a task, get blocked,
  or need a decision; messages marked as from Alex via Slack are Alex's
  instructions. Injected messages with `from_user` are headed
  `Message <id> from Alex via Slack` instead of `from <address>`.

Wiring: `internal/api/server_agentbus.go` constructs the bridge when the config
is complete, attaches it to the store, starts it, and stops/saves it on shutdown.

## Client side (agentbus mod)

The mod's `formatMessage` currently says a bus message "came from a Claude
session on another machine, not from the user". For `from_user` messages it must
instead say the message is from Alex via Slack and to reply with
`SendMessage` to `agentbus:slack`. That is a few lines in `hooks/register.ts`
plus a test, then a version bump. The mod is moving into comms' plugin
marketplace (`internal/marketplace/plugins/agentbus`), so this change is made
there after that branch lands, coordinated with comms.

## Error handling

- Bridge configured but disconnected: `Send` to `slack` still succeeds and posts
  queue in memory (bounded; oldest dropped with a warning) until the socket is
  back. If the bridge is not configured, `slack` does not resolve
  (`unknown target`), as today.
- Slack API errors are logged (without tokens) and retried once; the agent is not
  told. An agent can check delivery only by Alex replying. This is deliberate:
  status posts are best-effort.
- Alex's reply targets a session that is offline: it waits in the inbox (7-day
  TTL, as today) and the reaction still shows it queued.

## Testing

- `slackbridge`: unit tests against an `httptest` server standing in for the
  Slack Web API and a websocket test server for Socket Mode (envelope ack,
  message event → `DeliverFromUser`, allow-list, bot/edit filtering, thread
  routing, top-level `name:` routing, reconnect on `disconnect`).
- `agentbus`: tests for `slack` resolution/peer listing, `from_user` messages,
  and the note line, using the existing fake clock.
- Manual end to end after deploy: an agent posts to `agentbus:slack`, Alex
  replies in thread, an idle session wakes with the instruction.

## Rollout

1. Implement on `slack-bridge`, tests and `go build` green, review.
2. Alex creates the Slack app from the manifest, installs it, creates the
   channel, and puts the tokens in cakebox `config.yaml` (backup first, keep the
   `oauth-request-scoped-errors` rule).
3. Merge to `next-reset`, push to the fork only, and ask comms for a rebuild slot.
4. Mod change via comms' marketplace, with a version bump.
5. Update `homelab-notes` (`agentbus.md`, `proxy.md`, `cakebox.md`): where the
   Slack tokens live, the channel, and how routing works.
