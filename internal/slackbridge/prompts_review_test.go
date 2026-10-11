package slackbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	"github.com/tidwall/gjson"
)

func TestPromptRedactionOfSecretNamedSettings(t *testing.T) {
	b, _, _ := newTestBridge(t)
	ghs := "ghs_" + strings.Repeat("A1b2", 8)
	cases := map[string]string{
		"ANTHROPIC_AUTH_TOKEN=x":            "ANTHROPIC_AUTH_TOKEN=[redacted]",
		"GITHUB_TOKEN=" + ghs:               "GITHUB_TOKEN=[redacted]",
		"DB_PASSWORD=hunter2":               "DB_PASSWORD=[redacted]",
		"AWS_SECRET_ACCESS_KEY=x":           "AWS_SECRET_ACCESS_KEY=[redacted]",
		`{"password": "x"}`:                 `{"password": "[redacted]"}`,
		`{"apiKey":"abc def"}`:              `{"apiKey":"[redacted]"}`,
		"--password x":                      "--password [redacted]",
		"--token=x":                         "--token=[redacted]",
		"mysql --password 'a b' -h db":      "mysql --password '[redacted]' -h db",
		"login --token --verbose":           "login --token --verbose",
		"token=eyJhbGciOi.eyJzdWIiOi.s":     "token=[redacted]",
		"export CLIENT_SECRET=abc123":       "export CLIENT_SECRET=[redacted]",
		ghs:                                 "[redacted]",
		"ghr_" + strings.Repeat("z", 20):    "[redacted]",
		"token = tokens[i]":                 "token = tokens[i]",
		"secret = getSecret()":              "secret = getSecret()",
		"password = cfg.password":           "password = cfg.password",
		"pip install sk-learn":              "pip install sk-learn",
		"sk-ant-" + strings.Repeat("a", 20): "[redacted]",
		"sk-" + strings.Repeat("b", 20):     "[redacted]",
		"tokenizer=fast":                    "tokenizer=fast",
	}
	for in, want := range cases {
		if got := b.redact(in, nil); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPromptRedactsASecretCutShort(t *testing.T) {
	b, _, _ := newTestBridge(t)
	b.cfg.Secrets = []string{"my-proxy-api-key-0123456789"}
	cases := map[string]string{
		"key my-proxy-api-key-0123456789 end": "key [redacted] end",
		"key my-proxy-api-key-01…":            "key [redacted]…",
		"key my-proxy-api-key-01...":          "key [redacted]...",
		"key my-proxy-":                       "key [redacted]",
		"my-proxy-api more":                   "[redacted] more",
		"key my-prox":                         "key my-prox",
		"my-proxy-api-keyX":                   "my-proxy-api-keyX",
	}
	for in, want := range cases {
		if got := b.redact(in, nil); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
	// The bot token and the caller's bearer too.
	if got := b.redact("tok xoxb-te", nil); strings.Contains(got, "xoxb-te") {
		t.Errorf("a cut bot token survived: %q", got)
	}
	if got := b.redact("tok callers-bearer-1234…", []string{"callers-bearer-1234567"}); got != "tok [redacted]…" {
		t.Errorf("a cut bearer = %q", got)
	}
}

func TestPromptRedactionRunsBeforeTheCut(t *testing.T) {
	b, _, _ := newTestBridge(t)
	b.cfg.Secrets = []string{"my-proxy-api-key-0123456789"}
	tx := promptText{b: b}
	// The secret sits across the cut: it must be gone, not half shown.
	got := tx.mrkdwn("aaaa my-proxy-api-key-0123456789 tail", 12)
	if strings.Contains(got, "my-p") {
		t.Fatalf("part of a secret survived the cut: %q", got)
	}
}

func TestPromptBreaksEveryRunOfBackticks(t *testing.T) {
	e := newPromptEnv(t)
	detail := "a" + strings.Repeat("`", 5) + "b" + strings.Repeat("`", 6) + "c```d``e"
	e.open(permReq(sidA, "fence", map[string]any{"detail": detail}))
	text := gjson.Parse(e.promptPost(0).Form.Get("blocks")).Get("#(type==\"section\").text.text").String()
	body := strings.TrimSuffix(strings.TrimPrefix(text, "```\n"), "\n```")
	if regexp.MustCompile("`{3,}").MatchString(body) {
		t.Fatalf("a run of 3 or more backticks survived: %q", body)
	}
	if strings.Count(text, "```") != 2 {
		t.Fatalf("fence broken: %q", text)
	}
}

func TestPromptNeverExceedsFiftyBlocks(t *testing.T) {
	e := newPromptEnv(t)
	var qs []map[string]any
	for i := 0; i < 4; i++ {
		var opts []map[string]any
		for n := 0; n < 10; n++ {
			opts = append(opts, map[string]any{"id": "o" + strconv.Itoa(n), "label": "option " + strconv.Itoa(n), "description": "described " + strconv.Itoa(n)})
		}
		qs = append(qs, map[string]any{"id": "q" + strconv.Itoa(i), "text": "Question " + strconv.Itoa(i), "header": "H" + strconv.Itoa(i), "kind": "choice", "options": opts})
	}
	body := jsonOf(t, map[string]any{
		"session": sidA, "key": "big", "kind": "question", "title": "t", "agent": "sub", "questions": qs,
		"detail": strings.Repeat("x", 400000),
	})
	id := e.open(body)
	blocks := gjson.Parse(e.promptPost(0).Form.Get("blocks")).Array()
	if len(blocks) > maxMessageBlocks {
		t.Fatalf("%d blocks", len(blocks))
	}
	e.resolve(sidA, id, "answered")
	drainJobs(t, e.b)
	if n := len(gjson.Parse(e.updates()[0].Form.Get("blocks")).Array()); n > maxMessageBlocks {
		t.Fatalf("the redraw has %d blocks", n)
	}
}

func TestPromptNumberInputs(t *testing.T) {
	b, _, _ := newTestBridge(t)
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name     string
		q        agentbus.PromptQuestion
		decimals bool
		min, max string
	}{
		{"whole", agentbus.PromptQuestion{Min: f(0), Max: f(10), Step: f(1)}, false, "0", "10"},
		{"fractional min", agentbus.PromptQuestion{Min: f(0.5), Max: f(10), Step: f(1)}, true, "0.5", "10"},
		{"fractional max", agentbus.PromptQuestion{Min: f(0), Max: f(10.5), Step: f(1)}, true, "0", "10.5"},
		{"fractional step", agentbus.PromptQuestion{Min: f(0), Max: f(10), Step: f(0.25)}, true, "0", "10"},
		{"no step", agentbus.PromptQuestion{Min: f(0), Max: f(10)}, true, "0", "10"},
		{"float noise", agentbus.PromptQuestion{Min: f(0.1 + 0.2), Max: f(1e21), Step: f(1)}, true, "0.3", "1000000000000000000000"},
		{"tiny", agentbus.PromptQuestion{Min: f(1e-7), Step: f(1)}, true, "0.0000001", ""},
	}
	for _, c := range cases {
		c.q.ID, c.q.Text, c.q.Kind = "n", "How many?", agentbus.QuestionNumber
		view, err := b.promptModal(promptSnap{id: "p_x", req: agentbus.PromptRequest{Kind: agentbus.PromptKindQuestion, Questions: []agentbus.PromptQuestion{c.q}}})
		if err != nil {
			t.Fatal(err)
		}
		el := gjson.Get(view, "blocks.0.element")
		if el.Get("is_decimal_allowed").Bool() != c.decimals || el.Get("min_value").String() != c.min || el.Get("max_value").String() != c.max {
			t.Errorf("%s: element = %s", c.name, el.Raw)
		}
		if strings.ContainsAny(el.Get("min_value").String()+el.Get("max_value").String(), "eE") {
			t.Errorf("%s: exponent in %s", c.name, el.Raw)
		}
	}
}

func TestPromptCutNeverSplitsAnEscape(t *testing.T) {
	b, _, _ := newTestBridge(t)
	tx := promptText{b: b}
	got := tx.mrkdwn(strings.Repeat("&", 50), 12)
	if got != "&amp;&amp;" {
		t.Fatalf("cut = %q", got)
	}
	got = tx.mrkdwn("<<<<", 9)
	if got != "&lt;&lt;" {
		t.Fatalf("cut = %q", got)
	}
	// A question's header and text share one section and one budget.
	e := newPromptEnv(t)
	long := strings.Repeat("&", 3000)
	e.open(jsonOf(t, map[string]any{"session": sidA, "key": "q", "kind": "question", "title": "t", "questions": []map[string]any{
		{"id": "q1", "text": long, "header": "<h>", "kind": "text"},
	}}))
	sec := gjson.Parse(e.promptPost(0).Form.Get("blocks")).Get("#(type==\"section\").text.text").String()
	if len([]rune(sec)) > maxSectionRunes || strings.HasSuffix(sec, "&") || strings.HasSuffix(sec, "&am") {
		t.Fatalf("section of %d characters ends %q", len([]rune(sec)), sec[len(sec)-6:])
	}
}

func TestPromptTerminalNoteIsShownWithTheSlackAnswer(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k", nil))
	e.click("UALEX", id, "deny")
	w := e.do(http.MethodPost, "/prompt/resolve", jsonOf(t, map[string]string{"session": sidA, "id": id, "outcome": "ran", "note": "terminal"}))
	if w.Code != http.StatusOK {
		t.Fatalf("resolve = %d %s", w.Code, w.Body)
	}
	drainJobs(t, e.b)
	if got := e.lastStatus(); got != "alex chose Deny · ran (answered in the terminal first)" {
		t.Fatalf("status = %q", got)
	}
	// An allow answered in the terminal is not credited to Slack either.
	id2 := e.open(permReq(sidA, "k2", nil))
	e.click("UALEX", id2, "allow")
	e.do(http.MethodPost, "/prompt/resolve", jsonOf(t, map[string]string{"session": sidA, "id": id2, "outcome": "denied", "note": "terminal"}))
	drainJobs(t, e.b)
	if got := e.lastStatus(); got != "alex chose Allow · denied (answered in the terminal first)" {
		t.Fatalf("status = %q", got)
	}
}

// seedPromptThread makes sid's prompt thread known, so a prompt post is the
// only chat.postMessage.
func seedPromptThread(e *promptEnv, sid string) {
	e.b.promptMu.Lock()
	defer e.b.promptMu.Unlock()
	e.b.initPromptsLocked()
	e.b.promptThreads[sid] = promptThread{channel: ownerDM, ts: "1700000000.100001"}
}

func TestPromptPostIsNotRetriedAfterAnAmbiguousFailure(t *testing.T) {
	e := newPromptEnv(t)
	seedPromptThread(e, sidA)
	e.f.setHangup("chat.postMessage", true)
	w := e.do(http.MethodPost, "/prompt", permReq(sidA, "k", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("open = %d %s", w.Code, w.Body)
	}
	if got := len(e.f.callsTo("chat.postMessage")); got != 1 {
		t.Fatalf("post attempts = %d, want 1 (a retry could double-post)", got)
	}
	// The prompt is closed: the key opens afresh.
	e.f.setHangup("chat.postMessage", false)
	e.open(permReq(sidA, "k", nil))
}

func TestPromptPostIsRetriedWhenSlackRefusedIt(t *testing.T) {
	e := newPromptEnv(t)
	seedPromptThread(e, sidA)
	e.f.setFail("chat.postMessage", "internal_error")
	w := e.do(http.MethodPost, "/prompt", permReq(sidA, "k", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("open = %d %s", w.Code, w.Body)
	}
	if got := len(e.f.callsTo("chat.postMessage")); got != 2 {
		t.Fatalf("post attempts = %d, want 2", got)
	}
}

func TestPromptsNeedTheOwnersDMToOpen(t *testing.T) {
	b, f, _ := newTestBridge(t)
	b.connected.Store(true)
	f.setFail("conversations.open", "ratelimited")
	if b.PromptsAvailable() {
		t.Fatal("available although the owner's DM can't be opened")
	}
	f.setFail("conversations.open", "")
	if !b.PromptsAvailable() {
		t.Fatal("not available once the DM opens")
	}
	// The opened channel is cached: a later failure costs nothing.
	f.setFail("conversations.open", "ratelimited")
	if !b.PromptsAvailable() {
		t.Fatal("a cached DM was opened again")
	}
}

func TestPromptClickOnAMessageFromBeforeARestart(t *testing.T) {
	e := newPromptEnv(t)
	payload := map[string]any{
		"type": "block_actions", "trigger_id": "T1",
		"user":      map[string]any{"id": "UALEX"},
		"container": map[string]any{"channel_id": ownerDM, "message_ts": "1700000000.100009"},
		"message": map[string]any{"ts": "1700000000.100009", "text": "Claude needs you: Bash", "blocks": []map[string]any{
			{"type": "header", "text": map[string]any{"type": "plain_text", "text": "Bash wants to run"}},
			{"type": "actions", "block_id": "prompt_actions", "elements": []map[string]any{{"type": "button"}}},
		}},
		"actions": []map[string]any{{"action_id": "prompt:p_00000000000000000000000000000000:allow", "value": "allow"}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if ack := e.b.handleInteractive(data); ack != nil {
		t.Fatalf("ack = %v", ack)
	}
	drainJobs(t, e.b)
	eph := e.f.callsTo("chat.postEphemeral")
	if len(eph) != 1 || eph[0].Form.Get("text") != "This prompt is from before a restart and can't be answered here; answer in the terminal." {
		t.Fatalf("ephemerals = %+v", eph)
	}
	ups := e.updates()
	if len(ups) != 1 || ups[0].Form.Get("ts") != "1700000000.100009" {
		t.Fatalf("edits = %+v", ups)
	}
	blocks := gjson.Parse(ups[0].Form.Get("blocks")).Array()
	if len(blocks) != 2 || blocks[0].Get("type").String() != "header" || blocks[1].Get("type").String() != "context" {
		t.Fatalf("blocks = %s", ups[0].Form.Get("blocks"))
	}
	if strings.Contains(ups[0].Form.Get("blocks"), `"actions"`) {
		t.Fatal("the buttons are still there")
	}
}

func TestPromptEditsRunOnceTheBridgeIsStopped(t *testing.T) {
	// The redraw loop ends with its context, so Stop does not hang on it.
	e := newPromptEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.b.runRedraws(ctx)
	}()
	cancel()
	<-done
}
