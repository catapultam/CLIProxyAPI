package slackbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	log "github.com/sirupsen/logrus"
)

// dialError wraps a websocket dial error for logging. A *url.Error carries
// the whole socket URL, ticket included, so it is replaced outright.
func dialError(errDial error) error {
	var urlErr *url.Error
	if errors.As(errDial, &urlErr) {
		return errors.New("dial: malformed socket URL")
	}
	return fmt.Errorf("dial: %w", errDial)
}

type envelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload"`
}

type eventsPayload struct {
	EventID string       `json:"event_id"`
	Event   messageEvent `json:"event"`
}

// runSocket keeps a Socket Mode connection open until ctx ends. Slack sends
// pings and gorilla answers them, so there are no read deadlines.
func (b *Bridge) runSocket(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		connected, err := b.connectOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			attempt = 0
		}
		if err == nil {
			continue
		}
		attempt++
		log.Warnf("slack: socket: %v", err)
		if !b.sleep(ctx, b.backoff(attempt)) {
			return
		}
	}
}

// connectOnce runs one connection. It returns nil on a Slack-requested
// disconnect, and whether Slack said hello.
func (b *Bridge) connectOnce(ctx context.Context) (bool, error) {
	wsURL, err := b.api.openConnection(ctx, b.cfg.AppToken)
	if err != nil {
		return false, err
	}
	conn, _, errDial := b.dialer.DialContext(ctx, wsURL, nil)
	if errDial != nil {
		return false, dialError(errDial)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			log.Debugf("slack: close socket: %v", errClose)
		}
	}()
	connected := false
	for {
		var env envelope
		if errRead := conn.ReadJSON(&env); errRead != nil {
			return connected, fmt.Errorf("read: %w", errRead)
		}
		switch env.Type {
		case "hello":
			connected = true
		case "disconnect":
			return connected, nil
		case "events_api":
			var p eventsPayload
			if errJSON := json.Unmarshal(env.Payload, &p); errJSON != nil {
				log.Debugf("slack: bad events_api payload: %v", errJSON)
			} else {
				b.handleEvent(p.EventID, p.Event)
			}
		}
		if env.EnvelopeID != "" {
			if errAck := conn.WriteJSON(map[string]string{"envelope_id": env.EnvelopeID}); errAck != nil {
				return connected, fmt.Errorf("ack: %w", errAck)
			}
		}
	}
}
