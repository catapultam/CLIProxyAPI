# Slack Bridge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Claude Code sessions on the proxy post to a Slack channel (one thread per session) and take instructions from allowed Slack users, through the existing agentbus.

**Architecture:** A new `internal/slackbridge` package holds a Slack Socket Mode connection and a small Web API client. It plugs into `internal/agentbus` through a `Bridge` interface: bus messages to the reserved address `slack` go out to Slack, and allowed users' Slack messages come back in through `Store.Deliver` with `from_user: true`. The agentbus mod (`internal/marketplace/plugins/agentbus`) frames those messages as the user's instructions.

**Tech Stack:** Go 1.26, gin, gorilla/websocket (already a dependency), tidwall/gjson, logrus; TypeScript Claude Code mod tested with `claude plugin test`.

**Spec:** `docs/superpowers/specs/2026-10-02-slack-bridge-design.md`

## Global Constraints

- Branch `slack-bridge` in worktree `.claude/worktrees/slack-bridge`. Commit after each task. Never push; the controller merges and pushes.
- Commit messages: no `Co-Authored-By` trailers and no "Generated with" lines.
- `gofmt -w` on every changed Go file. After each Go task, run `go build -o test-output ./cmd/server && rm test-output`.
- Comments in English. Use logrus (`log "github.com/sirupsen/logrus"`). No `log.Fatal`, no panics.
- Shadowed errors use a method suffix (`errRead`, `errClose`).
- No wall-clock `time.Sleep` in tests. Use channels, the existing `fakeClock`, or zero backoff. A `select` with a 5-second failure timeout is allowed as a test guard.
- Tokens are never logged or put in error strings.
- Lock rule: `agentbus.Store` calls `Bridge` methods without holding `s.mu`. The bridge never calls into the store while holding its own lock.
- `from_user` is set only by `Store.Deliver`, which only the bridge calls. `POST /v1/agentbus/send` must never set it.
- The allowlist is matched on Slack user ID only. Labels are display-only.
- Reserved bus address: `slack` (constant `agentbus.SlackAddress`).
- Message body cap is the existing `agentbus.MaxBodyBytes` (16 KiB).
- comms' session-lease change is on `next-reset` (99b1f8a4): `Store.Hello(id, machine, cwd, name string, mod bool)`, `Store.Bye`, lease-based `statusLocked`, `Peers()` lists only non-offline sessions, `nameFree` skips offline sessions, mod 0.3.0. The mod version goes to one past comms' (0.3.1 if theirs is 0.3.0). The `slack` peer is not a bus session, so it needs no `Pin` and leases can't expire it.

## Review Focus

- A Slack thread reply with "Also send to channel" ticked arrives with subtype `thread_broadcast`. It must be delivered like a normal reply, not dropped (Task 6 test).
- An agent's message containing `<!channel>` or `<@U…>` must not ping anyone. Agent text is escaped before `@label` mentions are applied (Task 5 test).
- Slack redelivers an event it thinks wasn't acked. The same `event_id` must not reach an agent twice (Task 6 test).
- A session that names itself `slack` would hijack the address. The name `slack` is reserved (Task 1 test).
- `https://example.com` posted top-level must not be read as "message for agent `https`" (Task 4 test).
- After comms' leases, a session that has ended may be gone from the bus. A reply in its thread must say the session ended, not leak a raw session ID (Task 6 `notFound` text; re-check against the rebased store).

---

### Task 1: agentbus bridge hooks

**Files:**
- Create: `internal/agentbus/bridge.go`
- Modify: `internal/agentbus/store.go` (Message struct, Store struct, `nameFree`, `Send`, `Peers`)
- Test: `internal/agentbus/bridge_test.go`

**Interfaces:**
- Produces:
  - `const SlackAddress = "slack"`
  - `type Outbound struct { SessionID, Address, Name, Machine, Cwd, Body string }`
  - `type Bridge interface { Post(Outbound); Users() []string }`
  - `func (s *Store) SetBridge(b Bridge)`
  - `func (s *Store) Deliver(target, body, slackUser string) (sessionID string, err error)`. `target` is a session ID, name, or address.
  - `Message` gains `FromUser bool` (`json:"from_user,omitempty"`) and `SlackUser string` (`json:"slack_user,omitempty"`).
  - `Peers()` includes `{Address: "slack", Machine: "slack", Status: "idle"}` while a bridge is attached.

- [ ] **Step 1: Write the failing tests**

Create `internal/agentbus/bridge_test.go`:

```go
package agentbus

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type fakeBridge struct {
	mu    sync.Mutex
	posts []Outbound
	users []string
}

func (f *fakeBridge) Post(o Outbound) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, o)
}

func (f *fakeBridge) Users() []string { return f.users }

func TestSendToSlackGoesToBridge(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	fb := &fakeBridge{users: []string{"alex"}}
	s.SetBridge(fb)
	msg, err := s.Send(sidA, "Slack", "build is green", "")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if msg.To != SlackAddress || msg.ID == "" {
		t.Fatalf("msg = %+v", msg)
	}
	if len(fb.posts) != 1 {
		t.Fatalf("posts = %+v", fb.posts)
	}
	got := fb.posts[0]
	want := Outbound{SessionID: sidA, Address: "pc/flyer-aaaaaa", Name: "flyer", Machine: "pc", Cwd: "/work/flyer", Body: "build is green"}
	if got != want {
		t.Fatalf("post = %+v, want %+v", got, want)
	}
	if s.Pending(sidA) {
		t.Fatal("slack message landed in an inbox")
	}
}

func TestSlackUnknownWithoutBridge(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	if _, err := s.Send(sidA, SlackAddress, "x", ""); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Deliver(sidA, "x", "alex"); err != nil {
		t.Fatalf("deliver works without a bridge (the bridge attaches late): %v", err)
	}
}

func TestDeliverSetsFromUser(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "flyer", true)
	s.Hello(sidB, "pc", "/b", "", true)
	for _, target := range []string{sidA, "flyer", "FLYER", "pc/a-aaaaaa"} {
		sid, err := s.Deliver(target, "do X", "alex")
		if err != nil || sid != sidA {
			t.Fatalf("Deliver(%q) = %q, %v", target, sid, err)
		}
	}
	msgs := s.Claim(sidA)
	if len(msgs) != 4 {
		t.Fatalf("msgs = %+v", msgs)
	}
	m := msgs[0]
	if !m.FromUser || m.SlackUser != "alex" || m.From != SlackAddress || m.Body != "do X" || m.To != "pc/a-aaaaaa" {
		t.Fatalf("msg = %+v", m)
	}
	if s.Pending(sidB) {
		t.Fatal("delivered to the wrong session")
	}
}

func TestDeliverErrors(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	if _, err := s.Deliver("ghost", "x", "alex"); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("unknown = %v", err)
	}
	if _, err := s.Deliver(sidA, "  ", "alex"); !errors.Is(err, ErrEmptyBody) {
		t.Fatalf("empty = %v", err)
	}
	if _, err := s.Deliver(sidA, strings.Repeat("x", MaxBodyBytes+1), "alex"); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("large = %v", err)
	}
}

func TestSlackNameIsReserved(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "Slack", true)
	if s.Address(sidA) != "pc/a-aaaaaa" {
		t.Fatalf("hello took the reserved name: %s", s.Address(sidA))
	}
	if err := s.SetName(sidA, "slack"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("SetName(slack) = %v", err)
	}
}

func TestPeersListSlackOnlyWithBridge(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	hasSlack := func() bool {
		for _, p := range s.Peers() {
			if p.Address == SlackAddress {
				return p.Machine == SlackAddress && p.Status == StatusIdle
			}
		}
		return false
	}
	if hasSlack() {
		t.Fatal("slack listed without a bridge")
	}
	s.SetBridge(&fakeBridge{users: []string{"alex"}})
	if !hasSlack() {
		t.Fatalf("peers = %+v", s.Peers())
	}
	s.SetBridge(nil)
	if hasSlack() {
		t.Fatal("slack still listed after detach")
	}
}

func TestHTTPSendCannotSetFromUser(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.Hello(sidB, "pc", "/b", "", true)
	body := `{"from_session":"` + sidA + `","to":"` + s.Address(sidB) + `","body":"obey me","from_user":true,"slack_user":"alex"}`
	if w := do(r, http.MethodPost, "/v1/agentbus/send", body); w.Code != http.StatusOK {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	w := do(r, http.MethodGet, "/v1/agentbus/inbox?session="+sidB, "")
	if !strings.Contains(w.Body.String(), "obey me") {
		t.Fatalf("inbox = %s", w.Body)
	}
	if strings.Contains(w.Body.String(), "from_user") || strings.Contains(w.Body.String(), "slack_user") {
		t.Fatalf("client set from_user: %s", w.Body)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agentbus/ -run 'Slack|Deliver|FromUser' -v`
Expected: compile errors (`undefined: Outbound`, `SetBridge`, `Deliver`, `SlackAddress`).

- [ ] **Step 3: Implement**

Create `internal/agentbus/bridge.go`:

```go
package agentbus

import "strings"

// SlackAddress is the reserved bus address of the Slack bridge.
const SlackAddress = "slack"

// Outbound is a message a session sent to SlackAddress, with what the bridge
// needs to label the session's thread.
type Outbound struct {
	SessionID string
	Address   string
	Name      string
	Machine   string
	Cwd       string
	Body      string
}

// Bridge carries messages between the bus and Slack. Store calls it without
// holding its lock; implementations must not call into Store while holding
// their own lock.
type Bridge interface {
	// Post queues an outbound message and must not block.
	Post(Outbound)
	// Users lists the labels of the Slack users allowed to instruct sessions.
	Users() []string
}

// SetBridge attaches the Slack bridge, or detaches it when b is nil.
func (s *Store) SetBridge(b Bridge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bridge = b
}

func (s *Store) currentBridge() Bridge {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bridge
}

func isSlackAddress(target string) bool {
	return strings.EqualFold(strings.TrimSpace(target), SlackAddress)
}

// Deliver queues a message from an allowed Slack user for a session given by
// id, name or address, and returns that session's id. Only the Slack bridge
// calls it, and it is the only way a message gets FromUser.
func (s *Store) Deliver(target, body, slackUser string) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", ErrEmptyBody
	}
	if len(body) > MaxBodyBytes {
		return "", ErrBodyTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := strings.TrimSpace(target)
	if _, ok := s.byID[id]; !ok {
		resolved, found := s.resolveLocked(target)
		if !found {
			return "", ErrUnknownTarget
		}
		id = resolved
	}
	sess := s.byID[id]
	s.enqueueLocked(sess, Message{
		ID:        newMessageID(),
		From:      SlackAddress,
		To:        s.addressLocked(sess),
		Body:      body,
		FromUser:  true,
		SlackUser: slackUser,
		CreatedAt: s.now(),
	})
	return id, nil
}
```

In `internal/agentbus/store.go`:

1. Add to `Message`, after `ReplyTo`:

```go
	// FromUser marks an instruction from an allowed Slack user. Only Deliver
	// sets it; clients can never send it.
	FromUser  bool   `json:"from_user,omitempty"`
	SlackUser string `json:"slack_user,omitempty"`
```

2. Add to `Store`, after `dirty bool`:

```go
	// bridge relays SlackAddress traffic; nil when Slack is off.
	bridge Bridge
```

3. At the top of `nameFree`, before the loop:

```go
	if isSlackAddress(name) {
		return false
	}
```

4. Replace `Send` with:

