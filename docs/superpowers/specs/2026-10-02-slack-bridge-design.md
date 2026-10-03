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
  post labelled with the session (`*name* · address · machine`; the working directory is not sent to Slack). Later
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
  app_home:
    messages_tab_enabled: true
    messages_tab_read_only_enabled: false
oauth_config:
  scopes:
    bot:
      - chat:write
      - files:write
      - channels:history
      - groups:history
      - im:history
      - im:write
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
      - message.im
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
5. Nothing a session controls can imitate that framing. Session names must
   match `^[A-Za-z0-9._-]{1,64}$` (`/name` answers 400 otherwise; `/hello`
   ignores the name), `reply_to` is kept only when it looks like a message id
   (`m_<hex>`), every message body is rendered with each line prefixed by
   `> `, and interpolated values can't break a line or open or close the
   `<agentbus>` block. The note tells agents that only an unquoted header line
   reading `Message <id> from <name> via Slack (...)` (or the mod's
   `agentbus message <id> from <name> via Slack`) is an instruction.

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
  post is dropped with a warning. `remove` applies to the allowlist at once;
  `allow` jobs and command replies go on a separate small queue that the
  worker drains first and that drop-oldest never touches. A queued `allow`
  applies only if no later `allow`/`remove` for the same user came in.
- **State** — session id ↔ thread `ts` (a session posts in its first thread;
  every top-level `name:` thread that reached it stays linked, so replies
  there reach it too) and the Slack-added allowed users
  (ID + frozen label), persisted to `slack-state.json` next to
  `agentbus-state.json` (same save cadence and shutdown save), so threads and
  allowlist changes survive restarts.

Changes in `internal/agentbus`, behind an optional `Bridge` interface so the bus
works unchanged without Slack:

- `Resolve("slack")` succeeds when a bridge is attached. `Send` to it hands the
  message plus the sender's address, name and machine to the bridge
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

**Images.** A session posts an image with `POST /v1/agentbus/slack/upload`
(multipart: `session`, optional `caption`, `file`; same client API key as the
other agentbus routes). The proxy streams the body into memory, never to disk:
the file is capped at 10 MiB (413), its bytes are sniffed and only PNG, JPEG,
GIF and WebP are accepted (415), and an unknown session is a 400. The client
never picks a channel or thread: the image goes into the sending session's own
thread, and a session without one first gets its header (plus the caption)
posted to open it. The bridge uses Slack's external upload flow
(`files.getUploadURLExternal`, a raw POST of the bytes to the pre-signed URL
without the bot token, then `files.completeUploadExternal` with the thread and
the escaped caption), synchronously, so the agent gets 200 `{"ok":true}` or
502 with Slack's error code. The upload URL, tokens and image bytes are never
logged, and the request log skips this route. This needs the `files:write`
scope; an app installed before it was added must be reinstalled to grant it.
The same route takes `Content-Type: application/json` with
`{session, caption, reply_to, filename, data_base64}` (standard base64) under
a 14 MiB body cap; the decoded image goes through the same checks (10 MiB,
sniffing, upload slots, known session, Slack enabled). The mod uses it for
`output: image` commands, since a plugin can't build a multipart body.
The injected note's Slack lines include the curl command for it.

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

## Batch 2: remote commands, command registry, DMs

Date: 2026-10-03. Branch: `slack-features`. Plan:
`docs/superpowers/plans/2026-10-03-slack-commands-dms.md`.

User decisions:

- Commands use `!`, because Slack refuses messages that start with `/`: in an
  agent's thread or DM, `!compact`; at the top level, `name: !compact`. A bare
  top-level `!commands` also works.
- Only owners (config-seeded users) run commands. An allowed non-owner gets
  "Only owners can run commands." and nothing is delivered.
- Any Claude Code slash command passes through unfiltered. `!rename X` sets
  both the session title and the bus name.
- The registry is a directory of files on cakebox, so new commands need no
  plugin redeploy and no proxy restart. `shell` commands are allowed.
- DMs work in both directions, limited to allowed users (Task 3).
- The manifest above gains `im:history`, `im:write`, the `message.im` event
  and the app home messages tab; an installed app must be reinstalled to get
  them.

