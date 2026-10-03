package config

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// SanitizeModelRewrite trims routing.model-rewrite rules and drops entries with
// an empty match or target.
func (cfg *Config) SanitizeModelRewrite() {
	if cfg == nil {
		return
	}
	cfg.Routing.ModelRewrite = SanitizeModelRewriteRules(cfg.Routing.ModelRewrite)
}

// SanitizeModelRewriteRules returns trimmed rules in their original order,
// without entries whose match or target is empty. It returns nil when no rule remains.
func SanitizeModelRewriteRules(rules []ModelRewriteRule) []ModelRewriteRule {
	var out []ModelRewriteRule
	for _, rule := range rules {
		match := strings.TrimSpace(rule.Match)
		to := strings.TrimSpace(rule.To)
		if match == "" || to == "" {
			continue
		}
		out = append(out, ModelRewriteRule{Match: match, To: to})
	}
	return out
}

// SanitizeModelRewriteNode sanitizes routing.model-rewrite inside a YAML document
// root so the persisted list matches what the runtime applies.
func SanitizeModelRewriteNode(root *yaml.Node) error {
	node := yamlPath(root, "routing.model-rewrite")
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	var rules []ModelRewriteRule
	if err := node.Decode(&rules); err != nil {
		return err
	}
	var clean yaml.Node
	if err := clean.Encode(SanitizeModelRewriteRules(rules)); err != nil {
		return err
	}
	if clean.Kind != yaml.SequenceNode {
		clean = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	}
	clean.HeadComment, clean.LineComment, clean.FootComment = node.HeadComment, node.LineComment, node.FootComment
	*node = clean
	return nil
}

// MatchModelRewrite returns the target of the first rule whose pattern matches
// model. Rules are evaluated once, in order; the returned target is never
// matched against the rules again.
func MatchModelRewrite(rules []ModelRewriteRule, model string) (string, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", false
	}
	for _, rule := range rules {
		match := strings.TrimSpace(rule.Match)
		to := strings.TrimSpace(rule.To)
		if match == "" || to == "" {
			continue
		}
		if modelRewriteGlobMatch(match, model) {
			return to, true
		}
	}
	return "", false
}

// modelRewriteGlobMatch reports whether value matches pattern, case-insensitively.
// "*" matches any run of characters, including "/"; every other character is literal.
func modelRewriteGlobMatch(pattern, value string) bool {
	pattern = strings.ToLower(pattern)
	value = strings.ToLower(value)
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	first, last := parts[0], parts[len(parts)-1]
	if !strings.HasPrefix(value, first) {
		return false
	}
	value = value[len(first):]
	if !strings.HasSuffix(value, last) {
		return false
	}
	value = value[:len(value)-len(last)]
	for _, part := range parts[1 : len(parts)-1] {
		idx := strings.Index(value, part)
		if idx < 0 {
			return false
		}
		value = value[idx+len(part):]
	}
	return true
}
