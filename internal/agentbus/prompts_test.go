package agentbus

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

// fakePromptBridge is a Bridge that also takes prompts.
type fakePromptBridge struct {
	fakeBridge
	pmu       sync.Mutex
	available bool
	openErr   error
	wait      PromptWait
	resolveEr error
	opened    []PromptRequest
	outbound  []Outbound
	resolved  []string
	ended     []string
}

func (f *fakePromptBridge) PromptsAvailable() bool { return f.available }

func (f *fakePromptBridge) OpenPrompt(_ context.Context, o Outbound, req PromptRequest) (string, error) {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	f.opened = append(f.opened, req)
	f.outbound = append(f.outbound, o)
	if f.openErr != nil {
		return "", f.openErr
	}
	return "p_" + strings.Repeat("a", 32), nil
}

func (f *fakePromptBridge) WaitPrompt(context.Context, string, string) PromptWait { return f.wait }

func (f *fakePromptBridge) ResolvePrompt(session, id, outcome, note string) error {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	f.resolved = append(f.resolved, session+"|"+id+"|"+outcome+"|"+note)
	return f.resolveEr
}

func (f *fakePromptBridge) SessionEnded(session string) {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	f.ended = append(f.ended, session)
}

func promptServer(t *testing.T, fb *fakePromptBridge) (*Store, http.Handler) {
	t.Helper()
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	if fb != nil {
		s.SetBridge(fb)
	}
	return s, r
}

const goodPrompt = `{"session":"` + sidA + `","key":"abc","kind":"permission","title":"Bash wants to run","detail":"ls","always":true}`

func TestHelloReportsPrompts(t *testing.T) {
	hello := func(r http.Handler) gjson.Result {
		w := do(r, http.MethodPost, "/v1/agentbus/hello", `{"session":"`+sidA+`","mod":true}`)
		if w.Code != http.StatusOK {
			t.Fatalf("hello = %d %s", w.Code, w.Body)
		}
		return gjson.Parse(w.Body.String())
	}
	fb := &fakePromptBridge{available: true}
	_, r := promptServer(t, fb)
	if got := hello(r); got.Get("prompts").Type != gjson.True || got.Get("address").String() == "" {
		t.Fatalf("hello = %s", got.Raw)
	}
	fb.available = false
	if got := hello(r); got.Get("prompts").Type != gjson.False {
		t.Fatalf("hello = %s", got.Raw)
	}
	// A bridge that takes no prompts, and no bridge at all.
	_, r = promptServer(t, nil)
	if got := hello(r); got.Get("prompts").Type != gjson.False {
		t.Fatalf("no bridge: %s", got.Raw)
	}
	s, r := promptServer(t, nil)
	s.SetBridge(&fakeBridge{})
	if got := hello(r); got.Get("prompts").Type != gjson.False {
		t.Fatalf("plain bridge: %s", got.Raw)
	}
}

func TestPromptRouteStatuses(t *testing.T) {
	fb := &fakePromptBridge{available: true}
	_, r := promptServer(t, fb)
	w := do(r, http.MethodPost, "/v1/agentbus/prompt", goodPrompt)
	if w.Code != http.StatusOK || gjson.Get(w.Body.String(), "id").String() != "p_"+strings.Repeat("a", 32) {
		t.Fatalf("open = %d %s", w.Code, w.Body)
	}
	if len(fb.opened) != 1 || fb.opened[0].Kind != PromptKindPermission || !fb.opened[0].Always || fb.outbound[0].SessionID != sidA || fb.outbound[0].Name != "flyer" {
		t.Fatalf("opened = %+v %+v", fb.opened, fb.outbound)
	}
	for _, tc := range []struct {
		err  error
		want int
	}{{ErrPromptsOff, http.StatusNotFound}, {ErrPromptsBusy, http.StatusTooManyRequests}, {ErrPromptPost, http.StatusBadGateway}} {
		fb.openErr = tc.err
		if w := do(r, http.MethodPost, "/v1/agentbus/prompt", goodPrompt); w.Code != tc.want {
			t.Errorf("%v = %d", tc.err, w.Code)
		}
	}
	fb.openErr = nil
	fb.available = false
	if w := do(r, http.MethodPost, "/v1/agentbus/prompt", goodPrompt); w.Code != http.StatusNotFound {
		t.Errorf("unavailable = %d", w.Code)
	}
	fb.available = true
	unknown := strings.Replace(goodPrompt, sidA, sidB, 1)
	if w := do(r, http.MethodPost, "/v1/agentbus/prompt", unknown); w.Code != http.StatusNotFound {
		t.Errorf("unknown session = %d", w.Code)
	}
	for name, body := range map[string]string{
		"not json":                  `nope`,
		"no session":                `{"key":"k","kind":"permission","title":"t"}`,
		"no key":                    `{"session":"` + sidA + `","kind":"permission","title":"t"}`,
		"bad kind":                  `{"session":"` + sidA + `","key":"k","kind":"x","title":"t"}`,
		"questions on a permission": `{"session":"` + sidA + `","key":"k","kind":"permission","title":"t","questions":[{"id":"q","text":"x","kind":"text"}]}`,
		"five questions":            `{"session":"` + sidA + `","key":"k","kind":"question","title":"t","questions":[` + strings.Repeat(`{"id":"q","text":"x","kind":"text"},`, 4) + `{"id":"r","text":"x","kind":"text"}]}`,
		"duplicate ids":             `{"session":"` + sidA + `","key":"k","kind":"question","title":"t","questions":[{"id":"q","text":"x","kind":"text"},{"id":"q","text":"y","kind":"text"}]}`,
		"choice without options":    `{"session":"` + sidA + `","key":"k","kind":"question","title":"t","questions":[{"id":"q","text":"x","kind":"choice"}]}`,
		"min above max":             `{"session":"` + sidA + `","key":"k","kind":"question","title":"t","questions":[{"id":"q","text":"x","kind":"number","min":5,"max":1}]}`,
	} {
		if w := do(r, http.MethodPost, "/v1/agentbus/prompt", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s", name, w.Code, w.Body)
		}
	}
	ok := `{"session":"` + sidA + `","key":"k","kind":"question","title":"t","questions":[` +
		`{"id":"q1","text":"x","kind":"choice","options":[{"id":"o1","label":"a","description":"d"}],"multi":true},` +
		`{"id":"q2","text":"n","kind":"number","min":1,"max":10,"step":1,"unit":"px"}]}`
	if w := do(r, http.MethodPost, "/v1/agentbus/prompt", ok); w.Code != http.StatusOK {
		t.Fatalf("question prompt = %d %s", w.Code, w.Body)
	}
	last := fb.opened[len(fb.opened)-1]
	if len(last.Questions) != 2 || *last.Questions[1].Max != 10 || last.Questions[1].Unit != "px" || !last.Questions[0].Multi {
		t.Fatalf("questions = %+v", last.Questions)
	}
}