```go
// Send queues a message from a known session to a name or address. Messages to
// SlackAddress go to the bridge instead of an inbox.
func (s *Store) Send(fromID, to, body, replyTo string) (Message, error) {
	if strings.TrimSpace(body) == "" {
		return Message{}, ErrEmptyBody
	}
	if len(body) > MaxBodyBytes {
		return Message{}, ErrBodyTooLarge
	}
	s.mu.Lock()
	from, ok := s.byID[fromID]
	if !ok {
		s.mu.Unlock()
		return Message{}, ErrUnknownSender
	}
	msg := Message{
		ID:        newMessageID(),
		From:      s.addressLocked(from),
		Body:      body,
		ReplyTo:   strings.TrimSpace(replyTo),
		CreatedAt: s.now(),
	}
	if from.Name != "" {
		msg.From = from.Name + " (" + msg.From + ")"
	}
	if b := s.bridge; b != nil && isSlackAddress(to) {
		out := Outbound{
			SessionID: fromID,
			Address:   s.addressLocked(from),
			Name:      from.Name,
			Machine:   from.Machine,
			Cwd:       from.Cwd,
			Body:      body,
		}
		s.mu.Unlock()
		b.Post(out)
		msg.To = SlackAddress
		return msg, nil
	}
	defer s.mu.Unlock()
	targetID, ok := s.resolveLocked(to)
	if !ok {
		return Message{}, ErrUnknownTarget
	}
	target := s.byID[targetID]
	msg.To = s.addressLocked(target)
	s.enqueueLocked(target, msg)
	return msg, nil
}

func (s *Store) enqueueLocked(target *session, msg Message) {
	target.Inbox = append(target.Inbox, msg)
	if target.notify != nil {
		close(target.notify)
		target.notify = nil
	}
	s.dirty = true
}
```

5. In `Peers()`, just before `sort.Slice`:

```go
	if s.bridge != nil {
		out = append(out, Peer{Address: SlackAddress, Machine: SlackAddress, Status: StatusIdle, LastSeen: now})
	}
```

`sendRequest` in `http.go` stays as is. It has no `from_user` field, so JSON decoding drops it. The new test pins that.

- [ ] **Step 4: Run tests to verify they pass**

Run: `gofmt -w internal/agentbus && go test ./internal/agentbus/ -v`
Expected: all PASS, including the existing tests.

- [ ] **Step 5: Commit**

```bash
git add internal/agentbus/bridge.go internal/agentbus/bridge_test.go internal/agentbus/store.go
git commit -m "agentbus: reserved slack address and a Bridge hook for it"
```

---

### Task 2: agentbus note and message framing for Slack

**Files:**
- Modify: `internal/agentbus/inject.go` (`planInjection`, `noteText`)
- Test: `internal/agentbus/inject_test.go` (append)

**Interfaces:**
- Consumes: `Bridge`, `currentBridge()`, `Message.FromUser`, `Message.SlackUser` from Task 1.
- Produces: `noteText(sid, self, name, base string, mod bool, peers []string, note bool, msgs []Message, slackUsers []string) string`. A nil `slackUsers` means no bridge.

- [ ] **Step 1: Write the failing tests**

Append to `internal/agentbus/inject_test.go` (it already has `newInjectServer`, `post`, `lastUserTexts`, `stringContentBody`; `fakeBridge` comes from `bridge_test.go`):

```go
func TestInjectSlackLineWhenBridgeAttached(t *testing.T) {
	clock := &fakeClock{now: t0}
	s, r, got := newInjectServer(t, clock)
	s.SetBridge(&fakeBridge{users: []string{"alex", "jane"}})
	post(r, sidA, "", stringContentBody)
	note := strings.Join(lastUserTexts(got.body), "\n")
	for _, want := range []string{`alex, jane`, `"agentbus:slack"`, `@<name>`, `finish a task, get blocked, or need a decision`} {
		if !strings.Contains(note, want) {
			t.Fatalf("note missing %q:\n%s", want, note)
		}
	}
}

func TestInjectNoSlackLineWithoutBridge(t *testing.T) {
	clock := &fakeClock{now: t0}
	s, r, got := newInjectServer(t, clock)
	_ = s
	post(r, sidA, "", stringContentBody)
	if strings.Contains(strings.Join(lastUserTexts(got.body), "\n"), "Slack") {
		t.Fatalf("slack line without a bridge: %s", got.body)
	}
}

func TestInjectNoteAgainWhenSlackUsersChange(t *testing.T) {
	clock := &fakeClock{now: t0}
	s, r, got := newInjectServer(t, clock)
	fb := &fakeBridge{users: []string{"alex"}}
	s.SetBridge(fb)
	post(r, sidA, "", stringContentBody)
	post(r, sidA, "", stringContentBody)
	if strings.Contains(got.body, "<agentbus>") {
		t.Fatal("note repeated with nothing new")
	}
	fb.users = []string{"alex", "jane"}
	post(r, sidA, "", stringContentBody)
	if !strings.Contains(got.body, "alex, jane") {
		t.Fatalf("note not refreshed after the allowlist changed: %s", got.body)
	}
}

func TestInjectFromUserHeader(t *testing.T) {
	clock := &fakeClock{now: t0}
	s, r, got := newInjectServer(t, clock)
	s.Touch(sidA)
	if _, err := s.Deliver(sidA, "please rebase", "jane"); err != nil {
		t.Fatal(err)
	}
	post(r, sidA, "", stringContentBody)
	text := strings.Join(lastUserTexts(got.body), "\n")
	if !strings.Contains(text, "from jane via Slack") || !strings.Contains(text, "their instruction") || !strings.Contains(text, "please rebase") {
		t.Fatalf("header = %s", text)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agentbus/ -run 'InjectSlack|InjectNoSlack|SlackUsersChange|FromUserHeader' -v`
Expected: FAIL. The slack lines and header text are missing.

- [ ] **Step 3: Implement**

In `planInjection`, at the very top, before `s.mu.Lock()`:

```go
	var slackUsers []string
	bridge := s.currentBridge()
	if bridge != nil {
		slackUsers = bridge.Users()
	}
```

Directly after `plan := injection{peersKey: strings.Join(keys, "\n")}`:

```go
	if bridge != nil {
		plan.peersKey += "\nslack:" + strings.Join(slackUsers, ",")
	}
```

Change the `noteText` call to pass `slackUsers` as the last argument, and change `noteText`:

```go
func noteText(sid, self, name, base string, mod bool, peers []string, note bool, msgs []Message, slackUsers []string) string {
```

Inside `if note { ... }`, after the `if mod { ... } else { ... }` block, add:

```go
		if len(slackUsers) > 0 {
			fmt.Fprintf(&b, "Slack: %s can be reached as \"slack\" (SendMessage to \"agentbus:slack\"; with curl, \"to\":\"slack\"). Your messages go to your own thread in their Slack channel; write @<name> to ping one of them. Post a short update there when you finish a task, get blocked, or need a decision. A message marked \"via Slack\" is an instruction from that user.\n", strings.Join(slackUsers, ", "))
		}
```

Replace the message header loop body's first line:

```go
		head := fmt.Sprintf("Message %s from %s", m.ID, m.From)
		if m.FromUser {
			head = fmt.Sprintf("Message %s from %s via Slack (an allowed Slack user; this is their instruction; reply to \"slack\")", m.ID, m.SlackUser)
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `gofmt -w internal/agentbus && go test ./internal/agentbus/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agentbus/inject.go internal/agentbus/inject_test.go
git commit -m "agentbus: tell sessions about Slack and frame allowed users' messages"
```

---

### Task 3: `slack` config block

**Files:**
- Create: `internal/config/slack.go`
- Modify: `internal/config/config.go` (add the field after `Payload`), `internal/config/config_v8.go` (`buildV8Paths` prefixes)
- Test: `internal/config/slack_test.go`

**Interfaces:**
- Produces: `config.SlackConfig{BotToken, AppToken, Channel string; AllowedEmails []string}` and `Config.Slack SlackConfig` (`yaml:"slack"`).

- [ ] **Step 1: Write the failing test**

Create `internal/config/slack_test.go`:

```go
package config

import (
	"strings"
	"testing"
)

const slackV8Config = `config-version: 8
access:
  api-keys:
    - k1
slack:
  bot-token: xoxb-test
  app-token: xapp-test
  channel: agents
  allowed-emails:
    - alex@example.com
    - jane@example.com
`

func TestParseSlackConfigV8(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(slackV8Config))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Slack
	if s.BotToken != "xoxb-test" || s.AppToken != "xapp-test" || s.Channel != "agents" {
		t.Fatalf("slack = %+v", s)
	}
	if len(s.AllowedEmails) != 2 || s.AllowedEmails[1] != "jane@example.com" {
		t.Fatalf("emails = %v", s.AllowedEmails)
	}
}

