# Slack bridge batch 2: remote commands, command registry, screenshot, DMs

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** From Slack, owners can run Claude Code harness commands and registry-defined commands (including `!screenshot`) on any agent. Agents and allowed users can DM each other through the bot.

**Architecture:**
- The proxy parses `!name args` from owners and resolves it against a live-reloaded registry of YAML files on cakebox. Unknown names fall through to the harness slash command.
- The proxy delivers the resolved command as a structured, bridge-only `command` field on the bus message.
- The agentbus mod (0.3.3) executes it:
  - `slash` via `$.command.run`
  - `prompt` via `$.prompt.submit`
  - `shell` via `$.process.run` (per-OS argv, no shell)
- The mod reports the result back to the thread it came from. An image result goes through a JSON/base64 variant of the upload endpoint.
- DMs:
  - An agent addresses `slack@<label>` for outbound DMs.
  - Inbound DMs to the bot are routed like channel messages.

**Tech Stack:** Go 1.26, gorilla/websocket, gopkg.in/yaml.v3, TypeScript Claude Code mod (`claude plugin test`).

**Spec:** `docs/superpowers/specs/2026-10-02-slack-bridge-design.md` (base design). This plan adds the user-approved features below; record them in the spec as a "Batch 2" section in Task 1.

## User decisions (binding)

- `!command` syntax, because Slack refuses messages that start with `/`. Forms:
  - in an agent's thread or DM: `!compact`
  - top level: `bridge: !compact`
- Only **owners** (config-seeded users) can run commands. Allowed non-owners get a refusal.
- Any harness command passes through, with no filtering.
- `!rename X` sets both the Claude Code session title and the bus name. A local `/rename` updates the bus name too.
- The registry lives on cakebox as files. New commands need no plugin redeploy and no proxy restart.
- `shell` commands are allowed (owner-only, predefined argv, args passed as single argv values, never through a shell).
- `!screenshot` captures the agent's terminal window on Windows, falling back to the full screen. macOS and Linux get best-effort tools.
- DMs in both directions, limited to allowed users.
- The working-directory removal from thread headers (already committed, `bf412502`) ships in the same deploy.

## Global Constraints

- Branch `slack-features` in worktree `.claude/worktrees/slack-bridge`, based on `origin/catapultam`. Never push; the controller pushes.
- Commit messages carry no `Co-Authored-By` trailer and no "Generated with" line. Add docs with `git add -f` (docs/superpowers is gitignored).
- AGENTS.md:
  - gofmt.
  - English comments.
  - logrus.
  - No `log.Fatal`, no panics.
  - Method-suffixed error names.
  - No tokens, upload URLs, image bytes, message bodies or command output in logs.
  - No wall-clock sleeps in tests.
  - Timeouts only via the existing Slack API exception. `$.process.run`'s own timeout in the mod is the harness's, which is fine.
- Lock rule: never call the bridge while holding the store lock, and never call the store while holding bridge locks.
- `from_user` and `command` are set only by `Store.Deliver`, which only the bridge calls. A client `/send` can never set either. Prove it with a test like the existing `TestHTTPSendCannotSetFromUser`.
- Messages that carry a `command` are delivered ONLY through `/wait` (the mod). `InjectMiddleware` must leave them in the inbox, never inject them as prompt text.
- Mod version: `0.3.3`. Update `internal/marketplace/assets_test.go`, `doc_test.go` and `internal/api/server_plugins_test.go` (grep for `0.3.2`).
- Go: `export PATH="/c/Users/alex/sdk/go/bin:$PATH"`. There's no C toolchain; the controller runs `-race` elsewhere.
- Verify with gofmt, `go vet` on touched packages, `go test ./...`, the build check, and `claude plugin test` when the mod changes.

## Review Focus

- A non-owner, a peer session, or a `/send` client must never cause a `command` to run on an agent.
- A command message must never be shown to the model as plain text by the inject path. If it were, the model might "obey" it.
- A shell command's `{args}` must never be split or interpreted. Exactly one argv element.
- An inbound DM from a non-allowed user, a bot, or an edit is dropped, exactly like channel messages.
- An agent DMing a non-allowed label gets an error. No DM is ever sent to arbitrary workspace users.

---

### Task 1: Command registry and `!` parsing on the proxy