**Registry.** `slackbridge.Config.CommandsDir` (`<state dir>/agent-commands/`,
next to `slack-state.json`) holds one `<name>.yaml` per command; the name is
the file name and must match `^[a-z0-9][a-z0-9_-]{0,31}$` (`commands` is
built in). Fields: `description`; `kind` (`slash`, `prompt` or `shell`);
`slash`: `command` (without `/`) and optional `args` (may contain `{args}`);
`prompt`: `text` (may contain `{args}`); `shell`: `argv` (`windows`, `darwin`,
`linux` → list of strings, where `{args}` and `{out}` may only be whole
elements and never the program), optional `env` (name → value, names
`^[A-Za-z_][A-Za-z0-9_]{0,63}$`; a value is a literal without placeholders,
or exactly `{args}` or `{out}`, and only `AGENTBUS_*` names may take a
placeholder), `output` (`text` default, or `image`, which needs `{out}` in
each argv or in `env`), `timeout_seconds` (1-120, default 30), and at most
one of `args_pattern` (a regex that must compile on its own, then anchored as
`^(?:…)$`) and `args_enum` (a list). A command using `{args}` anywhere (argv
or env) must declare one of them; without `{args}` it takes no arguments,
and a shell argument never contains a line break. Free text never becomes
an argv element: `{args}` may be one only when the command declares
`args_enum` (owner-written values); with `args_pattern` it may only go
through `env`. The proxy sends `args_enum` with the command, and the mod
refuses `{args}` in argv unless `args` is in it. `{out}` (a temp path the mod
makes) may be an argv element. As defence in depth, interpreters and
wrappers never get either placeholder as an argument, since an interpreter
picks its script from its arguments (PowerShell 5.1 runs its first bare
argument as `-Command`; sh, python or node run the file their first argument
names; `python -m` names a module). Names are compared as Windows runs them:
base name, trailing dots and spaces dropped, lowercased, `.exe`/`.com`
suffixes removed repeatedly (re-trimming after each); an element matches as
that file name or with a `-preview` suffix and then a version run after its
letters removed (`python3.12`, `tclsh8.6`, `pwsh-preview`). Once an argv
element matches, no placeholder may follow it: sh, bash, zsh, dash, ksh,
fish, csh, tcsh, rbash, ash, mksh, yash, cmd, powershell, pwsh,
powershell_ise, python, python2, python3, py, pythonw, pypy, pypy3, node,
nodejs, deno, bun, perl, ruby, php, lua, tclsh, r, rscript, osascript,
wscript, cscript, mshta, awk, gawk, mawk, sed, ssh, wsl, ubuntu, debian,
env, xargs, busybox, sudo, su, doas, runas, watch, script, flock, nice,
nohup, timeout, stdbuf, time, chroot, setsid, unbuffer, conhost, forfiles,
rundll32, regsvr32, wt. A program whose normalized file name ends in `.bat`,
`.cmd`, `.ps1`, `.vbs`, `.js`, `.wsf` or `.hta` is refused (`run.bat.`
included). `tests/shell-cases.ts` in the mod is a table both sides' tests
run. The supported way to hand a value to a script is `env`, for example
`[sh, -c, 'gnome-screenshot -f "$AGENTBUS_OUT"']` with
`env: {AGENTBUS_OUT: "{out}"}`. A slash `args`
template without `{args}` takes no arguments either. Files are decoded
strictly (unknown keys, a second document or an empty file disable it). The
directory is re-read on lookup when its listing or a file's size or mtime
changes. A broken file, or a `.yaml` entry that can't be stat'ed or isn't a
regular file, is logged at Warn once per change and answers "`!name` is
misconfigured"; it never falls through to the harness command of the same
name. A directory read error other than "not found" keeps the last good
list. A registry name shadows a harness command. Command replies and
refusals use the bridge's command queue.

**Parsing and checks**, after the allowlist and dedup: `!name rest`, name
`^[a-z0-9][a-z0-9:_-]{0,63}$` (lowercased), rest trimmed and at most 2000
characters. Order: owner check, `!commands` (the bridge lists the registry
and notes that any `/command` works; nothing goes to an agent), registry
lookup (misconfigured → refuse; shell args rule → "`!name` takes no
arguments" or "invalid arguments for `!name`"), otherwise
`{kind: slash, command: name, args: rest}`. A registry slash command's
`{args}` is filled by the proxy; prompt and shell templates are sent as they
are with `args` = rest, and the mod substitutes. The target must be
`CommandCapable`: the session runs the mod (`mod`) and reported a mod version
(`ModVersion`, from `/hello`'s `version` or `/wait`'s `v`) of 0.3.3 or later,
compared numerically per segment. A mod-marked `/hello` or `/wait` without a
valid version clears `ModVersion`. Otherwise: "`name` can't run commands (agentbus plugin 0.3.3+
required)". A delivered command gets a :gear: reaction, its reply-map entry
(so the mod's result lands in the asking thread), and an Info log with the
owner's user ID, command name, kind and target address (never arguments or
output).