func TestSlackSectionSurvivesV8Normalization(t *testing.T) {
	out, _, err := NormalizeConfigLayout([]byte(slackV8Config), true)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, "# slack") || !strings.Contains(text, "\nslack:") || !strings.Contains(text, "allowed-emails:") {
		t.Fatalf("slack section was dropped or commented out:\n%s", text)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run Slack -v`
Expected: compile error, `cfg.Slack undefined`.

- [ ] **Step 3: Implement**

Create `internal/config/slack.go`:

```go
package config

// SlackConfig connects the agentbus to one Slack channel (catapultam fork).
// The bridge is off unless every field is set.
type SlackConfig struct {
	// BotToken is the bot user OAuth token (xoxb-).
	BotToken string `yaml:"bot-token,omitempty" json:"bot-token,omitempty"`
	// AppToken is the app-level Socket Mode token (xapp-).
	AppToken string `yaml:"app-token,omitempty" json:"app-token,omitempty"`
	// Channel is the channel name (with or without #) or ID.
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty"`
	// AllowedEmails seeds the users allowed to instruct sessions.
	AllowedEmails []string `yaml:"allowed-emails,omitempty" json:"allowed-emails,omitempty"`
}
```

In `internal/config/config.go`, after the `Payload` field:

```go

	// Slack connects the agentbus to a Slack channel.
	Slack SlackConfig `yaml:"slack,omitempty" json:"slack,omitempty"`
```

In `internal/config/config_v8.go` `buildV8Paths`, add `{"slack", "slack"},` to the `prefixes` list, after the `pprof` entry. It is an identity mapping, so `slack` becomes an allowed v8 root and isn't commented out.

- [ ] **Step 4: Run tests to verify they pass**

Run: `gofmt -w internal/config && go test ./internal/config/ -v 2>&1 | tail -30`
Expected: all PASS. If a clone or field-coverage test fails because of the new field, extend it the same way the neighbouring fields are handled (e.g. copy `AllowedEmails` in `clone.go`) and re-run.

- [ ] **Step 5: Commit**

```bash
git add internal/config/slack.go internal/config/slack_test.go internal/config/config.go internal/config/config_v8.go
git commit -m "config: slack block for the agentbus Slack bridge"
```

---

### Task 4: slackbridge Web API client, text helpers, and the fake Slack

**Files:**
- Create: `internal/slackbridge/api.go`, `internal/slackbridge/format.go`
- Test: `internal/slackbridge/fake_slack_test.go` (shared test fake), `internal/slackbridge/api_test.go`, `internal/slackbridge/format_test.go`

**Interfaces:**
- Produces (package-private, used by Tasks 5–7):
  - `newAPI(base string) *api`
  - `(*api).authTest(ctx, token) (userID string, err error)`
  - `(*api).findChannel(ctx, token, name string) (id string, err error)`
  - `(*api).lookupByEmail(ctx, token, email string) (userID string, err error)`
  - `(*api).userInfo(ctx, token, userID string) (label string, isBot bool, err error)`
  - `(*api).postMessage(ctx, token, channel, text, threadTS string) (ts string, err error)`
  - `(*api).addReaction(ctx, token, channel, ts, name string) error`
  - `(*api).openConnection(ctx, token) (wsURL string, err error)`
  - `*apiError{method, code}`
  - `escape(s) string`, `withMentions(text string, ids map[string]string) string` (label→ID), `plainText(text string, labels map[string]string) string` (ID→label), `parseAddressed(text) (target, body string, ok bool)`, `parseCommand(text, botID string) (verb, userID string, isCommand bool)`, `sanitizeLabel(s) string`, `emailLabel(email) string`, `sessionHeader(o agentbus.Outbound) string`
  - Test fake: `newFakeSlack(t) *fakeSlack` with fields `URL`, `users`, `channels`, `acks chan string`, and methods `apiBase()`, `callsTo(method) []fakeCall`, `push(envelope string)`, `setFail(method, code string)`, `opens() int`

- [ ] **Step 1: Write the shared fake and failing tests**

Create `internal/slackbridge/fake_slack_test.go`:

```go
package slackbridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

type fakeUser struct {
	ID, Name, Display, Real, Email string
	Bot                            bool
}

type fakeChannel struct{ ID, Name string }

type fakeCall struct {
	Method string
	Form   url.Values
	Auth   string
}

// fakeSlack stands in for slack.com/api and the Socket Mode websocket.
type fakeSlack struct {
	t        *testing.T
	srv      *httptest.Server
	URL      string
	mu       sync.Mutex
	calls    []fakeCall
	users    []fakeUser
	channels []fakeChannel
	nextTS   int
	opened   int
	fail     map[string]string // method -> Slack error code
	toClient chan string
	acks     chan string
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{
		t:        t,
		users:    []fakeUser{{ID: "UALEX", Name: "alex", Display: "Alex", Email: "alex@example.com"}, {ID: "UJANE", Name: "jane", Display: "Jane D", Email: "jane@example.com"}, {ID: "UJANE2", Name: "jane2", Display: "jane d", Email: "jane2@example.com"}, {ID: "UEVE", Name: "eve", Display: "Eve"}, {ID: "UHOOK", Name: "ci", Display: "CI", Bot: true}},
		channels: []fakeChannel{{ID: "CGEN", Name: "general"}, {ID: "CAGENTS", Name: "agents"}},
		fail:     map[string]string{},
		toClient: make(chan string, 16),
		acks:     make(chan string, 16),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", f.handleAPI)
	mux.HandleFunc("/socket", f.handleSocket)
	f.srv = httptest.NewServer(mux)
	f.URL = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) apiBase() string { return f.URL + "/api/" }

func (f *fakeSlack) callsTo(method string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeSlack) opens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened
}

func (f *fakeSlack) push(envelope string) { f.toClient <- envelope }

// setFail makes method return a Slack error (under the lock, so -race is clean).
func (f *fakeSlack) setFail(method, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[method] = code
}

func (f *fakeSlack) handleAPI(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	if errParse := r.ParseForm(); errParse != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Method: method, Form: r.PostForm, Auth: r.Header.Get("Authorization")})
	code := f.fail[method]
	f.mu.Unlock()
	if code != "" {
		writeJSON(w, map[string]any{"ok": false, "error": code})
		return
	}
	switch method {
	case "auth.test":
		writeJSON(w, map[string]any{"ok": true, "user_id": "UBOT"})
	case "conversations.list":
		var chans []map[string]any
		for _, c := range f.channels {
			chans = append(chans, map[string]any{"id": c.ID, "name": c.Name})
		}
		writeJSON(w, map[string]any{"ok": true, "channels": chans, "response_metadata": map[string]any{"next_cursor": ""}})
	case "users.lookupByEmail":
		for _, u := range f.users {
			if u.Email != "" && strings.EqualFold(u.Email, r.PostForm.Get("email")) {
				writeJSON(w, map[string]any{"ok": true, "user": map[string]any{"id": u.ID}})
				return
			}
		}
		writeJSON(w, map[string]any{"ok": false, "error": "users_not_found"})
	case "users.info":
		for _, u := range f.users {
			if u.ID == r.PostForm.Get("user") {
				writeJSON(w, map[string]any{"ok": true, "user": map[string]any{"id": u.ID, "name": u.Name, "is_bot": u.Bot, "profile": map[string]any{"display_name": u.Display, "real_name": u.Real}}})
				return
			}
		}
		writeJSON(w, map[string]any{"ok": false, "error": "user_not_found"})
	case "chat.postMessage":
		f.mu.Lock()
		f.nextTS++
		ts := "1700000000." + strconv.Itoa(100000+f.nextTS)
		f.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "ts": ts, "channel": r.PostForm.Get("channel")})
	case "reactions.add":
		writeJSON(w, map[string]any{"ok": true})
	case "apps.connections.open":
		f.mu.Lock()
		f.opened++
		f.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(f.URL, "http") + "/socket"})
	default:
		writeJSON(w, map[string]any{"ok": false, "error": "unknown_method"})
	}
}

