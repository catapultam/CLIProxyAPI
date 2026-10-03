package agentbus

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

const uploadPath = "/v1/agentbus/slack/upload"

// fakeImageBridge is a bridge that can post images. PostImage calls back into
// the store, so a handler that held the store lock would deadlock.
type fakeImageBridge struct {
	fakeBridge
	store  *Store
	mu     sync.Mutex
	images []postedImage
	err    error
}

type postedImage struct {
	out      Outbound
	filename string
	data     []byte
}

func (f *fakeImageBridge) PostImage(_ context.Context, o Outbound, filename string, data []byte) error {
	_ = f.store.Address(o.SessionID)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images = append(f.images, postedImage{out: o, filename: filename, data: append([]byte(nil), data...)})
	return f.err
}

func (f *fakeImageBridge) posted() []postedImage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]postedImage(nil), f.images...)
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type uploadField struct{ name, value string }

// uploadRequest builds a multipart upload. The file part claims image/png
// whatever its bytes are, so tests prove the handler sniffs instead.
func uploadRequest(t *testing.T, fields []uploadField, filename string, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range fields {
		if err := mw.WriteField(f.name, f.value); err != nil {
			t.Fatal(err)
		}
	}
	if data != nil {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
		h.Set("Content-Type", "image/png")
		part, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, uploadPath, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func serve(r http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func newUploadServer(t *testing.T) (*Store, *gin.Engine, *fakeImageBridge) {
	t.Helper()
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	fb := &fakeImageBridge{store: s}
	s.SetBridge(fb)
	return s, r, fb
}

func TestHTTPSlackUploadPostsImage(t *testing.T) {
	s, r, fb := newUploadServer(t)
	data := tinyPNG(t)
	req := uploadRequest(t, []uploadField{{"session", sidA}, {"caption", "the chart <!channel>"}}, "chart.png", data)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serve(r, req) }()
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("upload hung (bridge called under the store lock?)")
	}
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"ok":true}` {
		t.Fatalf("upload = %d %s", w.Code, w.Body)
	}
	got := fb.posted()
	if len(got) != 1 || !bytes.Equal(got[0].data, data) || got[0].filename != "chart.png" {
		t.Fatalf("posted = %+v", got)
	}
	want := Outbound{SessionID: sidA, Address: s.Address(sidA), Name: "flyer", Machine: "pc", Cwd: "/work/flyer", Body: "the chart <!channel>"}
	if got[0].out != want {
		t.Fatalf("outbound = %+v, want %+v", got[0].out, want)
	}
}

func TestHTTPSlackUploadFileBeforeFields(t *testing.T) {
	_, r, fb := newUploadServer(t)
	data := tinyPNG(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "a.png")
	_, _ = part.Write(data)
	_ = mw.WriteField("session", sidA)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, uploadPath, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if w := serve(r, req); w.Code != http.StatusOK || len(fb.posted()) != 1 || fb.posted()[0].out.Body != "" {
		t.Fatalf("upload = %d %s, posted %d", w.Code, w.Body, len(fb.posted()))
	}
}

func TestHTTPSlackUploadRejectsNonImages(t *testing.T) {
	_, r, fb := newUploadServer(t)
	for _, data := range [][]byte{[]byte("just some text, honest"), []byte("<html><body>x</body></html>"), {}} {
		w := serve(r, uploadRequest(t, []uploadField{{"session", sidA}}, "x.png", data))
		if w.Code != http.StatusUnsupportedMediaType || !strings.Contains(w.Body.String(), "only PNG, JPEG, GIF or WebP images") {
			t.Fatalf("upload %q = %d %s", data, w.Code, w.Body)
		}
	}
	if n := len(fb.posted()); n != 0 {
		t.Fatalf("posted %d non-images", n)
	}
}

func TestHTTPSlackUploadAcceptsEveryImageType(t *testing.T) {
	_, r, fb := newUploadServer(t)
	for _, data := range [][]byte{
		[]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00rest"),
		[]byte("GIF89a\x01\x00\x01\x00rest"),
		[]byte("RIFF\x10\x00\x00\x00WEBPVP8 rest"),
	} {
		if w := serve(r, uploadRequest(t, []uploadField{{"session", sidA}}, "x", data)); w.Code != http.StatusOK {
			t.Fatalf("upload %q = %d %s", data[:4], w.Code, w.Body)
		}
	}
	if n := len(fb.posted()); n != 3 {
		t.Fatalf("posted = %d", n)
	}
}

func TestHTTPSlackUploadTooLarge(t *testing.T) {
	_, r, fb := newUploadServer(t)
	big := append(tinyPNG(t), make([]byte, maxImageBytes)...)
	w := serve(r, uploadRequest(t, []uploadField{{"session", sidA}}, "big.png", big))
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "image exceeds 10 MiB") {
		t.Fatalf("big file = %d %s", w.Code, w.Body)
	}
	// A body over the overall cap fails while it streams, even in a part the
	// handler doesn't read.
	w = serve(r, uploadRequest(t, []uploadField{{"session", sidA}, {"junk", strings.Repeat("x", maxImageBytes+uploadFormHeadroom)}}, "a.png", tinyPNG(t)))
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "image exceeds 10 MiB") {
		t.Fatalf("big body = %d %s", w.Code, w.Body)
	}
	if n := len(fb.posted()); n != 0 {
		t.Fatalf("posted %d oversized uploads", n)
	}
}

func TestHTTPSlackUploadBadRequests(t *testing.T) {
	_, r, fb := newUploadServer(t)
	data := tinyPNG(t)
	cases := []struct {
		name string
		req  *http.Request
		want string
	}{
		{"unknown session", uploadRequest(t, []uploadField{{"session", sidB}}, "a.png", data), "session is not a known session"},
		{"no session", uploadRequest(t, nil, "a.png", data), "session is required"},
		{"no file", uploadRequest(t, []uploadField{{"session", sidA}}, "", nil), "file is required"},
		{"long caption", uploadRequest(t, []uploadField{{"session", sidA}, {"caption", strings.Repeat("c", maxFieldBytes+1)}}, "a.png", data), "caption exceeds 4 KiB"},
		{"not multipart", httptest.NewRequest(http.MethodPost, uploadPath, strings.NewReader(`{"session":"x"}`)), "multipart/form-data"},
	}
	for _, tc := range cases {
		w := serve(r, tc.req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("%s = %d %s", tc.name, w.Code, w.Body)
		}
	}
	if n := len(fb.posted()); n != 0 {
		t.Fatalf("posted %d bad uploads", n)
	}
}

func TestHTTPSlackUploadNeedsAnImageBridge(t *testing.T) {
	s, r := newTestServer(t)
	s.Hello(sidA, "pc", "/a", "", true)
	data := tinyPNG(t)
	for _, b := range []Bridge{nil, &fakeBridge{users: []string{"alex"}}} {
		s.SetBridge(b)
		w := serve(r, uploadRequest(t, []uploadField{{"session", sidA}}, "a.png", data))
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "slack is not enabled") {
			t.Fatalf("bridge %T: upload = %d %s", b, w.Code, w.Body)
		}
	}
}

func TestHTTPSlackUploadReportsSlackFailure(t *testing.T) {
	_, r, fb := newUploadServer(t)
	fb.err = errors.New("invalid_channel")
	w := serve(r, uploadRequest(t, []uploadField{{"session", sidA}}, "a.png", tinyPNG(t)))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"slack upload failed: invalid_channel"`) {
		t.Fatalf("upload = %d %s", w.Code, w.Body)
	}
}
