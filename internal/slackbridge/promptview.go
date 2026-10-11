package slackbridge

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

// Rendering of prompts as Block Kit: the message (buttons while open, a
// status line once answered or closed) and the modals. Every agent-supplied
// string is redacted first, then escaped for mrkdwn and cut to Slack's limits
// on an escape boundary, so a cut never splits an entity or a secret.

const (
	// maxHeaderRunes is Slack's header block text limit.
	maxHeaderRunes = 150
	// maxSectionRunes is how much escaped detail goes into one section block
	// (Slack allows 3,000 characters; the code fence needs a few).
	maxSectionRunes = 2900
	// maxDetailBlocks caps the detail sections of one message.
	maxDetailBlocks = 40
	// maxMessageBlocks is Slack's limit on the blocks of one message.
	maxMessageBlocks = 50
	// maxButtonRunes is Slack's button text limit.
	maxButtonRunes = 75
	// maxReasonInput caps the modal's reason field.
	maxReasonInput = 1000
	// maxLabelRunes caps a modal input label (Slack's limit is 2,000).
	maxLabelRunes = 1900

	// noteTerminal is the resolve note of a call settled in the terminal.
	noteTerminal = "terminal"

	otherSuffix = "_other"
	redacted    = "[redacted]"
)

// secretWord ends the name of a setting that holds a secret.
const secretWord = `(?:token|secret|passw(?:or)?d|api[-_]?key|access[-_]?key|private[-_]?key|credential)s?`

// secretShapes are the token shapes redact removes.
var secretShapes = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`),
	regexp.MustCompile(`xox[abpr]-[A-Za-z0-9-]+`),
	regexp.MustCompile(`xapp-[A-Za-z0-9-]+`),
	regexp.MustCompile(`\bsk-(?:ant-)?[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{16,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`),
}

// secretValue is a value: quoted, or a run of non-space characters.
const secretValue = `"[^"\n]*"|'[^'\n]*'|["']?\S+`

var (
	// secretAssign matches NAME=value, NAME: value and "NAME": "value" for any
	// name that ends in a secret word. Group 1 is everything up to the value,
	// group 2 the value.
	secretAssign = regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*` + secretWord + `["']?[ \t]*[=:][ \t]*)(` + secretValue + `)`)
	// secretFlag matches a command-line flag such as --password x or
	// --token=x. Group 1 is the flag with what precedes it, group 2 the
	// separator, group 3 the value.
	secretFlag = regexp.MustCompile(`(?im)((?:^|[\s'"])--?[A-Za-z0-9_-]*` + secretWord + `)(=|[ \t]+)(` + secretValue + `)`)
	// codeExpr is a value that is code, not a literal: an identifier called,
	// indexed or dereferenced.
	codeExpr = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*[\[(.]`)
	// fenceRun is a run of backticks long enough to open or close a fence.
	fenceRun = regexp.MustCompile("`{3,}")
)

// redactValue is value with its secret replaced by the placeholder, keeping
// matching quotes around it.
func redactValue(value string) string {
	if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
		return value[:1] + redacted + value[n-1:]
	}
	return redacted
}

// isCodeValue reports whether an unquoted value reads as code (token =
// tokens[i]) and not as a secret. A JWT looks like a dotted identifier but is
// a secret.
func isCodeValue(value string) bool {
	return codeExpr.MatchString(value) && !strings.HasPrefix(value, "eyJ")
}

// redactAssignments redacts the values of secret-named settings and flags.
func redactAssignments(s string) string {
	s = secretFlag.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretFlag.FindStringSubmatch(m)
		if strings.TrimSpace(sub[2]) == "" && strings.HasPrefix(sub[3], "-") {
			return m // the next flag, not a value
		}
		return sub[1] + sub[2] + redactValue(sub[3])
	})
	return secretAssign.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretAssign.FindStringSubmatch(m)
		if isCodeValue(sub[2]) {
			return m
		}
		return sub[1] + redactValue(sub[2])
	})
}

// minSecretPrefix is the shortest prefix of a configured secret that redact
// treats as that secret cut short.
const minSecretPrefix = 8

