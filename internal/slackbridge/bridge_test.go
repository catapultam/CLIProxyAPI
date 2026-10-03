package slackbridge

import (
	"context"
	"path/filepath"
	"runtime"
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

// drainJobs runs queued jobs the way runJobs picks them: command jobs first.
func drainJobs(t *testing.T, b *Bridge) {
	t.Helper()
	for {
		var j job
		select {
		case j = <-b.commands:
		default:
			select {
			case j = <-b.jobs:
			default:
				return
			}
		}
		if err := j(context.Background()); err != nil {
			t.Fatalf("job: %v", err)
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

// openersFor counts the callers holding or waiting for sid's opening lock.
func openersFor(b *Bridge, sid string) int {
	b.openMu.Lock()
	defer b.openMu.Unlock()
	if g := b.opening[sid]; g != nil {
		return g.refs
	}
	return 0
}

func TestConcurrentOpenersPostOneHeader(t *testing.T) {
	b, f, bus := newTestBridge(t)
	entered, release := f.holdPosts()
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	o := outboundFor(bus, sidB, "", "")
	type result struct {
		ts     string
		opened bool
		err    error
	}
	results := make(chan result, 2)
	open := func() {
		ts, opened, err := b.threadFor(context.Background(), o, "")
		results <- result{ts, opened, err}
	}
	timeout := time.After(5 * time.Second)
	go open()
	select {
	case <-entered:
	case <-timeout:
		t.Fatal("the first opener never posted its header")
	}
	// The first opener is inside chat.postMessage. Start the second and wait
	// until it queues behind the first, failing if it posts a header too.
	go open()
	for openersFor(b, sidB) < 2 {
		select {
		case <-entered:
			t.Fatal("a second opener posted its own header")
		case <-timeout:
			t.Fatal("the second opener never queued for the lock")
		default:
			runtime.Gosched()
		}
	}
	release()
	released = true
	r1, r2 := <-results, <-results
	if r1.err != nil || r2.err != nil {
		t.Fatalf("errors: %v, %v", r1.err, r2.err)
	}
	if r1.ts == "" || r1.ts != r2.ts || r1.opened == r2.opened {
		t.Fatalf("results = %+v, %+v (want one thread, opened once)", r1, r2)
	}
	if posts := f.callsTo("chat.postMessage"); len(posts) != 1 {
		t.Fatalf("header posts = %d, want 1", len(posts))
	}
	if n := openersFor(b, sidB); n != 0 {
		t.Fatalf("opening lock still held or leaked: refs %d", n)
	}
}

func TestOpenerWaitingForLockHonorsContext(t *testing.T) {
	b, f, bus := newTestBridge(t)
	entered, release := f.holdPosts()
	o := outboundFor(bus, sidB, "", "")
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _, _ = b.threadFor(context.Background(), o, "")
	}()
	// Let the first opener finish (it writes the state file) before the
	// test's temp dir is removed.
	defer func() {
		release()
		<-firstDone
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first opener never posted its header")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := b.threadFor(ctx, o, ""); err == nil {
		t.Fatal("a canceled opener waited for the lock and returned no error")
	}
	if n := openersFor(b, sidB); n != 1 {
		t.Fatalf("refs after the canceled waiter = %d, want 1", n)
	}
}
