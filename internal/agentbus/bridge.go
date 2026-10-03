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
	if isSlackAddress(target) {
		// A session id is whatever the client sends in /hello, so a client
		// could register "slack" as its own session id and otherwise reach
		// this through the byID fast path below. Reject it the same way
		// resolveLocked rejects the name/address forms.
		return "", ErrUnknownTarget
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
