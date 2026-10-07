package slackbridge

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
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
	// ackPayloads holds the payload each ack carried, by envelope id.
	ackPayloads map[string]string
	nextFile    int
	uploads     []fakeUpload
	// uploadMode makes /upload/<id> answer with an HTTP status ("" is 200)
	// or, with "hangup", close the connection without answering.
	uploadMode string
	// postHold, when set, makes chat.postMessage report on postEntered once
	// it is recorded, then wait until postHold is closed before answering.
	postHold    chan struct{}
	postEntered chan struct{}
	// reactions holds the bot's reactions now on each message, as Slack
	// would: adding one twice is already_reacted, removing an absent one
	// no_reaction.
	reactions map[reactionKey]bool
	// members answers conversations.members per channel; a DM ("D" + user)
	// without an entry has that user and the bot, anything else without one
	// is channel_not_found. memberPage > 0 pages the answer.
	members    map[string][]string
	memberPage int
	// files answers GET /files/<name> (a url_private_download link);
	// fileGets records each such request.
	files    map[string]fakeFile
	fileGets []fakeFileGet
}

// fakeFile is what GET /files/<name> answers: status ("" is 200), content
// type and body.
type fakeFile struct {
	Status      int
	ContentType string
	Location    string
	Body        []byte
}

// fakeFileGet is one GET of a shared file.
type fakeFileGet struct{ Path, Auth string }

// setFile makes GET /files/<name> answer file, and returns its link.
func (f *fakeSlack) setFile(name string, file fakeFile) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files == nil {
		f.files = map[string]fakeFile{}
	}
	f.files[name] = file
	return f.URL + "/files/" + name
}

func (f *fakeSlack) recordedFileGets() []fakeFileGet {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeFileGet(nil), f.fileGets...)
}

func (f *fakeSlack) handleFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/files/")
	f.mu.Lock()
	f.fileGets = append(f.fileGets, fakeFileGet{Path: r.URL.Path, Auth: r.Header.Get("Authorization")})
	file, ok := f.files[name]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if file.ContentType != "" {
		w.Header().Set("Content-Type", file.ContentType)
	}
	if file.Location != "" {
		w.Header().Set("Location", file.Location)
	}
	if file.Status != 0 {
		w.WriteHeader(file.Status)
	}
	_, _ = w.Write(file.Body)
}

type reactionKey struct{ channel, ts, name string }

// setMembers sets the member list conversations.members gives for channel.
func (f *fakeSlack) setMembers(channel string, ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.members == nil {
		f.members = map[string][]string{}
	}
	f.members[channel] = ids
}

// reactionsOn lists the bot's reactions now on message ts in channel, sorted.
func (f *fakeSlack) reactionsOn(channel, ts string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k, on := range f.reactions {
		if on && k.channel == channel && k.ts == ts {
			out = append(out, k.name)
		}
	}
	sort.Strings(out)
	return out
}

// holdPosts makes every chat.postMessage block until the returned release is
// called; entered receives once per post that reached the fake.
func (f *fakeSlack) holdPosts() (entered <-chan struct{}, release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postHold = make(chan struct{})
	f.postEntered = make(chan struct{}, 16)
	hold := f.postHold
	return f.postEntered, func() { close(hold) }
}

// fakeUpload is one raw POST to a pre-signed upload URL.
type fakeUpload struct {
	FileID string
	Body   []byte
	Auth   string
}

