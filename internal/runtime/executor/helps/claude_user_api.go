package helps

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// IsClaudeUserAPIKey identifies user API credentials, whose ordinary API access
// must not inherit Claude Code billing attribution from the downstream client.
func IsClaudeUserAPIKey(key string) bool {
	return strings.HasPrefix(strings.TrimSpace(key), "sk-ant-usr-")
}

// NormalizeClaudeUserAPIHeaders keeps user API requests on the ordinary API
// path. OAuth and explicitly selected CLI fingerprint profiles are handled by
// the caller and retain their native identity.
func NormalizeClaudeUserAPIHeaders(header http.Header) {
	if strings.Contains(strings.ToLower(header.Get("User-Agent")), "claude-cli/") {
		header.Set("User-Agent", "CLIProxyAPI/"+buildinfo.Version)
	}
	if strings.EqualFold(header.Get("X-App"), "cli") {
		header.Del("X-App")
	}
	betas := strings.Split(header.Get("Anthropic-Beta"), ",")
	kept := make([]string, 0, len(betas))
	for _, beta := range betas {
		beta = strings.TrimSpace(beta)
		if beta != "" && !strings.HasPrefix(beta, "claude-code-") && !strings.HasPrefix(beta, "oauth-") {
			kept = append(kept, beta)
		}
	}
	if len(kept) == 0 {
		header.Del("Anthropic-Beta")
	} else {
		header.Set("Anthropic-Beta", strings.Join(kept, ","))
	}
}

// NormalizeClaudeUserAPIBody removes billing attribution and the standalone
// official-client identity. Application instructions and conversation content
// remain intact.
func NormalizeClaudeUserAPIBody(body []byte) []byte {
	body = util.StripClaudeCodeAttributionSystem(body)
	const identity = "You are Claude Code, Anthropic's official CLI for Claude."
	system := gjson.GetBytes(body, "system")
	if system.Type == gjson.String && strings.TrimSpace(system.String()) == identity {
		updated, errDelete := sjson.DeleteBytes(body, "system")
		if errDelete == nil {
			return updated
		}
	}
	if !system.IsArray() {
		return body
	}
	kept := make([]string, 0, len(system.Array()))
	removed := false
	for _, block := range system.Array() {
		if block.Get("type").String() == "text" && strings.TrimSpace(block.Get("text").String()) == identity {
			removed = true
			continue
		}
		kept = append(kept, block.Raw)
	}
	if !removed {
		return body
	}
	if len(kept) == 0 {
		updated, errDelete := sjson.DeleteBytes(body, "system")
		if errDelete == nil {
			return updated
		}
		return body
	}
	updated, errSet := sjson.SetRawBytes(body, "system", []byte("["+strings.Join(kept, ",")+"]"))
	if errSet == nil {
		return updated
	}
	return body
}
