package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// nextResetStateMaxAge drops persisted snapshots older than one weekly window.
const nextResetStateMaxAge = 7 * 24 * time.Hour

// nextResetStateDirty is set whenever polled data or latches change, and
// cleared when the state file is written.
var nextResetStateDirty atomic.Bool

type nextResetStateFile struct {
	Version int                          `json:"version"`
	Polled  map[string]nextResetSnapshot `json:"polled"`
	Latches map[string]time.Time         `json:"latches"`
}

func (p *nextResetPolledStore) snapshot() map[string]nextResetSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]nextResetSnapshot, len(p.data))
	for k, v := range p.data {
		out[k] = v
	}
	return out
}

func (l *nextResetLatchStore) snapshot() map[string]time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]time.Time, len(l.since))
	for k, v := range l.since {
		out[k] = v
	}
	return out
}

// saveNextResetState writes polled snapshots and latches atomically.
func saveNextResetState(path string) error {
	if path == "" {
		return nil
	}
	nextResetStateDirty.Store(false)
	data, errMarshal := json.Marshal(nextResetStateFile{
		Version: 1,
		Polled:  nextResetPolled.snapshot(),
		Latches: nextResetLatches.snapshot(),
	})
	if errMarshal != nil {
		nextResetStateDirty.Store(true)
		return errMarshal
	}
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		nextResetStateDirty.Store(true)
		return errMkdir
	}
	tmp := path + ".tmp"
	if errWrite := os.WriteFile(tmp, data, 0o600); errWrite != nil {
		nextResetStateDirty.Store(true)
		return errWrite
	}
	if errRename := os.Rename(tmp, path); errRename != nil {
		nextResetStateDirty.Store(true)
		return errRename
	}
	return nil
}

// loadNextResetState restores polled snapshots and latches from path,
// skipping snapshots older than one weekly window. A missing file is fine.
func loadNextResetState(path string, now time.Time) {
	if path == "" {
		return
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if !os.IsNotExist(errRead) {
			log.Warnf("next-reset: read state %s: %v", path, errRead)
		}
		return
	}
	var state nextResetStateFile
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal != nil {
		log.Warnf("next-reset: parse state %s: %v", path, errUnmarshal)
		return
	}
	restored := 0
	for id, snap := range state.Polled {
		if now.Sub(snap.ObservedAt) > nextResetStateMaxAge {
			continue
		}
		nextResetPolled.set(id, snap)
		restored++
	}
	for id, since := range state.Latches {
		nextResetLatches.set(id, since)
	}
	log.Infof("next-reset: restored %d usage snapshots and %d latches from %s", restored, len(state.Latches), path)
}
