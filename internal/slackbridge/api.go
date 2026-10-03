// Package slackbridge connects the agentbus to one Slack channel over Socket
// Mode (catapultam fork).
package slackbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	defaultAPIBase = "https://slack.com/api/"
	// apiCallTimeout bounds one Web API call. Slack calls are control-plane
	// requests, not proxied upstream traffic (see the AGENTS.md exception).
	apiCallTimeout = 30 * time.Second
	maxAPIBody     = 4 << 20
)

// apiError is a Slack Web API "ok": false response.
type apiError struct{ method, code string }

func (e *apiError) Error() string { return "slack " + e.method + ": " + e.code }

type api struct {
	base string
	hc   *http.Client
	// upload sends raw bytes to pre-signed upload URLs. It doesn't follow
	// redirects, so a redirect's Location can't leak into an error.
	upload *http.Client
}

func newAPI(base string) *api {
	if base == "" {
		base = defaultAPIBase
	}
	return &api{
		base: base,
		hc:   &http.Client{Timeout: apiCallTimeout},
		upload: &http.Client{
			Timeout:       apiCallTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// call POSTs form params to a Web API method. A 429 is retried once after
// Retry-After.
func (a *api) call(ctx context.Context, token, method string, params url.Values) (gjson.Result, error) {
	for attempt := 0; ; attempt++ {
		req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, a.base+method, strings.NewReader(params.Encode()))
		if errReq != nil {
			return gjson.Result{}, fmt.Errorf("slack %s: %w", method, errReq)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, errDo := a.hc.Do(req)
		if errDo != nil {
			return gjson.Result{}, fmt.Errorf("slack %s: %w", method, errDo)
		}
		data, errRead := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("slack %s: close body: %v", method, errClose)
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt == 0 {
			wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			timer := time.NewTimer(time.Duration(wait) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return gjson.Result{}, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if errRead != nil {
			return gjson.Result{}, fmt.Errorf("slack %s: read: %w", method, errRead)
		}
		if resp.StatusCode != http.StatusOK {
			return gjson.Result{}, fmt.Errorf("slack %s: HTTP %d", method, resp.StatusCode)
		}
		body := gjson.ParseBytes(data)
		if !body.Get("ok").Bool() {
			return body, &apiError{method: method, code: body.Get("error").String()}
		}
		return body, nil
	}
}

func (a *api) authTest(ctx context.Context, token string) (string, error) {
	body, err := a.call(ctx, token, "auth.test", url.Values{})
	if err != nil {
		return "", err
	}
	return body.Get("user_id").String(), nil
}

// findChannel resolves a channel name (with or without #) or ID among the
// channels the bot can see.
func (a *api) findChannel(ctx context.Context, token, name string) (string, error) {
	want := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "#"))
	cursor := ""
	for {
		params := url.Values{"types": {"public_channel,private_channel"}, "exclude_archived": {"true"}, "limit": {"1000"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		body, err := a.call(ctx, token, "conversations.list", params)
		if err != nil {
			return "", err
		}
		for _, ch := range body.Get("channels").Array() {
			if strings.ToLower(ch.Get("name").String()) == want || strings.EqualFold(ch.Get("id").String(), want) {
				return ch.Get("id").String(), nil
			}
		}
		cursor = body.Get("response_metadata.next_cursor").String()
		if cursor == "" {
			return "", fmt.Errorf("slack channel %q not found (for a private channel, invite the bot first)", name)
		}
	}
}

func (a *api) lookupByEmail(ctx context.Context, token, email string) (string, error) {
	body, err := a.call(ctx, token, "users.lookupByEmail", url.Values{"email": {strings.TrimSpace(email)}})
	if err != nil {
		return "", err
	}
	return body.Get("user.id").String(), nil
}

// userInfo returns a display name for labelling and whether the user is a bot.
func (a *api) userInfo(ctx context.Context, token, userID string) (string, bool, error) {
	body, err := a.call(ctx, token, "users.info", url.Values{"user": {userID}})
	if err != nil {
		return "", false, err
	}
	user := body.Get("user")
	label := user.Get("profile.display_name").String()
	if label == "" {
		label = user.Get("profile.real_name").String()
	}
	if label == "" {
		label = user.Get("name").String()
	}
	return label, user.Get("is_bot").Bool(), nil
}

// openDM opens (or finds) the bot's direct message with userID and returns
// its channel id. It needs the im:write scope.
func (a *api) openDM(ctx context.Context, token, userID string) (string, error) {
	return a.openConversation(ctx, token, []string{userID})
}

// openConversation opens (or finds) the conversation of the bot with
// userIDs: a DM for one user (im:write), a group DM for more (mpim:write).
// It returns its channel id.
func (a *api) openConversation(ctx context.Context, token string, userIDs []string) (string, error) {
	const method = "conversations.open"
	body, err := a.call(ctx, token, method, url.Values{"users": {strings.Join(userIDs, ",")}})
	if err != nil {
		return "", err
	}
	channel := body.Get("channel.id").String()
	if channel == "" {
		return "", &apiError{method: method, code: "invalid_response"}
	}
	return channel, nil
}

func (a *api) postMessage(ctx context.Context, token, channel, text, threadTS string) (string, error) {
	params := url.Values{"channel": {channel}, "text": {text}}
	if threadTS != "" {
		params.Set("thread_ts", threadTS)
	}
	body, err := a.call(ctx, token, "chat.postMessage", params)
	if err != nil {
		return "", err
	}
	return body.Get("ts").String(), nil
}

// postEphemeral posts text in channel that only user sees.
func (a *api) postEphemeral(ctx context.Context, token, channel, user, text string) error {
	_, err := a.call(ctx, token, "chat.postEphemeral", url.Values{"channel": {channel}, "user": {user}, "text": {text}})
	return err
}

// openView opens a modal (view, as JSON) for the interaction triggerID
// came with.
func (a *api) openView(ctx context.Context, token, triggerID, view string) error {
	_, err := a.call(ctx, token, "views.open", url.Values{"trigger_id": {triggerID}, "view": {view}})
	return err
}

// permalink returns a link to message ts in channel.
func (a *api) permalink(ctx context.Context, token, channel, ts string) (string, error) {
	body, err := a.call(ctx, token, "chat.getPermalink", url.Values{"channel": {channel}, "message_ts": {ts}})
	if err != nil {
		return "", err
	}
	return body.Get("permalink").String(), nil
}

func (a *api) addReaction(ctx context.Context, token, channel, ts, name string) error {
	_, err := a.call(ctx, token, "reactions.add", url.Values{"channel": {channel}, "timestamp": {ts}, "name": {name}})
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.code == "already_reacted" {
		return nil
	}
	return err
}

// removeReaction takes the bot's reaction name off a message; one that isn't
// there (no_reaction) is fine.
func (a *api) removeReaction(ctx context.Context, token, channel, ts, name string) error {
	_, err := a.call(ctx, token, "reactions.remove", url.Values{"channel": {channel}, "timestamp": {ts}, "name": {name}})
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.code == "no_reaction" {
		return nil
	}
	return err
}

// getUploadURL starts an external file upload (files.upload is deprecated)
// and returns the pre-signed URL to POST the bytes to, and the file id.
func (a *api) getUploadURL(ctx context.Context, token, filename string, length int) (string, string, error) {
	const method = "files.getUploadURLExternal"
	body, err := a.call(ctx, token, method, url.Values{"filename": {filename}, "length": {strconv.Itoa(length)}})
	if err != nil {
		return "", "", err
	}
	uploadURL, fileID := body.Get("upload_url").String(), body.Get("file_id").String()
	if parsed, errParse := url.Parse(uploadURL); errParse != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || fileID == "" {
		return "", "", &apiError{method: method, code: "invalid_response"}
	}
	return uploadURL, fileID, nil
}

// uploadFile POSTs the raw bytes to a pre-signed upload URL. The URL is the
// credential here, so the request carries no bot token, and neither the URL
// nor the bytes ever go into an error or a log. Like the Web API calls, it is
// bounded by apiCallTimeout (control-plane traffic, see the AGENTS.md
// exception), through a.upload.
func (a *api) uploadFile(ctx context.Context, uploadURL string, data []byte) error {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(data))
	if errReq != nil {
		return errors.New("slack file upload: invalid upload URL")
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, errDo := a.upload.Do(req)
	if errDo != nil {
		// *url.Error quotes the URL; keep only what went wrong.
		var urlErr *url.Error
		if errors.As(errDo, &urlErr) {
			errDo = urlErr.Err
		}
		return fmt.Errorf("slack file upload: %w", errDo)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAPIBody))
	if errClose := resp.Body.Close(); errClose != nil {
		log.Debugf("slack file upload: close body: %v", errClose)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack file upload: HTTP %d", resp.StatusCode)
	}
	return nil
}

// completeUpload shares an uploaded file into a channel thread (or, with an
// empty threadTS, at the top level), with an optional comment posted
// alongside it.
func (a *api) completeUpload(ctx context.Context, token, fileID, title, channel, threadTS, comment string) error {
	files, errJSON := json.Marshal([]map[string]string{{"id": fileID, "title": title}})
	if errJSON != nil {
		return fmt.Errorf("slack files.completeUploadExternal: %w", errJSON)
	}
	params := url.Values{"files": {string(files)}, "channel_id": {channel}}
	if threadTS != "" {
		params.Set("thread_ts", threadTS)
	}
	if comment != "" {
		params.Set("initial_comment", comment)
	}
	_, err := a.call(ctx, token, "files.completeUploadExternal", params)
	return err
}

func (a *api) openConnection(ctx context.Context, token string) (string, error) {
	body, err := a.call(ctx, token, "apps.connections.open", url.Values{})
	if err != nil {
		return "", err
	}
	return body.Get("url").String(), nil
}
