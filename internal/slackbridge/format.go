package slackbridge

import (
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

var (
	slackMention   = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)
	slackLink      = regexp.MustCompile(`<((?:https?|mailto):[^|>]+)(?:\|[^>]*)?>`)
	labelMention   = regexp.MustCompile(`(?i)(^|[\s(\[{"'])@([a-z0-9][a-z0-9._-]*)`)
	addressed      = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._/-]*):[ \t]*(\S[\s\S]*)$`)
	atTagged       = regexp.MustCompile(`^@([A-Za-z0-9][A-Za-z0-9._/-]*):?\s+(\S[\s\S]*)$`)
	labelUnsafe    = regexp.MustCompile(`[^a-z0-9._-]+`)
	filenameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

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

// parseAddressed splits a "name: message" post.
func parseAddressed(text string) (string, string, bool) {
	m := addressed.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil || strings.HasPrefix(m[2], "//") {
		return "", "", false
	}
	return m[1], strings.TrimSpace(m[2]), true
}

// parseAtTagged splits "@name message" (or "@name: message"). raw is the
// event's text before plainText: a real Slack mention (<@U…>) renders as
// "@label" too, but it names a person, so it never counts.
func parseAtTagged(raw, text string) (string, string, bool) {
	if !strings.HasPrefix(strings.TrimSpace(raw), "@") {
		return "", "", false
	}
	m := atTagged.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return "", "", false
	}
	return m[1], strings.TrimSpace(m[2]), true
}

// botCommand is an "@bot <verb> …" command.
type botCommand struct {
	// verb is allow, remove, chat, dm, link, unlink or links.
	verb string
	// users: the person to allow or remove, to DM, to add to a chat, or
	// whose linked conversations to unlink.
	users []string
	// agent is the agent to chat, DM or link with, as written.
	agent string
	// conv is the conversation id "unlink <id>" names.
	conv string
	// ok is false when the arguments don't fit the verb.
	ok bool
}

// opensLink reports whether c opens, links or unlinks a conversation.
func (c botCommand) opensLink() bool {
	return c.verb == "chat" || c.verb == "dm" || c.verb == "link" || c.verb == "unlink"
}

var commandVerbs = map[string]bool{"allow": true, "remove": true, "chat": true, "dm": true, "link": true, "unlink": true, "links": true}

// conversationID is the shape of a Slack conversation id.
var conversationID = regexp.MustCompile(`^[A-Z0-9]{2,32}$`)

// parseCommand reads a message that starts by mentioning the bot
// (mentioned). When the next word is a command verb it returns the command:
//
//	@bot allow @user | @bot remove @user
//	@bot chat @user [@user …] with <agent> | @bot dm @user with <agent>
//	@bot link <agent> | @bot unlink [@user | <conversation id>] | @bot links
//
// Otherwise cmd.verb is empty and rest is the raw text after the mention.
func parseCommand(text, botID string) (cmd botCommand, rest string, mentioned bool) {
	t := strings.TrimSpace(text)
	loc := slackMention.FindStringSubmatchIndex(t)
	if loc == nil || loc[0] != 0 || t[loc[2]:loc[3]] != botID {
		return botCommand{}, "", false
	}
	rest = strings.TrimSpace(t[loc[1]:])
	fields := strings.Fields(rest)
	if len(fields) == 0 || !commandVerbs[strings.ToLower(fields[0])] {
		return botCommand{}, rest, true
	}
	cmd.verb = strings.ToLower(fields[0])
	args := fields[1:]
	switch cmd.verb {
	case "allow", "remove":
		if id, ok := mentionID(args); ok {
			cmd.users, cmd.ok = []string{id}, true
		}
	case "link":
		if len(args) == 1 {
			cmd.agent, cmd.ok = agentArg(args[0])
		}
	case "links":
		cmd.ok = len(args) == 0
	case "unlink":
		switch {
		case len(args) == 0:
			cmd.ok = true
		case len(args) > 1:
		case strings.HasPrefix(args[0], "<"):
			if id, ok := mentionID(args); ok {
				cmd.users, cmd.ok = []string{id}, true
			}
		default:
			if id := strings.Trim(args[0], "`"); conversationID.MatchString(id) {
				cmd.conv, cmd.ok = id, true
			}
		}
	case "chat", "dm":
		n := len(args)
		if n < 3 || !strings.EqualFold(args[n-2], "with") {
			break
		}
		for _, a := range args[:n-2] {
			id, ok := mentionID([]string{a})
			if !ok {
				return cmd, rest, true
			}
			cmd.users = append(cmd.users, id)
		}
		cmd.agent, cmd.ok = agentArg(args[n-1])
		if cmd.verb == "dm" && len(cmd.users) != 1 {
			cmd.ok = false
		}
	}
	return cmd, rest, true
}

// mentionID returns the user ID when args is exactly one Slack mention.
func mentionID(args []string) (string, bool) {
	if len(args) != 1 {
		return "", false
	}
	m := slackMention.FindStringSubmatch(args[0])
	if m == nil || m[0] != args[0] {
		return "", false
	}
	return m[1], true
}

// agentArg is an agent name or address as written after a command: a
// leading "@" is dropped, and Slack markup (a mention or link) is no agent.
func agentArg(word string) (string, bool) {
	word = strings.TrimPrefix(word, "@")
	if word == "" || strings.ContainsAny(word, "<>") {
		return "", false
	}
	return slackUnescaper.Replace(word), true
}

// sanitizeLabel makes a short lowercase handle agents can write as @label.
func sanitizeLabel(s string) string {
	label := strings.Trim(labelUnsafe.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-"), "._-")
	if label == "" {
		return "user"
	}
	return label
}

// maxFilenameLen caps a filename sent to Slack.
const maxFilenameLen = 100

// sanitizeFilename keeps a filename to [A-Za-z0-9._-]: other runs become "_",
// leading and trailing "._-" go, a long name keeps its end (the extension),
// and an empty result is "image".
func sanitizeFilename(name string) string {
	name = strings.Trim(filenameUnsafe.ReplaceAllString(name, "_"), "._-")
	if len(name) > maxFilenameLen {
		name = strings.TrimLeft(name[len(name)-maxFilenameLen:], "._-")
	}
	if name == "" {
		return "image"
	}
	return name
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
	return strings.Join(parts, " · ")
}