// redactCutSecret replaces a run of minSecretPrefix or more characters that is
// a proper prefix of lit (the secret after a client cut it short) when it
// ends the text or comes before whitespace or an ellipsis.
func redactCutSecret(s, lit string) string {
	if len(lit) <= minSecretPrefix {
		return s
	}
	head := lit[:minSecretPrefix]
	var out strings.Builder
	written, from := 0, 0
	for from < len(s) {
		j := strings.Index(s[from:], head)
		if j < 0 {
			break
		}
		start := from + j
		end := start + minSecretPrefix
		for end < len(s) && end-start < len(lit) && s[end] == lit[end-start] {
			end++
		}
		rest := s[end:]
		boundary := rest == "" || strings.HasPrefix(rest, "…") || strings.HasPrefix(rest, "...")
		if !boundary {
			r, _ := utf8.DecodeRuneInString(rest)
			boundary = unicode.IsSpace(r)
		}
		if boundary && end-start < len(lit) {
			out.WriteString(s[written:start])
			out.WriteString(redacted)
			written = end
			from = end
		} else {
			from = start + 1
		}
	}
	out.WriteString(s[written:])
	return out.String()
}

// breakFences puts a zero-width space after every backtick of a run of three
// or more, so no run can close a code fence.
func breakFences(s string) string {
	return fenceRun.ReplaceAllStringFunc(s, func(m string) string {
		return strings.ReplaceAll(m, "`", "`​")
	})
}

// redact removes secrets from s before it is posted: the configured tokens
// and extra values (the caller's own bearer token) whole or cut short, common
// token shapes, and the values of secret-named settings. It runs before any
// cut to Slack's limits.
func (b *Bridge) redact(s string, extra []string) string {
	literals := append([]string{b.cfg.BotToken, b.cfg.AppToken}, b.cfg.Secrets...)
	literals = append(literals, extra...)
	for _, lit := range literals {
		if len(strings.TrimSpace(lit)) >= 6 {
			s = strings.ReplaceAll(s, lit, redacted)
		}
	}
	for _, lit := range literals {
		if strings.TrimSpace(lit) != "" {
			s = redactCutSecret(s, lit)
		}
	}
	for _, re := range secretShapes {
		s = re.ReplaceAllString(s, redacted)
	}
	return redactAssignments(s)
}

// promptText is the text helper of one render.
type promptText struct {
	b     *Bridge
	extra []string
}

// mrkdwn is s redacted, then escaped for mrkdwn and cut to n characters of
// the escaped text, at an escape boundary.
func (t promptText) mrkdwn(s string, n int) string {
	return escapeCut(t.b.redact(s, t.extra), n)
}

// escapeCut escapes raw for mrkdwn, stopping before the character that would
// take the result past n characters, so an escape is never split.
func escapeCut(raw string, n int) string {
	var sb strings.Builder
	width := 0
	for _, r := range raw {
		w := escWidth(r)
		if width+w > n {
			break
		}
		sb.WriteString(escape(string(r)))
		width += w
	}
	return sb.String()
}

// plain is s redacted, cut to n characters, for a plain_text field (which
// Slack shows literally, so it is not escaped).
func (t promptText) plain(s string, n int) string {
	return cutWithEllipsis(strings.TrimSpace(t.b.redact(s, t.extra)), n)
}

