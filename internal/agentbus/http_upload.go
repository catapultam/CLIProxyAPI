package agentbus

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
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
	// maxJSONUploadBytes caps a JSON upload's body: a 10 MiB image in
	// base64 is about 13.4 MiB, plus the small fields.
	maxJSONUploadBytes = 14 << 20
	// maxFieldBytes caps the session and caption fields.
	maxFieldBytes = 4 << 10
	// maxUploads caps the image uploads in progress at once; each holds up
	// to maxImageBytes in memory.
	maxUploads = 4

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
	replyTo  string
	to       string
	filename string
	data     []byte
	hasFile  bool
}

// uploadError is a request the handler refuses, with its HTTP status.
type uploadError struct {
	status int
	msg    string
}

// handleSlackUpload posts an image into the sending session's Slack thread:
// its own, or, with a reply_to the bridge delivered to that session, the
// thread that message came from. With to = "slack@<label>" it goes to that
// allowed user's DM instead (the same rule as Send; reply_to is ignored).
// The client never names a thread or channel. The body is
// multipart/form-data (streamed part by part into memory, never spooled to
// disk) or application/json with the image in base64; both go through the
// same checks.
func (s *Store) handleSlackUpload(c *gin.Context) {
	bridge := s.currentBridge()
	poster, ok := bridge.(ImagePoster)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "slack is not enabled"})
		return
	}
	select {
	case s.uploadSlots <- struct{}{}:
		defer func() { <-s.uploadSlots }()
	default:
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many image uploads in progress"})
		return
	}
	var form uploadForm
	var errForm *uploadError
	if isJSONBody(c.Request) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxJSONUploadBytes)
		form, errForm = readUploadJSON(c.Request)
	} else {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxImageBytes+uploadFormHeadroom)
		form, errForm = readUploadForm(c.Request)
	}
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
	out.ReplyTo = cleanReplyTo(form.replyTo)
	if errTo := uploadTarget(bridge, &out, form.to); errTo != nil {
		c.JSON(errTo.status, gin.H{"error": errTo.msg})
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

// uploadTarget applies an upload's to field: empty or "slack" keeps the
// session's thread, "slack@<label>" makes out a DM to that allowed user
// (dropping its reply_to), and anything else is refused. It calls the
// bridge's Users, so the caller must not hold s.mu.
func uploadTarget(bridge Bridge, out *Outbound, to string) *uploadError {
	if to == "" || isSlackAddress(to) {
		return nil
	}
	label, dm := slackDMLabel(to)
	if !dm {
		return &uploadError{http.StatusBadRequest, `to must be "slack" or "slack@<label>"`}
	}
	canonical, errLabel := slackDMTarget(bridge, label)
	if errLabel != nil {
		return &uploadError{http.StatusNotFound, slackUserNotFound}
	}
	out.DM, out.ReplyTo = canonical, ""
	return nil
}

// readUploadForm streams the multipart body: session, caption, reply_to and
// to up to maxFieldBytes each, the file up to maxImageBytes. Other parts are
// skipped. A repeated field keeps its last value; a second file is refused.
func readUploadForm(r *http.Request) (uploadForm, *uploadError) {
	var form uploadForm
	mr, errReader := r.MultipartReader()
	if errReader != nil {
		return form, &uploadError{http.StatusBadRequest, "expected a multipart/form-data body with session, caption and file, or application/json with data_base64"}
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
		case "session", "caption", "reply_to", "to":
			value, errRead := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
			if errRead != nil {
				return form, readFailure(errRead)
			}
			if len(value) > maxFieldBytes {
				return form, &uploadError{http.StatusBadRequest, name + " exceeds 4 KiB"}
			}
			v := strings.TrimSpace(string(value))
			switch name {
			case "session":
				form.session = v
			case "caption":
				form.caption = v
			case "to":
				form.to = v
			default:
				form.replyTo = v
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

// isJSONBody reports whether the request declares an application/json body.
func isJSONBody(r *http.Request) bool {
	mediaType, _, errParse := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return errParse == nil && mediaType == "application/json"
}

// uploadJSON is the JSON upload body. The image is data_base64, standard
// padded base64.
type uploadJSON struct {
	Session    string  `json:"session"`
	Caption    string  `json:"caption"`
	ReplyTo    string  `json:"reply_to"`
	To         string  `json:"to"`
	Filename   string  `json:"filename"`
	DataBase64 *string `json:"data_base64"`
}

// readUploadJSON decodes a JSON upload into the same form the multipart
// reader fills, with the same field and image caps.
func readUploadJSON(r *http.Request) (uploadForm, *uploadError) {
	var form uploadForm
	var req uploadJSON
	if errDecode := json.NewDecoder(r.Body).Decode(&req); errDecode != nil {
		return form, bodyFailure(errDecode, "invalid JSON body")
	}
	for _, field := range []struct{ name, value string }{
		{"session", req.Session}, {"caption", req.Caption}, {"reply_to", req.ReplyTo}, {"to", req.To}, {"filename", req.Filename},
	} {
		if len(field.value) > maxFieldBytes {
			return form, &uploadError{http.StatusBadRequest, field.name + " exceeds 4 KiB"}
		}
	}
	form.session = strings.TrimSpace(req.Session)
	form.caption = strings.TrimSpace(req.Caption)
	form.replyTo = strings.TrimSpace(req.ReplyTo)
	form.to = strings.TrimSpace(req.To)
	form.filename = strings.TrimSpace(req.Filename)
	if req.DataBase64 == nil {
		return form, nil
	}
	if len(*req.DataBase64) > base64.StdEncoding.EncodedLen(maxImageBytes) {
		return form, &uploadError{http.StatusRequestEntityTooLarge, errImageTooLarge}
	}
	data, errDecode := base64.StdEncoding.DecodeString(*req.DataBase64)
	if errDecode != nil {
		return form, &uploadError{http.StatusBadRequest, "data_base64 is not valid base64"}
	}
	if len(data) > maxImageBytes {
		return form, &uploadError{http.StatusRequestEntityTooLarge, errImageTooLarge}
	}
	form.data, form.hasFile = data, true
	return form, nil
}

// readFailure maps a multipart body read error.
func readFailure(err error) *uploadError {
	return bodyFailure(err, "invalid multipart body")
}

// bodyFailure maps a body read error: over the MaxBytesReader cap is 413,
// anything else a malformed body (msg).
func bodyFailure(err error, msg string) *uploadError {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return &uploadError{http.StatusRequestEntityTooLarge, errImageTooLarge}
	}
	return &uploadError{http.StatusBadRequest, msg}
}