**Files:** create `internal/slackbridge/commands.go` and `commands_test.go`; modify `router.go`, `bridge.go`, `internal/agentbus/bridge.go` and `store.go` (`Message.Command`, `Deliver` variant), `inject.go` (skip command messages), the spec (Batch 2 section plus manifest scopes).

**Registry**
- Directory: `<runtime state dir>/agent-commands/`, next to `slack-state.json`, passed in `slackbridge.Config.CommandsDir`. Task 4 wires it.
- One `<name>.yaml` per command. The name comes from the filename and must match `^[a-z0-9][a-z0-9_-]{0,31}$`.
- Fields:
  - `description` (string)
  - `kind`: `slash` | `prompt` | `shell`
  - `slash`: `command` (slash name without `/`) and optional `args` (string; may contain `{args}`)
  - `prompt`: `text` (may contain `{args}`)
  - `shell`: `argv` maps an OS key (`windows`, `darwin`, `linux`) to a list of strings. The placeholders `{args}` and `{out}` may appear only as a WHOLE element; anywhere else they are rejected at load time.
  - `shell`: `output`: `text` | `image`; `timeout_seconds` (1-120, default 30)
- Load lazily on lookup. Re-read the directory when its listing or any file's mtime/size changes. Cache otherwise.
- A broken file is logged at Warn, once per change, and skipped. It never fails the bridge.
- A registry name shadows a harness command of the same name.

**Parsing**
- Runs after the existing allowlist and dedup checks, on the plain text the router already computes.
  - A thread reply, or a DM (Task 3), whose text starts with `!` is a command for that target.
  - A top-level `name: !…` is a command for `name`.
- `!name rest`: name `^[a-z0-9][a-z0-9:_-]{0,63}$` (lowercased). `rest` is trimmed, cap 2000 chars.
- Non-owner: reply "Only owners can run commands." and deliver nothing.
- `!commands`: the bridge itself replies with the registry list (name, kind, description) plus a line saying any Claude Code `/command` also works. Nothing goes to an agent.
- Resolution:
  - registry hit → that spec, with `{args}` filled later by the mod;
  - otherwise → `{kind: slash, command: name, args: rest}`.

**Delivery**
- `agentbus.Message` gains `Command *Command` with JSON tag `command,omitempty`, where:
  ```go
  type Command struct {
      Name    string
      Kind    string
      Command string
      Args    string
      Text    string
      Argv    map[string][]string
      Output  string
      Timeout int
  }
  ```
  Each field has a snake_case JSON tag.
- Add `Store.DeliverCommand(target string, cmd Command, slackUser string) (sessionID, msgID string, err error)`. It sets `FromUser`, `SlackUser` and `Command`; the body is a short human summary like `!compact`.
- Before delivering, the bridge checks the target supports commands. Add `Store.CommandCapable(target) (sessionID string, ok bool, err error)`: true only if the session has `Mod` set and the mod version recorded by `/hello` (Task 2 adds `version` to the hello request; store it as `ModVersion`) is >= `0.3.3`.
  - Not capable → reply "`<name>` can't run commands (agentbus plugin 0.3.3+ required)" and deliver nothing.
- `InjectMiddleware`'s `planInjection` leaves messages with `Command != nil` in the inbox. Test: such a message is never injected, and `/wait` still returns it.
- The bot reacts :gear: when a command is delivered (instead of :inbox_tray:).
- Record the reply map entry exactly as for normal deliveries, so the mod's result lands in the asking thread.

**Tests**
- Registry load, validation and reload (use a temp dir; change the mtime explicitly, no sleeps).
- Placeholder rule.
- Owner vs non-owner.
- `!commands` listing.
- Fallthrough slash.
- Non-capable target.
- Injection skip.
- A client `/send` with a `command` field arrives without it.

### Task 2: Mod 0.3.3 executes commands, reports results, `!rename`

**Files:** `internal/marketplace/plugins/agentbus/hooks/register.ts`, `tests/agentbus.test.ts`, `.claude-plugin/plugin.json`; the Go version tests; `internal/agentbus/http.go` (hello `version` field); `internal/agentbus/http_upload.go` (JSON variant).

**Hello**
- Send `version: "0.3.3"` in `/hello`. On `/wait`, send `&v=0.3.3`.
- The store records `ModVersion` and persists it.

