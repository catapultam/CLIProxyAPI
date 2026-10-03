package agentbus

import (
	"fmt"
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
	if cmd.Argv != nil {
		argv := make(map[string][]string, len(cmd.Argv))
		for osKey, list := range cmd.Argv {
			argv[osKey] = append([]string(nil), list...)
		}
		cmd.Argv = argv
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

// ClaimForWait claims a session's messages for the mod's /wait. A command
// message is handed out only while its sender is still an owner according to
// the attached bridge. Otherwise (the sender was demoted, no bridge is
// attached, or the bridge can't tell) it is dropped, logged, and refused in
// the Slack thread it came from. Owners are checked without the store lock.
func (s *Store) ClaimForWait(id string) []Message {
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
	bridge := s.currentBridge()
	owners, _ := bridge.(OwnerChecker)
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Command == nil || (owners != nil && m.SlackUserID != "" && owners.IsOwner(m.SlackUserID)) {
			out = append(out, m)
			continue
		}
		log.Warnf("agentbus: dropped command %s (!%s) for %s: sender %s is not an owner now", m.ID, m.Command.Name, m.To, m.SlackUserID)
		if bridge == nil {
			continue
		}
		refusal, errOut := s.outboundFor(id, fmt.Sprintf("`!%s` was not run: %s may no longer run commands.", m.Command.Name, m.SlackUser))
		if errOut != nil {
			continue
		}
		refusal.ReplyTo = m.ID
		bridge.Post(refusal)
	}
	return out
}
