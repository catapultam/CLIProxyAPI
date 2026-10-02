package api

import (
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

const agentbusSaveEvery = 30 * time.Second

// agentbusStatePath keeps the bus next to the other runtime state:
// WRITABLE_PATH when set, else next to the config file.
func (s *Server) agentbusStatePath() string {
	if base := util.WritablePath(); base != "" {
		return filepath.Join(base, "agentbus-state.json")
	}
	if s.configFilePath != "" {
		return filepath.Join(filepath.Dir(s.configFilePath), "agentbus-state.json")
	}
	return ""
}

// initAgentbus loads the bus state and saves it periodically while running.
func (s *Server) initAgentbus() {
	s.agentbus = agentbus.NewStore(s.agentbusStatePath(), nil)
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
}

func (s *Server) stopAgentbus() {
	if s.agentbusStop == nil {
		return
	}
	close(s.agentbusStop)
	s.agentbusStop = nil
	if errSave := s.agentbus.Save(); errSave != nil {
		log.Warnf("agentbus: save state: %v", errSave)
	}
}
