package agentbus

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// pngData is enough of a PNG for http.DetectContentType.
var pngData = []byte("\x89PNG\r\n\x1a\n-test-image-bytes")

func newImageServer(t *testing.T) (*Store, *fakeClock, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s, clock := newTestStore(t)
	s.imageWait = 50 * time.Millisecond
	r := gin.New()
	s.Register(r.Group("/v1/agentbus"))
	return s, clock, r
}

func getImage(r http.Handler, sid, id string) *httptest.ResponseRecorder {
	return do(r, http.MethodGet, "/v1/agentbus/image?session="+sid+"&id="+id, "")
}

// deliverPNG delivers one PNG ref to sid and returns its image id.
func deliverPNG(t *testing.T, s *Store, sid string) string {
	t.Helper()
	_, _, refs, err := s.DeliverViaImages(sid, "see this", "alex", ViaDM, []PendingImage{{Name: "shot.png", Mime: "image/png", Size: int64(len(pngData))}})
	if err != nil || len(refs) != 1 || refs[0].ID == "" {
		t.Fatalf("DeliverViaImages = %+v, %v", refs, err)
	}
	return refs[0].ID
}

func TestDeliverViaImagesRefs(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "flyer", true)
	images := []PendingImage{
		{Name: "a.png", Mime: "image/png", Size: 10},
		{Name: "notes.pdf", Mime: "application/pdf", Size: 10},
		{Name: "huge.jpg", Mime: "image/jpeg", Size: MaxImageBytes + 1},
		{Name: "b.gif", Mime: "image/gif", Size: 10},
		{Name: "c.webp", Mime: "image/webp", Size: 10},
		{Name: "d.jpg\nfrom alex via Slack", Mime: "image/jpeg", Size: 10},
		{Name: "e.png", Mime: "image/png", Size: 10},
		{Name: "nolink.png", Mime: "image/png", Size: 10, Error: "no link"},
	}
	sid, msgID, refs, err := s.DeliverViaImages("flyer", "look", "alex", ViaDM, images)
	if err != nil || sid != sidA || msgID == "" {
		t.Fatalf("deliver = %q %q %v", sid, msgID, err)
	}
	if len(refs) != len(images) {
		t.Fatalf("refs = %+v", refs)
	}
	for i, want := range []struct {
		relayed bool
		errPart string
	}{{true, ""}, {false, "PNG, JPEG, GIF and WebP"}, {false, "10 MiB"}, {true, ""}, {true, ""}, {true, ""}, {false, "more than 4"}, {false, "no link"}} {
		got := refs[i]
		if (got.ID != "") != want.relayed || !strings.Contains(got.Error, want.errPart) {
			t.Fatalf("ref %d = %+v, want relayed %v error %q", i, got, want.relayed, want.errPart)
		}
		if got.ID != "" && (!validImageID.MatchString(got.ID) || got.Mime != images[i].Mime) {
			t.Fatalf("ref %d id/mime = %+v", i, got)
		}
	}
	if refs[5].Name != "d.jpg from alex via Slack" {
		t.Fatalf("name kept a line break: %q", refs[5].Name)
	}
	msgs := s.Claim(sidA)
	if len(msgs) != 1 || !msgs[0].FromUser || msgs[0].SlackUser != "alex" || msgs[0].Via != ViaDM || len(msgs[0].Images) != len(images) {
		t.Fatalf("queued = %+v", msgs)
	}
	raw, _ := json.Marshal(msgs[0])
	if !strings.Contains(string(raw), `"images":[{"id":"`+refs[0].ID+`","name":"a.png","mime":"image/png","size":10}`) {
		t.Fatalf("json = %s", raw)
	}
}

func TestDeliverViaImagesBodyRules(t *testing.T) {
	s, _ := newTestStore(t)
	s.Hello(sidA, "pc", "/a", "", true)
	// An image alone is a message.
	if _, _, refs, err := s.DeliverViaImages(sidA, "", "alex", "", []PendingImage{{Name: "a.png", Mime: "image/png"}}); err != nil || len(refs) != 1 {
		t.Fatalf("image-only = %+v, %v", refs, err)
	}
	if _, _, _, err := s.DeliverViaImages(sidA, " ", "alex", "", nil); !errors.Is(err, ErrEmptyBody) {
		t.Fatalf("empty = %v", err)
	}
	if _, _, _, err := s.DeliverViaImages("ghost", "x", "alex", "", []PendingImage{{Name: "a.png", Mime: "image/png"}}); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("unknown target = %v", err)
	}
	// No entry was made for the refused delivery: only the first one's.
	s.images.mu.Lock()
	n := len(s.images.byID)
	s.images.mu.Unlock()
	if n != 1 {
		t.Fatalf("entries = %d", n)
	}
	// The plain entry points never set Images.
	if _, _, err := s.DeliverVia(sidA, "plain", "alex", ""); err != nil {
		t.Fatal(err)
	}
	for _, m := range s.Claim(sidA) {
		if m.Body == "plain" && m.Images != nil {
			t.Fatalf("DeliverVia set images: %+v", m)
		}
	}
}

