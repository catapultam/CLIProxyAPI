package agentbus

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

// MinCommandModVersion is the oldest agentbus mod that runs commands.
const MinCommandModVersion = "0.3.3"

// OwnerChecker is implemented by bridges that know which Slack users are
// owners (seeded from config). /wait asks it before handing out a command.
type OwnerChecker interface {
	// IsOwner reports whether userID may run commands right now. Store calls
	// it without holding its lock.
	IsOwner(userID string) bool
}

// DeliverCommand queues an owner's command for a session given by id, name
// or address, and returns that session's id and the new message's id. It
// sets FromUser, SlackUser, SlackUserID and Command; the body is only a
// summary ("!name args"). The session must be CommandCapable, else
// ErrCommandUnsupported. Only the Slack bridge calls it, after checking that
// slackUserID is an owner, and it is the only way a message gets a Command.
func (s *Store) DeliverCommand(target string, cmd Command, slackUser, slackUserID string) (sessionID, msgID string, err error) {
	return s.DeliverCommandVia(target, cmd, slackUser, slackUserID, "")
}

// DeliverCommandVia is DeliverCommand for a command written somewhere other
// than the bridge's main channel: via is as for DeliverVia (ViaSlash for
// /clanker), so a prompt command from a DM is framed as private and a report
// in a group conversation leaves the machine out.
func (s *Store) DeliverCommandVia(target string, cmd Command, slackUser, slackUserID, via string) (sessionID, msgID string, err error) {
	return s.deliverCommand(target, cmd, slackUser, slackUserID, via, false, nil, 0)
}

// DeliverCommandBroadcast is DeliverCommandVia for an owner's broadcast to
// all agents ("all: !cmd"): the message is marked Broadcast, to lists the
// broadcast's other recipients by bus address (BroadcastTo), and total is
// every recipient including this one (BroadcastCount). Only the Slack
// bridge calls it.
func (s *Store) DeliverCommandBroadcast(target string, cmd Command, slackUser, slackUserID, via string, to []string, total int) (sessionID, msgID string, err error) {
	return s.deliverCommand(target, cmd, slackUser, slackUserID, via, true, to, total)
}

