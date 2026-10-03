package slackbridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

const (
	sidA = "aaaaaa11-2222-3333-4444-555555555555"
	sidB = "bbbbbb11-2222-3333-4444-555555555555"
)

func testConfig(f *fakeSlack, dir string) Config {
	return Config{
		BotToken:      "xoxb-test",
		AppToken:      "xapp-test",
		Channel:       "#agents",
		AllowedEmails: []string{"alex@example.com", "nobody@example.com"},
		StatePath:     filepath.Join(dir, "slack-state.json"),
		APIBase:       f.apiBase(),
	}
}

// newTestBridge returns a resolved bridge attached to a bus with two
// sessions: sidA named "flyer" and sidB unnamed. Jobs run only via drainJobs.
func newTestBridge(t *testing.T) (*Bridge, *fakeSlack, *agentbus.Store) {
	t.Helper()
	f := newFakeSlack(t)
	bus := agentbus.NewStore("", nil)
	bus.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	bus.Hello(sidB, "pc", "/work/other", "", true)
	b, err := New(testConfig(f, t.TempDir()), bus)
	if err != nil || b == nil {
		t.Fatalf("New = %v, %v", b, err)
	}
	b.backoff = func(int) time.Duration { return 0 }
	b.retryDelay = 0
	if err = b.resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	bus.SetBridge(b)
	return b, f, bus
}

func drainJobs(t *testing.T, b *Bridge) {
	t.Helper()
	for {
		select {
		case j := <-b.jobs:
			if err := j(context.Background()); err != nil {
				t.Fatalf("job: %v", err)
			}
		default:
			return
		}
	}
}

func TestNewIsOffWhenIncomplete(t *testing.T) {
	f := newFakeSlack(t)
	full := testConfig(f, t.TempDir())
	for _, cfg := range []Config{
		{AppToken: full.AppToken, Channel: full.Channel, AllowedEmails: full.AllowedEmails},
		{BotToken: full.BotToken, Channel: full.Channel, AllowedEmails: full.AllowedEmails},
		{BotToken: full.BotToken, AppToken: full.AppToken, AllowedEmails: full.AllowedEmails},
		{BotToken: full.BotToken, AppToken: full.AppToken, Channel: full.Channel},
	} {
		if b, err := New(cfg, agentbus.NewStore("", nil)); b != nil || err != nil {
			t.Fatalf("New(%+v) = %v, %v", cfg, b, err)
		}
	}
}

func TestResolve(t *testing.T) {
	b, _, _ := newTestBridge(t)
	if b.channelID != "CAGENTS" || b.botUserID != "UBOT" {
		t.Fatalf("channel %q bot %q", b.channelID, b.botUserID)
	}
	if got := b.Users(); len(got) != 1 || got[0] != "alex" {
		t.Fatalf("users = %v (the unknown email must be skipped)", got)
	}
}

func TestResolveFailsWhenNoUserMatches(t *testing.T) {
	f := newFakeSlack(t)
	cfg := testConfig(f, t.TempDir())
	cfg.AllowedEmails = []string{"nobody@example.com"}
	b, _ := New(cfg, agentbus.NewStore("", nil))
	if err := b.resolve(context.Background()); err == nil {
		t.Fatal("resolved with no allowed users")
	}
	f.setFail("auth.test", "invalid_auth")
	cfg.AllowedEmails = []string{"alex@example.com"}
	b, _ = New(cfg, agentbus.NewStore("", nil))
	if err := b.resolve(context.Background()); err == nil || strings.Contains(err.Error(), "xoxb") {
		t.Fatalf("err = %v", err)
	}
}

func TestPostOpensThreadThenReplies(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if _, err := bus.Send(sidA, "slack", "starting @alex <!channel>", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Send(sidA, "slack", "done", ""); err != nil {
		t.Fatal(err)
	}
	drainJobs(t, b)
	posts := f.callsTo("chat.postMessage")
	if len(posts) != 2 {
		t.Fatalf("posts = %+v", posts)
	}
	first, second := posts[0].Form, posts[1].Form
	if first.Get("channel") != "CAGENTS" || first.Get("thread_ts") != "" {
		t.Fatalf("first = %v", first)
	}
	wantText := "*flyer* · pc/flyer-aaaaaa · pc · `/work/flyer`\nstarting <@UALEX> &lt;!channel&gt;"
	if first.Get("text") != wantText {
		t.Fatalf("first text = %q", first.Get("text"))
	}
	ts, _ := b.state.thread(sidA)
	if second.Get("thread_ts") != ts || second.Get("text") != "done" {
		t.Fatalf("second = %v (thread %q)", second, ts)
	}
}

func TestQueueDropsOldestWhenFull(t *testing.T) {
	b, _, _ := newTestBridge(t)
	for i := 0; i < jobQueueSize+5; i++ {
		b.Post(agentbus.Outbound{SessionID: sidA, Body: "x"})
	}
	if len(b.jobs) != jobQueueSize {
		t.Fatalf("queued = %d", len(b.jobs))
	}
}

func TestRunJobsRetriesOnce(t *testing.T) {
	b, _, _ := newTestBridge(t)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	done := make(chan struct{})
	b.enqueue(func(context.Context) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded
		}
		close(done)
		return nil
	})
	go b.runJobs(ctx)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("job not retried")
	}
	cancel()
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}
