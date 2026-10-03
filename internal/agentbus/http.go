package agentbus

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	defaultWaitTimeout = 50 * time.Second
	waitRecheckEvery   = time.Second
	slackUserNotFound  = "no allowed Slack user with that label (or Slack is off); slack@<label> takes a label the agentbus note lists"
)

// Register mounts the agentbus endpoints on group (mounted under
// /v1/agentbus, behind the client API key middleware).
func (s *Store) Register(group *gin.RouterGroup) {
	group.GET("/peers", s.handlePeers)
	group.POST("/send", s.handleSend)
	group.POST("/name", s.handleName)
	group.GET("/inbox", s.handleInbox)
	group.POST("/hello", s.handleHello)
	group.GET("/wait", s.handleWait)
	group.POST("/ack", s.handleAck)
	group.POST("/bye", s.handleBye)
	group.POST("/dismiss", s.handleDismiss)
	group.POST("/done", s.handleDone)
	group.POST("/working", s.handleWorking)
	group.POST("/slack/upload", s.handleSlackUpload)
}

func (s *Store) handlePeers(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"peers": s.Peers()})
}

type sendRequest struct {
	FromSession string `json:"from_session"`
	To          string `json:"to"`
	Body        string `json:"body"`
	ReplyTo     string `json:"reply_to"`
}

// controlWords are the bodies handleSend treats specially when sent with a
// reply_to to Slack, so a bare "ignore", "working" or "done" (and a trailing
// "done" line) is never posted as text: it guards curl and plugins that
// predate the mod's own handling of them (register.ts, the SendMessage
// hook), for the same effect either way.
const (
	ignoreWord  = "ignore"
	doneWord    = "done"
	workingWord = "working"
)

func (s *Store) handleSend(c *gin.Context) {
	var req sendRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}
	from := strings.TrimSpace(req.FromSession)
	if replyTo := cleanReplyTo(req.ReplyTo); replyTo != "" && isSlackAddress(req.To) {
		switch strings.ToLower(strings.TrimSpace(req.Body)) {
		case ignoreWord:
			c.JSON(http.StatusOK, gin.H{"dismissed": s.Dismiss(from, []string{replyTo})})
			return
		case doneWord:
			c.JSON(http.StatusOK, gin.H{"done": s.Done(from, []string{replyTo})})
			return
		case workingWord:
			c.JSON(http.StatusOK, gin.H{"working": s.Working(from, []string{replyTo})})
			return
		}
		if rest, ok := trailingDoneLine(req.Body); ok {
			msg, err := s.Send(from, req.To, rest, req.ReplyTo)
			if err == nil {
				s.Done(from, []string{replyTo})
			}
			s.writeSendResult(c, msg, err)
			return
		}
	}
	msg, err := s.Send(from, req.To, req.Body, req.ReplyTo)
	s.writeSendResult(c, msg, err)
}

// trailingDoneLine reports whether body's last non-empty line, trimmed and
// lowercased, is exactly "done", with other non-empty content above it, and
// returns body with that line (and any blank lines after it) removed. ok is
// false when body is only that line, or has no such line.
func trailingDoneLine(body string) (rest string, ok bool) {
	lines := strings.Split(body, "\n")
	last := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			last = i
			break
		}
	}
	if last < 0 || strings.ToLower(strings.TrimSpace(lines[last])) != doneWord {
		return "", false
	}
	rest = strings.TrimRight(strings.Join(lines[:last], "\n"), "\n")
	if strings.TrimSpace(rest) == "" {
		return "", false
	}
	return rest, true
}

// writeSendResult writes handleSend's response for what Send returned.
func (s *Store) writeSendResult(c *gin.Context, msg Message, err error) {
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"id": msg.ID, "to": msg.To})
	case errors.Is(err, ErrUnknownSlackUser):
		c.JSON(http.StatusNotFound, gin.H{"error": slackUserNotFound})
	case errors.Is(err, ErrUnknownTarget):
		c.JSON(http.StatusNotFound, gin.H{"error": "no session with that name or address; list peers first"})
	case errors.Is(err, ErrUnknownSender):
		c.JSON(http.StatusBadRequest, gin.H{"error": "from_session is not a known session"})
	case errors.Is(err, ErrBodyTooLarge):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body exceeds 16 KiB"})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	}
}

type nameRequest struct {
	Session string `json:"session"`
	Name    string `json:"name"`
}

func (s *Store) handleName(c *gin.Context) {
	var req nameRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}
	err := s.SetName(strings.TrimSpace(req.Session), req.Name)
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"address": s.Address(req.Session), "name": strings.TrimSpace(req.Name)})
	case errors.Is(err, ErrNameTaken):
		c.JSON(http.StatusConflict, gin.H{"error": "name already taken"})
	case errors.Is(err, ErrInvalidName):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid name: use 1-64 letters, digits, '.', '_' or '-' (no spaces)"})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	}
}

