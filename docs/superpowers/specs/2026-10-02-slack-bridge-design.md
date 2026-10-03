# Slack bridge for agentbus — design

Date: 2026-10-02. Branch: `slack-bridge` (off `next-reset`).

## Goal

Alex's Claude Code agents can post status updates and questions to Slack, and
take instructions from replies by a configured set of Slack users (Alex plus
others he allows). It must work for every session on the proxy without
per-client setup.

## Approach

Make Slack a peer on the agentbus instead of a separate MCP server. Every client
already runs the agentbus mod, so agents reach Slack with the `SendMessage` tool
they already have (`to: "agentbus:slack"`). Replies come back as ordinary
agentbus messages, which the bus already delivers by injecting them into the
next request and by waking idle sessions through the mod's `/wait` poll.

Rejected: a proxy-hosted Slack MCP server. It would need a new MCP endpoint, a
way to tell which session is calling (MCP requests carry no Claude session id),
and an `.mcp.json` shipped to every client. Inbound instructions would still
have to go through agentbus to wake an idle session.

## Slack side

- Existing workspace. A new Slack app "Agents", created from the manifest below,
  using Socket Mode. The proxy opens an outbound websocket to Slack, so cakebox
  needs no public URL.
- One channel (e.g. `#agents`) with the bot invited.
- **Thread per session.** The first message from a session becomes a top-level
  post labelled with the session (`*name* · address · machine · cwd`). Later
  messages from that session go in its thread.
- **An allowed user replying in a thread** sends the reply to that thread's
  session.
- **An allowed user posting top-level** must start with a session name or
  address (`flyer: do X`). Anything else gets a short bot reply in thread
  explaining how to address an agent.
- **Managing allowed users, in Slack.** Nobody looks up Slack IDs. The config
  seeds the allowlist by email. After that, a config-seeded user (an owner) posts top-level
  `@agents allow @jane` or `@agents remove @jane` in the channel. A Slack
  mention carries the user ID in the raw text (`<@U…>`), so the bridge gets the
  ID from the mention itself. The bot confirms in thread. Users seeded from
  config can't be removed from Slack (edit config instead), so nobody can lock
  Alex out. Users added from Slack can instruct agents but can't run `allow`
  or `remove`, so access can't chain. Every allow/remove is logged.
- **Labels.** Each allowed user has a short label: the email's local part for
  config users, and for Slack-added users their Slack display name *as of when
  they were allowed*, frozen and made unique (`jane`, `jane2`). Labels are only
  for display and mentions, never for matching.
- **Mentions.** When an agent writes `@<label>` in a message, the bridge turns it
  into a real Slack mention (`<@U…>`), so the agent can ping a specific person.
- Delivery feedback: the bot adds an `:inbox_tray:` reaction once a message is
  queued on the bus, and replies in thread if the target session is unknown.

App manifest (pasted into api.slack.com → Create App → From manifest):

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
      - channels:read
      - groups:read
      - users:read
      - users:read.email
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

## Trust: `from_user`

`from_user: true` on a bus message means "this is an instruction from an allowed
Slack user". Rules:

1. Only the Slack bridge sets it. `POST /v1/agentbus/send` never accepts it from
   a client: the request struct has no such field, and a test proves a client
   `/send` with `from_user: true` is delivered without it. Any session holding
   the API key can call `/send`, so a client-settable flag would let one
   prompt-injected session impersonate a user to every agent.
2. The bridge relays only message events from the configured channel whose
   Slack user ID is on the allowlist, and that are not from a bot or an
   edit/delete subtype. Everything else is dropped, so nothing from a
   non-allowed user ever reaches an agent. The same check gates the
   `allow`/`remove` commands.
