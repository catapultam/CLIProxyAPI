package agentbus

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	defaultWaitTimeout = 50 * time.Second
	waitRecheckEvery   = time.Second
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
	group.POST("/bye", s.handleBye)
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

func (s *Store) handleSend(c *gin.Context) {
	var req sendRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}
	msg, err := s.Send(strings.TrimSpace(req.FromSession), req.To, req.Body, req.ReplyTo)
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"id": msg.ID, "to": msg.To})
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
// those leave the store only through /wait (the mod).
func (s *Store) handleInbox(c *gin.Context) {
	id := strings.TrimSpace(c.Query("session"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is required"})
		return
	}
	msgs := s.ClaimPlain(id)
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
		if msgs := s.ClaimForWait(id, version); len(msgs) > 0 {
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
