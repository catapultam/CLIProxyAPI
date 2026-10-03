package slackbridge

import (
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

var (
	slackMention = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)
	slackLink    = regexp.MustCompile(`<((?:https?|mailto):[^|>]+)(?:\|[^>]*)?>`)
	labelMention = regexp.MustCompile(`(?i)(^|[\s(\[{"'])@([a-z0-9][a-z0-9._-]*)`)
	addressed    = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._/-]*):[ \t]*(\S[\s\S]*)$`)
	labelUnsafe  = regexp.MustCompile(`[^a-z0-9._-]+`)

	slackEscaper   = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	slackUnescaper = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")
)

// escape makes agent text inert for Slack: & < > are control characters, so an
// agent writing "<!channel>" or "<@U…>" can't ping anyone.
func escape(s string) string { return slackEscaper.Replace(s) }

// withMentions escapes text, then turns @label into a real mention for
// allowed users (ids maps label to Slack user ID).
func withMentions(text string, ids map[string]string) string {
	return labelMention.ReplaceAllStringFunc(escape(text), func(m string) string {
		sub := labelMention.FindStringSubmatch(m)
		prefix, word := sub[1], sub[2]
		label := strings.ToLower(strings.TrimRight(word, "._-"))
		id, ok := ids[label]
		if !ok {
			return m
		}
		return prefix + "<@" + id + ">" + word[len(label):]
	})
}

// plainText turns Slack message markup into what a person typed: mentions
// become @label (labels maps user ID to label), links lose their brackets.
func plainText(text string, labels map[string]string) string {
	text = slackMention.ReplaceAllStringFunc(text, func(m string) string {
		id := slackMention.FindStringSubmatch(m)[1]
		if label, ok := labels[id]; ok {
			return "@" + label
		}
		return "@" + id
	})
	text = slackLink.ReplaceAllString(text, "$1")
	return slackUnescaper.Replace(text)
}

// parseAddressed splits a top-level "name: message" post.
func parseAddressed(text string) (string, string, bool) {
	m := addressed.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil || strings.HasPrefix(m[2], "//") {
		return "", "", false
	}
	return m[1], strings.TrimSpace(m[2]), true
}

// parseCommand recognises "@bot allow @user" and "@bot remove @user". isCommand
// is true for anything that starts by mentioning the bot.
func parseCommand(text, botID string) (string, string, bool) {
	t := strings.TrimSpace(text)
	loc := slackMention.FindStringSubmatchIndex(t)
	if loc == nil || loc[0] != 0 || t[loc[2]:loc[3]] != botID {
		return "", "", false
	}
	rest := strings.Fields(t[loc[1]:])
	if len(rest) != 2 {
		return "", "", true
	}
	verb := strings.ToLower(rest[0])
	m := slackMention.FindStringSubmatch(rest[1])
	if (verb != "allow" && verb != "remove") || m == nil || m[0] != rest[1] {
		return "", "", true
	}
	return verb, m[1], true
}

// sanitizeLabel makes a short lowercase handle agents can write as @label.
func sanitizeLabel(s string) string {
	label := strings.Trim(labelUnsafe.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-"), "._-")
	if label == "" {
		return "user"
	}
	return label
}

func emailLabel(email string) string {
	local, _, _ := strings.Cut(strings.TrimSpace(email), "@")
	return sanitizeLabel(local)
}

// sessionHeader is the first line of a session's thread.
func sessionHeader(o agentbus.Outbound) string {
	who := o.Name
	if who == "" {
		who = o.Address
	}
	parts := []string{"*" + escape(who) + "*"}
	if o.Name != "" && o.Address != "" {
		parts = append(parts, escape(o.Address))
	}
	if o.Machine != "" {
		parts = append(parts, escape(o.Machine))
	}
	if o.Cwd != "" {
		parts = append(parts, "`"+escape(o.Cwd)+"`")
	}
	return strings.Join(parts, " · ")
}