// uploadSecret stands in for the signature on a pre-signed upload URL; it
// must never show up in an error or log.
const uploadSecret = "presigned-sig-0123"

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{
		t: t,
		users: []fakeUser{
			{ID: "UALEX", Name: "alex", Display: "Alex", Email: "alex@example.com"}, {ID: "UJANE", Name: "jane", Display: "Jane D", Email: "jane@example.com"},
			{ID: "UJANE2", Name: "jane2", Display: "jane d", Email: "jane2@example.com"}, {ID: "UEVE", Name: "eve", Display: "Eve"}, {ID: "UHOOK", Name: "ci", Display: "CI", Bot: true},
			// Guests: no email, so never allowed from config. UFAKE's display
			// name is an allowed user's label.
			{ID: "UBOB", Name: "bob", Display: "Bob"}, {ID: "UCAROL", Name: "carol", Display: "Carol"}, {ID: "UFAKE", Name: "alexfake", Display: "Alex"},
			// The bridge's own bot user (auth.test answers UBOT).
			{ID: "UBOT", Name: "agents", Display: "clanker-bro", Bot: true},
		},
		channels:  []fakeChannel{{ID: "CGEN", Name: "general"}, {ID: "CAGENTS", Name: "agents"}},
		fail:      map[string]string{},
		reactions: map[reactionKey]bool{},
		toClient:  make(chan string, 16),
		acks:      make(chan string, 16),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", f.handleAPI)
	mux.HandleFunc("/socket", f.handleSocket)
	mux.HandleFunc("/upload/", f.handleUpload)
	mux.HandleFunc("/files/", f.handleFile)
	f.srv = httptest.NewServer(mux)
	f.URL = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) apiBase() string { return f.URL + "/api/" }

// setUser changes fake user id (under the lock, so -race is clean).
func (f *fakeSlack) setUser(id string, change func(*fakeUser)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.users {
		if f.users[i].ID == id {
			change(&f.users[i])
		}
	}
}

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

// userLookups lists the users.info calls for anyone but the bot itself
// (resolve and the maintenance pass look the bot up for its name).
func userLookups(f *fakeSlack) []fakeCall {
	var out []fakeCall
	for _, c := range f.callsTo("users.info") {
		if c.Form.Get("user") != "UBOT" {
			out = append(out, c)
		}
	}
	return out
}

// methods lists every Web API method called, in order.
func (f *fakeSlack) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

func (f *fakeSlack) recordedUploads() []fakeUpload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeUpload(nil), f.uploads...)
}

func (f *fakeSlack) setUploadMode(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploadMode = mode
}

// handleUpload records the bytes posted to a pre-signed upload URL.
func (f *fakeSlack) handleUpload(w http.ResponseWriter, r *http.Request) {
	data, errRead := io.ReadAll(r.Body)
	if errRead != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	mode := f.uploadMode
	if r.URL.Query().Get("sig") == uploadSecret {
		f.uploads = append(f.uploads, fakeUpload{FileID: strings.TrimPrefix(r.URL.Path, "/upload/"), Body: data, Auth: r.Header.Get("Authorization")})
	}
	f.mu.Unlock()
	switch mode {
	case "":
		_, _ = w.Write([]byte("OK - " + strconv.Itoa(len(data))))
	case "hangup":
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, _, errHijack := hj.Hijack()
		if errHijack == nil {
			_ = conn.Close()
		}
	default:
		status, _ := strconv.Atoi(mode)
		http.Error(w, "upload refused", status)
	}
}

func (f *fakeSlack) opens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened
}

func (f *fakeSlack) push(envelope string) { f.toClient <- envelope }

