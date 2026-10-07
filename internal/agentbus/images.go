package agentbus

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// Images from Slack: an allowed user's message can carry images. The bridge
// delivers the message with an ImageRef for each one at once and downloads
// the files from Slack in the background; the session's mod gets them from
// GET /image. They are kept in memory only, never in the state file.
const (
	// MaxImagesPerMessage caps the images relayed from one Slack message.
	MaxImagesPerMessage = 4
	// MaxImageBytes caps one image relayed from Slack, the same cap as an
	// image posted to Slack.
	MaxImageBytes = maxImageBytes
	// maxImageRefs caps the refs one message carries, relayed or not.
	maxImageRefs = 10
	// maxImageNameBytes caps a file name as a ref and /image show it.
	maxImageNameBytes = 200
	// imageTTL is how long a relayed image stays readable.
	imageTTL = time.Hour
	// imageForgetAfter is when an expired image's record goes too; until
	// then /image answers 410 for it.
	imageForgetAfter = 24 * time.Hour
	// imageStoreCap caps the bytes of all images kept; the oldest go first.
	imageStoreCap = 64 << 20
	// defaultImageWait bounds how long /image waits for a download.
	defaultImageWait = 20 * time.Second
)

// MinImageModVersion is the oldest agentbus mod that gets the images a
// message carries (Message.Images). An older waiter ignores them, so
// ClaimForWait adds a line about them to the body instead.
const MinImageModVersion = "0.4.1"

// validImageID is the shape of an id from newImageID.
var validImageID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ImageRef is an image attached to a Slack message, as the message carries
// it. ID is empty, and Error says why, when the file was not relayed (not a
// relayed image type, too large, too many); otherwise GET /image gets it.
type ImageRef struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name"`
	Mime  string `json:"mime,omitempty"`
	Size  int64  `json:"size,omitempty"`
	Error string `json:"error,omitempty"`
}

// PendingImage is a file the bridge asks DeliverViaImages to attach. With
// Error set it is only listed as not relayed.
type PendingImage struct {
	Name  string
	Mime  string
	Size  int64
	Error string
}

// noteImagesForOldWaiter adds a line to the body of each message in msgs
// that carries Images, for a waiter older than MinImageModVersion: it says
// how many files there are and that the plugin can't show them. Without it
// an image-only message would reach the model as an empty one.
func noteImagesForOldWaiter(msgs []Message) {
	for i := range msgs {
		if n := len(msgs[i].Images); n > 0 {
			note := fmt.Sprintf("[%d attached file(s) that this agentbus plugin can't show; update it to %s or later.]", n, MinImageModVersion)
			if strings.TrimSpace(msgs[i].Body) == "" {
				msgs[i].Body = note
			} else {
				msgs[i].Body += "\n\n" + note
			}
		}
	}
}

// IsRelayedImageType reports whether mime is an image type relayed from
// Slack: PNG, JPEG, GIF or WebP, the types Slack shows inline.
func IsRelayedImageType(mime string) bool { return imageTypes[mime] }

// imageEntry is one relayed image. ready is closed once the download
// finished or failed; data and err are set before that and never change
// after. gone marks an image dropped by its TTL or the store cap.
type imageEntry struct {
	session string
	name    string
	mime    string
	created time.Time
	ready   chan struct{}
	done    bool
	data    []byte
	err     string
	gone    bool
}

// imageStore holds the relayed images. Its lock may be taken while holding
// Store.mu, never the other way round.
type imageStore struct {
	mu    sync.Mutex
	byID  map[string]*imageEntry
	bytes int
}

func newImageID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// cleanImageName keeps a file name to one line of printable text, capped.
func cleanImageName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if len(name) > maxImageNameBytes {
		name = name[:maxImageNameBytes]
		for !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
	}
	if name == "" {
		name = "image"
	}
	return name
}

// registerImagesLocked makes a pending entry for each relayable image in
// images, for session sid, and returns the refs the message carries. The
// caller holds s.mu.
func (s *Store) registerImagesLocked(sid string, images []PendingImage) []ImageRef {
	if len(images) == 0 {
		return nil
	}
	if len(images) > maxImageRefs {
		images = images[:maxImageRefs]
	}
	now := s.now()
	s.images.mu.Lock()
	defer s.images.mu.Unlock()
	s.sweepImagesLocked(now)
	if s.images.byID == nil {
		s.images.byID = make(map[string]*imageEntry)
	}
	refs := make([]ImageRef, 0, len(images))
	relayed := 0
	for _, img := range images {
		ref := ImageRef{Name: cleanImageName(img.Name), Size: max(img.Size, 0), Error: strings.TrimSpace(img.Error)}
		switch {
		case ref.Error != "":
		case !IsRelayedImageType(img.Mime):
			ref.Error = "only PNG, JPEG, GIF and WebP images are relayed"
		case img.Size > MaxImageBytes:
			ref.Error = "larger than 10 MiB"
		case relayed >= MaxImagesPerMessage:
			ref.Error = "more than 4 images in one message"
		}
		if ref.Error == "" {
			relayed++
			ref.ID, ref.Mime = newImageID(), img.Mime
			s.images.byID[ref.ID] = &imageEntry{session: sid, name: ref.Name, mime: img.Mime, created: now, ready: make(chan struct{})}
		}
		refs = append(refs, ref)
	}
	return refs
}

