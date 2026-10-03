// Package slackbridge connects the agentbus to one Slack channel over Socket
// Mode (catapultam fork).
package slackbridge

import (
	"context"
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
}

func newAPI(base string) *api {
	if base == "" {
		base = defaultAPIBase
	}
	return &api{base: base, hc: &http.Client{Timeout: apiCallTimeout}}
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

func (a *api) addReaction(ctx context.Context, token, channel, ts, name string) error {
	_, err := a.call(ctx, token, "reactions.add", url.Values{"channel": {channel}, "timestamp": {ts}, "name": {name}})
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.code == "already_reacted" {
		return nil
	}
	return err
}

func (a *api) openConnection(ctx context.Context, token string) (string, error) {
	body, err := a.call(ctx, token, "apps.connections.open", url.Values{})
	if err != nil {
		return "", err
	}
	return body.Get("url").String(), nil
}
