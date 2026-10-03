package agentbus

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SlackAddress is the reserved bus address of the Slack bridge. A DM to an
// allowed Slack user is addressed "slack@<label>"; every "slack@..." target
// is reserved too and never resolves to a session.
const SlackAddress = "slack"

// Via values: where in Slack a message reached the bridge, when not in its
// main channel.
const (
	// ViaDM marks a message written to the bot in a direct message.
	ViaDM = "dm"
	// ViaGroup marks a message written in a group DM or a channel other than
	// the main one, where other people can read the answer.
	ViaGroup = "group"
)

var (
	// ErrUnknownSlackUser is a "slack@<label>" target whose label isn't an
	// allowed Slack user. It is an ErrUnknownTarget.
	ErrUnknownSlackUser = fmt.Errorf("%w: no allowed Slack user with that label", ErrUnknownTarget)
	// ErrInvalidVia is a Via value DeliverVia doesn't know.
	ErrInvalidVia = errors.New("invalid via")
)

// Outbound is a message a session sent to SlackAddress, with what the bridge
// needs to label the session's thread.
type Outbound struct {
	SessionID string
	Address   string
	Name      string
	Machine   string
	Body      string
	// ReplyTo is a validated message id or empty. The bridge posts into the
	// Slack thread that message came from only when it was delivered to this
	// session; otherwise it uses the session's own thread.
	ReplyTo string
	// DM is the label of the allowed Slack user this message is a direct
	// message to, or empty. The Store sets it only to a label the bridge's
	// Users listed; a DM ignores ReplyTo.
	DM string
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

// BotNamer is implemented by bridges that know their Slack bot's display
// name, so the note can name it as people see it in Slack. Store calls it
// without holding its lock.
type BotNamer interface {
	BotName() string
}

// ImagePoster is implemented by bridges that can post images.
type ImagePoster interface {
	// PostImage posts data into the thread Post would pick for o, with o.Body
	// as the caption, and returns once Slack has answered. Store calls it without
	// holding its lock. An error's text must be safe to show the agent: no
	// tokens or upload URLs.
	PostImage(ctx context.Context, o Outbound, filename string, data []byte) error
}

// SetBridge attaches the Slack bridge, or detaches it when b is nil.
func (s *Store) SetBridge(b Bridge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bridge = b
}

// currentBridge returns the attached bridge, or nil when Slack is off.
func (s *Store) currentBridge() Bridge {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bridge
}

// outboundLocked is what the bridge needs to post body for the session id.
// The caller holds s.mu.
func (s *Store) outboundLocked(id string, sess *session, body string) Outbound {
	return Outbound{
		SessionID: id,
		Address:   s.addressLocked(sess),
		Name:      sess.Name,
		Machine:   sess.Machine,
		Body:      body,
	}
}

// outboundFor builds the Outbound for a known session, or returns
// ErrUnknownSender. The caller hands it to the bridge after this returns, so
// never under the lock.
func (s *Store) outboundFor(sessionID, body string) (Outbound, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[sessionID]
	if !ok {
		return Outbound{}, ErrUnknownSender
	}
	return s.outboundLocked(sessionID, sess, body), nil
}

// SessionOutbound returns what the bridge needs to post as the session with
// id sessionID (address, name and machine, with no body), or
// ErrUnknownSender. Only a session id matches, never a name or address.
func (s *Store) SessionOutbound(sessionID string) (Outbound, error) {
	return s.outboundFor(sessionID, "")
}

func isSlackAddress(target string) bool {
	return strings.EqualFold(strings.TrimSpace(target), SlackAddress)
}

// slackDMLabel splits a "slack@<label>" target (the prefix compared
// case-insensitively) and returns the trimmed label, which may be empty. ok
// is false for any other target.
func slackDMLabel(target string) (label string, ok bool) {
	target = strings.TrimSpace(target)
	prefix := SlackAddress + "@"
	if len(target) < len(prefix) || !strings.EqualFold(target[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(target[len(prefix):]), true
}

// isReservedTarget reports whether target is "slack" or "slack@...", which
// never name a session.
func isReservedTarget(target string) bool {
	_, dm := slackDMLabel(target)
	return dm || isSlackAddress(target)
}

// slackDMTarget checks a DM label against bridge's allowed users, compared
// case-insensitively, and returns it as the bridge spells it, or
// ErrUnknownSlackUser. It calls bridge.Users, so the caller must not hold
// s.mu.
func slackDMTarget(bridge Bridge, label string) (string, error) {
	if bridge == nil || label == "" {
		return "", ErrUnknownSlackUser
	}
	for _, u := range bridge.Users() {
		if strings.EqualFold(u, label) {
			return u, nil
		}
	}
	return "", ErrUnknownSlackUser
}

// sendDM hands body from a known session to the bridge as a DM to the allowed
// Slack user label names. A DM ignores reply_to.
func (s *Store) sendDM(fromID, label, body string) (Message, error) {
	bridge := s.currentBridge()
	// Users is asked before taking s.mu (the lock rule).
	canonical, errLabel := slackDMTarget(bridge, label)
	s.mu.Lock()
	from, ok := s.byID[fromID]
	if !ok {
		s.mu.Unlock()
		return Message{}, ErrUnknownSender
	}
	if errLabel != nil {
		s.mu.Unlock()
		return Message{}, errLabel
	}
	msg := s.sentLocked(from, body, "")
	msg.To = SlackAddress + "@" + canonical
	out := s.outboundLocked(fromID, from, body)
	out.DM = canonical
	s.mu.Unlock()
	bridge.Post(out)
	return msg, nil
}

// Deliver queues a message from an allowed Slack user for a session given by
// id, name or address, and returns that session's id and the new message's
// id. Only the Slack bridge calls it, and together with DeliverVia and
// DeliverCommand it is the only way a message gets FromUser.
func (s *Store) Deliver(target, body, slackUser string) (sessionID, msgID string, err error) {
	return s.DeliverVia(target, body, slackUser, "")
}

// DeliverVia is Deliver for a message that reached the bridge other than in
// the channel: via is ViaDM for a direct message, ViaGroup for a group DM or
// another channel, or empty. Only the Slack bridge calls it, and together
// with DeliverGuest it is the only way a message gets Via.
func (s *Store) DeliverVia(target, body, slackUser, via string) (sessionID, msgID string, err error) {
	return s.deliverFromSlack(target, body, via, func(m *Message) {
		m.FromUser = true
		m.SlackUser = slackUser
	})
}

// DeliverGuest queues a message from a guest: a Slack user who isn't
// allowed, writing in a conversation an owner linked to the session. label
// names the guest; via is as for DeliverVia. It sets Guest, never FromUser.
// Only the Slack bridge calls it, and it is the only way a message gets
// Guest.
func (s *Store) DeliverGuest(target, body, label, via string) (sessionID, msgID string, err error) {
	return s.deliverFromSlack(target, body, via, func(m *Message) {
		m.Guest = true
		m.SlackUser = label
	})
}

// DeliverNotice queues a notice from the Slack bridge itself, such as "you
// were linked to a conversation": From is SlackAddress, and it is neither a
// user's instruction nor a guest's input. Only the Slack bridge calls it.
func (s *Store) DeliverNotice(target, body string) (sessionID, msgID string, err error) {
	return s.deliverFromSlack(target, body, "", func(*Message) {})
}

// deliverFromSlack queues a message from SlackAddress for target (id, name
// or address), with fill marking who it is from.
func (s *Store) deliverFromSlack(target, body, via string, fill func(*Message)) (string, string, error) {
	if via != "" && via != ViaDM && via != ViaGroup {
		return "", "", ErrInvalidVia
	}
	if strings.TrimSpace(body) == "" {
		return "", "", ErrEmptyBody
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
	msg := Message{ID: newMessageID(), From: SlackAddress, To: s.addressLocked(sess), Body: body, Via: via, CreatedAt: s.now()}
	fill(&msg)
	s.enqueueLocked(sess, msg)
	return id, msg.ID, nil
}

// deliverTargetLocked finds the session the bridge addresses as target (id,
// name or address). A closed session is ErrUnknownTarget. The caller holds
// s.mu.
func (s *Store) deliverTargetLocked(target string) (string, *session, error) {
	if isReservedTarget(target) {
		// A session id is whatever the client sends in /hello, so a client
		// could register "slack" (or "slack@alex") as its own session id and
		// otherwise reach this through the byID fast path below. Reject it
		// the same way resolveLocked rejects the name/address forms.
		return "", nil, ErrUnknownTarget
	}
	id := strings.TrimSpace(target)
	if _, ok := s.byID[id]; !ok {
		resolved, found := s.resolveLocked(target)
		if !found {
			return "", nil, ErrUnknownTarget
		}
		id = resolved
	}
	sess := s.byID[id]
	if sess.Closed {
		// The session said bye: nothing would ever read it. After /clear,
		// /resume or /branch its successor took over its name (HandOff).
		return "", nil, ErrUnknownTarget
	}
	return id, sess, nil
}

// fromSlackLocked builds a message from an allowed Slack user to sess. The
// caller holds s.mu.
func (s *Store) fromSlackLocked(sess *session, body, slackUser string) Message {
	return Message{
		ID:        newMessageID(),
		From:      SlackAddress,
		To:        s.addressLocked(sess),
		Body:      body,
		FromUser:  true,
		SlackUser: slackUser,
		CreatedAt: s.now(),
	}
}