**Bus message.** `Message.command` (`name`, `kind`, `command`, `args`,
`text`, `argv`, `output`, `timeout`) and `Message.slack_user_id` are set only
by `Store.DeliverCommand`, which only the bridge calls; a client `/send`
can't set either. A command message is never injected into a request (the
model might obey it as text) and `/inbox` leaves it queued: it leaves the
store only through `/wait`, and only to a waiter that itself reports
`mod=1&v=` 0.3.3 or later (an older mod would show it to the model as an
instruction); for other waiters it stays queued. After the claim and outside
the store lock, the store asks the bridge (`OwnerChecker.IsOwner`) whether
`slack_user_id` is still an owner; if not, or if no bridge can tell, the
command is dropped, logged, and refused in its Slack thread. A command
message expires 10 minutes after it was sent (`commandTTL`), checked
wherever the inbox is read and again at hand-out; its thread is told
"`!name` expired before the agent picked it up".

**Mod (0.3.3).** Both `/hello` calls (session start, and the follow after
`/clear`, `/resume` or `/branch`) send `version: "0.3.3"`; every `/wait` sends
`mod=1&v=0.3.3`. A waited message with `command` runs only when it also has
`from_user`; otherwise it is dropped, neither run nor shown to the model. The
mod runs it in the background, so polling continues:

- `slash`: `$.command.run({command, args})`. Args holding a line break are
  refused. The output is the command's text, or "done".
- `prompt`: `text` with `{args}` replaced, submitted framed exactly like a
  `from_user` Slack message.
- `shell`: only where the machine opts in with `AGENTBUS_ALLOW_SHELL=1` in
  its environment; otherwise "shell commands are disabled on `<machine>` (set
  AGENTBUS_ALLOW_SHELL=1 there)". The argv is `windows` when `OS=Windows_NT`,
  else `darwin` when `uname -s` says `Darwin`, else `linux`. The mod repeats
  the registry's interpreter, script-file and env checks and refuses a
  definition that fails them with "unsafe command definition". An argv
  element or `env` value that is exactly `{args}` becomes `args`, whole;
  `{out}` becomes `<TEMP|TMP|TMPDIR|/tmp>/agentbus-<message id>.png`. `env` is
  set over the inherited environment. It runs through `$.process.run` (no
  shell) with `timeout` seconds. `text` output posts stdout, or stderr
  (falling back to stdout) on a non-zero exit, which is ❌. For `image`
  output the mod deletes any file at `{out}` first; afterwards `{out}` must be
  a non-empty file ("no image produced" otherwise), is read as bytes
  (`$.fs.read`, 4 MiB limit) and posted with the JSON upload and `reply_to`.
  Whenever `{out}` was used the file is deleted afterwards (the plugin API
  has no remove): on Windows by `powershell -NoProfile -NonInteractive
  -Command "Remove-Item -LiteralPath $env:AGENTBUS_OUT -Force -ErrorAction
  SilentlyContinue"` with the path in `AGENTBUS_OUT`, elsewhere by
  `rm -f -- <path>`.

Every run is reported with `/send` to `slack`, `reply_to` the command's
message id, from the session the command was delivered to (captured when
it starts, since `!clear` moves the session to a new id): `✅ !name:
<ok|exit 0|…>` or `❌ !name: <error>`, then `asked by <slack_user> · ran on
<machine> · <!name args>`, then the output in a code block cut to 3500
characters. The proxy token is redacted from every part before anything is
cut; nothing is logged locally. A `command.run` hook on `rename` calls
`next(e)` first, then, when the trimmed args match `^[A-Za-z0-9._-]{1,64}$`,
posts `/name`; an invalid or taken bus name keeps the new title and appends
a note to the command's output. A plugin's own `$.command.run` may skip the
plugin's own hooks (the test kit does), so a Slack `!rename` whose run the
hook didn't see renames on the bus itself, the same way.
