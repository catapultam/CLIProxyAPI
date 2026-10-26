// Package models builds model catalogs for Anthropic clients.
package models

import (
	"sort"
	"strings"
)

// claudeGatewayModelPrefix marks a non-Claude model ID in Anthropic model listings.
// Claude Code's gateway discovery drops IDs that do not contain "claude" or "anthropic",
// and it identifies models by searching the ID for family names such as
// "claude-fable-5". The prefix contains no family name, so Claude Code reads
// "claude-gw-gpt-6.1-sol" as its own unknown model (own modelSettings key, unknown-model
// defaults) until a client maps it with a modelPicker behavesAs row.
const claudeGatewayModelPrefix = "claude-gw-"

// claudeDDModelPrefix is the legacy encoding: "claude-fable-5-dd-" plus the original ID
// reversed. Claude Code read every such ID as Fable 5 (1M window, Fable 5 settings).
// Listings no longer produce it; requests are still decoded so sessions and transcripts
// that picked a legacy ID keep routing.
const claudeDDModelPrefix = "claude-fable-5-dd-"

// BuildResponse builds an Anthropic model response from available models.
// Models whose original ID matches hiddenModels are omitted; matching happens before cloaking.
func BuildResponse(availableModels []map[string]any, disableCloaking bool, hiddenModels []string) map[string]any {
	models := make([]map[string]any, 0, len(availableModels))
	for _, model := range availableModels {
		id, hasID := model["id"].(string)
		if hasID && isHidden(id, hiddenModels) {
			continue
		}
		cloned := cloneModel(model)
		if hasID && !disableCloaking {
			cloned["id"] = EnsureClaudeModelIDPrefix(id)
		}
		models = append(models, cloned)
	}

	sort.SliceStable(models, func(i, j int) bool {
		displayNameI, _ := models[i]["display_name"].(string)
		displayNameJ, _ := models[j]["display_name"].(string)
		if displayNameI != displayNameJ {
			return displayNameI < displayNameJ
		}
		idI, _ := models[i]["id"].(string)
		idJ, _ := models[j]["id"].(string)
		return idI < idJ
	})

	firstID := ""
	lastID := ""
	if len(models) > 0 {
		firstID, _ = models[0]["id"].(string)
		lastID, _ = models[len(models)-1]["id"].(string)
	}

	return map[string]any{
		"data":     models,
		"has_more": false,
		"first_id": firstID,
		"last_id":  lastID,
	}
}

// EnsureClaudeModelIDPrefix rewrites model IDs for Anthropic model listings.
// IDs that already start with "claude-" are returned unchanged; all other IDs
// become "claude-gw-" plus the original ID.
func EnsureClaudeModelIDPrefix(id string) string {
	if id == "" || strings.HasPrefix(id, "claude-") {
		return id
	}
	return claudeGatewayModelPrefix + id
}

// ResolveClaudeModelIDPrefix reverses EnsureClaudeModelIDPrefix for request routing.
// It also decodes the legacy claude-fable-5-dd-<reversed> form.
// Optional thinking suffixes in model(value) form are preserved.
func ResolveClaudeModelIDPrefix(id string) string {
	if id == "" {
		return id
	}
	base, suffix, hasSuffix := splitModelThinkingSuffix(id)
	var resolved string
	switch {
	case strings.HasPrefix(base, claudeGatewayModelPrefix):
		resolved = base[len(claudeGatewayModelPrefix):]
	case strings.HasPrefix(base, claudeDDModelPrefix):
		resolved = reverseModelID(base[len(claudeDDModelPrefix):])
	}
	if resolved == "" {
		return id
	}
	if hasSuffix {
		return resolved + "(" + suffix + ")"
	}
	return resolved
}

func cloneModel(model map[string]any) map[string]any {
	cloned := make(map[string]any, len(model))
	for key, value := range model {
		cloned[key] = value
	}
	return cloned
}

func splitModelThinkingSuffix(model string) (base, suffix string, hasSuffix bool) {
	lastOpen := strings.LastIndex(model, "(")
	if lastOpen == -1 || !strings.HasSuffix(model, ")") {
		return model, "", false
	}
	return model[:lastOpen], model[lastOpen+1 : len(model)-1], true
}

func reverseModelID(id string) string {
	runes := []rune(id)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

func isHidden(id string, patterns []string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, pattern := range patterns {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern != "" && matchWildcard(pattern, id) {
			return true
		}
	}
	return false
}

// matchWildcard matches value against a pattern where '*' matches any substring.
func matchWildcard(pattern, value string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	last := parts[len(parts)-1]
	if !strings.HasSuffix(value, last) {
		return false
	}
	value = value[:len(value)-len(last)]
	for _, segment := range parts[1 : len(parts)-1] {
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}
	return true
}
