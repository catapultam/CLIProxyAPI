package slackbridge

import (
	"context"
	"errors"
	"testing"
)

func TestAPICalls(t *testing.T) {
	f := newFakeSlack(t)
	a := newAPI(f.apiBase())
	ctx := context.Background()

	if id, err := a.authTest(ctx, "xoxb-1"); err != nil || id != "UBOT" {
		t.Fatalf("authTest = %q, %v", id, err)
	}
	if got := f.callsTo("auth.test")[0].Auth; got != "Bearer xoxb-1" {
		t.Fatalf("auth header = %q", got)
	}
	for _, name := range []string{"agents", "#agents", "AGENTS", "CAGENTS"} {
		if id, err := a.findChannel(ctx, "xoxb-1", name); err != nil || id != "CAGENTS" {
			t.Fatalf("findChannel(%q) = %q, %v", name, id, err)
		}
	}
	if _, err := a.findChannel(ctx, "xoxb-1", "nope"); err == nil {
		t.Fatal("missing channel resolved")
	}
	if id, err := a.lookupByEmail(ctx, "xoxb-1", "Alex@Example.com"); err != nil || id != "UALEX" {
		t.Fatalf("lookupByEmail = %q, %v", id, err)
	}
	var apiErr *apiError
	if _, err := a.lookupByEmail(ctx, "xoxb-1", "ghost@example.com"); !errors.As(err, &apiErr) || apiErr.code != "users_not_found" {
		t.Fatalf("lookup missing = %v", err)
	}
	if label, isBot, err := a.userInfo(ctx, "xoxb-1", "UJANE"); err != nil || label != "Jane D" || isBot {
		t.Fatalf("userInfo = %q %v %v", label, isBot, err)
	}
	ts, err := a.postMessage(ctx, "xoxb-1", "CAGENTS", "hi", "")
	if err != nil || ts == "" {
		t.Fatalf("postMessage = %q, %v", ts, err)
	}
	if _, err = a.postMessage(ctx, "xoxb-1", "CAGENTS", "re", ts); err != nil {
		t.Fatal(err)
	}
	posts := f.callsTo("chat.postMessage")
	if posts[0].Form.Get("thread_ts") != "" || posts[1].Form.Get("thread_ts") != ts || posts[1].Form.Get("text") != "re" {
		t.Fatalf("posts = %+v", posts)
	}
	if err = a.addReaction(ctx, "xoxb-1", "CAGENTS", ts, "inbox_tray"); err != nil {
		t.Fatal(err)
	}
	url, err := a.openConnection(ctx, "xapp-1")
	if err != nil || url == "" {
		t.Fatalf("openConnection = %q, %v", url, err)
	}
}

func TestAPIErrorNeverContainsToken(t *testing.T) {
	f := newFakeSlack(t)
	f.setFail("auth.test", "invalid_auth")
	_, err := newAPI(f.apiBase()).authTest(context.Background(), "xoxb-secret")
	if err == nil || err.Error() != "slack auth.test: invalid_auth" {
		t.Fatalf("err = %v", err)
	}
}

func TestAddReactionIgnoresAlreadyReacted(t *testing.T) {
	f := newFakeSlack(t)
	f.setFail("reactions.add", "already_reacted")
	if err := newAPI(f.apiBase()).addReaction(context.Background(), "x", "C", "1.1", "inbox_tray"); err != nil {
		t.Fatalf("err = %v", err)
	}
}
