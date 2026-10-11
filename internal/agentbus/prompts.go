package agentbus

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Prompts: blocking Claude Code prompts (permission dialogs, plan approval,
// questions) that the Slack bridge posts to an owner's DM so they can be
// answered there. The bridge owns the state; these routes are the mod's way
// in. Only a Slack click or modal can answer a prompt: no route here does.

// Prompt kinds.
const (
	PromptKindPermission = "permission"
	PromptKindPlan       = "plan"
	PromptKindQuestion   = "question"
	PromptKindDenial     = "denial"
)

// Question kinds.
const (
	QuestionChoice = "choice"
	QuestionText   = "text"
	QuestionNumber = "number"
)

// Resolve outcomes.
const (
	OutcomeRan       = "ran"
	OutcomeDenied    = "denied"
	OutcomeAnswered  = "answered"
	OutcomeCancelled = "cancelled"
	OutcomeEnded     = "ended"
)

const (
	maxQuestions       = 4
	maxQuestionOptions = 10
	maxPromptBody      = 1 << 20
	maxPromptKeyLen    = 128
)

var (
	// ErrPromptsOff is returned when prompts are unavailable (Slack is off
	// or has no owner DM to post to).
	ErrPromptsOff = errors.New("prompts are off")
	// ErrPromptsBusy means too many prompts are open.
	ErrPromptsBusy = errors.New("too many open prompts")
	// ErrPromptPost means Slack would not take the prompt.
	ErrPromptPost = errors.New("could not post the prompt")
	// ErrPromptUnknown is an id the session does not own, or one forgotten.
	ErrPromptUnknown = errors.New("unknown prompt")
)

// PromptOption is one choice of a question.
type PromptOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// PromptQuestion is one question of a kind "question" prompt.
type PromptQuestion struct {
	ID      string         `json:"id"`
	Text    string         `json:"text"`
	Header  string         `json:"header,omitempty"`
	Kind    string         `json:"kind"`
	Options []PromptOption `json:"options,omitempty"`
	Multi   bool           `json:"multi,omitempty"`
	Min     *float64       `json:"min,omitempty"`
	Max     *float64       `json:"max,omitempty"`
	Step    *float64       `json:"step,omitempty"`
	Unit    string         `json:"unit,omitempty"`
}

// PromptRequest is the body of POST /prompt.
type PromptRequest struct {
	Session   string           `json:"session"`
	Key       string           `json:"key"`
	Kind      string           `json:"kind"`
	Title     string           `json:"title"`
	Detail    string           `json:"detail,omitempty"`
	Agent     string           `json:"agent,omitempty"`
	Always    bool             `json:"always,omitempty"`
	Questions []PromptQuestion `json:"questions,omitempty"`
	// Secrets are literal values the bridge must also redact from what it
	// posts (the caller's own bearer token). Never read from the body.
	Secrets []string `json:"-"`
}

// PromptAnswer is what a Slack click or modal answered.
type PromptAnswer struct {
	Choice  string         `json:"choice"`
	Reason  string         `json:"reason,omitempty"`
	Answers map[string]any `json:"answers,omitempty"`
	By      string         `json:"by"`
}

// Prompt wait states.
const (
	PromptOpen     = "open"
	PromptAnswered = "answered"
	PromptClosed   = "closed"
	PromptGone     = "unknown"
)

// PromptWait is the result of one wait: State is a Prompt* constant, and
// Answer is set when State is PromptAnswered.
type PromptWait struct {
	State  string
	Answer *PromptAnswer
}

// PromptBridge is implemented by the Slack bridge. Store and the handlers
// call it without holding the Store's lock; implementations must not call
// into Store while holding their own.
type PromptBridge interface {
	// PromptsAvailable reports whether prompts can be posted now.
	PromptsAvailable() bool
	// OpenPrompt posts req (idempotent on session and key) and returns its
	// id. o is the session's Outbound (name, address, machine).
	OpenPrompt(ctx context.Context, o Outbound, req PromptRequest) (string, error)
	// WaitPrompt long-polls (about 25 s) for session's prompt id.
	WaitPrompt(ctx context.Context, session, id string) PromptWait
	// ResolvePrompt closes a prompt from the mod's side. It is idempotent;
	// an id the session does not own is ErrPromptUnknown.
	ResolvePrompt(session, id, outcome, note string) error
	// SessionEnded closes every prompt of a session that said bye.
	SessionEnded(session string)
}

func (s *Store) promptBridge() PromptBridge {
	pb, _ := s.currentBridge().(PromptBridge)
	return pb
}

// validPrompt checks req's shape; it returns a short reason, or "".
func validPrompt(req *PromptRequest) string {
	req.Session, req.Key = strings.TrimSpace(req.Session), strings.TrimSpace(req.Key)
	if req.Session == "" {
		return "session is required"
	}
	if req.Key == "" || len(req.Key) > maxPromptKeyLen {
		return "key is required (at most 128 characters)"
	}
	switch req.Kind {
	case PromptKindPermission, PromptKindPlan, PromptKindDenial:
		if len(req.Questions) > 0 {
			return "questions are only for kind question"
		}
	case PromptKindQuestion:
		if n := len(req.Questions); n < 1 || n > maxQuestions {
			return "kind question needs 1 to 4 questions"
		}
		seen := map[string]bool{}
		for _, q := range req.Questions {
			if reason := validQuestion(q, seen); reason != "" {
				return reason
			}
		}
	default:
		return "kind must be permission, plan, question or denial"
	}
	return ""
}

func validQuestion(q PromptQuestion, seen map[string]bool) string {
	if strings.TrimSpace(q.ID) == "" || seen[q.ID] {
		return "each question needs a unique id"
	}
	seen[q.ID] = true
	if strings.TrimSpace(q.Text) == "" {
		return "each question needs text"
	}
	switch q.Kind {
	case QuestionChoice:
		if n := len(q.Options); n < 1 || n > maxQuestionOptions {
			return "a choice question needs 1 to 10 options"
		}
		ids := map[string]bool{}
		for _, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" || ids[o.ID] {
				return "each option needs a label and a unique id"
			}
			ids[o.ID] = true
		}
	case QuestionText, QuestionNumber:
		if len(q.Options) > 0 {
			return "only a choice question has options"
		}
		if q.Min != nil && q.Max != nil && *q.Min > *q.Max {
			return "min is above max"
		}
	default:
		return "question kind must be choice, text or number"
	}
	return ""
}