func (s *Store) deliverCommand(target string, cmd Command, slackUser, slackUserID, via string, broadcast bool, to []string, total int) (string, string, error) {
	if !validVia(via) {
		return "", "", ErrInvalidVia
	}
	if !validCommand(cmd) {
		return "", "", ErrInvalidCommand
	}
	body := "!" + cmd.Name
	if cmd.Args != "" {
		body += " " + cmd.Args
	}
	if len(body) > MaxBodyBytes {
		return "", "", ErrBodyTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, sess, err := s.deliverTargetLocked(target)
	if err != nil {
		return "", "", err
	}
	if !commandCapable(sess) {
		return "", "", ErrCommandUnsupported
	}
	msg := s.fromSlackLocked(sess, body, slackUser)
	msg.SlackUserID = slackUserID
	msg.Via = via
	msg.Broadcast = broadcast
	if broadcast {
		msg.BroadcastTo = cappedBroadcastTo(to)
		msg.BroadcastCount = clampBroadcastCount(total)
	}
	if cmd.Argv != nil {
		argv := make(map[string][]string, len(cmd.Argv))
		for osKey, list := range cmd.Argv {
			argv[osKey] = append([]string(nil), list...)
		}
		cmd.Argv = argv
	}
	if cmd.Env != nil {
		cmd.Env = maps.Clone(cmd.Env)
	}
	if cmd.ArgsEnum != nil {
		cmd.ArgsEnum = slices.Clone(cmd.ArgsEnum)
	}
	msg.Command = &cmd
	s.enqueueLocked(sess, msg)
	return id, msg.ID, nil
}

// CommandCapable resolves target (id, name or address) and reports whether
// its session can run commands: it runs the agentbus mod, and the mod
// reported version MinCommandModVersion or later. An unknown target is
// ErrUnknownTarget.
func (s *Store) CommandCapable(target string) (sessionID string, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, sess, err := s.deliverTargetLocked(target)
	if err != nil {
		return "", false, err
	}
	return id, commandCapable(sess), nil
}

func commandCapable(sess *session) bool {
	return sess.Mod && versionAtLeast(sess.ModVersion, MinCommandModVersion)
}

func validCommand(cmd Command) bool {
	if strings.TrimSpace(cmd.Name) == "" {
		return false
	}
	switch cmd.Kind {
	case CommandSlash, CommandPrompt, CommandShell:
		return true
	}
	return false
}

// versionAtLeast compares dotted versions numerically per segment; a missing
// segment counts as 0. A malformed version is never at least min.
func versionAtLeast(version, min string) bool {
	if !validModVersion.MatchString(version) {
		return false
	}
	have, want := strings.Split(version, "."), strings.Split(min, ".")
	for i := 0; i < len(have) || i < len(want); i++ {
		h, w := versionSegment(have, i), versionSegment(want, i)
		if h != w {
			return h > w
		}
	}
	return true
}

func versionSegment(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, errAtoi := strconv.Atoi(parts[i])
	if errAtoi != nil {
		return -1
	}
	return n
}

// ClaimForWait claims a session's messages for a /wait (claimForWait). mod
// and waiterVersion are what the waiter reported (mod=1, v); without mod the
// version is ignored. The Slack messages are reported (Receipts) as received,
// awaiting the mod's /ack, only to a mod waiter at MinAckModVersion or later.
// Any other waiter (the legacy wait.sh hook, curl, an older mod) never acks
// and has consumed them, so they are read at once.
func (s *Store) ClaimForWait(id string, mod bool, waiterVersion string) []Message {
	if !mod {
		waiterVersion = ""
	}
	msgs := s.claimForWait(id, waiterVersion)
	s.received(id, msgs, versionAtLeast(strings.TrimSpace(waiterVersion), MinAckModVersion))
	if !versionAtLeast(strings.TrimSpace(waiterVersion), MinImageModVersion) {
		noteImagesForOldWaiter(msgs)
	}
	return msgs
}

// claimForWait claims a session's messages for the mod's /wait. A command
// message is handed out only while its sender is still an owner according to
// the attached bridge. Otherwise (the sender was demoted, no bridge is
// attached, or the bridge can't tell) it is dropped, logged, and refused in
// the Slack thread it came from. Owners are checked without the store lock.
//
// waiterVersion is the mod version the requesting waiter itself reported
// (/wait's v parameter). Below MinCommandModVersion, or absent, the waiter
// can't run commands (an older mod would show one to the model as an
// instruction), so command messages stay queued, where they expire after
// commandTTL.
func (s *Store) claimForWait(id, waiterVersion string) []Message {
	if !versionAtLeast(strings.TrimSpace(waiterVersion), MinCommandModVersion) {
		return s.ClaimPlain(id)
	}
	msgs := s.Claim(id)
	hasCommand := false
	for _, m := range msgs {
		if m.Command != nil {
			hasCommand = true
			break
		}
	}
	if !hasCommand {
		return msgs
	}
	owners, _ := s.currentBridge().(OwnerChecker)
	now := s.now()
	out := make([]Message, 0, len(msgs))
	var notices []commandNotice
	for _, m := range msgs {
		switch {
		case m.Command == nil:
			out = append(out, m)
		case commandExpired(m, now):
			// Claim expires these already; this guards the hand-out itself.
			log.Infof("agentbus: command %s (!%s) for %s expired unclaimed", m.ID, m.Command.Name, m.To)
			notices = append(notices, expiredNotice(id, m))
		case owners != nil && m.SlackUserID != "" && owners.IsOwner(m.SlackUserID):
			out = append(out, m)
		default:
			log.Warnf("agentbus: dropped command %s (!%s) for %s: sender %s is not an owner now", m.ID, m.Command.Name, m.To, m.SlackUserID)
			notices = append(notices, commandNotice{sessionID: id, msgID: m.ID,
				body: fmt.Sprintf("`!%s` was not run: %s may no longer run commands.", m.Command.Name, m.SlackUser)})
		}
	}
	if len(notices) > 0 {
		s.postNotices(notices...)
	}
	return out
}