// handleInbox returns a session's pending messages, except command messages:
// those leave the store only through /wait (the mod). Nothing acks what
// /inbox hands out, so Slack messages in it are reported read at once.
func (s *Store) handleInbox(c *gin.Context) {
	id := strings.TrimSpace(c.Query("session"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is required"})
		return
	}
	msgs := s.ClaimPlain(id)
	s.received(id, msgs, false)
	if msgs == nil {
		msgs = []Message{}
	}
	c.JSON(http.StatusOK, gin.H{"messages": msgs})
}

type helloRequest struct {
	Session string `json:"session"`
	Machine string `json:"machine"`
	Cwd     string `json:"cwd"`
	Name    string `json:"name"`
	Mod     bool   `json:"mod"`
	// Version is the mod's version; with mod set, empty means a mod older
	// than commands.
	Version string `json:"version"`
	// Previous is the session id this one replaced after /clear, /resume or
	// /branch; see Store.HandOff.
	Previous string `json:"previous"`
}

func (s *Store) handleHello(c *gin.Context) {
	var req helloRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil || strings.TrimSpace(req.Session) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is required"})
		return
	}
	s.Hello(strings.TrimSpace(req.Session), req.Machine, req.Cwd, req.Name, req.Mod)
	if req.Mod {
		// A mod without a version is older than commands: clear the record.
		s.SetModVersion(strings.TrimSpace(req.Session), req.Version)
	}
	if prev := strings.TrimSpace(req.Previous); prev != "" {
		if errHandOff := s.HandOff(prev, req.Session); errHandOff != nil {
			log.Infof("agentbus: %s can't take over %s: %v", s.Address(strings.TrimSpace(req.Session)), s.Address(prev), errHandOff)
		}
	}
	c.JSON(http.StatusOK, gin.H{"address": s.Address(strings.TrimSpace(req.Session))})
}

type byeRequest struct {
	Session string `json:"session"`
}

// handleBye marks a session closed so it drops out of Peers. An unknown
// session id is accepted but ignored (no session is created); only an empty
// session id is a 400.
func (s *Store) handleBye(c *gin.Context) {
	var req byeRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil || strings.TrimSpace(req.Session) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is required"})
		return
	}
	s.Bye(strings.TrimSpace(req.Session))
	c.Status(http.StatusNoContent)
}

// handleWait long-polls for messages. It returns 200 with the claimed
// messages, 204 when nothing arrived before the timeout, and 409 when a newer
// waiter for the same session took over.
func (s *Store) handleWait(c *gin.Context) {
	id := strings.TrimSpace(c.Query("session"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is required"})
		return
	}
	// version is this waiter's own mod version. Only a mod-marked waiter has
	// one, and only a waiter whose version can run commands gets them.
	mod, version := c.Query("mod") == "1", ""
	s.Hello(id, c.Query("machine"), c.Query("cwd"), c.Query("name"), mod)
	if mod {
		version = c.Query("v")
		s.SetModVersion(id, version)
	}
	gen := s.NewWaiter(id)
	timeout := s.waitTimeout
	if timeout <= 0 {
		timeout = defaultWaitTimeout
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	recheck := time.NewTicker(waitRecheckEvery)
	defer recheck.Stop()
	for {
		if !s.WaiterCurrent(id, gen) {
			c.JSON(http.StatusConflict, gin.H{"error": "superseded by a newer waiter"})
			return
		}
		if msgs := s.ClaimForWait(id, mod, version); len(msgs) > 0 {
			c.JSON(http.StatusOK, gin.H{"messages": msgs})
			return
		}
		notify := s.Notify(id)
		select {
		case <-c.Request.Context().Done():
			return
		case <-deadline.C:
			c.Status(http.StatusNoContent)
			return
		case <-notify:
		case <-recheck.C:
		}
	}
}

type ackRequest struct {
	Session string   `json:"session"`
	IDs     []string `json:"ids"`
}

// handleAck is the mod reporting that messages it got from /wait were read:
// the turn their prompt started completed, or their command ran. Only ids
// /wait handed to that session count (Store.Ack). It returns how many did.
func (s *Store) handleAck(c *gin.Context) {
	var req ackRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil || strings.TrimSpace(req.Session) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is required"})
		return
	}
	if len(req.IDs) > maxAckIDs {
		c.JSON(http.StatusBadRequest, gin.H{"error": "too many ids"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"acked": s.Ack(strings.TrimSpace(req.Session), req.IDs)})
}

// handleDismiss is an agent dismissing Slack messages that weren't meant for
// it ({"session", "ids"}, like /ack): their receipts come off and stay off.
// Only ids delivered to that session count (Store.Dismiss). It returns how
// many did.
func (s *Store) handleDismiss(c *gin.Context) {
	var req ackRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil || strings.TrimSpace(req.Session) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is required"})
		return
	}
	if len(req.IDs) > maxAckIDs {
		c.JSON(http.StatusBadRequest, gin.H{"error": "too many ids"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"dismissed": s.Dismiss(strings.TrimSpace(req.Session), req.IDs)})
}

// handleDone is an agent marking Slack messages done, believing it fully
// answered them ({"session", "ids"}, like /ack): the reaction becomes ✅,
// for good, and never moves back to an earlier state. Only ids delivered to
// that session count (Store.Done). It returns how many did.
func (s *Store) handleDone(c *gin.Context) {
	var req ackRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil || strings.TrimSpace(req.Session) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is required"})
		return
	}
	if len(req.IDs) > maxAckIDs {
		c.JSON(http.StatusBadRequest, gin.H{"error": "too many ids"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"done": s.Done(strings.TrimSpace(req.Session), req.IDs)})
}

// handleWorking is a turn still running on Slack messages 15s after it
// started ({"session", "ids"}, like /ack): the reaction becomes ⏳ until the
// turn completes and /ack moves it on. Only ids delivered to that session
// count (Store.Working). It returns how many did.
func (s *Store) handleWorking(c *gin.Context) {
	var req ackRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil || strings.TrimSpace(req.Session) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is required"})
		return
	}
	if len(req.IDs) > maxAckIDs {
		c.JSON(http.StatusBadRequest, gin.H{"error": "too many ids"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"working": s.Working(strings.TrimSpace(req.Session), req.IDs)})
}
