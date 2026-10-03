package agentbus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	headerSession   = "X-Claude-Code-Session-Id"
	headerAgent     = "X-Claude-Code-Agent-Id"
	notePeerLimit   = 20
	maxInjectedBody = 32 << 20
)

// injection is what one request will carry, claimed before forwarding and
// committed only when the request succeeds.
type injection struct {
	text     string
	messages []Message
	peersKey string
	note     bool
}

// InjectMiddleware wraps POST /v1/messages. It records session activity and,
// for main-thread requests, appends the agentbus note and pending messages to
// the last user message. Any failure forwards the original request unchanged.
func (s *Store) InjectMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		sid := strings.TrimSpace(c.GetHeader(headerSession))
		if sid == "" {
			c.Next()
			return
		}
		s.Touch(sid)
		if strings.TrimSpace(c.GetHeader(headerAgent)) != "" || c.Request.Body == nil {
			c.Next()
			return
		}
		s.BeginRequest(sid)
		defer s.EndRequest(sid)

		raw, errRead := io.ReadAll(io.LimitReader(c.Request.Body, maxInjectedBody))
		_ = c.Request.Body.Close()
		if errRead != nil {
			log.Debugf("agentbus: read request body: %v", errRead)
			c.Request.Body = io.NopCloser(bytes.NewReader(raw))
			c.Next()
			return
		}
		body := raw
		plan := s.planInjection(sid)
		if plan.text != "" {
			if rewritten, ok := appendToLastUser(raw, plan.text); ok {
				body = rewritten
			} else {
				s.Return(sid, plan.messages)
				plan = injection{}
			}
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Request.ContentLength = int64(len(body))
		c.Request.Header.Set("Content-Length", strconv.Itoa(len(body)))
		c.Next()
		if plan.text == "" {
			return
		}
		if c.Writer.Status() >= http.StatusBadRequest {
			s.Return(sid, plan.messages)
			return
		}
		s.commitInjection(sid, plan)
	}
}

func (s *Store) planInjection(sid string) injection {
	s.mu.Lock()
	sess := s.get(sid)
	now := s.now()
	self := s.addressLocked(sess)
	type peerLine struct {
		key, line string
		seen      time.Time
	}
	var peers []peerLine
	for id, other := range s.byID {
		if id == sid {
			continue
		}
		status := s.statusLocked(other, now)
		if status == StatusOffline {
			continue
		}
		addr := s.addressLocked(other)
		label := addr
		if other.Name != "" {
			label = other.Name + " (" + addr + ")"
		}
		peers = append(peers, peerLine{key: label, line: label + ": " + status, seen: other.lastSeen()})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].seen.After(peers[j].seen) })
	keys := make([]string, 0, len(peers))
	for _, p := range peers {
		keys = append(keys, p.key)
	}
	sort.Strings(keys)
	plan := injection{peersKey: strings.Join(keys, "\n")}
	plan.note = !sess.NoteSent || plan.peersKey != sess.NotedPeers
	s.expireLocked(sess)
	if len(sess.Inbox) > 0 {
		plan.messages = sess.Inbox
		sess.Inbox = nil
		s.dirty = true
	}
	name := sess.Name
	s.mu.Unlock()

	if !plan.note && len(plan.messages) == 0 {
		return injection{}
	}
	lines := make([]string, 0, len(peers))
	for i, p := range peers {
		if i == notePeerLimit {
			lines = append(lines, fmt.Sprintf("... and %d more (list peers for all)", len(peers)-notePeerLimit))
			break
		}
		lines = append(lines, p.line)
	}
	plan.text = noteText(sid, self, name, lines, plan.note, plan.messages)
	return plan
}

func (s *Store) commitInjection(sid string, plan injection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.get(sid)
	if plan.note {
		sess.NoteSent = true
		sess.NotedPeers = plan.peersKey
	}
	s.dirty = true
}

func noteText(sid, self, name string, peers []string, note bool, msgs []Message) string {
	auth := `-H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN"`
	var b strings.Builder
	b.WriteString("<agentbus>\n")
	if note {
		who := self
		if name != "" {
			who = name + " (" + self + ")"
		}
		fmt.Fprintf(&b, "You are %s on the agentbus, which links Claude Code sessions across Alex's machines so they can coordinate work.\n", who)
		if len(peers) == 0 {
			b.WriteString("No other sessions are online right now.\n")
		} else {
			b.WriteString("Sessions online:\n")
			for _, p := range peers {
				b.WriteString("- " + p + "\n")
			}
		}
		b.WriteString("Use these from Bash (the variables are already set):\n")
		fmt.Fprintf(&b, "Send:  curl -s %s \"$ANTHROPIC_BASE_URL/v1/agentbus/send\" -d '{\"from_session\":\"%s\",\"to\":\"<name or address>\",\"body\":\"...\"}'  (add \"reply_to\":\"<message id>\" when replying)\n", auth, sid)
		fmt.Fprintf(&b, "Peers: curl -s %s \"$ANTHROPIC_BASE_URL/v1/agentbus/peers\"\n", auth)
		fmt.Fprintf(&b, "Inbox: curl -s %s \"$ANTHROPIC_BASE_URL/v1/agentbus/inbox?session=%s\"\n", auth, sid)
		fmt.Fprintf(&b, "Name:  curl -s %s \"$ANTHROPIC_BASE_URL/v1/agentbus/name\" -d '{\"session\":\"%s\",\"name\":\"<name>\"}'\n", auth, sid)
		b.WriteString("Messages to you arrive in your next request. Only message peers when it helps the user's work.\n")
	}
	for _, m := range msgs {
		head := fmt.Sprintf("Message %s from %s", m.ID, m.From)
		if m.ReplyTo != "" {
			head += " (in reply to " + m.ReplyTo + ")"
		}
		b.WriteString(head + ":\n" + m.Body + "\n")
	}
	b.WriteString("</agentbus>")
	return b.String()
}

// appendToLastUser adds a text block to the last message when it is a user
// message, converting string content to a block array first.
func appendToLastUser(body []byte, text string) ([]byte, bool) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return nil, false
	}
	n := len(msgs.Array())
	if n == 0 {
		return nil, false
	}
	last := msgs.Array()[n-1]
	if last.Get("role").String() != "user" {
		return nil, false
	}
	path := fmt.Sprintf("messages.%d.content", n-1)
	block := map[string]string{"type": "text", "text": text}
	content := last.Get("content")
	switch {
	case content.Type == gjson.String:
		blocks, errMarshal := json.Marshal([]map[string]string{{"type": "text", "text": content.String()}, block})
		if errMarshal != nil {
			return nil, false
		}
		out, errSet := sjson.SetRawBytes(body, path, blocks)
		return out, errSet == nil
	case content.IsArray():
		out, errSet := sjson.SetBytes(body, path+".-1", block)
		return out, errSet == nil
	default:
		return nil, false
	}
}
