package config

// SlackConfig connects the agentbus to Slack (catapultam fork). The bridge
// is off unless the tokens and allowed-emails are set, and channel too
// unless home is "dm".
type SlackConfig struct {
	// BotToken is the bot user OAuth token (xoxb-).
	BotToken string `yaml:"bot-token,omitempty" json:"bot-token,omitempty"`
	// AppToken is the app-level Socket Mode token (xapp-).
	AppToken string `yaml:"app-token,omitempty" json:"app-token,omitempty"`
	// Channel is the channel name (with or without #) or ID.
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty"`
	// AllowedEmails seeds the users allowed to instruct sessions.
	AllowedEmails []string `yaml:"allowed-emails,omitempty" json:"allowed-emails,omitempty"`
	// Home is where sessions' threads open: "channel" (default) or "dm",
	// the first allowed-emails user's DM with the bot.
	Home string `yaml:"home,omitempty" json:"home,omitempty"`
}
