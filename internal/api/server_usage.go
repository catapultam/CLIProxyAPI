package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// pooledUsageHandler serves pooled 5h and weekly usage in the rate_limits shape
// Claude Code status lines read. Claude Code does not surface usage itself when
// it authenticates to a gateway, so status lines fetch it from here.
//
//	GET /v1/usage?model=<id>         credentials that serve the model, plus the
//	                                 weekly pool of every provider (seven_day_by_provider)
//	GET /v1/usage?provider=<name>    every credential of claude or codex (openai)
func (s *Server) pooledUsageHandler(c *gin.Context) {
	model := strings.TrimSpace(c.Query("model"))
	provider := strings.ToLower(strings.TrimSpace(c.Query("provider")))
	if provider == "openai" {
		provider = "codex"
	}
	if (model == "") == (provider == "") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "exactly one of model or provider is required"})
		return
	}
	if provider != "" && provider != "claude" && provider != "codex" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider must be claude, codex or openai"})
		return
	}
	if s.handlers == nil || s.handlers.AuthManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	if provider != "" {
		c.JSON(http.StatusOK, s.handlers.AuthManager.PooledUsageForProvider(provider, time.Now()))
		return
	}
	c.JSON(http.StatusOK, s.handlers.AuthManager.PooledUsageReport(model, time.Now()))
}