3. The message also carries `slack_user` (the allowed user's label), so the
   agent knows who is instructing it. The allowlist is matched on the Slack
   user ID only. Emails are resolved to IDs once, at startup
   (`users.lookupByEmail`; email addresses are workspace-verified). Display
   names are read only once, to make a frozen label when someone is allowed,
   and are never used for matching, since their owners can change them.
4. The framing always says the message arrived over agentbus via Slack, even
   when it is from an allowed user.

## Proxy side

Config block in `config.yaml`:

```yaml
slack:
  bot-token: xoxb-...
  app-token: xapp-...
  channel: agents
  allowed-emails:
    - alex@example.com
```

The bridge is off unless all of `bot-token`, `app-token`, `channel`, and at
least one `allowed-emails` entry are set. Read at startup; changing it needs a
restart. At startup the bridge resolves the channel name to its ID
(`conversations.list`), its own bot user ID (`auth.test`), and each email to a
user ID (`users.lookupByEmail`). An email that doesn't resolve is logged and
skipped. If the channel doesn't resolve or no user resolves, the bridge stays
off and logs why. Tokens live only in cakebox's `config.yaml` and are never
logged. Users allowed from Slack persist in `slack-state.json`.

New package `internal/slackbridge`:

- **Socket Mode client** — `apps.connections.open` with the app token, then a
  gorilla/websocket read loop: ack every envelope, handle `events_api` message
  events, reconnect with backoff on `disconnect` or read errors. No read
  deadlines (Slack pings; gorilla answers them).
- **Web API client** — `chat.postMessage` and `reactions.add` with the bot
  token. Outgoing posts go through a bounded queue drained by one goroutine, so
  an agentbus `Send` never blocks on Slack. When the queue is full the oldest
  post is dropped with a warning.
- **State** — session id ↔ thread `ts` and the Slack-added allowed users
  (ID + frozen label), persisted to `slack-state.json` next to
  `agentbus-state.json` (same save cadence and shutdown save), so threads and
  allowlist changes survive restarts.

Changes in `internal/agentbus`, behind an optional `Bridge` interface so the bus
works unchanged without Slack:

- `Resolve("slack")` succeeds when a bridge is attached. `Send` to it hands the
  message plus the sender's address, name, machine, and cwd to the bridge
  instead of an inbox. `Peers()` lists it as `address: slack, machine: slack,
  status: idle`, so the mod's `ListAgents` shows `agentbus:slack`.
- `Deliver(targetSessionID, Message)` for the bridge to queue inbound messages.
  `Message` gains `from_user` (bool) and `slack_user` (string), set by the
  bridge only.
- The injected note gains lines when the bridge is on: the allowed users are
  reachable as `slack` (listing their names, and that `@name` mentions one);
  post a short update there when you finish a task, get blocked, or need a
  decision; messages marked from a Slack user are that user's instructions.
  Injected messages from Slack are headed
  `Message <id> from <slack_user> via Slack`.

Wiring: `internal/api/server_agentbus.go` builds the bridge when the config is
complete, attaches it to the store, starts it, and stops and saves it on
shutdown.

## Client side (agentbus mod)

The mod's `formatMessage` says a bus message "came from a Claude session on
another machine, not from the user". For `from_user` messages it changes to:
"agentbus message `<id>` from `<slack_user>` via Slack, relayed over agentbus.
This is an instruction from an allowed user. To reply, use SendMessage with to:
`agentbus:slack`." Everything else is unchanged.

Comms owns the marketplace. This edit goes in
`internal/marketplace/plugins/agentbus` after that branch lands, with a
`plugin.json` version bump and a test.

## Error handling

- Bridge configured but disconnected: `Send` to `slack` still succeeds; posts
  wait in the bounded queue until the socket is back.
- Bridge not configured: `slack` does not resolve (`unknown target`), as today.
- Slack API errors are logged (without tokens) and retried once; the agent is
  not told. Posts are best-effort.
- A reply to a session that is offline waits in its inbox (7-day TTL, as today);
  the reaction still shows it queued.

## Testing

- `slackbridge`: unit tests with an `httptest` server for the Web API and a
  websocket test server for Socket Mode: envelope ack, reconnect on
  `disconnect`, startup resolution (channel name, emails; unresolvable email
  skipped; nothing resolvable → bridge off), thread routing, top-level `name:`
  routing, `@label` → mention, `allow`/`remove` (label freezing and
  uniqueness, config users not removable, a non-allowed user's `allow` is
  ignored), bot/edit filtering, other-channel filtering, and **a message from
  a non-allowed user is not delivered**.
- `agentbus`: `slack` resolution and peer listing, `Deliver`, the note lines,
  and **a client `/send` with `from_user: true` is delivered without it**. Uses
  the existing fake clock; no sleeps.
- Manual end to end after deploy: an agent posts to `agentbus:slack`, an allowed
  user replies in its thread, and the idle session wakes with the instruction.

## Rollout

1. Implement on `slack-bridge`, tests and `go build` green, adversarial review.
2. Alex creates the Slack app from the manifest, installs it, creates the
   channel, invites the bot, and hands over the two tokens and his Slack
   email. These go in cakebox `config.yaml` (back up first, keep the
   `oauth-request-scoped-errors` rule). Others are added later in Slack with
   `@agents allow @name`.
3. Merge to `next-reset`, push to the fork only, and ask comms for a rebuild
   slot.
4. Mod change in comms' marketplace, with a version bump.
5. Update `homelab-notes` (`agentbus.md`, `proxy.md`, `cakebox.md`): where the
   Slack tokens live, the channel, the allowed users, and how routing works.