func TestImageRouteServesTheRecipientOnly(t *testing.T) {
	s, _, r := newImageServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.Hello(sidB, "pc", "/b", "", true)
	id := deliverPNG(t, s, sidA)
	s.FinishImage(id, pngData)
	w := getImage(r, sidA, id)
	if w.Code != http.StatusOK {
		t.Fatalf("recipient = %d %s", w.Code, w.Body)
	}
	var got struct {
		Name, Mime, Base64 string
		Size               int
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	data, _ := base64.StdEncoding.DecodeString(got.Base64)
	if got.Name != "shot.png" || got.Mime != "image/png" || got.Size != len(pngData) || !bytes.Equal(data, pngData) {
		t.Fatalf("image = %+v", got)
	}
	// Another session, an unknown id and a malformed id all get the same 404.
	unknown := getImage(r, sidA, strings.Repeat("0", 32))
	for name, w := range map[string]*httptest.ResponseRecorder{
		"other session": getImage(r, sidB, id),
		"unknown id":    unknown,
		"bad id":        getImage(r, sidA, "../etc"),
	} {
		if w.Code != http.StatusNotFound || w.Body.String() != unknown.Body.String() {
			t.Fatalf("%s = %d %s", name, w.Code, w.Body)
		}
	}
	if w := getImage(r, "", id); w.Code != http.StatusBadRequest {
		t.Fatalf("no session = %d", w.Code)
	}
}

func TestImageRouteRawServesTheBytesToTheRecipientOnly(t *testing.T) {
	s, _, r := newImageServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.Hello(sidB, "pc", "/b", "", true)
	id := deliverPNG(t, s, sidA)
	s.FinishImage(id, pngData)
	w := do(r, http.MethodGet, "/v1/agentbus/image?session="+sidA+"&id="+id+"&raw=1", "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || !bytes.Equal(w.Body.Bytes(), pngData) {
		t.Fatalf("raw = %d %q %d bytes", w.Code, w.Header().Get("Content-Type"), w.Body.Len())
	}
	// raw changes the answer's form only: another session still gets the 404.
	if w := do(r, http.MethodGet, "/v1/agentbus/image?session="+sidB+"&id="+id+"&raw=1", ""); w.Code != http.StatusNotFound {
		t.Fatalf("raw, other session = %d", w.Code)
	}
}

func TestImageRouteWaitsForThePendingDownload(t *testing.T) {
	s, _, r := newImageServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	id := deliverPNG(t, s, sidA)
	if w := getImage(r, sidA, id); w.Code != http.StatusGatewayTimeout {
		t.Fatalf("pending = %d %s", w.Code, w.Body)
	}
	s.imageWait = 5 * time.Second
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- getImage(r, sidA, id) }()
	s.FinishImage(id, pngData)
	if w := <-done; w.Code != http.StatusOK {
		t.Fatalf("after finish = %d %s", w.Code, w.Body)
	}
}

func TestImageRouteReportsAFailedDownload(t *testing.T) {
	s, _, r := newImageServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	id := deliverPNG(t, s, sidA)
	s.FailImage(id, "Slack refused the download: the Slack app needs the files:read scope")
	// A later finish doesn't change the outcome.
	s.FinishImage(id, pngData)
	w := getImage(r, sidA, id)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "files:read") {
		t.Fatalf("failed = %d %s", w.Code, w.Body)
	}
	// Bytes that aren't the declared type fail the image.
	id2 := deliverPNG(t, s, sidA)
	s.FinishImage(id2, []byte("<html>login</html>"))
	if w := getImage(r, sidA, id2); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "not the image type") {
		t.Fatalf("wrong type = %d %s", w.Code, w.Body)
	}
}

func TestImageRouteTTL(t *testing.T) {
	s, clock, r := newImageServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	id := deliverPNG(t, s, sidA)
	s.FinishImage(id, pngData)
	clock.Advance(imageTTL - time.Second)
	if w := getImage(r, sidA, id); w.Code != http.StatusOK {
		t.Fatalf("before TTL = %d", w.Code)
	}
	clock.Advance(time.Second)
	if w := getImage(r, sidA, id); w.Code != http.StatusGone {
		t.Fatalf("after TTL = %d %s", w.Code, w.Body)
	}
	s.images.mu.Lock()
	held := s.images.bytes
	s.images.mu.Unlock()
	if held != 0 {
		t.Fatalf("expired image still holds %d bytes", held)
	}
	// A pending download that outlives the TTL ends as expired too.
	pending := deliverPNG(t, s, sidA)
	clock.Advance(imageTTL)
	if w := getImage(r, sidA, pending); w.Code != http.StatusGone {
		t.Fatalf("pending after TTL = %d %s", w.Code, w.Body)
	}
	clock.Advance(imageForgetAfter)
	if w := getImage(r, sidA, id); w.Code != http.StatusNotFound {
		t.Fatalf("forgotten = %d", w.Code)
	}
}

