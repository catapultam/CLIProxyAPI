package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	claudemodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/claude/models"
)

// claudeCodeSettingsModel is the sanitized wire shape of one gateway model row.
type claudeCodeSettingsModel struct {
	Model             string `json:"model"`
	Label             string `json:"label,omitempty"`
	BehavesAs         string `json:"behavesAs"`
	AutoCompactWindow int    `json:"autoCompactWindow,omitempty"`
	EffortLevel       string `json:"effortLevel,omitempty"`
}

// claudeCodeSettingsResponse is the wire shape for GET /v1/claude-code/settings.
type claudeCodeSettingsResponse struct {
	Version int                       `json:"version"`
	Models  []claudeCodeSettingsModel `json:"models"`
}

// claudeCodeSettingsHandler returns the gateway model rows Claude Code clients need to build
// their model picker settings. Only whitelisted fields are exposed and no upstream is contacted.
//
//	GET /v1/claude-code/settings
func (s *Server) claudeCodeSettingsHandler(c *gin.Context) {
	resp := claudeCodeSettingsResponse{Version: 1, Models: []claudeCodeSettingsModel{}}
	if cfg := s.getConfig(); cfg != nil {
		for _, row := range cfg.ClaudeCode.GatewayModels {
			resp.Models = append(resp.Models, claudeCodeSettingsModel{
				Model:             claudemodels.EnsureClaudeModelIDPrefix(row.ID),
				Label:             row.Label,
				BehavesAs:         row.BehavesAs,
				AutoCompactWindow: row.AutoCompactWindow,
				EffortLevel:       row.EffortLevel,
			})
		}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, resp)
}
