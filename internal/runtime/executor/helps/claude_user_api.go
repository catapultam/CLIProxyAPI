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

// NormalizeClaudeUserAPIBody removes billing attribution and normalizes the
// official-client identities used by main sessions and subagents. Application
// instructions and conversation content remain intact.
func NormalizeClaudeUserAPIBody(body []byte) []byte {
	body = util.StripClaudeCodeAttributionSystem(body)
	system := gjson.GetBytes(body, "system")
	if system.Type == gjson.String {
		text := normalizeClaudeUserAPIIdentity(system.String())
		if text == system.String() {
			return body
		}
		var updated []byte
		var err error
		if text == "" {
			updated, err = sjson.DeleteBytes(body, "system")
		} else {
			updated, err = sjson.SetBytes(body, "system", text)
		}
		if err == nil {
			return updated
		}
		return body
	}
	if !system.IsArray() {
		return body
	}
	kept := make([]string, 0, len(system.Array()))
	removed := false
	for _, block := range system.Array() {
		raw := block.Raw
		if block.Get("type").String() == "text" {
			original := block.Get("text").String()
			text := normalizeClaudeUserAPIIdentity(original)
			if text != original {
				removed = true
				if text == "" {
					continue
				}
				updated, errSet := sjson.Set(raw, "text", text)
				if errSet != nil {
					return body
				}
				raw = updated
			}
		}
		kept = append(kept, raw)
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

func normalizeClaudeUserAPIIdentity(text string) string {
	trimmed := strings.TrimSpace(text)
	for _, identity := range []string{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
		"You are an agent for Claude Code, Anthropic's official CLI for Claude.",
	} {
		if trimmed == identity {
			return ""
		}
		if strings.HasPrefix(trimmed, identity) {
			return strings.Replace(text, identity, "You are a coding assistant.", 1)
		}
	}
	return text
}
