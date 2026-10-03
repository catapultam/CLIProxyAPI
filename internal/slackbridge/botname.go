package slackbridge

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
)

// defaultBotName is used until Slack has told the bridge its bot's name.
const defaultBotName = "agents"

var _ agentbus.BotNamer = (*Bridge)(nil)

// BotName is the bot's display name as Slack shows it now (its display
// name, else real name, else user name), refreshed every maintainEvery. It
// implements agentbus.BotNamer.
func (b *Bridge) BotName() string {
	b.botNameMu.Lock()
	defer b.botNameMu.Unlock()
	if b.botName == "" {
		return defaultBotName
	}
	return b.botName
}

// botMention is how bot-authored texts write the bot: "@" and its name,
// made safe for Slack text and code spans.
func (b *Bridge) botMention() string {
	return "@" + escape(strings.ReplaceAll(b.BotName(), "`", "'"))
}

// refreshBotName asks Slack for the bot's name. A failed lookup keeps the
// last known one.
func (b *Bridge) refreshBotName(ctx context.Context) {
	if b.botUserID == "" {
		return
	}
	name, _, err := b.api.userInfo(ctx, b.cfg.BotToken, b.botUserID)
	if err != nil {
		log.Infof("slack: bot name lookup failed: %v", err)
		return
	}
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return
	}
	b.botNameMu.Lock()
	changed := b.botName != name
	b.botName = name
	b.botNameMu.Unlock()
	if changed {
		log.Infof("slack: the bot is @%s", name)
	}
}
