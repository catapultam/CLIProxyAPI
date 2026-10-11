package slackbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	"github.com/tidwall/gjson"
)

const ownerDM = "DUALEX"

type promptEnv struct {
	t   *testing.T
	b   *Bridge
	f   *fakeSlack
	bus *agentbus.Store
	r   *gin.Engine
}

// newPromptEnv is a test bridge with prompts routes mounted and the socket
// marked connected. Waits last 30 ms unless a test says otherwise.
func newPromptEnv(t *testing.T) *promptEnv {
	t.Helper()
	b, f, bus := newTestBridge(t)
	return mountPromptEnv(t, b, f, bus)
}

func mountPromptEnv(t *testing.T, b *Bridge, f *fakeSlack, bus *agentbus.Store) *promptEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	bus.Register(r.Group("/v1/agentbus"))
	b.connected.Store(true)
	b.promptWaitFor = 30 * time.Millisecond
	return &promptEnv{t: t, b: b, f: f, bus: bus, r: r}
}

func (e *promptEnv) do(method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/v1/agentbus"+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer proxy-key-123456")
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// permReq is a permission prompt request for sid.
func permReq(sid, key string, extra map[string]any) string {
	m := map[string]any{"session": sid, "key": key, "kind": "permission", "title": "Bash wants to run", "detail": "ls -la", "always": true}
	for k, v := range extra {
		m[k] = v
	}
	data, _ := json.Marshal(m)
	return string(data)
}

// open posts a prompt and returns its id.
func (e *promptEnv) open(body string) string {
	e.t.Helper()
	w := e.do(http.MethodPost, "/prompt", body)
	if w.Code != http.StatusOK {
		e.t.Fatalf("POST /prompt = %d %s", w.Code, w.Body)
	}
	id := gjson.Get(w.Body.String(), "id").String()
	if len(id) != 34 || !strings.HasPrefix(id, "p_") {
		e.t.Fatalf("id = %q", id)
	}
	return id
}

func (e *promptEnv) wait(sid, id string) *httptest.ResponseRecorder {
	return e.do(http.MethodGet, "/prompt/wait?session="+sid+"&id="+id, "")
}

func (e *promptEnv) resolve(sid, id, outcome string) *httptest.ResponseRecorder {
	return e.do(http.MethodPost, "/prompt/resolve", jsonOf(e.t, map[string]string{"session": sid, "id": id, "outcome": outcome}))
}

// click delivers a block_actions click by user on prompt id's button what.
func (e *promptEnv) click(user, id, what string) map[string]any {
	p := map[string]any{
		"type": "block_actions", "trigger_id": "TRIG1",
		"user":      map[string]any{"id": user},
		"container": map[string]any{"channel_id": ownerDM, "message_ts": "1700000000.100002"},
		"actions":   []map[string]any{{"action_id": "prompt:" + id + ":" + what, "value": what}},
	}
	return e.b.handleInteractive(json.RawMessage(jsonOf(e.t, p)))
}

// submit delivers the prompt modal's submission with values.
func (e *promptEnv) submit(user, id string, values map[string]any) map[string]any {
	p := map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": user},
		"view": map[string]any{"callback_id": promptModalID, "private_metadata": id, "state": map[string]any{"values": values}},
	}
	return e.b.handleInteractive(json.RawMessage(jsonOf(e.t, p)))
}

func (e *promptEnv) answerJSON(sid, id string) gjson.Result {
	e.t.Helper()
	w := e.wait(sid, id)
	if w.Code != http.StatusOK {
		e.t.Fatalf("wait = %d %s", w.Code, w.Body)
	}
	return gjson.Get(w.Body.String(), "answer")
}

// updates are the chat.update calls so far.
func (e *promptEnv) updates() []fakeCall { return e.f.callsTo("chat.update") }

// lastStatus is the status line of the newest chat.update.
func (e *promptEnv) lastStatus() string {
	e.t.Helper()
	ups := e.updates()
	if len(ups) == 0 {
		e.t.Fatal("no chat.update")
	}
	blocks := gjson.Parse(ups[len(ups)-1].Form.Get("blocks")).Array()
	last := blocks[len(blocks)-1]
	if last.Get("type").String() != "context" {
		e.t.Fatalf("last block = %s", last.Raw)
	}
	return last.Get("elements.0.text").String()
}

// promptBlocks are the blocks of the n-th (0-based) prompt post.
func (e *promptEnv) promptPost(n int) fakeCall {
	e.t.Helper()
	var posts []fakeCall
	for _, c := range e.f.callsTo("chat.postMessage") {
		if c.Form.Get("blocks") != "" {
			posts = append(posts, c)
		}
	}
	if n >= len(posts) {
		e.t.Fatalf("only %d block posts", len(posts))
	}
	return posts[n]
}

func buttonTexts(blocks string) []string {
	var out []string
	for _, blk := range gjson.Parse(blocks).Array() {
		if blk.Get("type").String() == "actions" {
			for _, el := range blk.Get("elements").Array() {
				out = append(out, el.Get("text.text").String())
			}
		}
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPromptOpenPostsToOwnerDMUnderOneHeader(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k1", nil))
	e.open(permReq(sidA, "k2", nil))
	posts := e.f.callsTo("chat.postMessage")
	if len(posts) != 3 {
		t.Fatalf("posts = %d, want a header and two prompts", len(posts))
	}
	hdr := posts[0].Form
	if hdr.Get("channel") != ownerDM || hdr.Get("thread_ts") != "" || hdr.Get("blocks") != "" ||
		hdr.Get("text") != "*flyer* · pc/flyer-aaaaaa · pc" {
		t.Fatalf("header = %v", hdr)
	}
	hdrTS := "1700000000.100001"
	for _, p := range posts[1:] {
		if p.Form.Get("channel") != ownerDM || p.Form.Get("thread_ts") != hdrTS {
			t.Fatalf("prompt post = %v", p.Form)
		}
	}
	first := posts[1].Form
	if first.Get("text") != "Claude needs you: Bash wants to run" {
		t.Fatalf("fallback text = %q", first.Get("text"))
	}
	if got := buttonTexts(first.Get("blocks")); !sameStrings(got, []string{"Allow", "Always allow", "Deny", "Deny with reason…"}) {
		t.Fatalf("buttons = %v", got)
	}
	if !strings.Contains(first.Get("blocks"), "prompt:"+id+":allow") {
		t.Fatalf("no action id in %s", first.Get("blocks"))
	}
	for _, p := range e.f.callsTo("chat.postMessage") {
		if p.Form.Get("channel") == "CAGENTS" {
			t.Fatalf("a prompt reached the channel: %v", p.Form)
		}
	}
	// A reply in the header thread reaches the session, as it does today.
	ev := dmMsg("UALEX", "use the other flag", "1700000000.200001", hdrTS)
	e.b.handleEvent("EvPromptThread", ev)
	drainJobs(t, e.b)
	if m := claimOne(t, e.bus, sidA); m.Body != "use the other flag" || !m.FromUser {
		t.Fatalf("thread reply = %+v", m)
	}
}

func TestPromptPostsInHomeThreadWhenItIsTheOwnerDM(t *testing.T) {
	e := newPromptEnv(t)
	e.b.state.moveThread(sidA, ownerDM, "1700000000.555555", "")
	e.open(permReq(sidA, "k1", nil))
	posts := e.f.callsTo("chat.postMessage")
	if len(posts) != 1 || posts[0].Form.Get("channel") != ownerDM || posts[0].Form.Get("thread_ts") != "1700000000.555555" {
		t.Fatalf("posts = %+v", posts)
	}
}

func TestPromptOpenIsIdempotent(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "same", nil))
	posts := len(e.f.callsTo("chat.postMessage"))
	if again := e.open(permReq(sidA, "same", nil)); again != id {
		t.Fatalf("repeat id = %s, want %s", again, id)
	}
	if got := len(e.f.callsTo("chat.postMessage")); got != posts {
		t.Fatalf("a repeat posted %d more", got-posts)
	}
	// Another session with the same key is its own prompt.
	if other := e.open(permReq(sidB, "same", nil)); other == id {
		t.Fatal("sessions share a prompt")
	}
}