func TestPromptWaitStatuses(t *testing.T) {
	fb := &fakePromptBridge{available: true}
	_, r := promptServer(t, fb)
	get := func() (int, string) {
		w := do(r, http.MethodGet, "/v1/agentbus/prompt/wait?session="+sidA+"&id=p_x", "")
		return w.Code, w.Body.String()
	}
	fb.wait = PromptWait{State: PromptAnswered, Answer: &PromptAnswer{Choice: "allow", By: "alex"}}
	if code, body := get(); code != http.StatusOK || body != `{"answer":{"choice":"allow","by":"alex"}}` {
		t.Fatalf("answered = %d %s", code, body)
	}
	fb.wait = PromptWait{State: PromptAnswered, Answer: &PromptAnswer{Choice: "submit", By: "alex", Answers: map[string]any{"q1": "dayjs", "q2": []string{"a"}, "q3": 7}}}
	if _, body := get(); gjson.Get(body, "answer.answers.q2.0").String() != "a" || gjson.Get(body, "answer.answers.q3").Raw != "7" {
		t.Fatalf("answers = %s", body)
	}
	for state, want := range map[string]int{PromptOpen: http.StatusNoContent, PromptClosed: http.StatusGone, PromptGone: http.StatusNotFound} {
		fb.wait = PromptWait{State: state}
		if code, _ := get(); code != want {
			t.Errorf("%s = %d, want %d", state, code, want)
		}
	}
	if w := do(r, http.MethodGet, "/v1/agentbus/prompt/wait?session="+sidA, ""); w.Code != http.StatusBadRequest {
		t.Errorf("no id = %d", w.Code)
	}
	_, r2 := promptServer(t, nil)
	if w := do(r2, http.MethodGet, "/v1/agentbus/prompt/wait?session="+sidA+"&id=p_x", ""); w.Code != http.StatusNotFound {
		t.Errorf("no bridge = %d", w.Code)
	}
}

func TestPromptResolveRoute(t *testing.T) {
	fb := &fakePromptBridge{available: true}
	_, r := promptServer(t, fb)
	for _, outcome := range []string{"ran", "denied", "answered", "cancelled", "ended"} {
		body := `{"session":"` + sidA + `","id":"p_x","outcome":"` + outcome + `","note":"terminal"}`
		if w := do(r, http.MethodPost, "/v1/agentbus/prompt/resolve", body); w.Code != http.StatusOK {
			t.Errorf("%s = %d %s", outcome, w.Code, w.Body)
		}
	}
	if len(fb.resolved) != 5 || fb.resolved[0] != sidA+"|p_x|ran|terminal" {
		t.Fatalf("resolved = %v", fb.resolved)
	}
	fb.resolveEr = ErrPromptUnknown
	if w := do(r, http.MethodPost, "/v1/agentbus/prompt/resolve", `{"session":"`+sidA+`","id":"p_x","outcome":"ran"}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown = %d", w.Code)
	}
	for name, body := range map[string]string{
		"bad outcome": `{"session":"` + sidA + `","id":"p_x","outcome":"maybe"}`,
		"no id":       `{"session":"` + sidA + `","outcome":"ran"}`,
		"not json":    `x`,
	} {
		if w := do(r, http.MethodPost, "/v1/agentbus/prompt/resolve", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d", name, w.Code)
		}
	}
}

func TestByeAndLateByeTellThePromptBridge(t *testing.T) {
	fb := &fakePromptBridge{available: true}
	s, r := promptServer(t, fb)
	if w := do(r, http.MethodPost, "/v1/agentbus/bye", `{"session":"`+sidA+`"}`); w.Code != http.StatusNoContent {
		t.Fatalf("bye = %d", w.Code)
	}
	s.Bye("ghost")
	if len(fb.ended) != 1 || fb.ended[0] != sidA {
		t.Fatalf("ended = %v", fb.ended)
	}
}
