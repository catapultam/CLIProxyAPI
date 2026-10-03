package agentbus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func newTestServer(t *testing.T) (*Store, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s := NewStore("", nil)
	s.waitTimeout = 300 * time.Millisecond
	r := gin.New()
	s.Register(r.Group("/v1/agentbus"))
	return s, r
}

func do(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHTTPSendThenInbox(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "")
	s.Hello(sidB, "pc", "/b", "")
	w := do(r, http.MethodPost, "/v1/agentbus/send", `{"from_session":"`+sidA+`","to":"`+s.Address(sidB)+`","body":"hi"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"m_`) {
		t.Fatalf("send = %d %s", w.Code, w.Body)
	}
	w = do(r, http.MethodGet, "/v1/agentbus/inbox?session="+sidB, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"body":"hi"`) {
		t.Fatalf("inbox = %d %s", w.Code, w.Body)
	}
	if w = do(r, http.MethodGet, "/v1/agentbus/inbox?session="+sidB, ""); strings.Contains(w.Body.String(), "hi") {
		t.Fatalf("delivered twice: %s", w.Body)
	}
}

func TestHTTPSendErrors(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "")
	if w := do(r, http.MethodPost, "/v1/agentbus/send", `{"from_session":"`+sidA+`","to":"ghost","body":"x"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown target = %d", w.Code)
	}
	if w := do(r, http.MethodPost, "/v1/agentbus/send", `not json`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad json = %d", w.Code)
	}
	if w := do(r, http.MethodPost, "/v1/agentbus/send", `{"from_session":"`+sidA+`","to":"`+s.Address(sidA)+`","body":"`+strings.Repeat("x", MaxBodyBytes+1)+`"}`); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large = %d", w.Code)
	}
}

func TestHTTPWaitReturnsPendingImmediately(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "")
	if _, err := s.Send(sidA, s.Address(sidA), "queued", ""); err != nil {
		t.Fatal(err)
	}
	w := do(r, http.MethodGet, "/v1/agentbus/wait?session="+sidA+"&machine=pc&cwd=/a", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "queued") {
		t.Fatalf("wait = %d %s", w.Code, w.Body)
	}
}

func TestHTTPWaitBlocksUntilSend(t *testing.T) {
	s, r := newTestServer(t)
	s.waitTimeout = 5 * time.Second
	s.Hello(sidA, "pc", "/a", "")
	s.Hello(sidB, "pc", "/b", "")
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- do(r, http.MethodGet, "/v1/agentbus/wait?session="+sidB, "") }()
	time.Sleep(100 * time.Millisecond)
	if _, err := s.Send(sidA, s.Address(sidB), "wake up", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-done:
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "wake up") {
			t.Fatalf("wait = %d %s", w.Code, w.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not return after send")
	}
}

func TestHTTPWaitTimesOut(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "")
	if w := do(r, http.MethodGet, "/v1/agentbus/wait?session="+sidA, ""); w.Code != http.StatusNoContent {
		t.Fatalf("wait = %d", w.Code)
	}
}

func TestHTTPNewerWaiterSupersedesOlder(t *testing.T) {
	s, r := newTestServer(t)
	s.waitTimeout = 5 * time.Second
	s.Hello(sidA, "pc", "/a", "")
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 2)
	wg.Add(1)
	go func() { defer wg.Done(); results[0] = do(r, http.MethodGet, "/v1/agentbus/wait?session="+sidA, "") }()
	time.Sleep(150 * time.Millisecond)
	wg.Add(1)
	go func() { defer wg.Done(); results[1] = do(r, http.MethodGet, "/v1/agentbus/wait?session="+sidA, "") }()
	time.Sleep(150 * time.Millisecond)
	if _, err := s.Send(sidA, s.Address(sidA), "once", ""); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if results[0].Code != http.StatusConflict {
		t.Fatalf("older waiter = %d %s", results[0].Code, results[0].Body)
	}
	if results[1].Code != http.StatusOK || strings.Count(results[1].Body.String(), "once") != 1 {
		t.Fatalf("newer waiter = %d %s", results[1].Code, results[1].Body)
	}
}

func TestHTTPSetupRoutesRemoved(t *testing.T) {
	_, r := newTestServer(t)
	if w := do(r, http.MethodGet, "/v1/agentbus/setup", ""); w.Code != http.StatusNotFound {
		t.Fatalf("setup = %d", w.Code)
	}
	if w := do(r, http.MethodGet, "/v1/agentbus/wait.sh", ""); w.Code != http.StatusNotFound {
		t.Fatalf("wait.sh = %d", w.Code)
	}
}

func TestHTTPNameAndHello(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "")
	if w := do(r, http.MethodPost, "/v1/agentbus/hello", `{"session":"`+sidB+`","machine":"fedora","cwd":"/srv/ci","name":"ci"}`); w.Code != http.StatusOK {
		t.Fatalf("hello = %d %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodPost, "/v1/agentbus/name", `{"session":"`+sidA+`","name":"ci"}`); w.Code != http.StatusConflict {
		t.Fatalf("taken name = %d", w.Code)
	}
	w := do(r, http.MethodGet, "/v1/agentbus/peers", "")
	var body struct{ Peers []Peer }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Peers) != 2 {
		t.Fatalf("peers = %s (%v)", w.Body, err)
	}
	found := false
	for _, p := range body.Peers {
		if p.Name == "ci" && p.Machine == "fedora" && p.Address == "fedora/ci-bbbbbb" {
			found = true
		}
	}
	if !found {
		t.Fatalf("hello not reflected in peers: %s", w.Body)
	}
}