func TestPromptConcurrentOpensShareOnePost(t *testing.T) {
	e := newPromptEnv(t)
	entered, release := e.f.holdPosts()
	ids := make([]string, 2)
	var wg sync.WaitGroup
	start := func(i int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := e.do(http.MethodPost, "/prompt", permReq(sidA, "race", nil))
			if w.Code != http.StatusOK {
				t.Errorf("open %d = %d %s", i, w.Code, w.Body)
			}
			ids[i] = gjson.Get(w.Body.String(), "id").String()
		}()
	}
	start(0)
	<-entered // the first open is reserved and posting
	start(1)
	release()
	wg.Wait()
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("ids = %v", ids)
	}
	blockPosts := 0
	for _, c := range e.f.callsTo("chat.postMessage") {
		if c.Form.Get("blocks") != "" {
			blockPosts++
		}
	}
	if blockPosts != 1 {
		t.Fatalf("prompt posts = %d", blockPosts)
	}
}

func TestPromptOpenFailureRetriesOnceThen502(t *testing.T) {
	e := newPromptEnv(t)
	e.f.setFail("chat.postMessage", "channel_not_found")
	w := e.do(http.MethodPost, "/prompt", permReq(sidA, "k", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("open = %d %s", w.Code, w.Body)
	}
	if got := len(e.f.callsTo("chat.postMessage")); got != 2 {
		t.Fatalf("post attempts = %d, want 2", got)
	}
	// The failed prompt is gone: the same key opens afresh once Slack is back.
	e.f.setFail("chat.postMessage", "")
	e.open(permReq(sidA, "k", nil))
}

func TestPromptOpenErrors(t *testing.T) {
	e := newPromptEnv(t)
	for name, body := range map[string]string{
		"not json":         `{`,
		"no session":       `{"key":"k","kind":"permission","title":"t"}`,
		"no key":           `{"session":"` + sidA + `","kind":"permission","title":"t"}`,
		"bad kind":         `{"session":"` + sidA + `","key":"k","kind":"nope","title":"t"}`,
		"question w/o qs":  `{"session":"` + sidA + `","key":"k","kind":"question","title":"t"}`,
		"too many options": jsonOf(t, map[string]any{"session": sidA, "key": "k", "kind": "question", "title": "t", "questions": []map[string]any{{"id": "q1", "text": "x", "kind": "choice", "options": manyOptions(11)}}}),
		"bad qkind":        jsonOf(t, map[string]any{"session": sidA, "key": "k", "kind": "question", "title": "t", "questions": []map[string]any{{"id": "q1", "text": "x", "kind": "dial"}}}),
	} {
		if w := e.do(http.MethodPost, "/prompt", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s", name, w.Code, w.Body)
		}
	}
	if w := e.do(http.MethodPost, "/prompt", permReq("ffffffff-0000-0000-0000-000000000000", "k", nil)); w.Code != http.StatusNotFound {
		t.Errorf("unknown session = %d", w.Code)
	}
	e.b.connected.Store(false)
	if w := e.do(http.MethodPost, "/prompt", permReq(sidA, "k", nil)); w.Code != http.StatusNotFound {
		t.Errorf("not connected = %d", w.Code)
	}
	e.b.connected.Store(true)
	if w := e.do(http.MethodGet, "/prompt/wait?session="+sidA, ""); w.Code != http.StatusBadRequest {
		t.Errorf("wait w/o id = %d", w.Code)
	}
	if w := e.do(http.MethodPost, "/prompt/resolve", `{"session":"`+sidA+`","id":"p_x","outcome":"weird"}`); w.Code != http.StatusBadRequest {
		t.Errorf("resolve bad outcome = %d", w.Code)
	}
}

func manyOptions(n int) []map[string]any {
	var out []map[string]any
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{"id": "o" + strconv.Itoa(i), "label": "opt " + strconv.Itoa(i)})
	}
	return out
}

func TestPromptHelloReportsPrompts(t *testing.T) {
	e := newPromptEnv(t)
	hello := func() gjson.Result {
		w := e.do(http.MethodPost, "/hello", `{"session":"`+sidA+`","machine":"pc","mod":true}`)
		if w.Code != http.StatusOK {
			t.Fatalf("hello = %d %s", w.Code, w.Body)
		}
		return gjson.Parse(w.Body.String())
	}
	if got := hello(); !got.Get("prompts").Bool() || got.Get("address").String() == "" {
		t.Fatalf("connected hello = %s", got.Raw)
	}
	e.b.connected.Store(false)
	if got := hello(); got.Get("prompts").Type != gjson.False {
		t.Fatalf("disconnected hello = %s", got.Raw)
	}
	e.bus.SetBridge(nil)
	if got := hello(); got.Get("prompts").Type != gjson.False {
		t.Fatalf("no-bridge hello = %s", got.Raw)
	}
}

func TestPromptSocketConnectedFlag(t *testing.T) {
	b, f, _ := newTestBridge(t)
	if b.PromptsAvailable() {
		t.Fatal("available before the socket said hello")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.runSocket(ctx)
	}()
	f.push(`{"type":"interactive","envelope_id":"env-flag","payload":{"type":"noop"}}`)
	waitAck(t, f, "env-flag")
	if !b.PromptsAvailable() {
		t.Fatal("not available while connected")
	}
	cancel()
	<-done
	if b.PromptsAvailable() {
		t.Fatal("available after the socket closed")
	}
}