// ackPayload is the payload the ack of envelope id carried ("" for none).
func (f *fakeSlack) ackPayload(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.ackPayloads[id]; p != "null" {
		return p
	}
	return ""
}

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
		f.mu.Lock()
		known := append([]fakeUser(nil), f.users...)
		f.mu.Unlock()
		for _, u := range known {
			if u.ID == r.PostForm.Get("user") {
				writeJSON(w, map[string]any{"ok": true, "user": map[string]any{"id": u.ID, "name": u.Name, "is_bot": u.Bot, "profile": map[string]any{"display_name": u.Display, "real_name": u.Real}}})
				return
			}
		}
		writeJSON(w, map[string]any{"ok": false, "error": "user_not_found"})
	case "chat.postMessage":
		f.mu.Lock()
		hold, entered := f.postHold, f.postEntered
		f.mu.Unlock()
		if hold != nil {
			entered <- struct{}{}
			<-hold
		}
		f.mu.Lock()
		f.nextTS++
		ts := "1700000000." + strconv.Itoa(100000+f.nextTS)
		f.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "ts": ts, "channel": r.PostForm.Get("channel")})
	case "reactions.add", "reactions.remove":
		key := reactionKey{r.PostForm.Get("channel"), r.PostForm.Get("timestamp"), r.PostForm.Get("name")}
		f.mu.Lock()
		has := f.reactions[key]
		switch {
		case method == "reactions.add" && has:
			code = "already_reacted"
		case method == "reactions.remove" && !has:
			code = "no_reaction"
		default:
			f.reactions[key] = method == "reactions.add"
		}
		f.mu.Unlock()
		if code != "" {
			writeJSON(w, map[string]any{"ok": false, "error": code})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case "conversations.open":
		// A 1:1 DM with the bot: its id is "D" + the user's id. A group DM:
		// "G" + the users' ids.
		users := r.PostForm.Get("users")
		switch {
		case users == "":
			writeJSON(w, map[string]any{"ok": false, "error": "invalid_users"})
		case strings.Contains(users, ","):
			writeJSON(w, map[string]any{"ok": true, "channel": map[string]any{"id": "G" + strings.ReplaceAll(users, ",", "")}})
		default:
			writeJSON(w, map[string]any{"ok": true, "channel": map[string]any{"id": "D" + users}})
		}
	case "conversations.members":
		channel := r.PostForm.Get("channel")
		f.mu.Lock()
		ids, known := f.members[channel]
		page := f.memberPage
		f.mu.Unlock()
		if !known && strings.HasPrefix(channel, "D") {
			ids, known = []string{strings.TrimPrefix(channel, "D"), "UBOT"}, true
		}
		if !known {
			writeJSON(w, map[string]any{"ok": false, "error": "channel_not_found"})
			return
		}
		start, _ := strconv.Atoi(r.PostForm.Get("cursor"))
		end, next := len(ids), ""
		if page > 0 && start+page < len(ids) {
			end, next = start+page, strconv.Itoa(start+page)
		}
		writeJSON(w, map[string]any{"ok": true, "members": ids[min(start, len(ids)):end], "response_metadata": map[string]any{"next_cursor": next}})
	case "views.open":
		writeJSON(w, map[string]any{"ok": true, "view": map[string]any{"id": "V1"}})
	case "chat.postEphemeral":
		writeJSON(w, map[string]any{"ok": true, "message_ts": "1700000000.999000"})
	case "chat.getPermalink":
		ch, ts := r.PostForm.Get("channel"), r.PostForm.Get("message_ts")
		writeJSON(w, map[string]any{"ok": true, "channel": ch, "permalink": "https://example.slack.com/archives/" + ch + "/p" + strings.ReplaceAll(ts, ".", "")})
	case "files.getUploadURLExternal":
		f.mu.Lock()
		f.nextFile++
		id := "F" + strconv.Itoa(1000+f.nextFile)
		f.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "upload_url": f.URL + "/upload/" + id + "?sig=" + uploadSecret, "file_id": id})
	case "files.completeUploadExternal":
		writeJSON(w, map[string]any{"ok": true, "files": []map[string]any{{"id": "F", "title": "t"}}})
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
				EnvelopeID string          `json:"envelope_id"`
				Payload    json.RawMessage `json:"payload"`
			}
			if errRead := conn.ReadJSON(&ack); errRead != nil {
				return
			}
			f.mu.Lock()
			if f.ackPayloads == nil {
				f.ackPayloads = map[string]string{}
			}
			f.ackPayloads[ack.EnvelopeID] = string(ack.Payload)
			f.mu.Unlock()
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