// sweepImagesLocked drops the data of images older than imageTTL, and the
// records of those older than imageForgetAfter. The caller holds images.mu.
func (s *Store) sweepImagesLocked(now time.Time) {
	for id, e := range s.images.byID {
		age := now.Sub(e.created)
		if age >= imageForgetAfter {
			s.dropImageLocked(e)
			delete(s.images.byID, id)
			continue
		}
		if age >= imageTTL && !e.gone {
			s.dropImageLocked(e)
		}
	}
}

// dropImageLocked marks e gone and frees its data. A pending download ends
// as gone too, so a waiter stops waiting. The caller holds images.mu.
func (s *Store) dropImageLocked(e *imageEntry) {
	s.images.bytes -= len(e.data)
	e.data, e.gone = nil, true
	if !e.done {
		e.done = true
		close(e.ready)
	}
}

// FinishImage stores the downloaded bytes of image id. data must sniff as
// the image type the ref declared and be at most MaxImageBytes, else the
// image fails instead. To keep under the store cap, the oldest images are
// dropped first. An unknown or already finished id is ignored.
func (s *Store) FinishImage(id string, data []byte) {
	s.images.mu.Lock()
	defer s.images.mu.Unlock()
	e, ok := s.images.byID[id]
	if !ok || e.done {
		return
	}
	switch got := http.DetectContentType(data); {
	case len(data) == 0:
		s.failImageLocked(e, "the download was empty")
		return
	case len(data) > MaxImageBytes:
		s.failImageLocked(e, "the image is larger than 10 MiB")
		return
	case got != e.mime:
		s.failImageLocked(e, "the file is not the image type Slack declared")
		return
	}
	for s.images.bytes+len(data) > imageStoreCap {
		var oldest *imageEntry
		for _, other := range s.images.byID {
			if len(other.data) > 0 && (oldest == nil || other.created.Before(oldest.created)) {
				oldest = other
			}
		}
		if oldest == nil {
			break
		}
		s.dropImageLocked(oldest)
	}
	e.data = data
	s.images.bytes += len(data)
	e.done = true
	close(e.ready)
}

// FailImage records why image id could not be downloaded. msg must be safe
// to show the agent: no tokens or URLs. An unknown or already finished id
// is ignored.
func (s *Store) FailImage(id, msg string) {
	s.images.mu.Lock()
	defer s.images.mu.Unlock()
	if e, ok := s.images.byID[id]; ok && !e.done {
		s.failImageLocked(e, msg)
	}
}

func (s *Store) failImageLocked(e *imageEntry, msg string) {
	if msg = strings.TrimSpace(msg); msg == "" {
		msg = "the download failed"
	}
	e.err, e.done = msg, true
	close(e.ready)
}

// mayReadImageLocked reports whether session sid may read an image sent to
// owner: it is owner, or took owner over through a chain of handoffs
// (HandOff), as its inbox did. The caller holds s.mu.
func (s *Store) mayReadImageLocked(owner, sid string) bool {
	id := owner
	for hop := 0; hop <= maxMoveHops; hop++ {
		if id == sid {
			return true
		}
		sess, ok := s.byID[id]
		if !ok || sess.MovedTo == "" {
			return false
		}
		id = sess.MovedTo
	}
	return false
}

// handleImage hands session ?session= the image ?id= that a Slack message
// to it (or to a session it took over) carried, as JSON with the bytes in
// base64. Any other session gets the same 404 as an unknown id. It waits up
// to 20 s for a download in progress (then 504), answers 410 once the image
// expired and 502 with the reason when the download failed.
func (s *Store) handleImage(c *gin.Context) {
	sid, id := strings.TrimSpace(c.Query("session")), strings.TrimSpace(c.Query("id"))
	if sid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is required"})
		return
	}
	notFound := gin.H{"error": "no such image"}
	if !validImageID.MatchString(id) {
		c.JSON(http.StatusNotFound, notFound)
		return
	}
	s.images.mu.Lock()
	s.sweepImagesLocked(s.now())
	e, ok := s.images.byID[id]
	s.images.mu.Unlock()
	if !ok {
		c.JSON(http.StatusNotFound, notFound)
		return
	}
	s.mu.Lock()
	allowed := s.mayReadImageLocked(e.session, sid)
	s.mu.Unlock()
	if !allowed {
		c.JSON(http.StatusNotFound, notFound)
		return
	}
	wait := s.imageWait
	if wait <= 0 {
		wait = defaultImageWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-e.ready:
	case <-timer.C:
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "the image is still downloading from Slack"})
		return
	case <-c.Request.Context().Done():
		return
	}
	s.images.mu.Lock()
	data, errText, gone := e.data, e.err, e.gone
	s.images.mu.Unlock()
	switch {
	case gone:
		c.JSON(http.StatusGone, gin.H{"error": "the image expired"})
	case errText != "":
		c.JSON(http.StatusBadGateway, gin.H{"error": errText})
	case c.Query("raw") == "1":
		// The bytes themselves, so a session without the mod can save one with a plain
		// `curl -fsS -o <file>` (no jq or base64 tool needed, e.g. Git for Windows' bash).
		c.Header("Content-Disposition", "attachment")
		c.Data(http.StatusOK, e.mime, data)
	default:
		c.JSON(http.StatusOK, gin.H{"name": e.name, "mime": e.mime, "size": len(data), "base64": base64.StdEncoding.EncodeToString(data)})
	}
}