// cutWithEllipsis cuts s to at most n characters, ending in "…" when cut.
func cutWithEllipsis(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// escWidth is how many characters r takes once escaped.
func escWidth(r rune) int {
	switch r {
	case '&':
		return 5
	case '<', '>':
		return 4
	}
	return 1
}

// chunkEscaped escapes raw and splits it into pieces of at most limit
// characters, at most maxChunks of them. notShown counts the raw characters
// past the last piece.
func chunkEscaped(raw string, limit, maxChunks int) (chunks []string, notShown int) {
	runes := []rune(raw)
	if maxChunks <= 0 {
		return nil, len(runes)
	}
	var cur strings.Builder
	width := 0
	for i, r := range runes {
		w := escWidth(r)
		if width+w > limit {
			chunks = append(chunks, cur.String())
			cur.Reset()
			width = 0
			if len(chunks) == maxChunks {
				return chunks, len(runes) - i
			}
		}
		cur.WriteString(escape(string(r)))
		width += w
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	return chunks, 0
}

func mrkdwnBlock(text string) map[string]any {
	return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}
}

func contextBlock(text string) map[string]any {
	return map[string]any{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": text}}}
}

func plainObj(text string) map[string]any {
	return map[string]any{"type": "plain_text", "text": text}
}

// hasPromptAction reports whether a block_actions payload clicked a prompt
// button.
func hasPromptAction(p interactivePayload) bool {
	for _, a := range p.Actions {
		if strings.HasPrefix(a.ActionID, promptActionPrefix) {
			return true
		}
	}
	return false
}

func questionBlock(i int) string { return "q" + strconv.Itoa(i) }

// renderPrompt is the message text (the plain fallback) and blocks (a JSON
// array) for the prompt's state now.
func (b *Bridge) renderPrompt(s promptSnap) (text, blocks string) {
	t := promptText{b: b, extra: s.req.Secrets}
	title := t.plain(s.req.Title, maxHeaderRunes)
	if title == "" {
		title = "Claude needs you"
	}
	var out []map[string]any
	out = append(out, map[string]any{"type": "header", "text": plainObj(title)})
	if agent := strings.TrimSpace(s.req.Agent); agent != "" {
		out = append(out, contextBlock("Subagent: *"+t.mrkdwn(agent, 100)+"*"))
	}
	// out holds the header and agent line; one more block, the actions or the
	// status, closes the message.
	out = append(out, b.detailBlocks(t, s.req, maxMessageBlocks-len(out)-1)...)
	status := b.promptStatus(t, s)
	if s.state == promptOpenState {
		out = append(out, promptActions(t, s.id, s.req))
		text = "Claude needs you: " + escape(title)
	} else {
		out = append(out, contextBlock(status))
		text = escape(title) + " · " + status
	}
	data, err := json.Marshal(out)
	if err != nil {
		return text, "[]"
	}
	return text, string(data)
}

// detailBlocks are the sections that show the prompt's detail (or its
// questions), at most room blocks in all; one of them stays free for the
// notice that says how much of the detail is not shown.
func (b *Bridge) detailBlocks(t promptText, req agentbus.PromptRequest, room int) []map[string]any {
	var out []map[string]any
	if req.Kind == agentbus.PromptKindQuestion {
		for _, q := range req.Questions {
			head := ""
			if h := strings.TrimSpace(q.Header); h != "" {
				head = "*" + t.mrkdwn(h, 100) + "*\n"
			}
			head += t.mrkdwn(q.Text, maxSectionRunes-len([]rune(head)))
			out = append(out, mrkdwnBlock(head))
			if lines := optionLines(t, q); lines != "" {
				out = append(out, mrkdwnBlock(lines))
			}
		}
	}
	detail := b.redact(req.Detail, req.Secrets)
	if strings.TrimSpace(detail) == "" {
		return out
	}
	fenced := req.Kind != agentbus.PromptKindPlan
	if fenced {
		// A fence inside the text would end ours early.
		detail = breakFences(detail)
	}
	budget := min(maxDetailBlocks, room-len(out)-1)
	chunks, notShown := chunkEscaped(detail, maxSectionRunes, budget)
	for _, c := range chunks {
		if fenced {
			c = "```\n" + c + "\n```"
		}
		out = append(out, mrkdwnBlock(c))
	}
	if notShown > 0 {
		out = append(out, contextBlock(fmt.Sprintf("%d more characters not shown here; the rest is in the terminal.", notShown)))
	}
	return out
}

// optionLines lists a question's options that have descriptions, as many
// whole lines as fit one section.
func optionLines(t promptText, q agentbus.PromptQuestion) string {
	var lines []string
	described := false
	for _, o := range q.Options {
		line := "• *" + t.mrkdwn(o.Label, maxButtonRunes) + "*"
		if d := strings.TrimSpace(o.Description); d != "" {
			line += ": " + t.mrkdwn(d, 300)
			described = true
		}
		lines = append(lines, line)
	}
	if !described {
		return ""
	}
	var sb strings.Builder
	width := 0
	for i, line := range lines {
		w := len([]rune(line)) + 1
		if width+w > maxSectionRunes-2 {
			sb.WriteString("\n…")
			break
		}
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(line)
		width += w
	}
	return sb.String()
}

func promptButton(label, id, what, style string) map[string]any {
	btn := map[string]any{"type": "button", "text": plainObj(label), "action_id": promptActionPrefix + id + ":" + what, "value": what}
	if style != "" {
		btn["style"] = style
	}
	return btn
}

// promptActions is the actions block of an open prompt.
func promptActions(t promptText, id string, req agentbus.PromptRequest) map[string]any {
	var els []map[string]any
	switch req.Kind {
	case agentbus.PromptKindPermission:
		els = append(els, promptButton("Allow", id, "allow", "primary"))
		if req.Always {
			els = append(els, promptButton("Always allow", id, "always", ""))
		}
		els = append(els, promptButton("Deny", id, "deny", "danger"), promptButton("Deny with reason…", id, "deny_reason", ""))
	case agentbus.PromptKindPlan:
		els = append(els,
			promptButton("Approve, auto-accept edits", id, "approve_accept_edits", "primary"),
			promptButton("Approve", id, "approve", ""),
			promptButton("Keep planning…", id, "keep_planning", ""))
	case agentbus.PromptKindDenial:
		els = append(els, promptButton("Allow once", id, "allow_once", "primary"), promptButton("Deny", id, "deny", "danger"))
	case agentbus.PromptKindQuestion:
		if singleChoice(req) {
			for i, o := range req.Questions[0].Options {
				btn := promptButton(t.plain(o.Label, maxButtonRunes), id, "opt:"+strconv.Itoa(i), "")
				btn["value"] = strconv.Itoa(i)
				els = append(els, btn)
			}
		}
		els = append(els, promptButton("Answer…", id, "answer", ""))
	}
	return map[string]any{"type": "actions", "block_id": "prompt_actions", "elements": els}
}

// choiceLabel is a choice as a button names it.
func choiceLabel(choice string) string {
	switch choice {
	case "allow":
		return "Allow"
	case "always":
		return "Always allow"
	case "deny":
		return "Deny"
	case "approve_accept_edits":
		return "Approve, auto-accept edits"
	case "approve":
		return "Approve"
	case "keep_planning":
		return "Keep planning"
	case "allow_once":
		return "Allow once"
	}
	return "Answer"
}

// choiceAllows reports whether choice lets the call go ahead.
func choiceAllows(choice string) bool {
	switch choice {
	case "deny", "keep_planning":
		return false
	}
	return true
}

// allowedVerb is what an allowing choice reads as once the call ran.
func allowedVerb(choice string) string {
	switch choice {
	case "always":
		return "Always allowed"
	case "approve_accept_edits":
		return "Approved, auto-accept edits"
	case "approve":
		return "Approved"
	case "allow_once":
		return "Allowed once"
	case "submit":
		return "Answered"
	}
	return "Allowed"
}

// promptStatus is the status line (mrkdwn) of a prompt, shown in place of the
// buttons once it is answered or closed.
func (b *Bridge) promptStatus(t promptText, s promptSnap) string {
	if s.state == promptOpenState {
		return ""
	}
	a := s.answer
	if a == nil {
		switch s.outcome {
		case agentbus.OutcomeRan:
			return "Answered in the terminal · ran"
		case agentbus.OutcomeDenied:
			return "Denied in the terminal"
		case agentbus.OutcomeAnswered:
			return "Answered in the terminal"
		case agentbus.OutcomeCancelled:
			return "Cancelled"
		case agentbus.OutcomeEnded:
			return "Session ended"
		case outcomeExpired:
			return "Expired"
		}
		return "Closed"
	}
	by := t.mrkdwn(a.By, 60)
	reason := ""
	if r := strings.TrimSpace(a.Reason); r != "" {
		reason = ": " + t.mrkdwn(r, 500)
	}
	chose := by + " chose " + choiceLabel(a.Choice)
	if a.Choice == "submit" {
		chose = by + " answered"
	}
	allows := choiceAllows(a.Choice)
	if s.state == promptAnsweredState {
		return chose + reason + " · waiting for the session"
	}
	if s.note == noteTerminal {
		// The call was settled at the keyboard: never claim the Slack answer
		// decided it.
		return chose + reason + " · " + s.outcome + " (answered in the terminal first)"
	}
	switch s.outcome {
	case agentbus.OutcomeRan:
		if allows {
			return allowedVerb(a.Choice) + " by " + by + " · ran"
		}
		return chose + reason + " · ran (answered in the terminal first)"
	case agentbus.OutcomeDenied:
		if !allows {
			if a.Choice == "keep_planning" {
				return "Kept planning by " + by + reason
			}
			return "Denied by " + by + reason
		}
		return chose + " · denied (answered in the terminal first)"
	case agentbus.OutcomeAnswered:
		if a.Choice == "submit" {
			return "Answered by " + by
		}
		return chose + reason + " · answered"
	case agentbus.OutcomeCancelled:
		return chose + reason + " · cancelled"
	case agentbus.OutcomeEnded:
		return chose + reason + " · session ended"
	case outcomeExpired:
		return chose + reason + " · expired"
	}
	return chose + reason
}

// promptModal is the modal view (JSON) for a prompt's "Deny with reason…",
// "Keep planning…" or "Answer…" button.
func (b *Bridge) promptModal(s promptSnap) (string, error) {
	t := promptText{b: b, extra: s.req.Secrets}
	title, submit := "Answer", "Send"
	var blocks []map[string]any
	switch s.req.Kind {
	case agentbus.PromptKindPermission:
		title = "Deny with reason"
		blocks = append(blocks, reasonInput("Reason", "Tell the agent why (optional)"))
	case agentbus.PromptKindPlan:
		title = "Keep planning"
		blocks = append(blocks, reasonInput("Feedback", "What should change? (optional)"))
	case agentbus.PromptKindQuestion:
		for i, q := range s.req.Questions {
			blocks = append(blocks, questionInputs(t, i, q)...)
		}
	}
	view := map[string]any{
		"type":             "modal",
		"callback_id":      promptModalID,
		"private_metadata": s.id,
		"title":            plainObj(title),
		"submit":           plainObj(submit),
		"close":            plainObj("Cancel"),
		"blocks":           blocks,
	}
	data, err := json.Marshal(view)
	return string(data), err
}

func reasonInput(label, placeholder string) map[string]any {
	return map[string]any{
		"type": "input", "block_id": "reason", "optional": true, "label": plainObj(label),
		"element": map[string]any{"type": "plain_text_input", "action_id": "reason", "multiline": true, "max_length": maxReasonInput, "placeholder": plainObj(placeholder)},
	}
}

// questionInputs are the input blocks of question i: the question's own
// input and, for a choice, an optional "Other" text input.
func questionInputs(t promptText, i int, q agentbus.PromptQuestion) []map[string]any {
	id := questionBlock(i)
	label := t.plain(q.Text, maxLabelRunes)
	if h := strings.TrimSpace(q.Header); h != "" {
		label = t.plain(h, 40) + ": " + label
		label = cutWithEllipsis(label, maxLabelRunes)
	}
	if u := strings.TrimSpace(q.Unit); u != "" && q.Kind == agentbus.QuestionNumber {
		label = cutWithEllipsis(label+" ("+t.plain(u, 20)+")", maxLabelRunes)
	}
	switch q.Kind {
	case agentbus.QuestionChoice:
		options := make([]map[string]any, 0, len(q.Options))
		for n, o := range q.Options {
			opt := map[string]any{"text": plainObj(t.plain(o.Label, maxButtonRunes)), "value": strconv.Itoa(n)}
			if d := strings.TrimSpace(o.Description); d != "" {
				opt["description"] = plainObj(t.plain(d, maxButtonRunes))
			}
			options = append(options, opt)
		}
		elemType := "radio_buttons"
		if q.Multi {
			elemType = "checkboxes"
		}
		return []map[string]any{
			{"type": "input", "block_id": id, "optional": true, "label": plainObj(label),
				"element": map[string]any{"type": elemType, "action_id": id, "options": options}},
			{"type": "input", "block_id": id + otherSuffix, "optional": true, "label": plainObj("Other"),
				"element": map[string]any{"type": "plain_text_input", "action_id": id + otherSuffix, "max_length": maxReasonRunes}},
		}
	case agentbus.QuestionNumber:
		elem := map[string]any{"type": "number_input", "action_id": id, "is_decimal_allowed": decimalsAllowed(q)}
		if q.Min != nil {
			elem["min_value"] = formatNumber(*q.Min)
		}
		if q.Max != nil {
			elem["max_value"] = formatNumber(*q.Max)
		}
		return []map[string]any{{"type": "input", "block_id": id, "label": plainObj(label), "element": elem}}
	}
	return []map[string]any{{"type": "input", "block_id": id, "label": plainObj(label),
		"element": map[string]any{"type": "plain_text_input", "action_id": id, "multiline": true, "max_length": maxReasonRunes}}}
}

// decimalsAllowed is false only for a number question with a whole step of at
// least 1 and whole bounds; any other step, or none, may take a decimal.
func decimalsAllowed(q agentbus.PromptQuestion) bool {
	if q.Step == nil || *q.Step < 1 || !wholeNumber(*q.Step) {
		return true
	}
	return (q.Min != nil && !wholeNumber(*q.Min)) || (q.Max != nil && !wholeNumber(*q.Max))
}

func wholeNumber(f float64) bool {
	return f == math.Trunc(f) && !math.IsInf(f, 0)
}

// formatNumber is f as plain decimal digits: no exponent and no float noise
// (0.30000000000000004 reads 0.3).
func formatNumber(f float64) string {
	r, err := strconv.ParseFloat(strconv.FormatFloat(f, 'g', 12, 64), 64)
	if err != nil {
		r = f
	}
	if r == 0 {
		r = 0 // no negative zero
	}
	return strconv.FormatFloat(r, 'f', -1, 64)
}
