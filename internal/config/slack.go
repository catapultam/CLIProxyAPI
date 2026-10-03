package config

// SlackConfig connects the agentbus to one Slack channel (catapultam fork).
// The bridge is off unless every field is set.
type SlackConfig struct {
	// BotToken is the bot user OAuth token (xoxb-).
	BotToken string `yaml:"bot-token,omitempty" json:"bot-token,omitempty"`
	// AppToken is the app-level Socket Mode token (xapp-).
	AppToken string `yaml:"app-token,omitempty" json:"app-token,omitempty"`
	// Channel is the channel name (with or without #) or ID.
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty"`
	// AllowedEmails seeds the users allowed to instruct sessions.
	AllowedEmails []string `yaml:"allowed-emails,omitempty" json:"allowed-emails,omitempty"`
}