func (f *fakeSlack) handleSocket(w http.ResponseWriter, r *http.Request) {
	conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if errUpgrade != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var ack struct {
				EnvelopeID string `json:"envelope_id"`
			}
			if errRead := conn.ReadJSON(&ack); errRead != nil {
				return
			}
			f.acks <- ack.EnvelopeID
		}
	}()
	if errHello := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello"}`)); errHello != nil {
		return
	}
	for {
		select {
		case <-done:
			return
		case msg := <-f.toClient:
			if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(msg)); errWrite != nil {
				return
			}
			if strings.Contains(msg, `"type":"disconnect"`) {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
```

Create `internal/slackbridge/api_test.go`:

```go
package slackbridge

import (
	"context"
	"errors"
	"testing"
)

func TestAPICalls(t *testing.T) {
	f := newFakeSlack(t)
	a := newAPI(f.apiBase())
	ctx := context.Background()

	if id, err := a.authTest(ctx, "xoxb-1"); err != nil || id != "UBOT" {
		t.Fatalf("authTest = %q, %v", id, err)
	}
	if got := f.callsTo("auth.test")[0].Auth; got != "Bearer xoxb-1" {
		t.Fatalf("auth header = %q", got)
	}
	for _, name := range []string{"agents", "#agents", "AGENTS", "CAGENTS"} {
		if id, err := a.findChannel(ctx, "xoxb-1", name); err != nil || id != "CAGENTS" {
			t.Fatalf("findChannel(%q) = %q, %v", name, id, err)
		}
	}
	if _, err := a.findChannel(ctx, "xoxb-1", "nope"); err == nil {
		t.Fatal("missing channel resolved")
	}
	if id, err := a.lookupByEmail(ctx, "xoxb-1", "Alex@Example.com"); err != nil || id != "UALEX" {
		t.Fatalf("lookupByEmail = %q, %v", id, err)
	}
	var apiErr *apiError
	if _, err := a.lookupByEmail(ctx, "xoxb-1", "ghost@example.com"); !errors.As(err, &apiErr) || apiErr.code != "users_not_found" {
		t.Fatalf("lookup missing = %v", err)
	}
	if label, isBot, err := a.userInfo(ctx, "xoxb-1", "UJANE"); err != nil || label != "Jane D" || isBot {
		t.Fatalf("userInfo = %q %v %v", label, isBot, err)
	}
	ts, err := a.postMessage(ctx, "xoxb-1", "CAGENTS", "hi", "")
	if err != nil || ts == "" {
		t.Fatalf("postMessage = %q, %v", ts, err)
	}
	if _, err = a.postMessage(ctx, "xoxb-1", "CAGENTS", "re", ts); err != nil {
		t.Fatal(err)
	}
	posts := f.callsTo("chat.postMessage")
	if posts[0].Form.Get("thread_ts") != "" || posts[1].Form.Get("thread_ts") != ts || posts[1].Form.Get("text") != "re" {
		t.Fatalf("posts = %+v", posts)
	}
	if err = a.addReaction(ctx, "xoxb-1", "CAGENTS", ts, "inbox_tray"); err != nil {
		t.Fatal(err)
	}
	url, err := a.openConnection(ctx, "xapp-1")
	if err != nil || url == "" {
		t.Fatalf("openConnection = %q, %v", url, err)
	}
}

func TestAPIErrorNeverContainsToken(t *testing.T) {
	f := newFakeSlack(t)
	f.setFail("auth.test", "invalid_auth")
	_, err := newAPI(f.apiBase()).authTest(context.Background(), "xoxb-secret")
	if err == nil || err.Error() != "slack auth.test: invalid_auth" {
		t.Fatalf("err = %v", err)
	}
}

func TestAddReactionIgnoresAlreadyReacted(t *testing.T) {
	f := newFakeSlack(t)
	f.setFail("reactions.add", "already_reacted")
	if err := newAPI(f.apiBase()).addReaction(context.Background(), "x", "C", "1.1", "inbox_tray"); err != nil {
		t.Fatalf("err = %v", err)
	}
}
```

Create `internal/slackbridge/format_test.go`:

```go
package slackbridge

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

func TestWithMentions(t *testing.T) {
	ids := map[string]string{"alex": "UALEX", "jane.d": "UJANE"}
	cases := map[string]string{
		"@alex done":                  "<@UALEX> done",
		"ping @Alex.":                 "ping <@UALEX>.",
		"(@jane.d) see":               "(<@UJANE>) see",
		"mail bob@alex.com":           "mail bob@alex.com",
		"@nobody hi":                  "@nobody hi",
		"<!channel> & <@UEVE> a<b>c": "&lt;!channel&gt; &amp; &lt;@UEVE&gt; a&lt;b&gt;c",
	}
	for in, want := range cases {
		if got := withMentions(in, ids); got != want {
			t.Errorf("withMentions(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlainText(t *testing.T) {
	labels := map[string]string{"UALEX": "alex"}
	got := plainText("<@UALEX> and <@UEVE|eve>: see <https://x.test/a?b=1&amp;c=2|link> &lt;tag&gt; &amp;", labels)
	want := "@alex and @UEVE: see https://x.test/a?b=1&c=2 <tag> &"
	if got != want {
		t.Fatalf("plainText = %q, want %q", got, want)
	}
}

func TestParseAddressed(t *testing.T) {
	ok := map[string][2]string{
		"flyer: do X":               {"flyer", "do X"},
		"pc/comms-3a9e9c:  rebase\nnow": {"pc/comms-3a9e9c", "rebase\nnow"},
	}
	for in, want := range ok {
		target, body, found := parseAddressed(in)
		if !found || target != want[0] || body != want[1] {
			t.Errorf("parseAddressed(%q) = %q, %q, %v", in, target, body, found)
		}
	}
	for _, in := range []string{"https://example.com", "hello there", "flyer:", ": x", "no colon"} {
		if _, _, found := parseAddressed(in); found {
			t.Errorf("parseAddressed(%q) matched", in)
		}
	}
}

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in, verb, user string
		isCmd          bool
	}{
		{"<@UBOT> allow <@UJANE>", "allow", "UJANE", true},
		{"<@UBOT>  Remove  <@UJANE|jane>", "remove", "UJANE", true},
		{"<@UBOT> hello", "", "", true},
		{"<@UOTHER> allow <@UJANE>", "", "", false},
		{"flyer: <@UBOT> allow <@UJANE>", "", "", false},
	}
	for _, c := range cases {
		verb, user, isCmd := parseCommand(c.in, "UBOT")
		if verb != c.verb || user != c.user || isCmd != c.isCmd {
			t.Errorf("parseCommand(%q) = %q %q %v", c.in, verb, user, isCmd)
		}
	}
}

func TestLabels(t *testing.T) {
	cases := map[string]string{"Jane D": "jane-d", "  Ñ!! ": "user", "alex.smith.": "alex.smith", "Bob_O'Neil": "bob_o-neil"}
	for in, want := range cases {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
	if got := emailLabel("Alex.Smith@example.com"); got != "alex.smith" {
		t.Fatalf("emailLabel = %q", got)
	}
}

func TestSessionHeader(t *testing.T) {
	got := sessionHeader(agentbus.Outbound{Name: "flyer", Address: "pc/flyer-aaaaaa", Machine: "pc", Cwd: `C:\work\<x>`})
	want := "*flyer* · pc/flyer-aaaaaa · pc · `C:\\work\\&lt;x&gt;`"
	if got != want {
		t.Fatalf("header = %q, want %q", got, want)
	}
	if got = sessionHeader(agentbus.Outbound{Address: "pc/a-aaaaaa", Machine: "pc"}); got != "*pc/a-aaaaaa* · pc" {
		t.Fatalf("unnamed header = %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/slackbridge/ -v`
Expected: compile errors (`undefined: newAPI`, `withMentions`, …).

- [ ] **Step 3: Implement**

Create `internal/slackbridge/api.go`:

```go
// Package slackbridge connects the agentbus to one Slack channel over Socket
// Mode (catapultam fork).
package slackbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	defaultAPIBase = "https://slack.com/api/"
	// apiCallTimeout bounds one Web API call. Slack calls are control-plane
	// requests, not proxied upstream traffic (see the AGENTS.md exception).
	apiCallTimeout = 30 * time.Second
	maxAPIBody     = 4 << 20
)

// apiError is a Slack Web API "ok": false response.
type apiError struct{ method, code string }

func (e *apiError) Error() string { return "slack " + e.method + ": " + e.code }

type api struct {
	base string
	hc   *http.Client
}

func newAPI(base string) *api {
	if base == "" {
		base = defaultAPIBase
	}
	return &api{base: base, hc: &http.Client{Timeout: apiCallTimeout}}
}

// call POSTs form params to a Web API method. A 429 is retried once after
// Retry-After.
func (a *api) call(ctx context.Context, token, method string, params url.Values) (gjson.Result, error) {
	for attempt := 0; ; attempt++ {
		req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, a.base+method, strings.NewReader(params.Encode()))
		if errReq != nil {
			return gjson.Result{}, fmt.Errorf("slack %s: %w", method, errReq)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, errDo := a.hc.Do(req)
		if errDo != nil {
			return gjson.Result{}, fmt.Errorf("slack %s: %w", method, errDo)
		}
		data, errRead := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("slack %s: close body: %v", method, errClose)
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt == 0 {
			wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			timer := time.NewTimer(time.Duration(wait) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return gjson.Result{}, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if errRead != nil {
			return gjson.Result{}, fmt.Errorf("slack %s: read: %w", method, errRead)
		}
		if resp.StatusCode != http.StatusOK {
			return gjson.Result{}, fmt.Errorf("slack %s: HTTP %d", method, resp.StatusCode)
		}
		body := gjson.ParseBytes(data)
		if !body.Get("ok").Bool() {
			return body, &apiError{method: method, code: body.Get("error").String()}
		}
		return body, nil
	}
}

func (a *api) authTest(ctx context.Context, token string) (string, error) {
	body, err := a.call(ctx, token, "auth.test", url.Values{})
	if err != nil {
		return "", err
	}
	return body.Get("user_id").String(), nil
}

// findChannel resolves a channel name (with or without #) or ID among the
// channels the bot can see.
func (a *api) findChannel(ctx context.Context, token, name string) (string, error) {
	want := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "#"))
	cursor := ""
	for {
		params := url.Values{"types": {"public_channel,private_channel"}, "exclude_archived": {"true"}, "limit": {"1000"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		body, err := a.call(ctx, token, "conversations.list", params)
		if err != nil {
			return "", err
		}
		for _, ch := range body.Get("channels").Array() {
			if strings.ToLower(ch.Get("name").String()) == want || strings.EqualFold(ch.Get("id").String(), want) {
				return ch.Get("id").String(), nil
			}
		}
		cursor = body.Get("response_metadata.next_cursor").String()
		if cursor == "" {
			return "", fmt.Errorf("slack channel %q not found (for a private channel, invite the bot first)", name)
		}
	}
}

func (a *api) lookupByEmail(ctx context.Context, token, email string) (string, error) {
	body, err := a.call(ctx, token, "users.lookupByEmail", url.Values{"email": {strings.TrimSpace(email)}})
	if err != nil {
		return "", err
	}
	return body.Get("user.id").String(), nil
}

// userInfo returns a display name for labelling and whether the user is a bot.
func (a *api) userInfo(ctx context.Context, token, userID string) (string, bool, error) {
	body, err := a.call(ctx, token, "users.info", url.Values{"user": {userID}})
	if err != nil {
		return "", false, err
	}
	user := body.Get("user")
	label := user.Get("profile.display_name").String()
	if label == "" {
		label = user.Get("profile.real_name").String()
	}
	if label == "" {
		label = user.Get("name").String()
	}
	return label, user.Get("is_bot").Bool(), nil
}

func (a *api) postMessage(ctx context.Context, token, channel, text, threadTS string) (string, error) {
	params := url.Values{"channel": {channel}, "text": {text}}
	if threadTS != "" {
		params.Set("thread_ts", threadTS)
	}
	body, err := a.call(ctx, token, "chat.postMessage", params)
	if err != nil {
		return "", err
	}
	return body.Get("ts").String(), nil
}

func (a *api) addReaction(ctx context.Context, token, channel, ts, name string) error {
	_, err := a.call(ctx, token, "reactions.add", url.Values{"channel": {channel}, "timestamp": {ts}, "name": {name}})
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.code == "already_reacted" {
		return nil
	}
	return err
}

func (a *api) openConnection(ctx context.Context, token string) (string, error) {
	body, err := a.call(ctx, token, "apps.connections.open", url.Values{})
	if err != nil {
		return "", err
	}
	return body.Get("url").String(), nil
}
```

Create `internal/slackbridge/format.go`:

```go
package slackbridge

import (
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

var (
	slackMention = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)
	slackLink    = regexp.MustCompile(`<((?:https?|mailto):[^|>]+)(?:\|[^>]*)?>`)
	labelMention = regexp.MustCompile(`(?i)(^|[\s(\[{"'])@([a-z0-9][a-z0-9._-]*)`)
	addressed    = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._/-]*):[ \t]*(\S[\s\S]*)$`)
	labelUnsafe  = regexp.MustCompile(`[^a-z0-9._-]+`)

	slackEscaper   = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	slackUnescaper = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")
)

// escape makes agent text inert for Slack: & < > are control characters, so an
// agent writing "<!channel>" or "<@U…>" can't ping anyone.
func escape(s string) string { return slackEscaper.Replace(s) }

// withMentions escapes text, then turns @label into a real mention for
// allowed users (ids maps label to Slack user ID).
func withMentions(text string, ids map[string]string) string {
	return labelMention.ReplaceAllStringFunc(escape(text), func(m string) string {
		sub := labelMention.FindStringSubmatch(m)
		prefix, word := sub[1], sub[2]
		label := strings.ToLower(strings.TrimRight(word, "._-"))
		id, ok := ids[label]
		if !ok {
			return m
		}
		return prefix + "<@" + id + ">" + word[len(label):]
	})
}

// plainText turns Slack message markup into what a person typed: mentions
// become @label (labels maps user ID to label), links lose their brackets.
func plainText(text string, labels map[string]string) string {
	text = slackMention.ReplaceAllStringFunc(text, func(m string) string {
		id := slackMention.FindStringSubmatch(m)[1]
		if label, ok := labels[id]; ok {
			return "@" + label
		}
		return "@" + id
	})
	text = slackLink.ReplaceAllString(text, "$1")
	return slackUnescaper.Replace(text)
}

// parseAddressed splits a top-level "name: message" post.
func parseAddressed(text string) (string, string, bool) {
	m := addressed.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil || strings.HasPrefix(m[2], "//") {
		return "", "", false
	}
	return m[1], strings.TrimSpace(m[2]), true
}

// parseCommand recognises "@bot allow @user" and "@bot remove @user". isCommand
// is true for anything that starts by mentioning the bot.
func parseCommand(text, botID string) (string, string, bool) {
	t := strings.TrimSpace(text)
	loc := slackMention.FindStringSubmatchIndex(t)
	if loc == nil || loc[0] != 0 || t[loc[2]:loc[3]] != botID {
		return "", "", false
	}
	rest := strings.Fields(t[loc[1]:])
	if len(rest) != 2 {
		return "", "", true
	}
	verb := strings.ToLower(rest[0])
	m := slackMention.FindStringSubmatch(rest[1])
	if (verb != "allow" && verb != "remove") || m == nil || m[0] != rest[1] {
		return "", "", true
	}
	return verb, m[1], true
}

// sanitizeLabel makes a short lowercase handle agents can write as @label.
func sanitizeLabel(s string) string {
	label := strings.Trim(labelUnsafe.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-"), "._-")
	if label == "" {
		return "user"
	}
	return label
}

func emailLabel(email string) string {
	local, _, _ := strings.Cut(strings.TrimSpace(email), "@")
	return sanitizeLabel(local)
}

// sessionHeader is the first line of a session's thread.
func sessionHeader(o agentbus.Outbound) string {
	who := o.Name
	if who == "" {
		who = o.Address
	}
	parts := []string{"*" + escape(who) + "*"}
	if o.Name != "" && o.Address != "" {
		parts = append(parts, escape(o.Address))
	}
	if o.Machine != "" {
		parts = append(parts, escape(o.Machine))
	}
	if o.Cwd != "" {
		parts = append(parts, "`"+escape(o.Cwd)+"`")
	}
	return strings.Join(parts, " · ")
}
```

Note for `TestLabels`: `"Bob_O'Neil"` → lower `bob_o'neil` → `'` becomes `-` → `bob_o-neil`. `"  Ñ!! "` → `ñ!!` is all unsafe → `-` → trimmed → empty → `user`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `gofmt -w internal/slackbridge && go test ./internal/slackbridge/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/slackbridge/
git commit -m "slackbridge: Web API client and Slack text helpers"
```

---

### Task 5: slackbridge state, bridge core, and outbound posts

**Files:**
- Create: `internal/slackbridge/state.go`, `internal/slackbridge/bridge.go`
- Test: `internal/slackbridge/state_test.go`, `internal/slackbridge/bridge_test.go`

**Interfaces:**
- Consumes: Task 1 (`agentbus.Store`, `Outbound`, `SetBridge`), Task 4 (`api`, helpers, fake).
- Produces:
  - `type Config struct { BotToken, AppToken, Channel string; AllowedEmails []string; StatePath string; APIBase string }`
  - `func New(cfg Config, bus *agentbus.Store) (*Bridge, error)` returns `nil, nil` when `cfg` is incomplete.
  - `(*Bridge).Post(agentbus.Outbound)`, `(*Bridge).Users() []string` (satisfies `agentbus.Bridge`)
  - `(*Bridge).resolve(ctx) error`, `(*Bridge).enqueue(job)`, `(*Bridge).runJobs(ctx)`, `type job func(ctx context.Context) error`
  - Fields used by later tasks: `b.bus`, `b.api`, `b.state`, `b.cfg`, `b.channelID`, `b.botUserID`, `b.jobs`, `b.backoff`, `b.retryDelay`, `b.dialer`
  - state: `loadState(path) (*state, error)`, `(*state).seed([]allowedUser)`, `user(id) (allowedUser, bool)`, `labels() []string`, `mentionIDs() map[string]string`, `idLabels() map[string]string`, `allow(id, rawLabel string) (allowedUser, bool, error)`, `remove(id string) error`, `thread(sid) (string, bool)`, `session(ts) (string, bool)`, `setThread(sid, ts string) bool`, `errConfigUser`, `errNotAllowed`
  - Test helper (in `bridge_test.go`): `newTestBridge(t) (*Bridge, *fakeSlack, *agentbus.Store)` and `drainJobs(t, b)`

- [ ] **Step 1: Write the failing tests**

Create `internal/slackbridge/state_test.go`:

```go
package slackbridge

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStateSeedAllowRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slack-state.json")
	st, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	st.seed([]allowedUser{{ID: "UALEX", Label: "alex", config: true}})
	u, added, err := st.allow("UJANE", "Jane D")
	if err != nil || !added || u.Label != "jane-d" {
		t.Fatalf("allow = %+v %v %v", u, added, err)
	}
	if _, added, _ = st.allow("UJANE", "whatever"); added {
		t.Fatal("allowed twice")
	}
	if u, _, _ = st.allow("UJANE2", "jane d"); u.Label != "jane-d2" {
		t.Fatalf("label not unique: %q", u.Label)
	}
	if u, _, _ = st.allow("UALEX2", "alex"); u.Label != "alex2" {
		t.Fatalf("label clashes with a config user: %q", u.Label)
	}
	if !reflect.DeepEqual(st.labels(), []string{"alex", "jane-d", "jane-d2", "alex2"}) {
		t.Fatalf("labels = %v", st.labels())
	}
	if !errors.Is(st.remove("UALEX"), errConfigUser) {
		t.Fatal("config user removed")
	}
	if !errors.Is(st.remove("UNOPE"), errNotAllowed) {
		t.Fatal("removed a stranger")
	}
	if err = st.remove("UJANE2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.user("UJANE2"); ok {
		t.Fatal("still allowed after remove")
	}
	if !st.setThread("sid-a", "1.1") || st.setThread("sid-a", "2.2") {
		t.Fatal("setThread must only set once")
	}

	reloaded, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.seed([]allowedUser{{ID: "UALEX", Label: "alex", config: true}, {ID: "UJANE", Label: "jane", config: true}})
	if ts, ok := reloaded.thread("sid-a"); !ok || ts != "1.1" {
		t.Fatalf("thread = %q %v", ts, ok)
	}
	if sid, ok := reloaded.session("1.1"); !ok || sid != "sid-a" {
		t.Fatalf("session = %q %v", sid, ok)
	}
	if u, ok := reloaded.user("UJANE"); !ok || u.Label != "jane" || !u.config {
		t.Fatalf("config must win over a persisted Slack-added entry: %+v", u)
	}
	if u, ok := reloaded.user("UALEX2"); !ok || u.Label != "alex2" {
		t.Fatalf("slack-added user lost: %+v %v", u, ok)
	}
	if !reflect.DeepEqual(reloaded.mentionIDs(), map[string]string{"alex": "UALEX", "jane": "UJANE", "alex2": "UALEX2"}) {
		t.Fatalf("mentionIDs = %v", reloaded.mentionIDs())
	}
}
```

Create `internal/slackbridge/bridge_test.go`:

```go
package slackbridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

const (
	sidA = "aaaaaa11-2222-3333-4444-555555555555"
	sidB = "bbbbbb11-2222-3333-4444-555555555555"
)

func testConfig(f *fakeSlack, dir string) Config {
	return Config{
		BotToken:      "xoxb-test",
		AppToken:      "xapp-test",
		Channel:       "#agents",
		AllowedEmails: []string{"alex@example.com", "nobody@example.com"},
		StatePath:     filepath.Join(dir, "slack-state.json"),
		APIBase:       f.apiBase(),
	}
}

// newTestBridge returns a resolved bridge attached to a bus with two
// sessions: sidA named "flyer" and sidB unnamed. Jobs run only via drainJobs.
func newTestBridge(t *testing.T) (*Bridge, *fakeSlack, *agentbus.Store) {
	t.Helper()
	f := newFakeSlack(t)
	bus := agentbus.NewStore("", nil)
	bus.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	bus.Hello(sidB, "pc", "/work/other", "", true)
	b, err := New(testConfig(f, t.TempDir()), bus)
	if err != nil || b == nil {
		t.Fatalf("New = %v, %v", b, err)
	}
	b.backoff = func(int) time.Duration { return 0 }
	b.retryDelay = 0
	if err = b.resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	bus.SetBridge(b)
	return b, f, bus
}

func drainJobs(t *testing.T, b *Bridge) {
	t.Helper()
	for {
		select {
		case j := <-b.jobs:
			if err := j(context.Background()); err != nil {
				t.Fatalf("job: %v", err)
			}
		default:
			return
		}
	}
}

func TestNewIsOffWhenIncomplete(t *testing.T) {
	f := newFakeSlack(t)
	full := testConfig(f, t.TempDir())
	for _, cfg := range []Config{
		{AppToken: full.AppToken, Channel: full.Channel, AllowedEmails: full.AllowedEmails},
		{BotToken: full.BotToken, Channel: full.Channel, AllowedEmails: full.AllowedEmails},
		{BotToken: full.BotToken, AppToken: full.AppToken, AllowedEmails: full.AllowedEmails},
		{BotToken: full.BotToken, AppToken: full.AppToken, Channel: full.Channel},
	} {
		if b, err := New(cfg, agentbus.NewStore("", nil)); b != nil || err != nil {
			t.Fatalf("New(%+v) = %v, %v", cfg, b, err)
		}
	}
}

func TestResolve(t *testing.T) {
	b, _, _ := newTestBridge(t)
	if b.channelID != "CAGENTS" || b.botUserID != "UBOT" {
		t.Fatalf("channel %q bot %q", b.channelID, b.botUserID)
	}
	if got := b.Users(); len(got) != 1 || got[0] != "alex" {
		t.Fatalf("users = %v (the unknown email must be skipped)", got)
	}
}

func TestResolveFailsWhenNoUserMatches(t *testing.T) {
	f := newFakeSlack(t)
	cfg := testConfig(f, t.TempDir())
	cfg.AllowedEmails = []string{"nobody@example.com"}
	b, _ := New(cfg, agentbus.NewStore("", nil))
	if err := b.resolve(context.Background()); err == nil {
		t.Fatal("resolved with no allowed users")
	}
	f.setFail("auth.test", "invalid_auth")
	cfg.AllowedEmails = []string{"alex@example.com"}
	b, _ = New(cfg, agentbus.NewStore("", nil))
	if err := b.resolve(context.Background()); err == nil || strings.Contains(err.Error(), "xoxb") {
		t.Fatalf("err = %v", err)
	}
}

func TestPostOpensThreadThenReplies(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if _, err := bus.Send(sidA, "slack", "starting @alex <!channel>", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Send(sidA, "slack", "done", ""); err != nil {
		t.Fatal(err)
	}
	drainJobs(t, b)
	posts := f.callsTo("chat.postMessage")
	if len(posts) != 2 {
		t.Fatalf("posts = %+v", posts)
	}
	first, second := posts[0].Form, posts[1].Form
	if first.Get("channel") != "CAGENTS" || first.Get("thread_ts") != "" {
		t.Fatalf("first = %v", first)
	}
	wantText := "*flyer* · pc/flyer-aaaaaa · pc · `/work/flyer`\nstarting <@UALEX> &lt;!channel&gt;"
	if first.Get("text") != wantText {
		t.Fatalf("first text = %q", first.Get("text"))
	}
	ts, _ := b.state.thread(sidA)
	if second.Get("thread_ts") != ts || second.Get("text") != "done" {
		t.Fatalf("second = %v (thread %q)", second, ts)
	}
}

func TestQueueDropsOldestWhenFull(t *testing.T) {
	b, _, _ := newTestBridge(t)
	for i := 0; i < jobQueueSize+5; i++ {
		b.Post(agentbus.Outbound{SessionID: sidA, Body: "x"})
	}
	if len(b.jobs) != jobQueueSize {
		t.Fatalf("queued = %d", len(b.jobs))
	}
}

func TestRunJobsRetriesOnce(t *testing.T) {
	b, _, _ := newTestBridge(t)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	done := make(chan struct{})
	b.enqueue(func(context.Context) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded
		}
		close(done)
		return nil
	})
	go b.runJobs(ctx)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("job not retried")
	}
	cancel()
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/slackbridge/ -v`
Expected: compile errors (`undefined: loadState`, `New`, `Bridge`, …).

- [ ] **Step 3: Implement**

Create `internal/slackbridge/state.go`:

```go
package slackbridge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

var (
	errConfigUser = errors.New("user is set in config.yaml")
	errNotAllowed = errors.New("user is not allowed")
)

type allowedUser struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// config marks users seeded from allowed-emails; they are not persisted
	// and can't be removed from Slack.
	config bool
}

type stateFile struct {
	Threads map[string]string `json:"threads"`
	Allowed []allowedUser     `json:"allowed"`
}

// state holds session threads and the allowlist. Every change is written
// through to disk; changes are rare.
type state struct {
	path     string
	mu       sync.Mutex
	threads  map[string]string // session id -> thread ts
	sessions map[string]string // thread ts -> session id
	users    []allowedUser     // config users first
}

// loadState reads path; a missing file is an empty state. On a corrupt file it
// returns an empty state and the error.
func loadState(path string) (*state, error) {
	st := &state{path: path, threads: map[string]string{}, sessions: map[string]string{}}
	if path == "" {
		return st, nil
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return st, nil
		}
		return st, errRead
	}
	var file stateFile
	if errJSON := json.Unmarshal(data, &file); errJSON != nil {
		return st, errJSON
	}
	for sid, ts := range file.Threads {
		st.threads[sid] = ts
		st.sessions[ts] = sid
	}
	st.users = file.Allowed
	return st, nil
}

// seed puts config users first. A persisted Slack-added entry for the same ID
// is replaced by the config entry, and clashing labels are renumbered.
func (st *state) seed(config []allowedUser) {
	st.mu.Lock()
	defer st.mu.Unlock()
	previous := st.users
	st.users = nil
	seen := map[string]bool{}
	for _, u := range config {
		if seen[u.ID] {
			continue
		}
		seen[u.ID] = true
		u.Label = st.uniqueLabelLocked(u.Label)
		u.config = true
		st.users = append(st.users, u)
	}
	for _, u := range previous {
		if seen[u.ID] || u.config {
			continue
		}
		seen[u.ID] = true
		u.Label = st.uniqueLabelLocked(u.Label)
		st.users = append(st.users, u)
	}
}

func (st *state) uniqueLabelLocked(base string) string {
	taken := func(label string) bool {
		for _, u := range st.users {
			if u.Label == label {
				return true
			}
		}
		return false
	}
	if !taken(base) {
		return base
	}
	for i := 2; ; i++ {
		if label := base + strconv.Itoa(i); !taken(label) {
			return label
		}
	}
}

func (st *state) user(id string) (allowedUser, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, u := range st.users {
		if u.ID == id {
			return u, true
		}
	}
	return allowedUser{}, false
}

func (st *state) labels() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]string, 0, len(st.users))
	for _, u := range st.users {
		out = append(out, u.Label)
	}
	return out
}

// mentionIDs maps label to user ID.
func (st *state) mentionIDs() map[string]string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]string, len(st.users))
	for _, u := range st.users {
		out[u.Label] = u.ID
	}
	return out
}

// idLabels maps user ID to label.
func (st *state) idLabels() map[string]string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]string, len(st.users))
	for _, u := range st.users {
		out[u.ID] = u.Label
	}
	return out
}

// allow adds a user with a frozen label made from rawLabel. It returns the
// existing entry and false when the user is already allowed.
func (st *state) allow(id, rawLabel string) (allowedUser, bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, u := range st.users {
		if u.ID == id {
			return u, false, nil
		}
	}
	u := allowedUser{ID: id, Label: st.uniqueLabelLocked(sanitizeLabel(rawLabel))}
	st.users = append(st.users, u)
	return u, true, st.saveLocked()
}

func (st *state) remove(id string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, u := range st.users {
		if u.ID != id {
			continue
		}
		if u.config {
			return errConfigUser
		}
		st.users = append(st.users[:i], st.users[i+1:]...)
		return st.saveLocked()
	}
	return errNotAllowed
}

func (st *state) thread(sid string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	ts, ok := st.threads[sid]
	return ts, ok
}

func (st *state) session(ts string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sid, ok := st.sessions[ts]
	return sid, ok
}

// setThread links a session to a thread unless it already has one.
func (st *state) setThread(sid, ts string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.threads[sid]; ok {
		return false
	}
	st.threads[sid] = ts
	st.sessions[ts] = sid
	if errSave := st.saveLocked(); errSave != nil {
		logSaveError(errSave)
	}
	return true
}

func (st *state) saveLocked() error {
	if st.path == "" {
		return nil
	}
	file := stateFile{Threads: st.threads}
	for _, u := range st.users {
		if !u.config {
			file.Allowed = append(file.Allowed, u)
		}
	}
	data, errJSON := json.Marshal(file)
	if errJSON != nil {
		return errJSON
	}
	if errMkdir := os.MkdirAll(filepath.Dir(st.path), 0o700); errMkdir != nil {
		return errMkdir
	}
	tmp := st.path + ".tmp"
	if errWrite := os.WriteFile(tmp, data, 0o600); errWrite != nil {
		return errWrite
	}
	return os.Rename(tmp, st.path)
}
```

Create `internal/slackbridge/bridge.go`:

```go
package slackbridge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
)

const (
	jobQueueSize = 256
	maxBackoff   = 5 * time.Minute
	seenEvents   = 512
)

// Config is what the bridge needs from config.yaml plus where to keep state.
type Config struct {
	BotToken      string
	AppToken      string
	Channel       string
	AllowedEmails []string
	StatePath     string
	// APIBase overrides https://slack.com/api/ (tests).
	APIBase string
}

func (c Config) complete() bool {
	return strings.TrimSpace(c.BotToken) != "" && strings.TrimSpace(c.AppToken) != "" &&
		strings.TrimSpace(c.Channel) != "" && len(c.AllowedEmails) > 0
}

type job func(ctx context.Context) error

// Bridge links the agentbus to one Slack channel. It implements
// agentbus.Bridge.
type Bridge struct {
	cfg        Config
	bus        *agentbus.Store
	api        *api
	dialer     *websocket.Dialer
	state      *state
	jobs       chan job
	backoff    func(attempt int) time.Duration
	retryDelay time.Duration

	// Set by resolve before the bridge is attached or the socket runs.
	channelID string
	botUserID string

	seenMu   sync.Mutex
	seen     map[string]bool
	seenRing []string

	cancel context.CancelFunc
	done   chan struct{}
}

// New builds a bridge, or returns nil, nil when cfg is incomplete (Slack off).
func New(cfg Config, bus *agentbus.Store) (*Bridge, error) {
	if !cfg.complete() {
		return nil, nil
	}
	st, errLoad := loadState(cfg.StatePath)
	if errLoad != nil {
		log.Warnf("slack: load state (starting empty): %v", errLoad)
	}
	return &Bridge{
		cfg:        cfg,
		bus:        bus,
		api:        newAPI(cfg.APIBase),
		dialer:     websocket.DefaultDialer,
		state:      st,
		jobs:       make(chan job, jobQueueSize),
		backoff:    defaultBackoff,
		retryDelay: 2 * time.Second,
		seen:       map[string]bool{},
	}, nil
}

func defaultBackoff(attempt int) time.Duration {
	d := time.Second << min(attempt, 9)
	return min(d, maxBackoff)
}

func logSaveError(err error) { log.Warnf("slack: save state: %v", err) }

// Users lists the allowed users' labels.
func (b *Bridge) Users() []string { return b.state.labels() }

// Post queues a session's message for its thread. It never blocks.
func (b *Bridge) Post(o agentbus.Outbound) {
	b.enqueue(func(ctx context.Context) error { return b.postOutbound(ctx, o) })
}

func (b *Bridge) postOutbound(ctx context.Context, o agentbus.Outbound) error {
	text := withMentions(o.Body, b.state.mentionIDs())
	if ts, ok := b.state.thread(o.SessionID); ok {
		_, err := b.api.postMessage(ctx, b.cfg.BotToken, b.channelID, text, ts)
		return err
	}
	ts, err := b.api.postMessage(ctx, b.cfg.BotToken, b.channelID, sessionHeader(o)+"\n"+text, "")
	if err != nil {
		return err
	}
	b.state.setThread(o.SessionID, ts)
	return nil
}

// enqueue adds a job, dropping the oldest queued job when the queue is full.
func (b *Bridge) enqueue(j job) {
	for {
		select {
		case b.jobs <- j:
			return
		default:
		}
		select {
		case <-b.jobs:
			log.Warn("slack: outgoing queue full, dropped the oldest post")
		default:
		}
	}
}

// runJobs runs queued jobs one at a time, retrying a failed job once.
func (b *Bridge) runJobs(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-b.jobs:
			err := j(ctx)
			if err == nil || ctx.Err() != nil {
				continue
			}
			log.Warnf("slack: %v (retrying once)", err)
			if !b.sleep(ctx, b.retryDelay) {
				return
			}
			if errRetry := j(ctx); errRetry != nil && ctx.Err() == nil {
				log.Warnf("slack: %v (dropped)", errRetry)
			}
		}
	}
}

// sleep waits d or until ctx ends; it reports whether ctx is still live.
func (b *Bridge) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// resolve finds the bot, the channel and the config users. Emails Slack
// doesn't know are skipped; other errors abort so Start retries.
func (b *Bridge) resolve(ctx context.Context) error {
	botID, err := b.api.authTest(ctx, b.cfg.BotToken)
	if err != nil {
		return err
	}
	channelID, err := b.api.findChannel(ctx, b.cfg.BotToken, b.cfg.Channel)
	if err != nil {
		return err
	}
	var users []allowedUser
	for _, email := range b.cfg.AllowedEmails {
		id, errLookup := b.api.lookupByEmail(ctx, b.cfg.BotToken, email)
		var apiErr *apiError
		if errors.As(errLookup, &apiErr) && apiErr.code == "users_not_found" {
			log.Warnf("slack: no Slack user with allowed email %s; skipped", email)
			continue
		}
		if errLookup != nil {
			return errLookup
		}
		users = append(users, allowedUser{ID: id, Label: emailLabel(email), config: true})
	}
	if len(users) == 0 {
		return errors.New("slack: none of allowed-emails matched a Slack user")
	}
	b.botUserID, b.channelID = botID, channelID
	b.state.seed(users)
	return nil
}
```

`min` on `time.Duration` is the Go 1.21+ builtin.

- [ ] **Step 4: Run tests to verify they pass**

Run: `gofmt -w internal/slackbridge && go test ./internal/slackbridge/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/slackbridge/
git commit -m "slackbridge: allowlist and thread state, outbound posts"
```

---

### Task 6: slackbridge inbound routing and allow/remove commands

**Files:**
- Create: `internal/slackbridge/router.go`
- Test: `internal/slackbridge/router_test.go`

**Interfaces:**
- Consumes: Task 5 (`Bridge`, `state`, `enqueue`, `newTestBridge`, `drainJobs`), Task 4 (helpers), Task 1 (`Store.Deliver`, `Store.Peers`, `agentbus.ErrUnknownTarget`, `agentbus.ErrBodyTooLarge`).
- Produces: `type messageEvent struct{ Type, Subtype, Channel, User, BotID, Text, TS, ThreadTS string }` (JSON tags `type`, `subtype`, `channel`, `user`, `bot_id`, `text`, `ts`, `thread_ts`) and `(*Bridge).handleEvent(eventID string, ev messageEvent)`. It only touches memory and the job queue.

- [ ] **Step 1: Write the failing tests**

Create `internal/slackbridge/router_test.go`:

```go
package slackbridge

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

// threadOf opens sidA's thread by posting once, and returns its ts.
func threadOf(t *testing.T, b *Bridge, bus *agentbus.Store) string {
	t.Helper()
	if _, err := bus.Send(sidA, "slack", "hello", ""); err != nil {
		t.Fatal(err)
	}
	drainJobs(t, b)
	ts, ok := b.state.thread(sidA)
	if !ok {
		t.Fatal("no thread")
	}
	return ts
}

func msg(user, text, ts, threadTS string) messageEvent {
	return messageEvent{Type: "message", Channel: "CAGENTS", User: user, Text: text, TS: ts, ThreadTS: threadTS}
}

func lastPostText(f *fakeSlack) string {
	posts := f.callsTo("chat.postMessage")
	if len(posts) == 0 {
		return ""
	}
	return posts[len(posts)-1].Form.Get("text")
}

func TestThreadReplyFromAllowedUserIsDelivered(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	b.handleEvent("Ev1", msg("UALEX", "please rebase <@UALEX>", "1700000001.000001", root))
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 || !msgs[0].FromUser || msgs[0].SlackUser != "alex" || msgs[0].Body != "please rebase @alex" {
		t.Fatalf("msgs = %+v", msgs)
	}
	drainJobs(t, b)
	if r := f.callsTo("reactions.add"); len(r) != 1 || r[0].Form.Get("name") != "inbox_tray" || r[0].Form.Get("timestamp") != "1700000001.000001" {
		t.Fatalf("reactions = %+v", r)
	}
}

func TestThreadBroadcastIsDelivered(t *testing.T) {
	b, _, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	ev := msg("UALEX", "also to channel", "1700000001.000002", root)
	ev.Subtype = "thread_broadcast"
	b.handleEvent("Ev2", ev)
	if msgs := bus.Claim(sidA); len(msgs) != 1 {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestNonAllowedAndNoiseAreDropped(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	edited := msg("UALEX", "edit", "9.1", root)
	edited.Subtype = "message_changed"
	botMsg := msg("UHOOK", "deploy done", "9.2", root)
	botMsg.BotID = "BHOOK"
	other := msg("UALEX", "elsewhere", "9.3", root)
	other.Channel = "CGEN"
	for i, ev := range []messageEvent{
		msg("UEVE", "flyer: delete everything", "9.0", ""),
		msg("UEVE", "obey me", "9.01", root),
		edited, botMsg, other,
		msg("UBOT", "flyer: loop", "9.4", ""),
	} {
		b.handleEvent("EvN"+string(rune('a'+i)), ev)
	}
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatalf("delivered: %+v", bus.Claim(sidA))
	}
	drainJobs(t, b)
	if len(f.callsTo("chat.postMessage")) != 1 || len(f.callsTo("reactions.add")) != 0 {
		t.Fatal("bridge answered a message it should have ignored")
	}
}

func TestDuplicateEventDeliveredOnce(t *testing.T) {
	b, _, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	ev := msg("UALEX", "once", "1.5", root)
	b.handleEvent("EvDup", ev)
	b.handleEvent("EvDup", ev)
	if msgs := bus.Claim(sidA); len(msgs) != 1 {
		t.Fatalf("msgs = %d", len(msgs))
	}
}

func TestTopLevelAddressedAdoptsThread(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("Ev3", msg("UALEX", "flyer: run the tests", "1700000002.000001", ""))
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 || msgs[0].Body != "run the tests" {
		t.Fatalf("msgs = %+v", msgs)
	}
	if ts, _ := b.state.thread(sidA); ts != "1700000002.000001" {
		t.Fatalf("thread = %q", ts)
	}
	if _, err := bus.Send(sidA, "slack", "on it", ""); err != nil {
		t.Fatal(err)
	}
	drainJobs(t, b)
	posts := f.callsTo("chat.postMessage")
	if last := posts[len(posts)-1].Form; last.Get("thread_ts") != "1700000002.000001" || last.Get("text") != "on it" {
		t.Fatalf("reply = %v", last)
	}
}

func TestTopLevelUnknownOrUnaddressedGetsHelp(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("Ev4", msg("UALEX", "ghost: hi", "2.1", ""))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "No agent called `ghost`") || !strings.Contains(got, "flyer") {
		t.Fatalf("reply = %q", got)
	}
	if f.callsTo("chat.postMessage")[0].Form.Get("thread_ts") != "2.1" {
		t.Fatal("help not posted in thread")
	}
	b.handleEvent("Ev5", msg("UALEX", "https://example.com", "2.2", ""))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "name: message") {
		t.Fatalf("reply = %q", got)
	}
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("delivered")
	}
}

func TestReplyInUnlinkedThreadGetsHelp(t *testing.T) {
	b, f, _ := newTestBridge(t)
	b.handleEvent("Ev6", msg("UALEX", "hm", "3.2", "3.1"))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "isn't linked to an agent") {
		t.Fatalf("reply = %q", got)
	}
}

func TestAllowAndRemoveFromSlack(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)

	b.handleEvent("Ev7", msg("UEVE", "<@UBOT> allow <@UEVE>", "4.0", ""))
	drainJobs(t, b)
	if _, ok := b.state.user("UEVE"); ok {
		t.Fatal("a non-allowed user allowed themselves")
	}

	b.handleEvent("Ev8", msg("UALEX", "<@UBOT> allow <@UJANE>", "4.1", ""))
	drainJobs(t, b)
	u, ok := b.state.user("UJANE")
	if !ok || u.Label != "jane-d" {
		t.Fatalf("jane = %+v %v", u, ok)
	}
	if got := lastPostText(f); !strings.Contains(got, "<@UJANE>") || !strings.Contains(got, "@jane-d") {
		t.Fatalf("confirm = %q", got)
	}
	b.handleEvent("Ev9", msg("UJANE", "from jane", "4.2", root))
	if msgs := bus.Claim(sidA); len(msgs) != 1 || msgs[0].SlackUser != "jane-d" {
		t.Fatalf("msgs = %+v", msgs)
	}

	b.handleEvent("Ev10", msg("UALEX", "<@UBOT> allow <@UHOOK>", "4.3", ""))
	drainJobs(t, b)
	if _, ok = b.state.user("UHOOK"); ok || !strings.Contains(lastPostText(f), "Bots") {
		t.Fatal("allowed a bot")
	}

	b.handleEvent("Ev11", msg("UJANE", "<@UBOT> remove <@UALEX>", "4.4", ""))
	drainJobs(t, b)
	if _, ok = b.state.user("UALEX"); !ok || !strings.Contains(lastPostText(f), "config.yaml") {
		t.Fatal("removed a config user")
	}

	b.handleEvent("Ev12", msg("UALEX", "<@UBOT> remove <@UJANE>", "4.5", ""))
	drainJobs(t, b)
	b.handleEvent("Ev13", msg("UJANE", "still here?", "4.6", root))
	if bus.Pending(sidA) {
		t.Fatal("removed user still delivered")
	}

	b.handleEvent("Ev14", msg("UALEX", "<@UBOT> what", "4.7", ""))
	drainJobs(t, b)
	if got := lastPostText(f); !strings.Contains(got, "allow @person") {
		t.Fatalf("help = %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/slackbridge/ -run 'Thread|Allowed|Dropped|Duplicate|TopLevel|Unlinked|Allow' -v`
Expected: compile errors (`undefined: messageEvent`, `handleEvent`).

- [ ] **Step 3: Implement**

Create `internal/slackbridge/router.go`:

```go
package slackbridge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

const (
	howToAddress = "To reach an agent, reply in its thread, or post `name: message` at the top level (the name or address from its thread header)."
	commandHelp  = "Commands: `@agents allow @person` lets someone instruct agents; `@agents remove @person` takes that back."
	onlineLimit  = 10
)

type messageEvent struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	Channel  string `json:"channel"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
}

// relayedSubtypes are the message subtypes that are a person writing:
// a plain message, a thread reply also sent to the channel, a message with a file.
var relayedSubtypes = map[string]bool{"": true, "thread_broadcast": true, "file_share": true}

// handleEvent routes one Slack message. It only touches memory and the job
// queue, so the socket loop can ack as soon as it returns.
func (b *Bridge) handleEvent(eventID string, ev messageEvent) {
	if ev.Type != "message" || !relayedSubtypes[ev.Subtype] || ev.BotID != "" ||
		ev.Channel != b.channelID || ev.User == "" || ev.User == b.botUserID {
		return
	}
	if b.alreadySeen(eventID) {
		return
	}
	user, ok := b.state.user(ev.User)
	if !ok {
		return
	}
	if verb, target, isCommand := parseCommand(ev.Text, b.botUserID); isCommand {
		b.command(ev, verb, target)
		return
	}
	text := plainText(ev.Text, b.state.idLabels())
	if ev.ThreadTS != "" && ev.ThreadTS != ev.TS {
		sid, linked := b.state.session(ev.ThreadTS)
		if !linked {
			b.reply(ev, "This thread isn't linked to an agent. "+howToAddress)
			return
		}
		b.deliver(ev, sid, text, user, "That agent's session has ended, so this wasn't delivered.")
		return
	}
	target, body, addressedOK := parseAddressed(text)
	if !addressedOK {
		b.reply(ev, howToAddress)
		return
	}
	notFound := fmt.Sprintf("No agent called `%s`. %s", escape(target), b.onlineHint())
	if sid, delivered := b.deliver(ev, target, body, user, notFound); delivered {
		b.state.setThread(sid, ev.TS)
	}
}

func (b *Bridge) alreadySeen(eventID string) bool {
	if eventID == "" {
		return false
	}
	b.seenMu.Lock()
	defer b.seenMu.Unlock()
	if b.seen[eventID] {
		return true
	}
	b.seen[eventID] = true
	b.seenRing = append(b.seenRing, eventID)
	if len(b.seenRing) > seenEvents {
		delete(b.seen, b.seenRing[0])
		b.seenRing = b.seenRing[1:]
	}
	return false
}

// deliver queues body for target; notFound is the reply when the target is unknown.
func (b *Bridge) deliver(ev messageEvent, target, body string, user allowedUser, notFound string) (string, bool) {
	sid, err := b.bus.Deliver(target, body, user.Label)
	switch {
	case err == nil:
		b.react(ev, "inbox_tray")
		return sid, true
	case errors.Is(err, agentbus.ErrUnknownTarget):
		b.reply(ev, notFound)
	case errors.Is(err, agentbus.ErrBodyTooLarge):
		b.reply(ev, "That message is over the 16 KiB agentbus limit and was not delivered.")
	default:
		b.reply(ev, "Not delivered: "+escape(err.Error()))
	}
	return "", false
}

func (b *Bridge) onlineHint() string {
	var names []string
	for _, p := range b.bus.Peers() {
		if p.Address == agentbus.SlackAddress || p.Status == agentbus.StatusOffline {
			continue
		}
		label := p.Address
		if p.Name != "" {
			label = p.Name + " (" + p.Address + ")"
		}
		names = append(names, "`"+escape(label)+"`")
		if len(names) == onlineLimit {
			break
		}
	}
	if len(names) == 0 {
		return "No agents are online."
	}
	return "Online: " + strings.Join(names, ", ")
}

func (b *Bridge) command(ev messageEvent, verb, target string) {
	switch verb {
	case "allow":
		b.enqueue(func(ctx context.Context) error {
			label, isBot, err := b.api.userInfo(ctx, b.cfg.BotToken, target)
			if err != nil {
				return b.replyNow(ctx, ev, "Couldn't look that user up: "+escape(err.Error()))
			}
			if isBot {
				return b.replyNow(ctx, ev, "Bots can't be allowed.")
			}
			u, added, errAllow := b.state.allow(target, label)
			if errAllow != nil {
				logSaveError(errAllow)
			}
			if !added {
				return b.replyNow(ctx, ev, fmt.Sprintf("<@%s> is already allowed, as @%s.", u.ID, u.Label))
			}
			return b.replyNow(ctx, ev, fmt.Sprintf("<@%s> can now instruct agents. Agents know them as @%s.", u.ID, u.Label))
		})
	case "remove":
		u, _ := b.state.user(target)
		switch err := b.state.remove(target); {
		case errors.Is(err, errConfigUser):
			b.reply(ev, fmt.Sprintf("@%s is set in config.yaml (allowed-emails) and can't be removed from Slack.", u.Label))
		case errors.Is(err, errNotAllowed):
			b.reply(ev, fmt.Sprintf("<@%s> isn't on the list.", target))
		default:
			if err != nil {
				logSaveError(err)
			}
			b.reply(ev, fmt.Sprintf("<@%s> can no longer instruct agents.", target))
		}
	default:
		b.reply(ev, commandHelp)
	}
}

// replyThread is the thread a reply to ev belongs in.
func replyThread(ev messageEvent) string {
	if ev.ThreadTS != "" {
		return ev.ThreadTS
	}
	return ev.TS
}

func (b *Bridge) reply(ev messageEvent, text string) {
	b.enqueue(func(ctx context.Context) error { return b.replyNow(ctx, ev, text) })
}

func (b *Bridge) replyNow(ctx context.Context, ev messageEvent, text string) error {
	_, err := b.api.postMessage(ctx, b.cfg.BotToken, b.channelID, text, replyThread(ev))
	return err
}

func (b *Bridge) react(ev messageEvent, name string) {
	b.enqueue(func(ctx context.Context) error {
		return b.api.addReaction(ctx, b.cfg.BotToken, b.channelID, ev.TS, name)
	})
}
```

Note: a top-level `@agents allow` confirmation goes in that message's own thread (`replyThread` falls back to `ev.TS`). That matches `TestTopLevelUnknownOrUnaddressedGetsHelp`, which checks `thread_ts == "2.1"`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `gofmt -w internal/slackbridge && go test ./internal/slackbridge/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/slackbridge/router.go internal/slackbridge/router_test.go
git commit -m "slackbridge: route allowed users' messages to sessions; allow/remove from Slack"
```

---

### Task 7: Socket Mode connection and bridge lifecycle

**Files:**
- Create: `internal/slackbridge/socket.go`
- Modify: `internal/slackbridge/bridge.go` (add `Start`, `Stop`)
- Test: `internal/slackbridge/socket_test.go`

**Interfaces:**
- Consumes: Tasks 5–6.
- Produces: `func (b *Bridge) Start()` (non-blocking: resolves with retry, then `bus.SetBridge(b)`, runs jobs and the socket) and `func (b *Bridge) Stop()` (detaches from the bus, cancels, waits).

- [ ] **Step 1: Write the failing test**

Create `internal/slackbridge/socket_test.go`:

```go
package slackbridge

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

func waitAck(t *testing.T, f *fakeSlack, want string) {
	t.Helper()
	select {
	case got := <-f.acks:
		if got != want {
			t.Fatalf("ack = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no ack for %s", want)
	}
}

func TestSocketDeliversAcksAndReconnects(t *testing.T) {
	f := newFakeSlack(t)
	bus := agentbus.NewStore("", nil)
	bus.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	b, err := New(testConfig(f, t.TempDir()), bus)
	if err != nil {
		t.Fatal(err)
	}
	b.backoff = func(int) time.Duration { return 0 }
	b.Start()

	// The first envelope only gets through once the socket is up; the ack
	// proves the event was handled first.
	f.push(`{"type":"events_api","envelope_id":"env-1","payload":{"event_id":"Ev1","event":{"type":"message","channel":"CAGENTS","user":"UALEX","text":"flyer: hi","ts":"5.1"}}}`)
	waitAck(t, f, "env-1")
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 || msgs[0].Body != "hi" || !msgs[0].FromUser {
		t.Fatalf("msgs = %+v", msgs)
	}
	attached := false
	for _, p := range bus.Peers() {
		attached = attached || p.Address == agentbus.SlackAddress
	}
	if !attached {
		t.Fatal("bridge not attached to the bus")
	}

	f.push(`{"type":"disconnect","reason":"refresh_requested"}`)
	f.push(`{"type":"events_api","envelope_id":"env-2","payload":{"event_id":"Ev2","event":{"type":"message","channel":"CAGENTS","user":"UALEX","text":"flyer: again","ts":"5.2"}}}`)
	waitAck(t, f, "env-2")
	if f.opens() < 2 {
		t.Fatalf("opens = %d, want a reconnect", f.opens())
	}

	b.Stop()
	for _, p := range bus.Peers() {
		if p.Address == agentbus.SlackAddress {
			t.Fatal("still attached after Stop")
		}
	}
}

func TestStopBeforeResolveSucceeds(t *testing.T) {
	f := newFakeSlack(t)
	f.setFail("auth.test", "invalid_auth")
	b, _ := New(testConfig(f, t.TempDir()), agentbus.NewStore("", nil))
	b.backoff = func(int) time.Duration { return time.Hour }
	b.Start()
	done := make(chan struct{})
	go func() { b.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung while resolve was backing off")
	}
}
```

The disconnect test relies on the fake: after writing a `disconnect` envelope, the fake handler returns and closes the socket. The next queued envelope (`env-2`) goes out on the new connection.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/slackbridge/ -run 'Socket|StopBefore' -v`
Expected: compile errors (`b.Start undefined`).

- [ ] **Step 3: Implement**

Create `internal/slackbridge/socket.go`:

```go
package slackbridge

import (
	"context"
	"encoding/json"
	"fmt"

	log "github.com/sirupsen/logrus"
)

type envelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload"`
}

type eventsPayload struct {
	EventID string       `json:"event_id"`
	Event   messageEvent `json:"event"`
}

// runSocket keeps a Socket Mode connection open until ctx ends. Slack sends
// pings and gorilla answers them, so there are no read deadlines.
func (b *Bridge) runSocket(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		connected, err := b.connectOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			attempt = 0
		}
		if err == nil {
			continue
		}
		attempt++
		log.Warnf("slack: socket: %v", err)
		if !b.sleep(ctx, b.backoff(attempt)) {
			return
		}
	}
}

// connectOnce runs one connection. It returns nil on a Slack-requested
// disconnect, and whether Slack said hello.
func (b *Bridge) connectOnce(ctx context.Context) (bool, error) {
	wsURL, err := b.api.openConnection(ctx, b.cfg.AppToken)
	if err != nil {
		return false, err
	}
	conn, _, errDial := b.dialer.DialContext(ctx, wsURL, nil)
	if errDial != nil {
		return false, fmt.Errorf("dial: %w", errDial)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			log.Debugf("slack: close socket: %v", errClose)
		}
	}()
	connected := false
	for {
		var env envelope
		if errRead := conn.ReadJSON(&env); errRead != nil {
			return connected, fmt.Errorf("read: %w", errRead)
		}
		switch env.Type {
		case "hello":
			connected = true
		case "disconnect":
			return connected, nil
		case "events_api":
			var p eventsPayload
			if errJSON := json.Unmarshal(env.Payload, &p); errJSON != nil {
				log.Debugf("slack: bad events_api payload: %v", errJSON)
			} else {
				b.handleEvent(p.EventID, p.Event)
			}
		}
		if env.EnvelopeID != "" {
			if errAck := conn.WriteJSON(map[string]string{"envelope_id": env.EnvelopeID}); errAck != nil {
				return connected, fmt.Errorf("ack: %w", errAck)
			}
		}
	}
}
```

Append to `internal/slackbridge/bridge.go`:

```go
// Start resolves the bot, channel and users in the background (retrying with
// backoff), then attaches to the bus and keeps the socket open until Stop.
func (b *Bridge) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.done = make(chan struct{})
	go func() {
		defer close(b.done)
		for attempt := 1; ; attempt++ {
			err := b.resolve(ctx)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return
			}
			log.Warnf("slack: bridge not ready: %v", err)
			if !b.sleep(ctx, b.backoff(attempt)) {
				return
			}
		}
		b.bus.SetBridge(b)
		log.Infof("slack: bridge on, channel %s, allowed users %s", b.cfg.Channel, strings.Join(b.Users(), ", "))
		go b.runJobs(ctx)
		b.runSocket(ctx)
	}()
}

// Stop detaches from the bus and closes the connection.
func (b *Bridge) Stop() {
	if b.cancel == nil {
		return
	}
	b.bus.SetBridge(nil)
	b.cancel()
	<-b.done
	b.cancel = nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `gofmt -w internal/slackbridge && go test ./internal/slackbridge/ -race -count=3 -v 2>&1 | tail -40`
Expected: all PASS three times under `-race`.

- [ ] **Step 5: Commit**

```bash
git add internal/slackbridge/
git commit -m "slackbridge: Socket Mode connection and Start/Stop"
```

---

### Task 8: wire the bridge into the server

**Files:**
- Modify: `internal/api/server_agentbus.go`, `internal/api/server.go` (Server struct field), `AGENTS.md` (timeout exception list), `config.example.yaml` (commented example)

**Interfaces:**
- Consumes: `config.Config.Slack` (Task 3), `slackbridge.New`/`Start`/`Stop` (Tasks 5, 7).

- [ ] **Step 1: Implement**

In `internal/api/server.go`, after the `agentbus *agentbus.Store` field:

```go
	// slack bridges the agentbus to a Slack channel; nil when not configured.
	slack *slackbridge.Bridge
```

and add the import `"github.com/router-for-me/CLIProxyAPI/v8/internal/slackbridge"`.

In `internal/api/server_agentbus.go`, replace `agentbusStatePath` with a shared helper and add the Slack wiring:

```go
// runtimeStatePath keeps runtime state next to the other runtime state:
// WRITABLE_PATH when set, else next to the config file.
func (s *Server) runtimeStatePath(name string) string {
	if base := util.WritablePath(); base != "" {
		return filepath.Join(base, name)
	}
	if s.configFilePath != "" {
		return filepath.Join(filepath.Dir(s.configFilePath), name)
	}
	return ""
}
```

Change `agentbus.NewStore(s.agentbusStatePath(), nil)` to `agentbus.NewStore(s.runtimeStatePath("agentbus-state.json"), nil)`. At the end of `initAgentbus` add `s.initSlack()`, and add:

```go
// initSlack starts the Slack bridge when config.yaml has a complete slack
// block. Changing the block needs a restart.
func (s *Server) initSlack() {
	if s.cfg == nil {
		return
	}
	sc := s.cfg.Slack
	bridge, errNew := slackbridge.New(slackbridge.Config{
		BotToken:      sc.BotToken,
		AppToken:      sc.AppToken,
		Channel:       sc.Channel,
		AllowedEmails: sc.AllowedEmails,
		StatePath:     s.runtimeStatePath("slack-state.json"),
	}, s.agentbus)
	if errNew != nil {
		log.Warnf("slack: %v", errNew)
		return
	}
	if bridge == nil {
		return
	}
	s.slack = bridge
	bridge.Start()
}
```

At the start of `stopAgentbus`, before the `agentbusStop == nil` check:

```go
	if s.slack != nil {
		s.slack.Stop()
		s.slack = nil
	}
```

In `AGENTS.md`, in the timeouts bullet, add to the list of intentional exceptions: `the Slack Web API call timeout and Socket Mode reconnect backoff in internal/slackbridge`. Put it before "and the `cmd/fetch_antigravity_models` utility timeouts".

In `config.example.yaml`, at the end of the file, add:

```yaml

# Slack bridge for the agentbus (catapultam fork). Sessions reach the channel
# as "slack"; allowed users instruct them by replying in a session's thread.
# More users can be allowed from Slack with "@agents allow @person".
# slack:
#   bot-token: "xoxb-..."
#   app-token: "xapp-..."
#   channel: "agents"
#   allowed-emails:
#     - "you@example.com"
```

- [ ] **Step 2: Verify**

Run: `gofmt -w internal/api && go build -o test-output ./cmd/server && rm test-output && go test ./internal/api/ ./internal/agentbus/ ./internal/slackbridge/ ./internal/config/ 2>&1 | tail -20`
Expected: build succeeds; all listed packages `ok`.

- [ ] **Step 3: Commit**

```bash
git add internal/api/server.go internal/api/server_agentbus.go AGENTS.md config.example.yaml
git commit -m "api: start the Slack bridge with the agentbus"
```

---

### Task 9: agentbus mod frames Slack messages as user instructions

**Files:**
- Modify: `internal/marketplace/plugins/agentbus/hooks/register.ts` (`BusMessage`, `formatMessage`), `internal/marketplace/plugins/agentbus/.claude-plugin/plugin.json` (version: one past comms' lease release, e.g. `0.3.1`), `internal/marketplace/assets_test.go:72-73`, `internal/marketplace/doc_test.go:54`
- Test: `internal/marketplace/plugins/agentbus/tests/agentbus.test.ts` (append)

- [ ] **Step 1: Write the failing test**

Append to `tests/agentbus.test.ts`:

```ts
test('a Slack message from an allowed user is framed as their instruction', async ($, on) => {
  const clock = mock.clock(on)
  const msg = { id: 'm_7', from: 'slack', body: 'please rebase', from_user: true, slack_user: 'jane' }
  wire($, on, [{ status: 200, text: JSON.stringify({ messages: [msg] }) }])
  const prompts: string[] = []
  on('prompt.submit', (_$, e) => {
    prompts.push(e.text)
    return { text: e.text }
  })
  await $.session.start({ surface: 'terminal', isInteractive: true, cwd: 'C:/work/comms' })

  await clock.advance(1000)
  await clock.settle()
  expect(prompts.length).toBe(1)
  expect(prompts[0]).toContain('from jane via Slack')
  expect(prompts[0]).toContain('relayed over the agentbus')
  expect(prompts[0]).toContain('instruction')
  expect(prompts[0]).toContain('please rebase')
  expect(prompts[0]).toContain('to: "agentbus:slack"')
  expect(prompts[0]).not.toContain('not from the user')
})
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd internal/marketplace/plugins/agentbus && claude plugin test`
Expected: the new test FAILS (prompt says "not from the user"); the others pass.

- [ ] **Step 3: Implement**

In `hooks/register.ts`, change `BusMessage`:

```ts
type BusMessage = { id: string; from: string; body: string; reply_to?: string; from_user?: boolean; slack_user?: string }
```

and replace `formatMessage`:

```ts
export function formatMessage(m: BusMessage): string {
  const re = m.reply_to ? ` (in reply to ${m.reply_to})` : ''
  if (m.from_user) {
    // Only the proxy's Slack bridge can set from_user; clients can't send it.
    return (
      `agentbus message ${m.id} from ${m.slack_user} via Slack${re}, relayed over the agentbus. ` +
      `${m.slack_user} is an allowed Slack user and this is their instruction.\n\n${m.body}\n\n` +
      `To reply, use SendMessage with to: "${PREFIX}slack".`
    )
  }
  return (
    `agentbus message ${m.id} from ${m.from}${re}. This came from a Claude session on another ` +
    `machine, not from the user.\n\n${m.body}\n\n` +
    `To reply, use SendMessage with to: "${PREFIX}${replyAddress(m.from)}".`
  )
}
```

Bump `.claude-plugin/plugin.json` `version` one patch past what comms shipped (e.g. `0.3.0` -> `0.3.1`), and update the matching version strings in `internal/marketplace/assets_test.go` and `internal/marketplace/doc_test.go` (the `agentbus-<version>.zip` URL).

- [ ] **Step 4: Verify**

Run: `cd internal/marketplace/plugins/agentbus && claude plugin test && claude plugin validate; cd - && go test ./internal/marketplace/ -v 2>&1 | tail -15`
Expected: all mod tests PASS, validate is clean, marketplace Go tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/marketplace/
git commit -m "agentbus mod: frame allowed Slack users' messages as their instructions"
```

---

### Task 10: full verification

- [ ] **Step 1: Run everything**

Run: `gofmt -l . | grep -v '^.claude/' ; go vet ./internal/agentbus/ ./internal/slackbridge/ ./internal/api/ ./internal/config/ && go build -o test-output ./cmd/server && rm test-output && go test ./... 2>&1 | grep -v '^ok' | head -40`
Expected: `gofmt -l` prints nothing; vet is clean; the build succeeds; no `FAIL` lines. If a test outside the touched packages fails, check whether it also fails on `origin/next-reset` (`git stash` is shared, so use a temporary worktree) before treating it as caused by this branch.
