package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// pooledUsageHandler serves GET /v1/usage?model=<id>: pooled 5h and weekly
// usage across the credentials that serve the model, in the rate_limits shape
// Claude Code status lines read. Claude Code does not surface usage itself when
// it authenticates to a gateway, so status lines fetch it from here.
func (s *Server) pooledUsageHandler(c *gin.Context) {
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}
	if s.handlers == nil || s.handlers.AuthManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, s.handlers.AuthManager.PooledUsageForModel(model, time.Now()))
}
