package slackbridge

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

func TestDialErrorHidesSocketTicket(t *testing.T) {
	// A control character makes url.Parse fail before any network access.
	_, _, errDial := websocket.DefaultDialer.Dial("wss://127.0.0.1:1/link/?ticket=SECRET-TICKET\x7f", nil)
	if errDial == nil {
		t.Fatal("malformed URL dialed")
	}
	if !strings.Contains(errDial.Error(), "SECRET-TICKET") {
		t.Fatalf("precondition: gorilla's error no longer includes the URL: %v", errDial)
	}
	if got := dialError(errDial).Error(); strings.Contains(got, "SECRET-TICKET") || got != "dial: malformed socket URL" {
		t.Fatalf("dialError = %q", got)
	}
	if got := dialError(errors.New("connection refused")).Error(); got != "dial: connection refused" {
		t.Fatalf("dialError = %q", got)
	}
}

func TestStopWithoutStartIsSafe(t *testing.T) {
	f := newFakeSlack(t)
	bus := agentbus.NewStore("", nil)
	b, _ := New(testConfig(f, t.TempDir()), bus)
	b.Stop()
	b.Stop()
}

func waitAck(t *testing.T, f *fakeSlack, want string) {
	t.Helper()
	select {
	case got := <-f.acks:
		if got != want {
			t.Fatalf("ack = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no ack for %s", want)
	}
}

func TestSocketDeliversAcksAndReconnects(t *testing.T) {
	f := newFakeSlack(t)
	bus := agentbus.NewStore("", nil)
	bus.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	b, err := New(testConfig(f, t.TempDir()), bus)
	if err != nil {
		t.Fatal(err)
	}
	b.backoff = func(int) time.Duration { return 0 }
	b.Start()

	// The first envelope only gets through once the socket is up; the ack
	// proves the event was handled first.
	f.push(`{"type":"events_api","envelope_id":"env-1","payload":{"event_id":"Ev1","event":{"type":"message","channel":"CAGENTS","user":"UALEX","text":"flyer: hi","ts":"5.1"}}}`)
	waitAck(t, f, "env-1")
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 || msgs[0].Body != "hi" || !msgs[0].FromUser {
		t.Fatalf("msgs = %+v", msgs)
	}
	attached := false
	for _, p := range bus.Peers() {
		attached = attached || p.Address == agentbus.SlackAddress
	}
	if !attached {
		t.Fatal("bridge not attached to the bus")
	}

	f.push(`{"type":"disconnect","reason":"refresh_requested"}`)
	f.push(`{"type":"events_api","envelope_id":"env-2","payload":{"event_id":"Ev2","event":{"type":"message","channel":"CAGENTS","user":"UALEX","text":"flyer: again","ts":"5.2"}}}`)
	waitAck(t, f, "env-2")
	if f.opens() < 2 {
		t.Fatalf("opens = %d, want a reconnect", f.opens())
	}

	b.Stop()
	for _, p := range bus.Peers() {
		if p.Address == agentbus.SlackAddress {
			t.Fatal("still attached after Stop")
		}
	}
}

func TestStopBeforeResolveSucceeds(t *testing.T) {
	f := newFakeSlack(t)
	f.setFail("auth.test", "invalid_auth")
	b, _ := New(testConfig(f, t.TempDir()), agentbus.NewStore("", nil))
	b.backoff = func(int) time.Duration { return time.Hour }
	b.Start()
	done := make(chan struct{})
	go func() { b.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung while resolve was backing off")
	}
}
