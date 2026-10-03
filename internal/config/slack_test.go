package config

import (
	"strings"
	"testing"
)

const slackV8Config = `config-version: 8
access:
  api-keys:
    - k1
slack:
  bot-token: xoxb-test
  app-token: xapp-test
  channel: agents
  home: dm
  allowed-emails:
    - alex@example.com
    - jane@example.com
`

func TestParseSlackConfigV8(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(slackV8Config))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Slack
	if s.BotToken != "xoxb-test" || s.AppToken != "xapp-test" || s.Channel != "agents" || s.Home != "dm" {
		t.Fatalf("slack = %+v", s)
	}
	if len(s.AllowedEmails) != 2 || s.AllowedEmails[1] != "jane@example.com" {
		t.Fatalf("emails = %v", s.AllowedEmails)
	}
}

func TestSlackSectionSurvivesV8Normalization(t *testing.T) {
	out, _, err := NormalizeConfigLayout([]byte(slackV8Config), true)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, "# slack") || !strings.Contains(text, "\nslack:") || !strings.Contains(text, "allowed-emails:") || !strings.Contains(text, "home: dm") {
		t.Fatalf("slack section was dropped or commented out:\n%s", text)
	}
}
