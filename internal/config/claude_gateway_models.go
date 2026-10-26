package config

import (
	"fmt"
	"strings"
	"unicode"
)

const (
	// MaxClaudeGatewayModels caps the number of gateway model rows.
	MaxClaudeGatewayModels = 200
	// MaxClaudeGatewayFieldLength caps id, label, and behaves-as.
	MaxClaudeGatewayFieldLength = 256
	// MinClaudeGatewayAutoCompactWindow and MaxClaudeGatewayAutoCompactWindow bound auto-compact-window.
	MinClaudeGatewayAutoCompactWindow = 100000
	MaxClaudeGatewayAutoCompactWindow = 1000000
)

// ClaudeGatewayModel describes one non-Claude upstream model that Claude Code clients
// present as a Claude model.
type ClaudeGatewayModel struct {
	// ID is the original provider model ID (for example gpt-6.1-sol). It must not start with "claude-".
	ID string `yaml:"id" json:"id"`
	// Label is an optional display name.
	Label string `yaml:"label,omitempty" json:"label,omitempty"`
	// BehavesAs is the canonical Claude model ID whose client-side behavior the model borrows.
	BehavesAs string `yaml:"behaves-as" json:"behaves-as"`
	// AutoCompactWindow is the client auto-compact window in tokens; 0 or omitted uses the client default.
	AutoCompactWindow int `yaml:"auto-compact-window,omitempty" json:"auto-compact-window,omitempty"`
	// EffortLevel is one of low, medium, high, xhigh; empty inherits the client setting.
	EffortLevel string `yaml:"effort-level,omitempty" json:"effort-level,omitempty"`
	// HideFromDiscovery hides the original ID from Anthropic model lists. Nil means true.
	HideFromDiscovery *bool `yaml:"hide-from-discovery,omitempty" json:"hide-from-discovery,omitempty"`
}

// Hidden reports whether the row's original ID is hidden from model discovery.
func (m ClaudeGatewayModel) Hidden() bool {
	return m.HideFromDiscovery == nil || *m.HideFromDiscovery
}

// EffectiveHiddenModels returns HiddenModels plus the original IDs of gateway rows that are hidden
// from discovery. The configuration itself is not modified.
func (c ClaudeCodeConfig) EffectiveHiddenModels() []string {
	if len(c.GatewayModels) == 0 {
		return c.HiddenModels
	}
	out := make([]string, 0, len(c.HiddenModels)+len(c.GatewayModels))
	out = append(out, c.HiddenModels...)
	for _, row := range c.GatewayModels {
		id := strings.TrimSpace(row.ID)
		if id == "" || !row.Hidden() || strings.Contains(id, "*") {
			continue
		}
		out = append(out, id)
	}
	return out
}

// ValidateGatewayModels checks the gateway model rows.
func (c ClaudeCodeConfig) ValidateGatewayModels() error {
	rows := c.GatewayModels
	if len(rows) > MaxClaudeGatewayModels {
		return fmt.Errorf("gateway-models: at most %d rows are allowed, got %d", MaxClaudeGatewayModels, len(rows))
	}
	seen := make(map[string]int, len(rows))
	for i, row := range rows {
		where := fmt.Sprintf("gateway-models[%d]", i)
		if err := validateGatewayModelID(row.ID); err != nil {
			return fmt.Errorf("%s: id %w", where, err)
		}
		key := strings.ToLower(row.ID)
		if first, dup := seen[key]; dup {
			return fmt.Errorf("%s: duplicate id %q (same as gateway-models[%d], case-insensitive)", where, row.ID, first)
		}
		seen[key] = i
		if len(row.Label) > MaxClaudeGatewayFieldLength {
			return fmt.Errorf("%s: label exceeds %d characters", where, MaxClaudeGatewayFieldLength)
		}
		if strings.TrimSpace(row.Label) != row.Label {
			return fmt.Errorf("%s: label must not have leading or trailing whitespace", where)
		}
		for _, r := range row.Label {
			if unicode.IsControl(r) {
				return fmt.Errorf("%s: label must not contain control characters", where)
			}
		}
		if err := validateGatewayBehavesAs(row.BehavesAs); err != nil {
			return fmt.Errorf("%s: behaves-as %w", where, err)
		}
		if row.AutoCompactWindow != 0 && (row.AutoCompactWindow < MinClaudeGatewayAutoCompactWindow || row.AutoCompactWindow > MaxClaudeGatewayAutoCompactWindow) {
			return fmt.Errorf("%s: auto-compact-window must be 0 or between %d and %d", where, MinClaudeGatewayAutoCompactWindow, MaxClaudeGatewayAutoCompactWindow)
		}
		switch row.EffortLevel {
		case "", "low", "medium", "high", "xhigh":
		default:
			return fmt.Errorf("%s: effort-level must be one of low, medium, high, xhigh", where)
		}
	}
	return nil
}

func validateGatewayModelID(id string) error {
	if id == "" {
		return fmt.Errorf("is required")
	}
	if len(id) > MaxClaudeGatewayFieldLength {
		return fmt.Errorf("exceeds %d characters", MaxClaudeGatewayFieldLength)
	}
	for i, r := range id {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alnum && (i == 0 || !strings.ContainsRune("._:/-", r)) {
			return fmt.Errorf("%q may only contain letters, digits, '.', '_', ':', '/', and '-', and must start with a letter or digit", id)
		}
	}
	if strings.HasPrefix(strings.ToLower(id), "claude-") {
		return fmt.Errorf("%q must be a non-Claude ID and must not start with claude-", id)
	}
	return nil
}

func validateGatewayBehavesAs(id string) error {
	if id == "" {
		return fmt.Errorf("is required")
	}
	if len(id) > MaxClaudeGatewayFieldLength {
		return fmt.Errorf("exceeds %d characters", MaxClaudeGatewayFieldLength)
	}
	if !strings.HasPrefix(id, "claude-") || id == "claude-" {
		return fmt.Errorf("%q must be a Claude model ID starting with claude-", id)
	}
	for _, r := range id {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
		if !ok {
			return fmt.Errorf("%q may only contain letters, digits, and dashes", id)
		}
	}
	return nil
}