// bearerToken is the token of an "Authorization: Bearer ..." header.
func bearerToken(c *gin.Context) string {
	h := strings.TrimSpace(c.GetHeader("Authorization"))
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// handlePrompt opens a prompt: 200 {"id"}, 400 malformed, 404 prompts off or
// session unknown, 429 too many open, 502 Slack refused the post.
func (s *Store) handlePrompt(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPromptBody)
	var req PromptRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}
	if reason := validPrompt(&req); reason != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": reason})
		return
	}
	pb := s.promptBridge()
	if pb == nil || !pb.PromptsAvailable() {
		c.JSON(http.StatusNotFound, gin.H{"error": "prompts are off"})
		return
	}
	o, errSession := s.SessionOutbound(req.Session)
	if errSession != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown session"})
		return
	}
	if tok := bearerToken(c); tok != "" {
		req.Secrets = []string{tok}
	}
	id, err := pb.OpenPrompt(c.Request.Context(), o, req)
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"id": id})
	case errors.Is(err, ErrPromptsOff):
		c.JSON(http.StatusNotFound, gin.H{"error": "prompts are off"})
	case errors.Is(err, ErrPromptsBusy):
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many open prompts"})
	default:
		log.Warnf("agentbus: prompt for %s not posted: %v", s.Address(req.Session), err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not post the prompt"})
	}
}

// handlePromptWait long-polls for a prompt's answer: 200 {"answer"}, 204
// still open, 410 closed without an answer, 404 unknown.
func (s *Store) handlePromptWait(c *gin.Context) {
	session, id := strings.TrimSpace(c.Query("session")), strings.TrimSpace(c.Query("id"))
	if session == "" || id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session and id are required"})
		return
	}
	pb := s.promptBridge()
	if pb == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown prompt"})
		return
	}
	res := pb.WaitPrompt(c.Request.Context(), session, id)
	switch res.State {
	case PromptAnswered:
		c.JSON(http.StatusOK, gin.H{"answer": res.Answer})
	case PromptClosed:
		c.JSON(http.StatusGone, gin.H{"error": "prompt closed"})
	case PromptGone:
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown prompt"})
	default:
		if c.Request.Context().Err() == nil {
			c.Status(http.StatusNoContent)
		}
	}
}

type promptResolveRequest struct {
	Session string `json:"session"`
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Note    string `json:"note"`
}

// handlePromptResolve closes a prompt from the mod's side: 200 {"ok":true},
// 400 malformed, 404 unknown.
func (s *Store) handlePromptResolve(c *gin.Context) {
	var req promptResolveRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}
	req.Session, req.ID = strings.TrimSpace(req.Session), strings.TrimSpace(req.ID)
	switch req.Outcome {
	case OutcomeRan, OutcomeDenied, OutcomeAnswered, OutcomeCancelled, OutcomeEnded:
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "outcome must be ran, denied, answered, cancelled or ended"})
		return
	}
	if req.Session == "" || req.ID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session and id are required"})
		return
	}
	pb := s.promptBridge()
	if pb == nil || errors.Is(pb.ResolvePrompt(req.Session, req.ID, req.Outcome, req.Note), ErrPromptUnknown) {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown prompt"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
