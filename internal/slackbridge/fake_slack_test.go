package slackbridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

type fakeUser struct {
	ID, Name, Display, Real, Email string
	Bot                            bool
}

type fakeChannel struct{ ID, Name string }

type fakeCall struct {
	Method string
	Form   url.Values
	Auth   string
}

// fakeSlack stands in for slack.com/api and the Socket Mode websocket.
type fakeSlack struct {
	t        *testing.T
	srv      *httptest.Server
	URL      string
	mu       sync.Mutex
	calls    []fakeCall
	users    []fakeUser
	channels []fakeChannel
	nextTS   int
	opened   int
	fail     map[string]string // method -> Slack error code
	toClient chan string
	acks     chan string
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{
		t:        t,
		users:    []fakeUser{{ID: "UALEX", Name: "alex", Display: "Alex", Email: "alex@example.com"}, {ID: "UJANE", Name: "jane", Display: "Jane D", Email: "jane@example.com"}, {ID: "UJANE2", Name: "jane2", Display: "jane d", Email: "jane2@example.com"}, {ID: "UEVE", Name: "eve", Display: "Eve"}, {ID: "UHOOK", Name: "ci", Display: "CI", Bot: true}},
		channels: []fakeChannel{{ID: "CGEN", Name: "general"}, {ID: "CAGENTS", Name: "agents"}},
		fail:     map[string]string{},
		toClient: make(chan string, 16),
		acks:     make(chan string, 16),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", f.handleAPI)
	mux.HandleFunc("/socket", f.handleSocket)
	f.srv = httptest.NewServer(mux)
	f.URL = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) apiBase() string { return f.URL + "/api/" }

func (f *fakeSlack) callsTo(method string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeSlack) opens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened
}

func (f *fakeSlack) push(envelope string) { f.toClient <- envelope }

// setFail makes method return a Slack error (under the lock, so -race is clean).
func (f *fakeSlack) setFail(method, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[method] = code
}

func (f *fakeSlack) handleAPI(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	if errParse := r.ParseForm(); errParse != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Method: method, Form: r.PostForm, Auth: r.Header.Get("Authorization")})
	code := f.fail[method]
	f.mu.Unlock()
	if code != "" {
		writeJSON(w, map[string]any{"ok": false, "error": code})
		return
	}
	switch method {
	case "auth.test":
		writeJSON(w, map[string]any{"ok": true, "user_id": "UBOT"})
	case "conversations.list":
		var chans []map[string]any
		for _, c := range f.channels {
			chans = append(chans, map[string]any{"id": c.ID, "name": c.Name})
		}
		writeJSON(w, map[string]any{"ok": true, "channels": chans, "response_metadata": map[string]any{"next_cursor": ""}})
	case "users.lookupByEmail":
		for _, u := range f.users {
			if u.Email != "" && strings.EqualFold(u.Email, r.PostForm.Get("email")) {
				writeJSON(w, map[string]any{"ok": true, "user": map[string]any{"id": u.ID}})
				return
			}
		}
		writeJSON(w, map[string]any{"ok": false, "error": "users_not_found"})
	case "users.info":
		for _, u := range f.users {
			if u.ID == r.PostForm.Get("user") {
				writeJSON(w, map[string]any{"ok": true, "user": map[string]any{"id": u.ID, "name": u.Name, "is_bot": u.Bot, "profile": map[string]any{"display_name": u.Display, "real_name": u.Real}}})
				return
			}
		}
		writeJSON(w, map[string]any{"ok": false, "error": "user_not_found"})
	case "chat.postMessage":
		f.mu.Lock()
		f.nextTS++
		ts := "1700000000." + strconv.Itoa(100000+f.nextTS)
		f.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "ts": ts, "channel": r.PostForm.Get("channel")})
	case "reactions.add":
		writeJSON(w, map[string]any{"ok": true})
	case "apps.connections.open":
		f.mu.Lock()
		f.opened++
		f.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(f.URL, "http") + "/socket"})
	default:
		writeJSON(w, map[string]any{"ok": false, "error": "unknown_method"})
	}
}

func (f *fakeSlack) handleSocket(w http.ResponseWriter, r *http.Request) {
	conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if errUpgrade != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var ack struct {
				EnvelopeID string `json:"envelope_id"`
			}
			if errRead := conn.ReadJSON(&ack); errRead != nil {
				return
			}
			f.acks <- ack.EnvelopeID
		}
	}()
	if errHello := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello"}`)); errHello != nil {
		return
	}
	for {
		select {
		case <-done:
			return
		case msg := <-f.toClient:
			if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(msg)); errWrite != nil {
				return
			}
			if strings.Contains(msg, `"type":"disconnect"`) {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
