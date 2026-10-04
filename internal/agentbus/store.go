// Package agentbus links Claude Code sessions across machines through the
// proxy they already send inference to: a registry of sessions learned from
// traffic, per-session inboxes, and delivery by request injection, long-poll
// (/wait), or explicit read.
package agentbus

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// MaxBodyBytes caps one message body.
	MaxBodyBytes = 16 * 1024

	messageTTL = 7 * 24 * time.Hour
	// commandTTL is how long a command message waits for the mod. An owner
	// expects a command to run now, not whenever the session comes back.
	commandTTL      = 10 * time.Minute
	idleWithin      = 2 * time.Minute
	awayWithin      = 30 * time.Minute
	offlineListFor  = 24 * time.Hour
	leaseTTL        = 90 * time.Second
	unknownMachine  = "unknown"
	fallbackFolder  = "session"
	addressIDLength = 6
	// maxQueuedGuest caps the guest messages queued for one session; the
	// oldest go first, so a flood in a linked conversation can't fill an
	// inbox.
	maxQueuedGuest = 50
	// MaxBroadcastTo caps the names a broadcast message's BroadcastTo
	// lists, so a huge broadcast doesn't balloon every delivered message.
	MaxBroadcastTo = 30
)

// Peer statuses.
const (
	StatusBusy    = "busy"
	StatusIdle    = "idle"
	StatusAway    = "away"
	StatusOffline = "offline"
)

var (
	ErrUnknownTarget = errors.New("unknown target")
	ErrUnknownSender = errors.New("unknown sender session")
	ErrBodyTooLarge  = errors.New("message body too large")
	ErrEmptyBody     = errors.New("message body is empty")
	ErrNameTaken     = errors.New("name already taken")
	ErrInvalidName   = errors.New("invalid name")
	// ErrCommandUnsupported means the target session's agentbus mod can't
	// run commands (no mod, or older than MinCommandModVersion).
	ErrCommandUnsupported = errors.New("session can't run commands")
	ErrInvalidCommand     = errors.New("invalid command")
	// ErrInvalidApproval is an approval whose request id isn't a message id.
	ErrInvalidApproval = errors.New("invalid approval request id")
)

var (
	unsafeAddressChars = regexp.MustCompile(`[^a-z0-9._-]+`)
	// validName keeps a name to one plain word, so it can't imitate the
	// header the proxy writes for a Slack user's instruction.
	validName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	// validMachine keeps a client-reported machine to one plain word too:
	// /peers returns it verbatim and the mod prints it into ListAgents.
	validMachine = validName
	// validReplyTo is the shape of an id from newMessageID.
	validReplyTo = regexp.MustCompile(`^m_[0-9a-f]{1,64}$`)
	// validModVersion is a plain dotted version as the mod reports it.
	validModVersion = regexp.MustCompile(`^[0-9]{1,6}(\.[0-9]{1,6}){0,3}$`)
)