**Executing a message with `from_user && command`** (from the `/wait` path only)
- `slash`: `await $.command.run({command, args})`. The result is `text`, or "done" if empty.
- `prompt`: `$.prompt.submit` with the `text` (`{args}` substituted), framed exactly like a from_user Slack message.
- `shell`:
  - Pick `argv` by OS: `windows` when `OS=Windows_NT` in env; otherwise the output of `uname -s`, where `Darwin` → `darwin` and anything else → `linux`.
  - Substitute `{args}` with the single string. Substitute `{out}` with a fresh temp file path (`TEMP`/`TMPDIR`/`/tmp` plus `agentbus-<id>.png`).
  - Run `$.process.run(argv, {timeoutMs})`.
  - `output: text`: post stdout, or stderr when the exit code isn't 0, truncated to 3500 chars.
  - `output: image`: read `{out}` with `$.fs.read(path, {as: 'bytes'})` (check the exact API in the type defs), POST it to the JSON upload variant, then delete the file. Use the fs API if one exists; otherwise run `rm`, or `cmd /c del` on Windows.
- Report the result with SendMessage-equivalent `/send` to `slack` with `reply_to` = the message id. Format: `✅ !name` plus the output in a code block, or `❌ !name: <error>`.
- Errors never leak tokens.

**Rename hook**
- Hook `command.run` with `{command: "rename"}`. Call `next(e)`, then, if `args.trim()` matches `^[A-Za-z0-9._-]{1,64}$`, POST `/name` with it.
- If the name is invalid for the bus or already taken, keep the session title and say so in the command output text.

**JSON upload variant**
- `POST /v1/agentbus/slack/upload` with `Content-Type: application/json` and body `{session, caption, reply_to, filename, data_base64}`.
- MaxBytesReader is 14 MiB. Decode the base64, then apply the same 10 MiB, sniff, slot and ownership rules as the multipart path. Share the code.

**Mod tests**
- A slash command runs and its result is sent with `reply_to`.
- A prompt command is submitted.
- Shell:
  - the per-OS argv is picked;
  - `{args}` is one element;
  - image output reads, uploads and deletes the file;
  - non-zero exit → ❌.
- A command on a non-from_user message is ignored.
- `/rename` posts `/name`.
- `hello` carries the version.

### Task 3: DMs both directions

**Files:** `internal/agentbus/bridge.go` and `store.go` (Send/Resolve for `slack@<label>`); `internal/slackbridge/api.go` (`conversations.open`); `bridge.go`; `router.go`; `state.go`; the note line in `inject.go`; the mod framing (a DM origin hint); the spec manifest.

**Outbound**
- `Send(from, "slack@<label>", …)`:
  - Allowed only when a bridge is attached and `<label>` (case-insensitive) is in `bridge.Users()`, which is called outside the store lock.
  - Otherwise `ErrUnknownTarget`.
  - `Outbound` gains `DM string` (the label).
- The bridge resolves label → user ID from state, calls `conversations.open` (users=ID) to get the DM channel (cached in memory), and posts there.
  - The first DM from a session in that channel starts with the session header line.
  - Record `dmLast[userID] = sessionID` (persisted, 7-day TTL). Also record `dm msg ts → session` so thread replies route.
- Images: the upload endpoint accepts an optional `to` = `slack@<label>`, with the same rule.

**Inbound**
- Accept `message` events with `channel_type == "im"` (or a channel ID starting with `D`) from allowed users.
  - Same bot, subtype and edit filters, and the same dedup.
  - Never from the bot itself.
- Routing:
  - A thread reply to an agent's DM message → that agent.
  - A top-level `name: …` → that agent; that agent becomes `dmLast` for this user.
  - A plain message → `dmLast[user]` if it's present and not expired; otherwise reply with help (who you can address).
- `!commands` and `!…` work in DMs with the same owner rule.
- Record the reply map with `channel = D…`. When the thread is empty, reply top-level in the DM.
- Deliveries from DMs have `SlackUser` set as usual. Add a `via` hint, `(DM)`, to the injected header and the mod framing, so the agent knows it came privately.

**Scopes**
- Add `im:write` and `im:history` to the spec manifest, plus the `message.im` bot event.
- Set `app_home: messages_tab_enabled: true` and `messages_tab_read_only_enabled: false`. Without these, users can't DM the bot.

