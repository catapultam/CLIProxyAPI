package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/automode"
)

// autoModeResponse is the wire shape for GET /v1/auto-mode.
type autoModeResponse struct {
	Session string `json:"session"`
	State   string `json:"state"`
	Since   string `json:"since"`
	Updated string `json:"updated"`
}

// autoModeHandler reports the auto-mode server-review status Claude Code's
// own /status shows as "Auto mode server: Enabled/Disabled", for the given
// session, so a status line can display it.
//
//	GET /v1/auto-mode?session=<id>
func (s *Server) autoModeHandler(c *gin.Context) {
	sessionID := strings.TrimSpace(c.Query("session"))
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is required"})
		return
	}

	state, ok := automode.Lookup(sessionID)
	if !ok {
		c.JSON(http.StatusOK, autoModeResponse{Session: sessionID, State: "unknown"})
		return
	}
	c.JSON(http.StatusOK, autoModeResponse{
		Session: sessionID,
		State:   state.Mode,
		Since:   state.Since.Format(time.RFC3339),
		Updated: state.Updated.Format(time.RFC3339),
	})
}