// Message is one queued message.
type Message struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Body      string    `json:"body"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// FromUser marks an instruction from an allowed Slack user. Only Deliver
	// and DeliverCommand set it; clients can never send it.
	FromUser  bool   `json:"from_user,omitempty"`
	SlackUser string `json:"slack_user,omitempty"`
	// Guest marks input from a Slack user who isn't allowed, written in a
	// conversation an owner linked to the session; SlackUser is their label.
	// It is never an instruction. Only DeliverGuest sets it; clients can
	// never send it.
	Guest bool `json:"guest,omitempty"`
	// Via is ViaDM, ViaGroup, ViaShortcut or ViaSlash when the Slack message
	// reached the bridge other than in its main channel. Only DeliverVia,
	// DeliverGuest and DeliverCommandVia set it; clients can never send it.
	Via string `json:"via,omitempty"`
	// Approval marks an allowed Slack user's approval (their 👍) of the
	// request the session posted with "confirm:"; it is that request's
	// message id. Only DeliverApproval sets it, always with FromUser; clients
	// can never send it.
	Approval string `json:"approval,omitempty"`
	// Broadcast marks an owner's message (or command) to all agents ("all:
	// ..."). Only DeliverBroadcast and DeliverCommandBroadcast set it, always
	// with FromUser; clients can never send it.
	Broadcast bool `json:"broadcast,omitempty"`
	// BroadcastTo lists a broadcast's other recipients (this message's own
	// one left out), by their unique bus address — never a name, which can
	// repeat once an offline session's name is reused by a later one. It
	// lets a recipient SendMessage the others to coordinate. Capped at
	// MaxBroadcastTo. Only DeliverBroadcast and DeliverCommandBroadcast set
	// it, always with Broadcast; clients can never send it.
	BroadcastTo []string `json:"broadcast_to,omitempty"`
	// BroadcastCount is a broadcast's total recipients, including this
	// message's own one (so it is always at least len(BroadcastTo)+1). A
	// broadcast delivered before this field existed has it zero (legacy),
	// rendered with no number. Only DeliverBroadcast and
	// DeliverCommandBroadcast set it, always with Broadcast; clients can
	// never send it.
	BroadcastCount int `json:"broadcast_count,omitempty"`
	// SlackUserID is the Slack user ID of the owner who sent Command. Only
	// DeliverCommand sets it, and /wait re-checks it before handing the
	// command out.
	SlackUserID string `json:"slack_user_id,omitempty"`
	// Command is an owner's remote command for the agentbus mod to run.
	// Only DeliverCommand sets it; clients can never send it, and such a
	// message leaves the store only through /wait, never by injection.
	Command *Command `json:"command,omitempty"`
}

// Command is a remote command for the session's agentbus mod. Kind picks the
// fields used:
//   - slash: Command (without "/") and Args, final as given;
//   - prompt: Text, with {args} replaced by Args by the mod;
//   - shell: Argv (OS key -> argv), where an element that is exactly {args}
//     becomes Args as one argv value and {out} a temp file path; Env is
//     set for the process, where a value that is exactly {args} or {out}
//     is replaced the same way; ArgsEnum is the owner-written list Args
//     must be in when an argv element is {args} (free text only goes
//     through Env); Output is text or image; Timeout is in
//     seconds.
type Command struct {
	Name     string              `json:"name"`
	Kind     string              `json:"kind"`
	Command  string              `json:"command,omitempty"`
	Args     string              `json:"args,omitempty"`
	Text     string              `json:"text,omitempty"`
	Argv     map[string][]string `json:"argv,omitempty"`
	Env      map[string]string   `json:"env,omitempty"`
	ArgsEnum []string            `json:"args_enum,omitempty"`
	Output   string              `json:"output,omitempty"`
	Timeout  int                 `json:"timeout,omitempty"`
}

// Command kinds.
const (
	CommandSlash  = "slash"
	CommandPrompt = "prompt"
	CommandShell  = "shell"
)

// Peer is one session as other sessions see it.
type Peer struct {
	Address  string    `json:"address"`
	Name     string    `json:"name,omitempty"`
	Machine  string    `json:"machine"`
	Cwd      string    `json:"cwd,omitempty"`
	Status   string    `json:"status"`
	LastSeen time.Time `json:"last_seen"`
}

type session struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	Machine     string    `json:"machine,omitempty"`
	Cwd         string    `json:"cwd,omitempty"`
	FirstSeen   time.Time `json:"first_seen"`
	LastRequest time.Time `json:"last_request"`
	WaiterSeen  time.Time `json:"waiter_seen"`
	Mod         bool      `json:"mod,omitempty"`
	ModVersion  string    `json:"mod_version,omitempty"`
	Waits       bool      `json:"waits,omitempty"`
	Closed      bool      `json:"closed,omitempty"`
	NoteSent    bool      `json:"note_sent,omitempty"`
	NotedPeers  string    `json:"noted_peers,omitempty"`
	Inbox       []Message `json:"inbox,omitempty"`
	// Unacked holds the Slack messages /wait handed out that the mod hasn't
	// acknowledged (/ack) yet, with when; see Receipts.
	Unacked map[string]time.Time `json:"unacked,omitempty"`
	// MovedTo is the session that took this one over (HandOff), or empty.
	MovedTo   string `json:"moved_to,omitempty"`
	inflight  int
	waiterGen uint64
	notify    chan struct{}
}

func (s *session) lastSeen() time.Time {
	if s.WaiterSeen.After(s.LastRequest) {
		return s.WaiterSeen
	}
	return s.LastRequest
}

// Store holds sessions and inboxes. It is safe for concurrent use.
type Store struct {
	path  string
	now   func() time.Time
	mu    sync.Mutex
	byID  map[string]*session
	dirty bool
	// bridge relays SlackAddress traffic; nil when Slack is off.
	bridge Bridge
	// waitTimeout bounds one /wait long-poll (defaultWaitTimeout when zero).
	waitTimeout time.Duration
	// uploadSlots holds one token per image upload in progress.
	uploadSlots chan struct{}
	// notices are command notices queued under mu (by expireLocked) for
	// postNotices to hand to the bridge once mu is released.
	notices []commandNotice
}

// NewStore returns an empty store persisted at path (empty disables saving).
func NewStore(path string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{path: path, now: now, byID: make(map[string]*session), uploadSlots: make(chan struct{}, maxUploads)}
}

func (s *Store) get(id string) *session {
	sess, ok := s.byID[id]
	if !ok {
		sess = &session{ID: id, FirstSeen: s.now()}
		s.byID[id] = sess
		s.dirty = true
	}
	return sess
}

// Touch records a request from a session (main thread or subagent).
func (s *Store) Touch(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.get(id)
	sess.LastRequest = s.now()
	sess.Closed = false
	s.dirty = true
}

// BeginRequest and EndRequest bracket an in-flight main-thread request.
func (s *Store) BeginRequest(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.get(id).inflight++
}

func (s *Store) EndRequest(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.byID[id]; sess != nil && sess.inflight > 0 {
		sess.inflight--
	}
}

// Hello records what a client reports: machine, cwd and an optional friendly
// name. A name already used by another session, or one validName rejects, is
// ignored, and so is a machine validMachine rejects. /hello and /wait
// are also reachable from curl and the legacy wait.sh hook, so reaching this
// method does not by itself mean the agentbus mod is running: mod is true
// only when the caller sent an explicit marker (the /hello body's "mod"
// field, or /wait's "mod=1" query parameter). Once set, Mod is never cleared
// back to false.
func (s *Store) Hello(id, machine, cwd, name string, mod bool) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.get(id)
	if mod {
		sess.Mod = true
	}
	sess.Closed = false
	if machine = strings.TrimSpace(machine); validMachine.MatchString(machine) {
		sess.Machine = machine
	}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		sess.Cwd = cwd
	}
	sess.WaiterSeen = s.now()
	if name = strings.TrimSpace(name); validName.MatchString(name) && !strings.EqualFold(name, sess.Name) && s.nameFree(name, id) {
		sess.Name = name
	}
	s.dirty = true
}

// SetModVersion records the agentbus mod version a known session's mod
// reports. Callers pass what a mod-marked /hello or /wait carried; an empty
// or invalid value clears the record, since an older mod (which sends none)
// has taken over the session and can't run commands.
func (s *Store) SetModVersion(id, version string) {
	version = strings.TrimSpace(version)
	if !validModVersion.MatchString(version) {
		version = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok || sess.ModVersion == version {
		return
	}
	sess.ModVersion = version
	s.dirty = true
}

func (s *Store) nameFree(name, exceptID string) bool {
	if isReservedTarget(name) {
		return false
	}
	now := s.now()
	lower := strings.ToLower(name)
	for id, other := range s.byID {
		if id == exceptID || s.statusLocked(other, now) == StatusOffline {
			continue
		}
		if strings.EqualFold(other.Name, name) {
			return false
		}
		if s.addressLocked(other) == lower {
			return false
		}
	}
	return true
}

// SetName claims or (with an empty name) clears a session's friendly name. A
// name validName rejects is ErrInvalidName.
func (s *Store) SetName(id, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return ErrUnknownSender
	}
	name = strings.TrimSpace(name)
	if name != "" && !validName.MatchString(name) {
		return ErrInvalidName
	}
	if name != "" && !s.nameFree(name, id) {
		return ErrNameTaken
	}
	sess.Name = name
	s.dirty = true
	return nil
}

func sanitizeAddressPart(v string) string {
	v = unsafeAddressChars.ReplaceAllString(strings.ToLower(strings.TrimSpace(v)), "-")
	return strings.Trim(v, "-")
}

func (s *Store) addressLocked(sess *session) string {
	machine := sanitizeAddressPart(sess.Machine)
	if machine == "" {
		machine = unknownMachine
	}
	folder := ""
	if sess.Cwd != "" {
		folder = sanitizeAddressPart(filepath.Base(strings.ReplaceAll(sess.Cwd, `\`, "/")))
	}
	if folder == "" {
		folder = fallbackFolder
	}
	short := sess.ID
	if len(short) > addressIDLength {
		short = short[:addressIDLength]
	}
	// A session id is whatever the client sends, so its prefix is sanitized
	// like the other parts (a no-op for the usual UUIDs).
	short = unsafeAddressChars.ReplaceAllString(strings.ToLower(short), "-")
	return machine + "/" + folder + "-" + short
}

// Address is the session's automatic address.
func (s *Store) Address(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return ""
	}
	return s.addressLocked(sess)
}

// SessionSeen returns when a known session was last seen on the bus: its
// latest request or waiter.
func (s *Store) SessionSeen(id string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return time.Time{}, false
	}
	return sess.lastSeen(), true
}

// Resolve finds a session by friendly name or address (case-insensitive).
func (s *Store) Resolve(target string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveLocked(target)
}

// resolveLocked finds a session by friendly name or address. The reserved
// "slack" and "slack@<label>" addresses never resolve to a session, even a
// legacy one still carrying that name from before nameFree rejected it.
func (s *Store) resolveLocked(target string) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" || isReservedTarget(target) {
		return "", false
	}
	lower := strings.ToLower(target)
	return s.bestMatchLocked(func(sess *session) bool {
		return (sess.Name != "" && strings.EqualFold(sess.Name, target)) || s.addressLocked(sess) == lower
	})
}

// bestMatchLocked picks the session to deliver to among those that match.
// A name can be reused once its holder goes offline, so a live match wins
// over offline ones, and among equals the most recently seen wins.
func (s *Store) bestMatchLocked(match func(*session) bool) (string, bool) {
	now := s.now()
	bestID, bestLive := "", false
	var bestSeen time.Time
	for id, sess := range s.byID {
		if !match(sess) {
			continue
		}
		live := s.statusLocked(sess, now) != StatusOffline
		seen := sess.lastSeen()
		if bestID == "" || (live && !bestLive) || (live == bestLive && seen.After(bestSeen)) {
			bestID, bestLive, bestSeen = id, live, seen
		}
	}
	return bestID, bestID != ""
}

func newMessageID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "m_" + hex.EncodeToString(b[:])
}

// Send queues a message from a known session to a name or address. Messages
// to SlackAddress go to the bridge instead of an inbox, and so do messages to
// "slack@<label>" (a DM) when label is one of the bridge's allowed users;
// any other "slack@..." is ErrUnknownSlackUser. A replyTo that isn't a
// message id is dropped.
func (s *Store) Send(fromID, to, body, replyTo string) (Message, error) {
	if strings.TrimSpace(body) == "" {
		return Message{}, ErrEmptyBody
	}
	if len(body) > MaxBodyBytes {
		return Message{}, ErrBodyTooLarge
	}
	if label, dm := slackDMLabel(to); dm {
		return s.sendDM(fromID, label, body)
	}
	s.mu.Lock()
	from, ok := s.byID[fromID]
	if !ok {
		s.mu.Unlock()
		return Message{}, ErrUnknownSender
	}
	replyTo = cleanReplyTo(replyTo)
	msg := s.sentLocked(from, body, replyTo)
	if b := s.bridge; b != nil && isSlackAddress(to) {
		out := s.outboundLocked(fromID, from, body)
		out.ID = msg.ID
		out.ReplyTo = replyTo
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

// sentLocked builds a message from session from, without To. The caller
// holds s.mu.
func (s *Store) sentLocked(from *session, body, replyTo string) Message {
	msg := Message{
		ID:        newMessageID(),
		From:      s.addressLocked(from),
		Body:      body,
		ReplyTo:   replyTo,
		CreatedAt: s.now(),
	}
	if from.Name != "" {
		msg.From = from.Name + " (" + msg.From + ")"
	}
	return msg
}

// cleanReplyTo returns replyTo trimmed, or empty when it isn't a message id.
func cleanReplyTo(replyTo string) string {
	if replyTo = strings.TrimSpace(replyTo); validReplyTo.MatchString(replyTo) {
		return replyTo
	}
	return ""
}

func (s *Store) enqueueLocked(target *session, msg Message) {
	target.Inbox = append(target.Inbox, msg)
	if msg.Guest {
		s.capGuestLocked(target)
	}
	if target.notify != nil {
		close(target.notify)
		target.notify = nil
	}
	s.dirty = true
}

// capGuestLocked drops sess's oldest queued guest messages beyond
// maxQueuedGuest, logging each (never the body). The caller holds s.mu.
func (s *Store) capGuestLocked(sess *session) {
	guests := 0
	for _, m := range sess.Inbox {
		if m.Guest {
			guests++
		}
	}
	if guests <= maxQueuedGuest {
		return
	}
	drop := guests - maxQueuedGuest
	kept := sess.Inbox[:0]
	for _, m := range sess.Inbox {
		if m.Guest && drop > 0 {
			drop--
			log.Warnf("agentbus: dropped guest message %s for %s: more than %d guest messages queued", m.ID, m.To, maxQueuedGuest)
			continue
		}
		kept = append(kept, m)
	}
	sess.Inbox = kept
}

// expireLocked drops a session's messages older than messageTTL, and command
// messages older than commandTTL. Each expired command queues a notice for
// its Slack thread; the caller must call postNotices after releasing s.mu.
func (s *Store) expireLocked(sess *session) {
	now := s.now()
	cutoff := now.Add(-messageTTL)
	kept := sess.Inbox[:0]
	for _, m := range sess.Inbox {
		switch {
		case m.Command != nil && commandExpired(m, now):
			log.Infof("agentbus: command %s (!%s) for %s expired unclaimed", m.ID, m.Command.Name, m.To)
			s.notices = append(s.notices, expiredNotice(sess.ID, m))
		case m.CreatedAt.After(cutoff):
			kept = append(kept, m)
		}
	}
	if len(kept) != len(sess.Inbox) {
		s.dirty = true
	}
	sess.Inbox = kept
	s.expireUnackedLocked(sess)
}

// commandExpired reports whether command message m is older than commandTTL.
func commandExpired(m Message, now time.Time) bool {
	return !m.CreatedAt.After(now.Add(-commandTTL))
}

// commandNotice is a bridge post about a command message, made in the Slack
// thread the command came from.
type commandNotice struct {
	sessionID, msgID, body string
}

func expiredNotice(sessionID string, m Message) commandNotice {
	return commandNotice{sessionID: sessionID, msgID: m.ID, body: fmt.Sprintf("`!%s` expired before the agent picked it up.", m.Command.Name)}
}

// postNotices hands extra and the notices expireLocked queued to the bridge
// (dropping them when Slack is off). It takes s.mu itself, so callers call
// it after releasing the lock, and it posts without holding it.
func (s *Store) postNotices(extra ...commandNotice) {
	s.mu.Lock()
	notices := append(s.notices, extra...)
	s.notices = nil
	bridge := s.bridge
	var outs []Outbound
	if bridge != nil {
		for _, n := range notices {
			sess, ok := s.byID[n.sessionID]
			if !ok {
				continue
			}
			out := s.outboundLocked(n.sessionID, sess, n.body)
			out.ReplyTo = n.msgID
			outs = append(outs, out)
		}
	}
	s.mu.Unlock()
	for _, out := range outs {
		bridge.Post(out)
	}
}

// Claim removes and returns every pending message for a session.
func (s *Store) Claim(id string) []Message {
	return s.claimWhere(id, func(Message) bool { return true })
}

// ClaimPlain removes and returns a session's pending messages except those
// carrying a Command, which stay queued for the mod's /wait.
func (s *Store) ClaimPlain(id string) []Message {
	return s.claimWhere(id, func(m Message) bool { return m.Command == nil })
}

// claimWhere removes and returns the pending messages take accepts, in
// order, and leaves the rest queued.
func (s *Store) claimWhere(id string, take func(Message) bool) []Message {
	// Deferred first, so it runs after the unlock below.
	defer s.postNotices()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return nil
	}
	s.expireLocked(sess)
	out, kept := splitInbox(sess.Inbox, take)
	if len(out) == 0 {
		return nil
	}
	sess.Inbox = kept
	s.dirty = true
	return out
}

// splitInbox splits msgs into those take accepts and the rest, both in
// order, without sharing a backing array with msgs.
func splitInbox(msgs []Message, take func(Message) bool) (taken, kept []Message) {
	for _, m := range msgs {
		if take(m) {
			taken = append(taken, m)
		} else {
			kept = append(kept, m)
		}
	}
	return taken, kept
}

// Pending reports whether a session has unclaimed messages.
func (s *Store) Pending(id string) bool {
	// Deferred first, so it runs after the unlock below.
	defer s.postNotices()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return false
	}
	s.expireLocked(sess)
	return len(sess.Inbox) > 0
}

// Return puts claimed messages back at the front of a session's inbox, for
// deliveries that failed.
func (s *Store) Return(id string, msgs []Message) {
	if len(msgs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.get(id)
	sess.Inbox = append(append([]Message(nil), msgs...), sess.Inbox...)
	if sess.notify != nil {
		close(sess.notify)
		sess.notify = nil
	}
	s.dirty = true
}

// Notify returns a channel that is closed when a message arrives for id.
func (s *Store) Notify(id string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.get(id)
	if sess.notify == nil {
		sess.notify = make(chan struct{})
	}
	return sess.notify
}

// NewWaiter starts a wait for id and returns its generation; an older
// generation is superseded.
func (s *Store) NewWaiter(id string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.get(id)
	sess.waiterGen++
	sess.WaiterSeen = s.now()
	sess.Waits = true
	sess.Closed = false
	return sess.waiterGen
}

// WaiterCurrent reports whether gen is still the newest waiter for id, and
// refreshes the waiter's last-seen time when it is.
func (s *Store) WaiterCurrent(id string, gen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok || sess.waiterGen != gen {
		return false
	}
	sess.WaiterSeen = s.now()
	return true
}

// Bye marks a known session as closed, so it reports offline immediately and
// drops out of Peers. Unknown ids are ignored; Bye never creates a session.
// The session itself stays in byID (as an offline entry) so its id remains
// resolvable afterward.
func (s *Store) Bye(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return
	}
	sess.Closed = true
	s.dirty = true
}

func (s *Store) statusLocked(sess *session, now time.Time) string {
	switch {
	case sess.Closed:
		return StatusOffline
	case sess.inflight > 0:
		return StatusBusy
	case sess.Waits:
		// The lease is the waiter: an interactive session whose waiter
		// stopped refreshing for leaseTTL is gone, even if it made a plain
		// request more recently.
		if now.Sub(sess.WaiterSeen) <= leaseTTL {
			return StatusIdle
		}
		return StatusOffline
	case now.Sub(sess.WaiterSeen) <= idleWithin:
		return StatusIdle
	case now.Sub(sess.lastSeen()) <= awayWithin:
		return StatusAway
	default:
		return StatusOffline
	}
}

// Peers lists sessions that are not offline, most recent first.
func (s *Store) Peers() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]Peer, 0, len(s.byID))
	for _, sess := range s.byID {
		status := s.statusLocked(sess, now)
		if status == StatusOffline {
			continue
		}
		machine := sess.Machine
		if machine == "" {
			machine = unknownMachine
		}
		out = append(out, Peer{
			Address:  s.addressLocked(sess),
			Name:     sess.Name,
			Machine:  machine,
			Cwd:      sess.Cwd,
			Status:   status,
			LastSeen: sess.lastSeen(),
		})
	}
	if s.bridge != nil {
		out = append(out, Peer{Address: SlackAddress, Machine: SlackAddress, Status: StatusIdle, LastSeen: now})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].Address < out[j].Address
	})
	return out
}

type stateFile struct {
	Version  int        `json:"version"`
	Sessions []*session `json:"sessions"`
}

// Save writes the store when it changed since the last save.
func (s *Store) Save() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	state := stateFile{Version: 1}
	cutoff := s.now().Add(-offlineListFor)
	for _, sess := range s.byID {
		s.expireLocked(sess)
		if sess.lastSeen().Before(cutoff) && len(sess.Inbox) == 0 && sess.Name == "" {
			continue
		}
		cp := *sess
		cp.Inbox = append([]Message(nil), sess.Inbox...)
		cp.Unacked = maps.Clone(sess.Unacked)
		state.Sessions = append(state.Sessions, &cp)
	}
	s.dirty = false
	s.mu.Unlock()
	s.postNotices()

	data, errMarshal := json.Marshal(state)
	if errMarshal == nil {
		if errMkdir := os.MkdirAll(filepath.Dir(s.path), 0o700); errMkdir != nil {
			errMarshal = errMkdir
		}
	}
	if errMarshal == nil {
		tmp := s.path + ".tmp"
		if errWrite := os.WriteFile(tmp, data, 0o600); errWrite != nil {
			errMarshal = errWrite
		} else if errRename := os.Rename(tmp, s.path); errRename != nil {
			errMarshal = errRename
		}
	}
	if errMarshal != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
	return errMarshal
}

// Load restores sessions and inboxes; a missing file is not an error. Names,
// machines and queued messages saved before they were restricted are cleaned
// up.
func (s *Store) Load() error {
	if s.path == "" {
		return nil
	}
	data, errRead := os.ReadFile(s.path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return nil
		}
		return errRead
	}
	var state stateFile
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal != nil {
		return errUnmarshal
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range state.Sessions {
		if sess == nil || sess.ID == "" {
			continue
		}
		if sess.Name != "" && !validName.MatchString(sess.Name) {
			sess.Name = ""
			s.dirty = true
		}
		if sess.Machine != "" && !validMachine.MatchString(sess.Machine) {
			sess.Machine = ""
			s.dirty = true
		}
		if sess.ModVersion != "" && !validModVersion.MatchString(sess.ModVersion) {
			sess.ModVersion = ""
			s.dirty = true
		}
		for i := range sess.Inbox {
			if cleanLoadedMessage(&sess.Inbox[i]) {
				s.dirty = true
			}
		}
		for id := range sess.Unacked {
			if !validReplyTo.MatchString(id) {
				delete(sess.Unacked, id)
				s.dirty = true
			}
		}
		s.byID[sess.ID] = sess
	}
	return nil
}

// cleanLoadedMessage drops an invalid reply_to, any via validVia rejects on
// a Slack user's or guest's message (and every via on anything else), an
// approval on anything but a Slack user's message or with an invalid request
// id, a broadcast mark on anything but a Slack user's message, BroadcastTo
// and BroadcastCount on anything that isn't marked Broadcast (trimming
// BroadcastTo to MaxBroadcastTo and clamping a negative BroadcastCount to
// zero either way) and, on a message from a session, a sender name
// validName rejects (keeping the sender's address). It reports whether it
// changed m.
func cleanLoadedMessage(m *Message) bool {
	changed := false
	if m.ReplyTo != "" && !validReplyTo.MatchString(m.ReplyTo) {
		m.ReplyTo = ""
		changed = true
	}
	if m.Via != "" && (!validVia(m.Via) || (!m.FromUser && !m.Guest)) {
		m.Via = ""
		changed = true
	}
	if m.Approval != "" && (!m.FromUser || m.Guest || !validReplyTo.MatchString(m.Approval)) {
		m.Approval = ""
		changed = true
	}
	if m.Broadcast && (!m.FromUser || m.Guest) {
		m.Broadcast = false
		changed = true
	}
	if !m.Broadcast {
		if len(m.BroadcastTo) > 0 {
			m.BroadcastTo = nil
			changed = true
		}
		if m.BroadcastCount != 0 {
			m.BroadcastCount = 0
			changed = true
		}
	}
	if len(m.BroadcastTo) > MaxBroadcastTo {
		m.BroadcastTo = m.BroadcastTo[:MaxBroadcastTo]
		changed = true
	}
	if m.BroadcastCount < 0 {
		m.BroadcastCount = 0
		changed = true
	}
	if m.FromUser || m.Guest {
		return changed
	}
	if i := strings.LastIndex(m.From, " ("); i >= 0 && strings.HasSuffix(m.From, ")") {
		if name, addr := m.From[:i], m.From[i+2:len(m.From)-1]; !validName.MatchString(name) {
			m.From = addr
			changed = true
		}
	}
	return changed
}
