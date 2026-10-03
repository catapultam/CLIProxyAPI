package slackbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\n-not-really-a-full-png-but-the-bridge-does-not-care")

func outboundFor(bus *agentbus.Store, sid, name, body string) agentbus.Outbound {
	return agentbus.Outbound{SessionID: sid, Address: bus.Address(sid), Name: name, Machine: "pc", Body: body}
}

// assertSafeError fails when err leaks the upload URL, its signature or a token.
func assertSafeError(t *testing.T, f *fakeSlack, err error) {
	t.Helper()
	for _, secret := range []string{uploadSecret, "/upload/", f.URL, "xoxb", "xapp"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error %q leaks %q", err, secret)
		}
	}
}

func TestPostImageIntoExistingThread(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if _, err := bus.Send(sidA, "slack", "starting", ""); err != nil {
		t.Fatal(err)
	}
	drainJobs(t, b)
	ts, ok := b.state.thread(sidA)
	if !ok {
		t.Fatal("no thread after the first post")
	}
	err := b.PostImage(context.Background(), outboundFor(bus, sidA, "flyer", "look <!channel> @alex"), "my shot (1).png", pngBytes)
	if err != nil {
		t.Fatal(err)
	}
	if posts := f.callsTo("chat.postMessage"); len(posts) != 1 {
		t.Fatalf("posts = %+v (the image must not open a second thread)", posts)
	}
	gets := f.callsTo("files.getUploadURLExternal")
	if len(gets) != 1 || gets[0].Form.Get("filename") != "my_shot_1_.png" || gets[0].Form.Get("length") != strconv.Itoa(len(pngBytes)) || gets[0].Auth != "Bearer xoxb-test" {
		t.Fatalf("getUploadURLExternal = %+v", gets)
	}
	ups := f.recordedUploads()
	if len(ups) != 1 || !bytes.Equal(ups[0].Body, pngBytes) {
		t.Fatalf("uploads = %d, bytes match = %v", len(ups), len(ups) == 1 && bytes.Equal(ups[0].Body, pngBytes))
	}
	if ups[0].Auth != "" {
		t.Fatalf("the raw upload carried Authorization %q", ups[0].Auth)
	}
	done := f.callsTo("files.completeUploadExternal")
	if len(done) != 1 {
		t.Fatalf("completeUploadExternal = %+v", done)
	}
	form := done[0].Form
	if form.Get("channel_id") != "CAGENTS" || form.Get("thread_ts") != ts {
		t.Fatalf("complete form = %v (thread %q)", form, ts)
	}
	if got := form.Get("initial_comment"); got != "look &lt;!channel&gt; <@UALEX>" {
		t.Fatalf("initial_comment = %q", got)
	}
	var files []map[string]string
	if errJSON := json.Unmarshal([]byte(form.Get("files")), &files); errJSON != nil || len(files) != 1 || files[0]["id"] != ups[0].FileID || files[0]["title"] != "my_shot_1_.png" {
		t.Fatalf("files = %q (%v)", form.Get("files"), errJSON)
	}
}

func TestPostImageOpensThreadWithHeaderFirst(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if err := b.PostImage(context.Background(), outboundFor(bus, sidB, "", "the chart"), "chart.png", pngBytes); err != nil {
		t.Fatal(err)
	}
	methods := f.methods()
	want := []string{"chat.postMessage", "files.getUploadURLExternal", "files.completeUploadExternal"}
	tail := methods[len(methods)-len(want):]
	if strings.Join(tail, ",") != strings.Join(want, ",") {
		t.Fatalf("methods = %v", methods)
	}
	posts := f.callsTo("chat.postMessage")
	if len(posts) != 1 || posts[0].Form.Get("thread_ts") != "" {
		t.Fatalf("posts = %+v", posts)
	}
	if text := posts[0].Form.Get("text"); !strings.HasPrefix(text, "*pc/other-bbbbbb*") || !strings.HasSuffix(text, "\nthe chart") {
		t.Fatalf("header post = %q", text)
	}
	ts, ok := b.state.thread(sidB)
	if !ok {
		t.Fatal("the header post did not become the session's thread")
	}
	form := f.callsTo("files.completeUploadExternal")[0].Form
	if form.Get("thread_ts") != ts || form.Get("initial_comment") != "" {
		t.Fatalf("complete form = %v (thread %q; the caption already went with the header)", form, ts)
	}
	// A later text post goes into the same thread.
	if _, err := bus.Send(sidB, "slack", "more", ""); err != nil {
		t.Fatal(err)
	}
	drainJobs(t, b)
	if posts = f.callsTo("chat.postMessage"); len(posts) != 2 || posts[1].Form.Get("thread_ts") != ts {
		t.Fatalf("posts = %+v", posts)
	}
}

func TestPostImageWithoutCaptionOpensThreadWithHeaderOnly(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if err := b.PostImage(context.Background(), outboundFor(bus, sidB, "", "  "), "", pngBytes); err != nil {
		t.Fatal(err)
	}
	if text := f.callsTo("chat.postMessage")[0].Form.Get("text"); strings.Contains(text, "\n") {
		t.Fatalf("header post = %q", text)
	}
	if got := f.callsTo("files.getUploadURLExternal")[0].Form.Get("filename"); got != "image" {
		t.Fatalf("filename = %q", got)
	}
}

func TestPostImageSlackErrorIsCodeOnly(t *testing.T) {
	b, f, bus := newTestBridge(t)
	f.setFail("files.completeUploadExternal", "invalid_channel")
	err := b.PostImage(context.Background(), outboundFor(bus, sidA, "flyer", "x"), "a.png", pngBytes)
	if err == nil || err.Error() != "invalid_channel" {
		t.Fatalf("err = %v", err)
	}
	assertSafeError(t, f, err)
}

func TestPostImageUploadFailuresNeverLeakTheURL(t *testing.T) {
	for _, mode := range []string{"500", "hangup"} {
		b, f, bus := newTestBridge(t)
		f.setUploadMode(mode)
		err := b.PostImage(context.Background(), outboundFor(bus, sidA, "flyer", "x"), "a.png", pngBytes)
		if err == nil || err.Error() != "request_failed" {
			t.Fatalf("%s: err = %v", mode, err)
		}
		assertSafeError(t, f, err)
		if n := len(f.callsTo("files.completeUploadExternal")); n != 0 {
			t.Fatalf("%s: completed a failed upload", mode)
		}
		// The api-level error (what gets logged) is URL-free too.
		uploadURL, _, errGet := b.api.getUploadURL(context.Background(), "xoxb-test", "a.png", len(pngBytes))
		if errGet != nil {
			t.Fatal(errGet)
		}
		errUpload := b.api.uploadFile(context.Background(), uploadURL, pngBytes)
		if errUpload == nil {
			t.Fatalf("%s: upload succeeded", mode)
		}
		assertSafeError(t, f, errUpload)
	}
}

func TestSanitizeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"shot.png":          "shot.png",
		"my shot (1).png":   "my_shot_1_.png",
		"":                  "image",
		"...":               "image",
		"ünïcödé.webp":      "n_c_d_.webp",
		"a\nb.gif":          "a_b.gif",
		"Screen-Shot_2.JPG": "Screen-Shot_2.JPG",
	} {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeFilename(strings.Repeat("a", 300) + ".png"); len(got) > maxFilenameLen || !strings.HasSuffix(got, ".png") {
		t.Errorf("long name = %q (%d)", got, len(got))
	}
}