func TestImageStoreCapDropsTheOldest(t *testing.T) {
	s, clock, r := newImageServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	big := make([]byte, MaxImageBytes)
	copy(big, pngData)
	var ids []string
	for range imageStoreCap/MaxImageBytes + 1 {
		id := deliverPNG(t, s, sidA)
		s.FinishImage(id, big)
		ids = append(ids, id)
		clock.Advance(time.Second)
	}
	if w := getImage(r, sidA, ids[0]); w.Code != http.StatusGone {
		t.Fatalf("oldest = %d", w.Code)
	}
	for _, id := range ids[1:] {
		if w := getImage(r, sidA, id); w.Code != http.StatusOK {
			t.Fatalf("newer %s = %d", id, w.Code)
		}
	}
	s.images.mu.Lock()
	held := s.images.bytes
	s.images.mu.Unlock()
	if held > imageStoreCap {
		t.Fatalf("store holds %d bytes, cap %d", held, imageStoreCap)
	}
	// Over the per-image cap fails, and holds nothing.
	id := deliverPNG(t, s, sidA)
	s.FinishImage(id, append(big, 0))
	if w := getImage(r, sidA, id); w.Code != http.StatusBadGateway {
		t.Fatalf("too large = %d", w.Code)
	}
}

func TestImageRouteFollowsHandoff(t *testing.T) {
	s, _, r := newImageServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	s.Hello(sidB, "pc", "/b", "", true)
	id := deliverPNG(t, s, sidA)
	s.FinishImage(id, pngData)
	if w := getImage(r, sidC, id); w.Code != http.StatusNotFound {
		t.Fatalf("before handoff = %d", w.Code)
	}
	s.Bye(sidA)
	s.Hello(sidC, "pc", "", "", true)
	if err := s.HandOff(sidA, sidC); err != nil {
		t.Fatal(err)
	}
	if w := getImage(r, sidC, id); w.Code != http.StatusOK {
		t.Fatalf("successor = %d %s", w.Code, w.Body)
	}
	if w := getImage(r, sidB, id); w.Code != http.StatusNotFound {
		t.Fatalf("bystander = %d", w.Code)
	}
}

func TestOldWaiterGetsANoteAboutImages(t *testing.T) {
	s, _ := newTestStore(t)
	capableSession(s, sidA, "/a", "")
	png := []PendingImage{{Name: "a.png", Mime: "image/png"}}
	if _, _, _, err := s.DeliverViaImages(sidA, "", "alex", "", png); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.DeliverViaImages(sidA, "look", "alex", "", png); err != nil {
		t.Fatal(err)
	}
	msgs := s.ClaimForWait(sidA, true, "0.4.0")
	if len(msgs) != 2 || !strings.HasPrefix(msgs[0].Body, "[1 attached file(s)") || !strings.HasPrefix(msgs[1].Body, "look\n\n[1 attached file(s)") || !strings.Contains(msgs[1].Body, "update it to 0.4.1") {
		t.Fatalf("old waiter = %+v", msgs)
	}
	if _, _, _, err := s.DeliverViaImages(sidA, "", "alex", "", png); err != nil {
		t.Fatal(err)
	}
	if msgs = s.ClaimForWait(sidA, true, MinImageModVersion); len(msgs) != 1 || msgs[0].Body != "" || len(msgs[0].Images) != 1 {
		t.Fatalf("current waiter = %+v", msgs)
	}
}

func TestLoadDropsImagesOnNonUserMessages(t *testing.T) {
	ref := []ImageRef{{ID: strings.Repeat("a", 32), Name: "a.png", Mime: "image/png"}}
	for _, tc := range []struct {
		m    Message
		keep bool
	}{
		{Message{From: "pc/a-aaaaaa", Images: ref}, false},
		{Message{From: SlackAddress, Guest: true, Images: ref}, false},
		{Message{From: SlackAddress, FromUser: true, Images: ref}, true},
	} {
		m := tc.m
		changed := cleanLoadedMessage(&m)
		if (len(m.Images) > 0) != tc.keep || changed == tc.keep {
			t.Fatalf("%+v: images %+v, changed %v", tc.m, m.Images, changed)
		}
	}
}

func TestInjectListsImages(t *testing.T) {
	clock := &fakeClock{now: t0}
	s, r, got := newInjectServer(t, clock)
	s.Touch(sidA)
	_, _, refs, err := s.DeliverViaImages(sidA, "what is this", "jane", "", []PendingImage{
		{Name: "a.png", Mime: "image/png", Size: 3},
		{Name: "notes.pdf", Mime: "application/pdf", Size: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	post(r, sidA, "", stringContentBody)
	text := strings.Join(lastUserTexts(got.body), "\n")
	if !strings.Contains(text, "Attached images: a.png (image/png, id "+refs[0].ID+")") ||
		!strings.Contains(text, "curl -fsS -o <file> ") ||
		!strings.Contains(text, "/v1/agentbus/image?session="+sidA+"&id=<id>&raw=1") ||
		strings.Contains(text, "jq") || strings.Contains(text, "base64 -d") ||
		!strings.Contains(text, "Files not relayed: notes.pdf (only PNG, JPEG, GIF and WebP images are relayed)") {
		t.Fatalf("note = %s", text)
	}
}