func TestPromptButtonAnswersAndEveryWaiterGetsIt(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k", nil))
	if w := e.wait(sidA, id); w.Code != http.StatusNoContent {
		t.Fatalf("open wait = %d %s", w.Code, w.Body)
	}
	e.b.promptWaitFor = 5 * time.Second
	got := make(chan *httptest.ResponseRecorder, 1)
	go func() { got <- e.wait(sidA, id) }()
	if ack := e.click("UALEX", id, "always"); ack != nil {
		t.Fatalf("ack = %v", ack)
	}
	select {
	case w := <-got:
		if w.Code != http.StatusOK || gjson.Get(w.Body.String(), "answer.choice").String() != "always" || gjson.Get(w.Body.String(), "answer.by").String() != "alex" {
			t.Fatalf("waiter = %d %s", w.Code, w.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter not woken by the click")
	}
	if a := e.answerJSON(sidA, id); a.Get("choice").String() != "always" {
		t.Fatalf("second waiter = %s", a.Raw)
	}
	// Another session cannot read it.
	if w := e.wait(sidB, id); w.Code != http.StatusNotFound {
		t.Fatalf("foreign wait = %d", w.Code)
	}
	if w := e.resolve(sidB, id, "ran"); w.Code != http.StatusNotFound {
		t.Fatalf("foreign resolve = %d", w.Code)
	}
	if w := e.wait(sidA, "p_00000000000000000000000000000000"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d", w.Code)
	}
}

func TestPromptAlwaysButtonOnlyWhenOffered(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k", map[string]any{"always": false}))
	if got := buttonTexts(e.promptPost(0).Form.Get("blocks")); !sameStrings(got, []string{"Allow", "Deny", "Deny with reason…"}) {
		t.Fatalf("buttons = %v", got)
	}
	e.click("UALEX", id, "always")
	if w := e.wait(sidA, id); w.Code != http.StatusNoContent {
		t.Fatalf("an unoffered always answered: %d %s", w.Code, w.Body)
	}
}

func TestPromptPlanAndDenialButtons(t *testing.T) {
	e := newPromptEnv(t)
	plan := e.open(jsonOf(t, map[string]any{"session": sidA, "key": "p", "kind": "plan", "title": "Plan ready", "detail": "# Plan\n1. do it"}))
	denial := e.open(jsonOf(t, map[string]any{"session": sidA, "key": "d", "kind": "denial", "title": "Auto mode denied rm"}))
	if got := buttonTexts(e.promptPost(0).Form.Get("blocks")); !sameStrings(got, []string{"Approve, auto-accept edits", "Approve", "Keep planning…"}) {
		t.Fatalf("plan buttons = %v", got)
	}
	if got := buttonTexts(e.promptPost(1).Form.Get("blocks")); !sameStrings(got, []string{"Allow once", "Deny"}) {
		t.Fatalf("denial buttons = %v", got)
	}
	// Plan text is plain mrkdwn, not fenced; permission detail is.
	if strings.Contains(e.promptPost(0).Form.Get("blocks"), "```") {
		t.Fatal("plan detail was fenced")
	}
	e.click("UALEX", plan, "approve_accept_edits")
	e.click("UALEX", denial, "allow_once")
	if e.answerJSON(sidA, plan).Get("choice").String() != "approve_accept_edits" || e.answerJSON(sidA, denial).Get("choice").String() != "allow_once" {
		t.Fatal("wrong choices")
	}
	// A button the kind does not offer answers nothing.
	other := e.open(jsonOf(t, map[string]any{"session": sidA, "key": "d2", "kind": "denial", "title": "x"}))
	e.click("UALEX", other, "allow")
	if w := e.wait(sidA, other); w.Code != http.StatusNoContent {
		t.Fatalf("allow on a denial = %d", w.Code)
	}
}

func TestPromptDenyWithReasonAndKeepPlanningModals(t *testing.T) {
	e := newPromptEnv(t)
	perm := e.open(permReq(sidA, "k", nil))
	e.click("UALEX", perm, "deny_reason")
	e.b.viewsWG.Wait()
	opens := e.f.callsTo("views.open")
	if len(opens) != 1 || opens[0].Form.Get("trigger_id") != "TRIG1" {
		t.Fatalf("views.open = %+v", opens)
	}
	view := opens[0].Form.Get("view")
	if gjson.Get(view, "callback_id").String() != promptModalID || gjson.Get(view, "private_metadata").String() != perm ||
		gjson.Get(view, "blocks.0.element.type").String() != "plain_text_input" || !gjson.Get(view, "blocks.0.optional").Bool() {
		t.Fatalf("view = %s", view)
	}
	if ack := e.submit("UALEX", perm, map[string]any{"reason": map[string]any{"reason": map[string]any{"type": "plain_text_input", "value": "  too risky  "}}}); ack != nil {
		t.Fatalf("submit ack = %v", ack)
	}
	a := e.answerJSON(sidA, perm)
	if a.Get("choice").String() != "deny" || a.Get("reason").String() != "too risky" || a.Get("by").String() != "alex" {
		t.Fatalf("answer = %s", a.Raw)
	}

	plan := e.open(jsonOf(t, map[string]any{"session": sidA, "key": "p", "kind": "plan", "title": "Plan", "detail": "x"}))
	e.click("UALEX", plan, "keep_planning")
	e.b.viewsWG.Wait()
	if got := gjson.Get(e.f.callsTo("views.open")[1].Form.Get("view"), "title.text").String(); got != "Keep planning" {
		t.Fatalf("modal title = %q", got)
	}
	e.submit("UALEX", plan, map[string]any{"reason": map[string]any{"reason": map[string]any{"value": "skip the migration"}}})
	if a := e.answerJSON(sidA, plan); a.Get("choice").String() != "keep_planning" || a.Get("reason").String() != "skip the migration" {
		t.Fatalf("plan answer = %s", a.Raw)
	}
	// Empty feedback is fine: the mod says "Keep planning.".
	plan2 := e.open(jsonOf(t, map[string]any{"session": sidA, "key": "p2", "kind": "plan", "title": "Plan", "detail": "x"}))
	e.submit("UALEX", plan2, map[string]any{})
	if a := e.answerJSON(sidA, plan2); a.Get("choice").String() != "keep_planning" || a.Get("reason").String() != "" {
		t.Fatalf("empty feedback answer = %s", a.Raw)
	}
}

func questionReq(sid, key string) string {
	min, max := 1.0, 10.0
	return jsonOf2(map[string]any{"session": sid, "key": key, "kind": "question", "title": "Claude asks", "questions": []map[string]any{
		{"id": "q1", "text": "Which library?", "header": "Library", "kind": "choice", "options": []map[string]any{
			{"id": "o1", "label": "dayjs", "description": "small"}, {"id": "o2", "label": "moment"}, {"id": "o3", "label": "luxon"}}},
		{"id": "q2", "text": "Which features?", "kind": "choice", "multi": true, "options": []map[string]any{
			{"id": "a", "label": "tz"}, {"id": "b", "label": "i18n"}, {"id": "c", "label": "plugins"}}},
		{"id": "q3", "text": "Anything else?", "kind": "text"},
		{"id": "q4", "text": "How many?", "kind": "number", "min": min, "max": max, "unit": "px"},
	}})
}

func jsonOf2(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func radio(v string) map[string]any {
	return map[string]any{"type": "radio_buttons", "selected_option": map[string]any{"value": v}}
}

func checks(vs ...string) map[string]any {
	var sel []map[string]any
	for _, v := range vs {
		sel = append(sel, map[string]any{"value": v})
	}
	return map[string]any{"type": "checkboxes", "selected_options": sel}
}

func textVal(v string) map[string]any { return map[string]any{"type": "plain_text_input", "value": v} }

// qvalues is a submitted question modal: block id to its value.
func qvalues(m map[string]map[string]any) map[string]any {
	out := map[string]any{}
	for block, v := range m {
		out[block] = map[string]any{block: v}
	}
	return out
}

func TestPromptQuestionModalAnswers(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(questionReq(sidA, "q"))
	// More than one question: only an Answer button.
	if got := buttonTexts(e.promptPost(0).Form.Get("blocks")); !sameStrings(got, []string{"Answer…"}) {
		t.Fatalf("buttons = %v", got)
	}
	e.click("UALEX", id, "answer")
	e.b.viewsWG.Wait()
	view := e.f.callsTo("views.open")[0].Form.Get("view")
	want := map[string]string{
		"q0": "radio_buttons", "q0_other": "plain_text_input", "q1": "checkboxes", "q1_other": "plain_text_input",
		"q2": "plain_text_input", "q3": "number_input",
	}
	for _, blk := range gjson.Get(view, "blocks").Array() {
		if got := blk.Get("element.type").String(); got != want[blk.Get("block_id").String()] {
			t.Errorf("block %s = %s", blk.Get("block_id").String(), got)
		}
		delete(want, blk.Get("block_id").String())
	}
	if len(want) != 0 {
		t.Fatalf("missing blocks %v in %s", want, view)
	}
	if gjson.Get(view, "blocks.#(block_id==\"q3\").element.min_value").String() != "1" || gjson.Get(view, "blocks.#(block_id==\"q3\").element.max_value").String() != "10" {
		t.Fatalf("number bounds missing: %s", view)
	}

	// Invalid: nothing picked, empty text, bad number. The prompt stays open.
	ack := e.submit("UALEX", id, qvalues(map[string]map[string]any{"q2": textVal(" "), "q3": textVal("abc")}))
	errs, _ := ack["errors"].(map[string]string)
	if ack["response_action"] != "errors" || errs["q0"] == "" || errs["q1"] == "" || errs["q2"] == "" || errs["q3"] == "" {
		t.Fatalf("invalid ack = %+v", ack)
	}
	if ack = e.submit("UALEX", id, qvalues(map[string]map[string]any{"q0": radio("0"), "q1": checks("0"), "q2": textVal("x"), "q3": textVal("11")})); ack == nil || ack["errors"].(map[string]string)["q3"] == "" {
		t.Fatalf("out of range ack = %+v", ack)
	}
	if w := e.wait(sidA, id); w.Code != http.StatusNoContent {
		t.Fatalf("answered by an invalid submit: %d", w.Code)
	}

	ack = e.submit("UALEX", id, qvalues(map[string]map[string]any{
		"q0": radio("0"), "q1": checks("0", "2"), "q2": textVal(" ship it "), "q3": textVal("7"),
	}))
	if ack != nil {
		t.Fatalf("valid ack = %+v", ack)
	}
	a := e.answerJSON(sidA, id)
	if a.Get("choice").String() != "submit" || a.Get("by").String() != "alex" ||
		a.Get("answers.q1").String() != "dayjs" || a.Get("answers.q2").Raw != `["tz","plugins"]` ||
		a.Get("answers.q3").String() != "ship it" || a.Get("answers.q4").Raw != "7" {
		t.Fatalf("answer = %s", a.Raw)
	}
}

func TestPromptQuestionOtherText(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(questionReq(sidA, "q"))
	vals := qvalues(map[string]map[string]any{
		"q0": radio("1"), "q0_other": textVal("date-fns"), "q1": checks("1"), "q1_other": textVal("workers"),
		"q2": textVal("-"), "q3": textVal("2.5"),
	})
	if ack := e.submit("UALEX", id, vals); ack != nil {
		t.Fatalf("ack = %+v", ack)
	}
	a := e.answerJSON(sidA, id)
	// Other text beats a pick for a single choice, and joins a multi.
	if a.Get("answers.q1").String() != "date-fns" || a.Get("answers.q2").Raw != `["i18n","workers"]` || a.Get("answers.q4").Raw != "2.5" {
		t.Fatalf("answer = %s", a.Raw)
	}
	// Other alone is enough for a multi with nothing ticked.
	id2 := e.open(questionReq(sidA, "q-b"))
	e.submit("UALEX", id2, qvalues(map[string]map[string]any{"q0": radio("2"), "q1_other": textVal("only this"), "q2": textVal("-"), "q3": textVal("3")}))
	if a := e.answerJSON(sidA, id2); a.Get("answers.q2").Raw != `["only this"]` || a.Get("answers.q1").String() != "luxon" {
		t.Fatalf("answer = %s", a.Raw)
	}
}

func TestPromptSingleChoiceQuestionHasOptionButtons(t *testing.T) {
	e := newPromptEnv(t)
	long := strings.Repeat("x", 100)
	id := e.open(jsonOf(t, map[string]any{"session": sidA, "key": "s", "kind": "question", "title": "Pick", "questions": []map[string]any{
		{"id": "q1", "text": "Which?", "kind": "choice", "options": []map[string]any{{"id": "a", "label": "dayjs"}, {"id": "b", "label": long}}},
	}}))
	blocks := e.promptPost(0).Form.Get("blocks")
	got := buttonTexts(blocks)
	if len(got) != 3 || got[0] != "dayjs" || got[2] != "Answer…" {
		t.Fatalf("buttons = %v", got)
	}
	if r := []rune(got[1]); len(r) != 75 || string(r[74]) != "…" {
		t.Fatalf("cut label = %q (%d)", got[1], len(r))
	}
	e.click("UALEX", id, "opt:0")
	a := e.answerJSON(sidA, id)
	if a.Get("choice").String() != "submit" || a.Get("answers.q1").String() != "dayjs" {
		t.Fatalf("answer = %s", a.Raw)
	}
	// The answer carries the label as sent, not the cut one.
	id2 := e.open(jsonOf(t, map[string]any{"session": sidA, "key": "s2", "kind": "question", "title": "Pick", "questions": []map[string]any{
		{"id": "q1", "text": "Which?", "kind": "choice", "options": []map[string]any{{"id": "b", "label": long}}},
	}}))
	e.click("UALEX", id2, "opt:0")
	if a := e.answerJSON(sidA, id2); a.Get("answers.q1").String() != long {
		t.Fatalf("answer = %s", a.Raw)
	}
	// Answer… opens the modal for free text.
	id3 := e.open(jsonOf(t, map[string]any{"session": sidA, "key": "s3", "kind": "question", "title": "Pick", "questions": []map[string]any{
		{"id": "q1", "text": "Which?", "kind": "choice", "options": []map[string]any{{"id": "a", "label": "dayjs"}}},
	}}))
	e.click("UALEX", id3, "answer")
	e.b.viewsWG.Wait()
	if len(e.f.callsTo("views.open")) != 1 {
		t.Fatal("Answer… did not open a modal")
	}
	e.click("UALEX", id3, "opt:5") // out of range
	if w := e.wait(sidA, id3); w.Code != http.StatusNoContent {
		t.Fatalf("bad option answered: %d", w.Code)
	}
}

func TestPromptOnlyOwnersAnswer(t *testing.T) {
	e := newPromptEnv(t)
	if _, _, err := e.b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	id := e.open(permReq(sidA, "k", nil))
	calls := len(e.f.methods())
	for _, user := range []string{"UJANE", "UBOB"} {
		if ack := e.click(user, id, "allow"); ack != nil {
			t.Fatalf("ack for %s = %v", user, ack)
		}
	}
	if got := len(e.f.methods()); got != calls {
		t.Fatalf("a click made %d Slack calls before its job ran", got-calls)
	}
	drainJobs(t, e.b)
	eph := e.f.callsTo("chat.postEphemeral")
	if len(eph) != 2 || eph[0].Form.Get("text") != "Only an owner can answer this." || eph[0].Form.Get("user") != "UJANE" || eph[1].Form.Get("user") != "UBOB" || eph[0].Form.Get("channel") != ownerDM {
		t.Fatalf("ephemerals = %+v", eph)
	}
	if w := e.wait(sidA, id); w.Code != http.StatusNoContent {
		t.Fatalf("a non-owner answered: %d %s", w.Code, w.Body)
	}
	if len(e.updates()) != 0 {
		t.Fatal("the message was edited")
	}
	// Nor can they open a modal or submit one.
	e.click("UJANE", id, "deny_reason")
	e.b.viewsWG.Wait()
	if len(e.f.callsTo("views.open")) != 0 {
		t.Fatal("a non-owner got a modal")
	}
	ack := e.submit("UJANE", id, map[string]any{"reason": map[string]any{"reason": map[string]any{"value": "no"}}})
	if ack == nil || ack["response_action"] != "errors" {
		t.Fatalf("submit ack = %+v", ack)
	}
	if w := e.wait(sidA, id); w.Code != http.StatusNoContent {
		t.Fatalf("a non-owner's modal answered: %d", w.Code)
	}
}

func TestPromptLateClickChangesNothing(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k", nil))
	e.click("UALEX", id, "allow")
	e.click("UALEX", id, "deny")
	drainJobs(t, e.b)
	if a := e.answerJSON(sidA, id); a.Get("choice").String() != "allow" {
		t.Fatalf("answer = %s", a.Raw)
	}
	eph := e.f.callsTo("chat.postEphemeral")
	if len(eph) != 1 || eph[0].Form.Get("text") != "Already answered." {
		t.Fatalf("ephemerals = %+v", eph)
	}
	// Also after the prompt closed, and for a modal submit.
	e.resolve(sidA, id, "ran")
	e.click("UALEX", id, "deny")
	// A late modal submit closes the modal (no errors map naming a block the
	// modal may not have) and says so privately.
	if ack := e.submit("UALEX", id, map[string]any{}); ack != nil {
		t.Fatalf("late submit ack = %+v", ack)
	}
	drainJobs(t, e.b)
	eph = e.f.callsTo("chat.postEphemeral")
	if len(eph) != 3 || eph[2].Form.Get("text") != "This prompt is no longer open." {
		t.Fatalf("ephemerals = %+v", eph)
	}
	if a := e.answerJSON(sidA, id); a.Get("choice").String() != "allow" {
		t.Fatalf("answer changed to %s", a.Raw)
	}
}

func TestPromptResolveOutcomesAndEditedText(t *testing.T) {
	cases := []struct {
		name    string
		click   string
		outcome string
		want    string
	}{
		{"allowed and ran", "allow", "ran", "Allowed by alex · ran"},
		{"always and ran", "always", "ran", "Always allowed by alex · ran"},
		{"denied", "deny", "denied", "Denied by alex"},
		{"deny but ran", "deny", "ran", "alex chose Deny · ran (answered in the terminal first)"},
		{"allow but denied", "allow", "denied", "alex chose Allow · denied (answered in the terminal first)"},
		{"allow then cancelled", "allow", "cancelled", "alex chose Allow · cancelled"},
		{"allow then ended", "allow", "ended", "alex chose Allow · session ended"},
		{"terminal ran", "", "ran", "Answered in the terminal · ran"},
		{"terminal denied", "", "denied", "Denied in the terminal"},
		{"cancelled", "", "cancelled", "Cancelled"},
		{"ended", "", "ended", "Session ended"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPromptEnv(t)
			id := e.open(permReq(sidA, "k", nil))
			if tc.click != "" {
				e.click("UALEX", id, tc.click)
			}
			if w := e.resolve(sidA, id, tc.outcome); w.Code != http.StatusOK {
				t.Fatalf("resolve = %d %s", w.Code, w.Body)
			}
			// Resolving again is fine and changes nothing.
			if w := e.resolve(sidA, id, "cancelled"); w.Code != http.StatusOK {
				t.Fatalf("second resolve = %d", w.Code)
			}
			drainJobs(t, e.b)
			if got := e.lastStatus(); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
			if strings.Contains(e.updates()[len(e.updates())-1].Form.Get("blocks"), `"actions"`) {
				t.Fatal("the buttons are still there")
			}
			if tc.click == "" {
				if w := e.wait(sidA, id); w.Code != http.StatusGone {
					t.Fatalf("wait = %d", w.Code)
				}
			} else if a := e.answerJSON(sidA, id); a.Get("choice").String() != tc.click {
				t.Fatalf("answer = %s", a.Raw)
			}
		})
	}
}

func TestPromptAnsweredStateAndReasonAreShown(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k", nil))
	e.click("UALEX", id, "allow")
	drainJobs(t, e.b)
	if got := e.lastStatus(); got != "alex chose Allow · waiting for the session" {
		t.Fatalf("status = %q", got)
	}
	id2 := e.open(permReq(sidA, "k2", nil))
	e.submit("UALEX", id2, map[string]any{"reason": map[string]any{"reason": map[string]any{"value": "no <!channel>"}}})
	e.resolve(sidA, id2, "denied")
	drainJobs(t, e.b)
	if got := e.lastStatus(); got != "Denied by alex: no &lt;!channel&gt;" {
		t.Fatalf("status = %q", got)
	}
}

func TestPromptRedrawsAreCoalescedAndRenderTheLastState(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k", nil))
	e.click("UALEX", id, "allow")
	e.resolve(sidA, id, "ran")
	if got := len(e.updates()); got != 0 {
		t.Fatalf("%d edits before the job ran", got)
	}
	if got := len(e.b.commands); got != 0 {
		t.Fatalf("%d jobs on the command queue, want 0 (redraws have their own path)", got)
	}
	if got := len(e.b.promptDirty); got != 1 {
		t.Fatalf("%d prompts marked dirty, want 1", got)
	}
	drainJobs(t, e.b)
	if got := len(e.updates()); got != 1 {
		t.Fatalf("edits = %d, want 1", got)
	}
	up := e.updates()[0].Form
	if up.Get("channel") != ownerDM || up.Get("ts") != "1700000000.100002" || e.lastStatus() != "Allowed by alex · ran" {
		t.Fatalf("edit = %v", up)
	}
	// Further transitions after the job ran draw again.
	e.resolve(sidA, id, "cancelled") // already closed: no change, no edit
	drainJobs(t, e.b)
	if got := len(e.updates()); got != 1 {
		t.Fatalf("a no-op resolve edited: %d", got)
	}
}

func TestPromptFailedEditIsRetriedWithTheLatestState(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k", nil))
	e.f.setFail("chat.update", "internal_error")
	e.click("UALEX", id, "allow")
	drainJobs(t, e.b)
	if got := len(e.updates()); got != 1 {
		t.Fatalf("edits = %d", got)
	}
	e.resolve(sidA, id, "ran") // the state moves on while the edit is failing
	e.f.setFail("chat.update", "")
	e.b.sweepPrompts()
	drainJobs(t, e.b)
	if got := e.lastStatus(); got != "Allowed by alex · ran" {
		t.Fatalf("status = %q", got)
	}
	n := len(e.updates())
	e.b.sweepPrompts()
	drainJobs(t, e.b)
	if len(e.updates()) != n {
		t.Fatal("a drawn prompt was redrawn again")
	}
}

func TestPromptEditDoesNotUseTheCommandQueue(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k", nil))
	for len(e.b.commands) < cap(e.b.commands) {
		e.b.commands <- func(context.Context) error { return nil }
	}
	e.click("UALEX", id, "allow")
	e.resolve(sidA, id, "ran")
	// The edit lands although the command queue is full, and leaves it full.
	e.b.drainRedraws(context.Background())
	if got := e.lastStatus(); got != "Allowed by alex · ran" {
		t.Fatalf("status = %q", got)
	}
	if len(e.b.commands) != cap(e.b.commands) {
		t.Fatal("a redraw took a command queue slot")
	}
}

func TestPromptRedrawLoopRunsWithTheBridge(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k", nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.b.runRedraws(ctx)
	}()
	e.click("UALEX", id, "allow")
	deadline := time.After(5 * time.Second)
	for len(e.updates()) == 0 {
		select {
		case <-deadline:
			t.Fatal("the redraw loop never edited the message")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestPromptPermanentEditFailureGivesUp(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k", nil))
	e.f.setFail("chat.update", "message_not_found")
	e.click("UALEX", id, "allow")
	drainJobs(t, e.b)
	e.b.sweepPrompts()
	drainJobs(t, e.b)
	if got := len(e.updates()); got != 1 {
		t.Fatalf("edits = %d, want 1", got)
	}
}

func TestPermanentEditErrors(t *testing.T) {
	for _, code := range []string{"msg_too_long", "invalid_arguments", "invalid_blocks", "message_not_found", "cant_update_message", "channel_not_found"} {
		if !permanentEditError(&apiError{method: "chat.update", code: code}) {
			t.Errorf("%s is not permanent", code)
		}
	}
	for _, code := range []string{"internal_error", "ratelimited"} {
		if permanentEditError(&apiError{method: "chat.update", code: code}) {
			t.Errorf("%s is permanent", code)
		}
	}
}

func TestPromptClickMakesNoSlackCallBeforeItsAck(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k", nil))
	calls := len(e.f.methods())
	if ack := e.click("UALEX", id, "allow"); ack != nil {
		t.Fatalf("ack = %v", ack)
	}
	if got := len(e.f.methods()); got != calls {
		t.Fatalf("click made %d Slack calls", got-calls)
	}
	e.resolve(sidA, id, "ran")
	id2 := e.open(questionReq(sidA, "q"))
	calls = len(e.f.methods())
	if ack := e.submit("UALEX", id2, qvalues(map[string]map[string]any{"q0": radio("0"), "q1": checks("0"), "q2": textVal("a"), "q3": textVal("2")})); ack != nil {
		t.Fatalf("ack = %v", ack)
	}
	if got := len(e.f.methods()); got != calls {
		t.Fatalf("submit made %d Slack calls", got-calls)
	}
}

func TestPromptEscapingRedactionAndSplitting(t *testing.T) {
	e := newPromptEnv(t)
	secret := "echo xoxb-test and Bearer abcdefghijklmno and sk-ant-api03-ABCDEFGH12345 and proxy-key-123456 and AKIAABCDEFGHIJKLMNOP password=hunter2 end"
	e.open(permReq(sidA, "esc", map[string]any{
		"title":  "Run <!channel> & <@UALEX>",
		"detail": "<!here> <@UALEX> &amp; ``` " + secret,
		"agent":  "sub<!channel>",
	}))
	post := e.promptPost(0).Form
	blocks := post.Get("blocks")
	for _, bad := range []string{"<!channel>", "<!here>", "<@UALEX>", "xoxb-test", "abcdefghijklmno", "ABCDEFGH12345", "proxy-key-123456", "AKIAABCDEFGHIJKLMNOP", "hunter2"} {
		if strings.Contains(blocks, bad) || strings.Contains(post.Get("text"), bad) {
			t.Errorf("post still contains %q: %s", bad, blocks)
		}
	}
	detail := gjson.Parse(blocks).Get("#(type==\"section\").text.text").String()
	if !strings.Contains(detail, "&lt;!here&gt; &lt;@UALEX&gt; &amp;amp;") || !strings.Contains(detail, "[redacted]") || !strings.Contains(detail, "password=[redacted]") {
		t.Errorf("detail not escaped and redacted: %s", detail)
	}
	if post.Get("text") != "Claude needs you: Run &lt;!channel&gt; &amp; &lt;@UALEX&gt;" {
		t.Errorf("fallback = %q", post.Get("text"))
	}
	// The fence cannot be closed early from inside.
	if strings.Count(detail, "```") != 2 {
		t.Errorf("fence broken: %q", detail)
	}
}

func TestPromptSplitsLongDetailAndSaysWhatIsCut(t *testing.T) {
	e := newPromptEnv(t)
	e.open(permReq(sidA, "long", map[string]any{"detail": strings.Repeat("a", 7000)}))
	var sections []string
	for _, blk := range gjson.Parse(e.promptPost(0).Form.Get("blocks")).Array() {
		if blk.Get("type").String() == "section" {
			sections = append(sections, blk.Get("text.text").String())
		}
	}
	if len(sections) != 3 {
		t.Fatalf("sections = %d", len(sections))
	}
	for _, s := range sections {
		if !strings.HasPrefix(s, "```\n") || !strings.HasSuffix(s, "\n```") || len([]rune(s)) > 3000 {
			t.Fatalf("section = %.40q (%d)", s, len([]rune(s)))
		}
	}

	e2 := newPromptEnv(t)
	e2.open(permReq(sidA, "huge", map[string]any{"detail": strings.Repeat("b", 200000)}))
	blocks := gjson.Parse(e2.promptPost(0).Form.Get("blocks")).Array()
	n := 0
	for _, blk := range blocks {
		if blk.Get("type").String() == "section" {
			n++
		}
	}
	notice := ""
	for _, blk := range blocks {
		if blk.Get("type").String() == "context" && strings.Contains(blk.Raw, "not shown") {
			notice = blk.Get("elements.0.text").String()
		}
	}
	wantCut := 200000 - 40*2900
	if n != 40 || !strings.HasPrefix(notice, strconv.Itoa(wantCut)+" more characters not shown") || len(blocks) > 50 {
		t.Fatalf("sections %d notice %q blocks %d", n, notice, len(blocks))
	}
}

func TestPromptEscapedChunksNeverSplitAnEntity(t *testing.T) {
	chunks, cut := chunkEscaped(strings.Repeat("<&>", 2000), 2900, 40)
	if cut != 0 || len(chunks) < 2 {
		t.Fatalf("chunks = %d cut = %d", len(chunks), cut)
	}
	joined := ""
	for _, c := range chunks {
		if len([]rune(c)) > 2900 {
			t.Fatalf("chunk of %d characters", len([]rune(c)))
		}
		if strings.HasSuffix(c, "&") || strings.HasSuffix(c, "&l") || strings.HasSuffix(c, "&am") {
			t.Fatalf("a chunk ends inside an entity: ...%q", c[len(c)-4:])
		}
		joined += c
	}
	if joined != strings.Repeat("&lt;&amp;&gt;", 2000) {
		t.Fatal("chunks do not add up to the escaped text")
	}
}

func TestPromptExpiryAndRetention(t *testing.T) {
	e := newPromptEnv(t)
	clock := newTestClock()
	e.b.state.now = clock.now
	id := e.open(permReq(sidA, "k", nil))
	clock.advance(promptTTL + time.Minute)
	if w := e.wait(sidA, id); w.Code != http.StatusGone {
		t.Fatalf("expired wait = %d", w.Code)
	}
	drainJobs(t, e.b)
	if got := e.lastStatus(); got != "Expired" {
		t.Fatalf("status = %q", got)
	}

	// A resolved prompt stays findable by id for promptRetain: its waiter
	// still gets the answer, then 410, then 404 once forgotten.
	id2 := e.open(permReq(sidA, "k2", nil))
	e.click("UALEX", id2, "allow")
	e.resolve(sidA, id2, "ran")
	clock.advance(promptRetain - time.Minute)
	if a := e.answerJSON(sidA, id2); a.Get("choice").String() != "allow" {
		t.Fatalf("answer inside the window = %s", a.Raw)
	}
	clock.advance(2 * time.Minute)
	if w := e.wait(sidA, id2); w.Code != http.StatusGone {
		t.Fatalf("wait after the window = %d", w.Code)
	}
	clock.advance(promptForget + time.Minute)
	e.b.sweepPrompts()
	if w := e.wait(sidA, id2); w.Code != http.StatusNotFound {
		t.Fatalf("forgotten prompt = %d", w.Code)
	}
}

func TestPromptSweepExpiresOpenPrompts(t *testing.T) {
	e := newPromptEnv(t)
	clock := newTestClock()
	e.b.state.now = clock.now
	id := e.open(permReq(sidA, "k", nil))
	clock.advance(promptTTL + time.Minute)
	e.b.sweepPrompts()
	e.b.promptMu.Lock()
	state := e.b.promptByID[id].state
	e.b.promptMu.Unlock()
	if state != promptClosedState {
		t.Fatal("the sweep left an expired prompt open")
	}
	// A repeat open of the key gets a new prompt.
	if again := e.open(permReq(sidA, "k", nil)); again == id {
		t.Fatal("an expired prompt was reused")
	}
}

func TestPromptReopenAfterResolveMakesANewPromptWithoutAReplayedAnswer(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "same", nil))
	e.click("UALEX", id, "allow")
	// Answered but not yet resolved: still the same prompt.
	if again := e.open(permReq(sidA, "same", nil)); again != id {
		t.Fatalf("an answered, unresolved prompt got a new id %s", again)
	}
	e.resolve(sidA, id, "ran")
	posts := len(e.f.callsTo("chat.postMessage"))
	fresh := e.open(permReq(sidA, "same", nil))
	if fresh == id {
		t.Fatal("a resolved prompt was reused")
	}
	if got := len(e.f.callsTo("chat.postMessage")); got != posts+1 {
		t.Fatalf("the new prompt made %d posts, want 1", got-posts)
	}
	// The new prompt is open: no replayed answer.
	if w := e.wait(sidA, fresh); w.Code != http.StatusNoContent {
		t.Fatalf("wait on the new prompt = %d %s", w.Code, w.Body)
	}
	// A waiter that still holds the old id gets its answer.
	if a := e.answerJSON(sidA, id); a.Get("choice").String() != "allow" {
		t.Fatalf("old id answer = %s", a.Raw)
	}
}

func TestPromptAnswerWhoseResolveWasLostIsNotReplayedToANewCall(t *testing.T) {
	e := newPromptEnv(t)
	clock := newTestClock()
	e.b.state.now = clock.now
	id := e.open(permReq(sidA, "same", nil))
	e.click("UALEX", id, "allow")
	// The session never resolves it. Within promptRetain a re-entry still
	// shares the prompt; past it a new call gets a new, unanswered one.
	clock.advance(promptRetain - time.Minute)
	if again := e.open(permReq(sidA, "same", nil)); again != id {
		t.Fatalf("a recent answered prompt got a new id %s", again)
	}
	clock.advance(2 * time.Minute)
	fresh := e.open(permReq(sidA, "same", nil))
	if fresh == id {
		t.Fatal("an answer past promptRetain was handed to a new call")
	}
	if w := e.wait(sidA, fresh); w.Code != http.StatusNoContent {
		t.Fatalf("wait on the new prompt = %d %s", w.Code, w.Body)
	}
}

func TestPromptClosedRecordsAreCapped(t *testing.T) {
	e := newPromptEnv(t)
	first := e.open(permReq(sidA, "first", nil))
	e.resolve(sidA, first, "ran")
	for i := 0; i < maxClosedPrompts; i++ {
		id := e.open(permReq(sidA, "k"+strconv.Itoa(i), nil))
		e.resolve(sidA, id, "ran")
	}
	e.b.promptMu.Lock()
	n, kept := len(e.b.closedPrompts), e.b.promptByID[first]
	e.b.promptMu.Unlock()
	if n != maxClosedPrompts || kept != nil {
		t.Fatalf("closed records %d (want %d), oldest kept %v", n, maxClosedPrompts, kept != nil)
	}
	if w := e.wait(sidA, first); w.Code != http.StatusNotFound {
		t.Fatalf("evicted prompt = %d", w.Code)
	}
}

func TestPromptClosedRecordDropsItsDetailOnceDrawn(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k", map[string]any{"detail": strings.Repeat("x", 5000)}))
	e.click("UALEX", id, "allow")
	e.resolve(sidA, id, "ran")
	e.b.promptMu.Lock()
	has := e.b.promptByID[id].req.Detail != ""
	e.b.promptMu.Unlock()
	if !has {
		t.Fatal("the detail went before the final message was drawn")
	}
	drainJobs(t, e.b)
	e.b.promptMu.Lock()
	has = e.b.promptByID[id].req.Detail != ""
	e.b.promptMu.Unlock()
	if has {
		t.Fatal("the detail is still held after the final draw")
	}
	if got := e.lastStatus(); got != "Allowed by alex · ran" {
		t.Fatalf("status = %q", got)
	}
}

func TestPromptCapsReturn429(t *testing.T) {
	e := newPromptEnv(t)
	for i := 0; i < maxPromptsPerSession; i++ {
		e.open(permReq(sidA, "k"+strconv.Itoa(i), nil))
	}
	if w := e.do(http.MethodPost, "/prompt", permReq(sidA, "over", nil)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("21st prompt = %d %s", w.Code, w.Body)
	}
	// A repeat of an open one is not a new prompt; another session is fine.
	e.open(permReq(sidA, "k0", nil))
	id := e.open(permReq(sidB, "k0", nil))
	// Closing one frees a slot.
	e.resolve(sidB, id, "ran")
	for i := 1; i < maxPromptsPerSession; i++ {
		e.open(permReq(sidB, "m"+strconv.Itoa(i), nil))
	}
	// Overall cap: fill with other sessions until 200 are open.
	open := maxPromptsPerSession + maxPromptsPerSession - 1
	for s := 0; open < maxPrompts; s++ {
		sid := "cccccc" + strconv.Itoa(s) + "1-2222-3333-4444-555555555555"
		e.bus.Hello(sid, "pc", "/w", "", true)
		for i := 0; i < maxPromptsPerSession && open < maxPrompts; i++ {
			e.open(permReq(sid, "k"+strconv.Itoa(i), nil))
			open++
		}
	}
	e.bus.Hello("dddddd11-2222-3333-4444-555555555555", "pc", "/w", "", true)
	if w := e.do(http.MethodPost, "/prompt", permReq("dddddd11-2222-3333-4444-555555555555", "x", nil)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("201st prompt = %d %s", w.Code, w.Body)
	}
}

func TestPromptByeClosesSessionPrompts(t *testing.T) {
	e := newPromptEnv(t)
	a := e.open(permReq(sidA, "k", nil))
	b := e.open(permReq(sidB, "k", nil))
	e.bus.Bye(sidA)
	if w := e.wait(sidA, a); w.Code != http.StatusGone {
		t.Fatalf("wait after bye = %d", w.Code)
	}
	if w := e.wait(sidB, b); w.Code != http.StatusNoContent {
		t.Fatalf("the other session's prompt = %d", w.Code)
	}
	drainJobs(t, e.b)
	if got := e.lastStatus(); got != "Session ended" {
		t.Fatalf("status = %q", got)
	}
	// Also through the route.
	c := e.open(permReq(sidB, "k2", nil))
	if w := e.do(http.MethodPost, "/bye", `{"session":"`+sidB+`"}`); w.Code != http.StatusNoContent {
		t.Fatalf("bye = %d", w.Code)
	}
	if w := e.wait(sidB, c); w.Code != http.StatusGone {
		t.Fatalf("wait after bye route = %d", w.Code)
	}
}

func TestPromptLeaseLapseClosesPrompts(t *testing.T) {
	clock := newTestClock()
	f := newFakeSlack(t)
	bus := agentbus.NewStore("", clock.now)
	bus.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	bus.Hello(sidB, "pc", "/work/other", "", true)
	b, err := New(testConfig(f, t.TempDir()), bus)
	if err != nil || b == nil {
		t.Fatalf("New = %v, %v", b, err)
	}
	b.backoff = func(int) time.Duration { return 0 }
	b.retryDelay = 0
	b.state.now = clock.now
	if err = b.resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	bus.SetBridge(b)
	e := mountPromptEnv(t, b, f, bus)
	a := e.open(permReq(sidA, "k", nil))
	bb := e.open(permReq(sidB, "k", nil))
	b.sweepPrompts()
	if w := e.wait(sidA, a); w.Code != http.StatusNoContent {
		t.Fatalf("live session's prompt closed: %d", w.Code)
	}
	clock.advance(20 * time.Minute)
	bus.Hello(sidB, "pc", "/work/other", "", true) // B stays alive, A lapses
	clock.advance(15 * time.Minute)
	bus.Hello(sidB, "pc", "/work/other", "", true)
	b.sweepPrompts()
	if w := e.wait(sidA, a); w.Code != http.StatusGone {
		t.Fatalf("lapsed session's prompt = %d", w.Code)
	}
	if w := e.wait(sidB, bb); w.Code != http.StatusNoContent {
		t.Fatalf("live session's prompt = %d", w.Code)
	}
	drainJobs(t, b)
	if got := e.lastStatus(); got != "Session ended" {
		t.Fatalf("status = %q", got)
	}
}

func TestPromptHandOffClosesOldSessionPrompts(t *testing.T) {
	clock := newTestClock()
	f := newFakeSlack(t)
	bus := agentbus.NewStore("", clock.now)
	bus.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	b, err := New(testConfig(f, t.TempDir()), bus)
	if err != nil || b == nil {
		t.Fatalf("New = %v, %v", b, err)
	}
	b.backoff = func(int) time.Duration { return 0 }
	b.retryDelay = 0
	b.state.now = clock.now
	if err = b.resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	bus.SetBridge(b)
	e := mountPromptEnv(t, b, f, bus)
	old := e.open(permReq(sidA, "k", nil))
	// The old session goes offline, then the new one takes over through the
	// store's real HandOff, which calls SessionMoved on the bridge.
	clock.advance(35 * time.Minute)
	bus.Hello(sidB, "pc", "/work/flyer", "", true)
	if err = bus.HandOff(sidA, sidB); err != nil {
		t.Fatalf("HandOff = %v", err)
	}
	if w := e.wait(sidA, old); w.Code != http.StatusGone {
		t.Fatalf("old session's prompt after hand-off = %d", w.Code)
	}
	drainJobs(t, e.b)
	if got := e.lastStatus(); got != "Session ended" {
		t.Fatalf("status = %q", got)
	}
	// The new session gets the old session's prompt thread.
	e.open(permReq(sidB, "k", nil))
	posts := e.f.callsTo("chat.postMessage")
	if len(posts) != 3 || posts[2].Form.Get("thread_ts") != "1700000000.100001" {
		t.Fatalf("posts after hand-off = %d", len(posts))
	}
	b.promptMu.Lock()
	_, oldThread := b.promptThreads[sidA]
	b.promptMu.Unlock()
	if oldThread {
		t.Fatal("the old session's prompt thread is still held")
	}
}

func TestPromptThreadsAreForgottenOnBye(t *testing.T) {
	e := newPromptEnv(t)
	e.open(permReq(sidA, "k", nil))
	e.b.promptMu.Lock()
	_, has := e.b.promptThreads[sidA]
	e.b.promptMu.Unlock()
	if !has {
		t.Fatal("no prompt thread to prune")
	}
	e.bus.Bye(sidA)
	e.b.promptMu.Lock()
	_, has = e.b.promptThreads[sidA]
	e.b.promptMu.Unlock()
	if has {
		t.Fatal("a session that said bye keeps its prompt thread")
	}
}

func TestPromptsAreGoneAfterARestart(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k", nil))
	e2 := newPromptEnv(t)
	if w := e2.wait(sidA, id); w.Code != http.StatusNotFound {
		t.Fatalf("wait on a restarted proxy = %d", w.Code)
	}
	if w := e2.resolve(sidA, id, "ran"); w.Code != http.StatusNotFound {
		t.Fatalf("resolve on a restarted proxy = %d", w.Code)
	}
	// The mod re-opens with the same key.
	if again := e2.open(permReq(sidA, "k", nil)); again == id {
		t.Fatal("same id after a restart")
	}
}

func TestPromptWaitStopsWhenTheClientLeaves(t *testing.T) {
	e := newPromptEnv(t)
	id := e.open(permReq(sidA, "k", nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan agentbus.PromptWait, 1)
	e.b.promptWaitFor = time.Minute
	go func() { done <- e.b.WaitPrompt(ctx, sidA, id) }()
	cancel()
	select {
	case res := <-done:
		if res.State != agentbus.PromptOpen {
			t.Fatalf("state = %s", res.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait ignored the cancelled context")
	}
}

func TestPromptRedactsConfiguredSecrets(t *testing.T) {
	e := newPromptEnv(t)
	e.b.cfg.Secrets = []string{"my-proxy-api-key"}
	e.open(permReq(sidA, "k", map[string]any{"detail": "curl -H 'x: my-proxy-api-key' xapp-test"}))
	blocks := e.promptPost(0).Form.Get("blocks")
	if strings.Contains(blocks, "my-proxy-api-key") || strings.Contains(blocks, "xapp-test") {
		t.Fatalf("secret in %s", blocks)
	}
}
