package slackbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

// imageRoute mounts bus's agentbus routes, for GET /image.
func imageRoute(bus *agentbus.Store) http.Handler {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	bus.Register(r.Group("/v1/agentbus"))
	return r
}

// fetchImage GETs /image for sid and id, and returns the status and body.
func fetchImage(t *testing.T, r http.Handler, sid, id string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/agentbus/image?session="+sid+"&id="+id, nil))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

// dmWithFiles is a file_share message in alex's DM with the bot.
func dmWithFiles(text, ts string, files ...slackFile) messageEvent {
	ev := dmMsg("UALEX", text, ts, "")
	ev.Subtype = "file_share"
	ev.Files = files
	return ev
}

func TestMessageEventParsesFiles(t *testing.T) {
	raw := `{"type":"message","subtype":"file_share","channel":"DUALEX","channel_type":"im","user":"UALEX","text":"look","ts":"1.2",
		"files":[{"id":"F1","name":"shot.png","mimetype":"image/png","size":1234,"url_private_download":"https://files.slack.com/files-pri/T1-F1/download/shot.png","url_private":"x"}]}`
	var ev messageEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	want := slackFile{ID: "F1", Name: "shot.png", Mimetype: "image/png", Size: 1234, URLPrivateDownload: "https://files.slack.com/files-pri/T1-F1/download/shot.png"}
	if len(ev.Files) != 1 || ev.Files[0] != want {
		t.Fatalf("files = %+v", ev.Files)
	}
}

func TestPendingImages(t *testing.T) {
	got := pendingImages([]slackFile{
		{ID: "F1", Name: "a.png", Mimetype: "IMAGE/PNG", Size: 5, URLPrivateDownload: "https://files.slack.com/a"},
		{ID: "F2", Mimetype: "image/png", Size: 5},
		{ID: "F3", Name: "doc.pdf", Mimetype: "application/pdf", Size: 5},
	})
	if len(got) != 3 || got[0] != (agentbus.PendingImage{Name: "a.png", Mime: "image/png", Size: 5}) {
		t.Fatalf("pending = %+v", got)
	}
	// No link: listed, never fetched. A file without a name is named by id.
	if got[1].Name != "F2" || !strings.Contains(got[1].Error, "no download link") {
		t.Fatalf("no link = %+v", got[1])
	}
	// Type rules are the Store's.
	if got[2].Error != "" {
		t.Fatalf("pdf = %+v", got[2])
	}
}

func TestInboundDMImageIsRelayed(t *testing.T) {
	b, f, bus := newTestBridge(t)
	r := imageRoute(bus)
	link := f.setFile("shot.png", fakeFile{ContentType: "image/png", Body: pngBytes})
	b.handleEvent("EvImg1", dmWithFiles("flyer: what is wrong here?", "1700000800.000001",
		slackFile{ID: "F1", Name: "shot.png", Mimetype: "image/png", Size: int64(len(pngBytes)), URLPrivateDownload: link},
		slackFile{ID: "F2", Name: "notes.pdf", Mimetype: "application/pdf", Size: 10, URLPrivateDownload: f.setFile("notes.pdf", fakeFile{ContentType: "application/pdf"})},
	))
	b.downloadsWG.Wait()
	msg := claimOne(t, bus, sidA)
	if !msg.FromUser || msg.Body != "what is wrong here?" || len(msg.Images) != 2 {
		t.Fatalf("msg = %+v", msg)
	}
	img, pdf := msg.Images[0], msg.Images[1]
	if img.ID == "" || img.Name != "shot.png" || img.Mime != "image/png" || pdf.ID != "" || !strings.Contains(pdf.Error, "only PNG") {
		t.Fatalf("refs = %+v", msg.Images)
	}
	gets := f.recordedFileGets()
	if len(gets) != 1 || gets[0].Path != "/files/shot.png" || gets[0].Auth != "Bearer xoxb-test" {
		t.Fatalf("file gets = %+v", gets)
	}
	status, body := fetchImage(t, r, sidA, img.ID)
	if status != http.StatusOK || body["name"] != "shot.png" || body["mime"] != "image/png" {
		t.Fatalf("image = %d %v", status, body)
	}
	if data, _ := base64.StdEncoding.DecodeString(body["base64"].(string)); string(data) != string(pngBytes) {
		t.Fatal("image bytes differ")
	}
	if status, _ = fetchImage(t, r, sidB, img.ID); status != http.StatusNotFound {
		t.Fatalf("other session = %d", status)
	}
}

func TestInboundImageOnlyMessageIsDelivered(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvImg2", dmMsg("UALEX", "flyer: hi", "1700000810.000001", ""))
	claimOne(t, bus, sidA)
	link := f.setFile("only.png", fakeFile{ContentType: "image/png", Body: pngBytes})
	// No text: it goes to the agent the user last wrote to.
	b.handleEvent("EvImg3", dmWithFiles("", "1700000811.000001", slackFile{ID: "F9", Name: "only.png", Mimetype: "image/png", Size: 5, URLPrivateDownload: link}))
	b.downloadsWG.Wait()
	msg := claimOne(t, bus, sidA)
	if msg.Body != "" || len(msg.Images) != 1 || msg.Images[0].ID == "" {
		t.Fatalf("msg = %+v", msg)
	}
	drainJobs(t, b)
	for _, p := range f.callsTo("chat.postMessage") {
		if strings.Contains(p.Form.Get("text"), "Not delivered") {
			t.Fatalf("refused: %s", p.Form.Get("text"))
		}
	}
}

func TestInboundImageDownloadFailures(t *testing.T) {
	b, f, bus := newTestBridge(t)
	r := imageRoute(bus)
	files := []slackFile{
		{ID: "F1", Name: "forbidden.png", Mimetype: "image/png", Size: 5, URLPrivateDownload: f.setFile("forbidden.png", fakeFile{Status: http.StatusForbidden})},
		{ID: "F2", Name: "login.png", Mimetype: "image/png", Size: 5, URLPrivateDownload: f.setFile("login.png", fakeFile{ContentType: "text/html; charset=utf-8", Body: []byte("<html>sign in</html>")})},
		{ID: "F3", Name: "elsewhere.png", Mimetype: "image/png", Size: 5, URLPrivateDownload: "https://attacker.example/x.png"},
		{ID: "F4", Name: "fake.png", Mimetype: "image/png", Size: 5, URLPrivateDownload: f.setFile("fake.png", fakeFile{ContentType: "image/png", Body: []byte("GIF89a-not-a-png")})},
	}
	b.handleEvent("EvImg4", dmWithFiles("flyer: four", "1700000820.000001", files...))
	b.downloadsWG.Wait()
	msg := claimOne(t, bus, sidA)
	if len(msg.Images) != 4 {
		t.Fatalf("refs = %+v", msg.Images)
	}
	for i, want := range []string{"files:read", "files:read", "not a Slack link", "not the image type"} {
		status, body := fetchImage(t, r, sidA, msg.Images[i].ID)
		errText, _ := body["error"].(string)
		if status != http.StatusBadGateway || !strings.Contains(errText, want) {
			t.Fatalf("%s = %d %v, want %q", files[i].Name, status, body, want)
		}
		for _, secret := range []string{"xoxb", f.URL, "attacker.example"} {
			if strings.Contains(errText, secret) {
				t.Fatalf("error %q leaks %q", errText, secret)
			}
		}
	}
	// The foreign link was refused before any request (TestDownloadFileLimits
	// covers that the token never leaves): only the three Slack links were
	// fetched.
	if gets := f.recordedFileGets(); len(gets) != 3 {
		t.Fatalf("file gets = %+v", gets)
	}
}

func TestDownloadFileLimits(t *testing.T) {
	f := newFakeSlack(t)
	a := newAPI(f.apiBase())
	big := make([]byte, 64)
	copy(big, pngBytes)
	link := f.setFile("big.png", fakeFile{ContentType: "image/png", Body: big})
	if _, err := a.downloadFile(context.Background(), "xoxb-1", link, 63); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("over the cap = %v", err)
	}
	if data, err := a.downloadFile(context.Background(), "xoxb-1", link, 64); err != nil || len(data) != 64 {
		t.Fatalf("at the cap = %d, %v", len(data), err)
	}
	if _, err := a.downloadFile(context.Background(), "xoxb-1", f.URL+"/files/missing.png", 64); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("missing = %v", err)
	}
	for _, raw := range []string{"http://files.slack.com/x", "https://slack.com.attacker.example/x", "https://user:pw@files.slack.com/x", "ftp://x", ""} {
		if _, err := a.downloadFile(context.Background(), "xoxb-1", raw, 64); err != errDownloadHost {
			t.Fatalf("%q = %v", raw, err)
		}
	}
	// A forced download may come as octet-stream; the Store sniffs it later.
	octet := f.setFile("octet.png", fakeFile{ContentType: "application/octet-stream", Body: pngBytes})
	if data, err := a.downloadFile(context.Background(), "xoxb-1", octet, 64); err != nil || string(data) != string(pngBytes) {
		t.Fatalf("octet-stream = %v", err)
	}
	if _, err := a.downloadFile(context.Background(), "xoxb-1", f.setFile("text.png", fakeFile{ContentType: "text/plain", Body: pngBytes}), 64); err == nil || !strings.Contains(err.Error(), "did not send an image") {
		t.Fatalf("text/plain = %v", err)
	}
	// A redirect to a host that isn't Slack's is not followed.
	away := f.setFile("away.png", fakeFile{Status: http.StatusFound, Location: "https://attacker.example/x.png"})
	if _, err := a.downloadFile(context.Background(), "xoxb-1", away, 64); err == nil || !strings.Contains(err.Error(), "HTTP 302") || strings.Contains(err.Error(), "attacker") {
		t.Fatalf("redirect = %v", err)
	}
	u, _ := url.Parse("https://files.slack.com/files-pri/T1-F1/download/a.png")
	if !a.downloadAllowed(u) {
		t.Fatal("files.slack.com refused")
	}
}

func TestGuestImagesAreNotRelayed(t *testing.T) {
	b, f, bus := newTestBridge(t)
	linkGroup(t, b, bus, sidA, "flyer", "EvGL1")
	ev := foreignMsg("UBOB", "look at this", "1700009100.000001", "")
	ev.Subtype = "file_share"
	ev.Files = []slackFile{{ID: "F1", Name: "a.png", Mimetype: "image/png", Size: 5, URLPrivateDownload: f.setFile("a.png", fakeFile{ContentType: "image/png", Body: pngBytes})}}
	b.handleEvent("EvGI1", ev)
	drainJobs(t, b)
	b.downloadsWG.Wait()
	if m := claimOne(t, bus, sidA); !m.Guest || m.Images != nil {
		t.Fatalf("guest msg = %+v", m)
	}
	if gets := f.recordedFileGets(); len(gets) != 0 {
		t.Fatalf("a guest's file was fetched: %+v", gets)
	}
}