**Tests**
- Outbound to an allowed label goes through `conversations.open` and posts to the DM channel.
- An unknown label → `ErrUnknownTarget`.
- An inbound DM from an allowed user → delivered.
- From a non-allowed user, a bot, or an edit → dropped.
- `dmLast` routing and its expiry (injected clock).
- A thread reply in the DM routes to the right agent.
- A command in a DM from an owner → delivered as a command; from a non-owner → refused.

### Task 4: Wiring, screenshot command, docs

**Files:** `internal/api/server_agentbus.go` (`CommandsDir: s.runtimeStatePath("agent-commands")`); a new `docs/agent-commands/` with `screenshot.yaml` and a `README.md` (format reference); `config.example.yaml` comment; spec.

**screenshot.yaml**
- `kind: shell`, `output: image`, `timeout_seconds: 30`.
- `windows` argv: `powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command <script> {out}`. The script:
  - walks up from its parent process to the first ancestor with a `MainWindowHandle`;
  - captures that window with user32 `PrintWindow` (PW_RENDERFULLCONTENT=2) into a bitmap;
  - falls back to `CopyFromScreen` of the primary screen;
  - saves the PNG to `$args[0]`.
  The script is inline in the YAML (block scalar), passed as the `-Command` element, with `{out}` as the next element.
- `darwin`: `screencapture -x {out}`.
- `linux`: `sh -c 'gnome-screenshot -f "$1" || grim "$1" || import -window root "$1"' sh {out}`. `{out}` is its own element; the `sh -c` script is a fixed string.
- Test the Windows script on this machine (CPLT-4A) in a terminal: it must produce a non-empty PNG of the terminal window. Report what was captured.

**Docs**
- `docs/agent-commands/README.md` documents the YAML format with one example of each kind.
- The spec gets the Batch 2 section and the updated manifest (scopes `im:write`, `im:history`, event `message.im`, app_home messages tab).

### Task 5: Read receipts, subagent guard, one-shot hint

User decisions: 📥 (`inbox_tray`) means queued; 👀 (`eyes`) means actually read. Only the main session may talk on the agentbus; subagents report to their parent.

**Read receipts**
- Record the Slack message's own `ts` in the reply record (state.go), next to channel/thread_ts/session. Persist it.
- Add an optional bridge interface in agentbus: `Seer{ Seen(msgIDs []string) }`. The store calls it outside its lock in two places:
  1. after `commitInjection` succeeds (the model received the message in a request);
  2. when the mod acknowledges. The mod (0.3.3) POSTs `/v1/agentbus/ack {session, ids}` after `$.prompt.submit` resolves for each message it got from `/wait`. Command messages are acked after the command runs.
  The `/wait` claim itself does NOT count as read.
- The bridge reacts `eyes` on the original Slack message for each id it knows. Unknown ids are ignored. Apply the same rule to DMs.
- Messages delivered to sessions without the mod get 👀 only through the injection path.
- Tests:
  - injection commit → `Seen`;
  - `/ack` → `Seen`, only for ids delivered to that session;
  - an id belonging to another session is ignored;
  - the reaction is posted to the right channel/ts.

**Subagent guard (mod)**
- In the `SendMessage` hook, when the target starts with `agentbus:` and the event has `agentId` (a subagent or teammate loop), refuse with `{success:false, message:"Only the main session talks on the agentbus. Report this to your parent agent, and it will send it."}`.
- Test it.

**One-shot hint (proxy)**
- In inject.go, when a session has no mod and injected messages include any from Slack, add one line: "This message is shown to you once. If you can't act on it now, write it into your task list."

### Task 6: Verify, review, deploy (controller)

- Full suite, `-race` in a throwaway container on cakebox (then clean up), and a final whole-branch review.
- Rebase onto `origin/catapultam`, push the branch, and hand it to comms.
- After deploy:
  - install `screenshot.yaml` into `/mnt/user/appdata/cliproxyapi/agent-commands/`;
  - the user updates the Slack app (scopes, event, messages tab) and reinstalls;
  - live test from Slack: `!commands`, `!rename`, `!screenshot`, a DM each way.
- Update homelab-notes `slack.md`.
