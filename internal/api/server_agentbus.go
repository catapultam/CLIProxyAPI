package api

import (
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/slackbridge"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

const agentbusSaveEvery = 30 * time.Second

// runtimeStatePath keeps runtime state next to the other runtime state:
// WRITABLE_PATH when set, else next to the config file.
func (s *Server) runtimeStatePath(name string) string {
	if base := util.WritablePath(); base != "" {
		return filepath.Join(base, name)
	}
	if s.configFilePath != "" {
		return filepath.Join(filepath.Dir(s.configFilePath), name)
	}
	return ""
}

// initAgentbus loads the bus state and saves it periodically while running.
func (s *Server) initAgentbus() {
	s.agentbus = agentbus.NewStore(s.runtimeStatePath("agentbus-state.json"), nil)
	if errLoad := s.agentbus.Load(); errLoad != nil {
		log.Warnf("agentbus: load state: %v", errLoad)
	}
	s.agentbusStop = make(chan struct{})
	go func(store *agentbus.Store, stop <-chan struct{}) {
		ticker := time.NewTicker(agentbusSaveEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if errSave := store.Save(); errSave != nil {
					log.Warnf("agentbus: save state: %v", errSave)
				}
			}
		}
	}(s.agentbus, s.agentbusStop)
	s.initSlack()
}

// initSlack starts the Slack bridge when config.yaml has a complete slack
// block. Changing the block needs a restart.
func (s *Server) initSlack() {
	if s.cfg == nil {
		return
	}
	sc := s.cfg.Slack
	bridge, errNew := slackbridge.New(slackbridge.Config{
		BotToken:      sc.BotToken,
		AppToken:      sc.AppToken,
		Channel:       sc.Channel,
		AllowedEmails: sc.AllowedEmails,
		Home:          sc.Home,
		StatePath:     s.runtimeStatePath("slack-state.json"),
	}, s.agentbus)
	if errNew != nil {
		log.Warnf("slack: %v", errNew)
		return
	}
	if bridge == nil {
		return
	}
	s.slack = bridge
	bridge.Start()
}

func (s *Server) stopAgentbus() {
	if s.slack != nil {
		s.slack.Stop()
		s.slack = nil
	}
	if s.agentbusStop == nil {
		return
	}
	close(s.agentbusStop)
	s.agentbusStop = nil
	if errSave := s.agentbus.Save(); errSave != nil {
		log.Warnf("agentbus: save state: %v", errSave)
	}
}
