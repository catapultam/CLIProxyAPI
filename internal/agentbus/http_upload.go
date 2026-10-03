package agentbus

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	// maxImageBytes caps one image posted to Slack.
	maxImageBytes = 10 << 20
	// uploadFormHeadroom is what the multipart framing and the small fields
	// may add to the image.
	uploadFormHeadroom = 64 << 10
	// maxFieldBytes caps the session and caption fields.
	maxFieldBytes = 4 << 10

	errImageTooLarge = "image exceeds 10 MiB"
)

// imageTypes are the sniffed content types Slack shows inline.
var imageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

type uploadForm struct {
	session  string
	caption  string
	filename string
	data     []byte
	hasFile  bool
}

// uploadError is a request the handler refuses, with its HTTP status.
type uploadError struct {
	status int
	msg    string
}

// handleSlackUpload posts an image into the sending session's own Slack
// thread. The client never picks the thread or channel. The body is streamed
// part by part into memory and never spooled to disk.
func (s *Store) handleSlackUpload(c *gin.Context) {
	poster, ok := s.currentBridge().(ImagePoster)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "slack is not enabled"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxImageBytes+uploadFormHeadroom)
	form, errForm := readUploadForm(c.Request)
	if errForm != nil {
		c.JSON(errForm.status, gin.H{"error": errForm.msg})
		return
	}
	if form.session == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is required"})
		return
	}
	out, errOut := s.outboundFor(form.session, form.caption)
	if errOut != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is not a known session"})
		return
	}
	if !form.hasFile {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is required"})
		return
	}
	contentType := http.DetectContentType(form.data)
	if !imageTypes[contentType] {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "only PNG, JPEG, GIF or WebP images"})
		return
	}
	log.Infof("agentbus: %s is posting a %d-byte %s image to Slack", out.Address, len(form.data), contentType)
	if errPost := poster.PostImage(c.Request.Context(), out, form.filename, form.data); errPost != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "slack upload failed: " + errPost.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// readUploadForm streams the multipart body: session and caption up to
// maxFieldBytes each, the file up to maxImageBytes. Other parts are skipped.
func readUploadForm(r *http.Request) (uploadForm, *uploadError) {
	var form uploadForm
	mr, errReader := r.MultipartReader()
	if errReader != nil {
		return form, &uploadError{http.StatusBadRequest, "expected a multipart/form-data body with session, caption and file"}
	}
	for {
		part, errPart := mr.NextPart()
		if errors.Is(errPart, io.EOF) {
			return form, nil
		}
		if errPart != nil {
			return form, readFailure(errPart)
		}
		switch name := part.FormName(); name {
		case "session", "caption":
			value, errRead := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
			if errRead != nil {
				return form, readFailure(errRead)
			}
			if len(value) > maxFieldBytes {
				return form, &uploadError{http.StatusBadRequest, name + " exceeds 4 KiB"}
			}
			if name == "session" {
				form.session = strings.TrimSpace(string(value))
			} else {
				form.caption = strings.TrimSpace(string(value))
			}
		case "file":
			if form.hasFile {
				return form, &uploadError{http.StatusBadRequest, "send one file per upload"}
			}
			data, errRead := io.ReadAll(io.LimitReader(part, maxImageBytes+1))
			if errRead != nil {
				return form, readFailure(errRead)
			}
			if len(data) > maxImageBytes {
				return form, &uploadError{http.StatusRequestEntityTooLarge, errImageTooLarge}
			}
			form.data, form.filename, form.hasFile = data, part.FileName(), true
		}
		// NextPart skips whatever is left of an unread part.
	}
}

// readFailure maps a body read error: over the MaxBytesReader cap is 413,
// anything else a malformed body.
func readFailure(err error) *uploadError {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return &uploadError{http.StatusRequestEntityTooLarge, errImageTooLarge}
	}
	return &uploadError{http.StatusBadRequest, "invalid multipart body"}
}
