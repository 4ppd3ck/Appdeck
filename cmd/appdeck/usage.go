// Usage stats: how often and how recently each app was started, persisted
// to <data>/usage.json next to state.json. The card shows "last run 2h ago"
// and the deck gains a "Recently used" sort. Written only on start and stop
// — the natural moments — so the file stays tiny and cold.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// appUsage is one app's row in usage.json.
type appUsage struct {
	LastStarted float64 `json:"lastStarted"` // unix seconds; 0 = never started
	RunCount    int     `json:"runCount"`
	SecondsRun  float64 `json:"secondsRun"` // accumulated across every managed stop
}

func usagePath(dataDir string) string {
	return filepath.Join(dataDir, "usage.json")
}

func loadUsage(dataDir string) map[string]appUsage {
	out := map[string]appUsage{}
	data, err := os.ReadFile(usagePath(dataDir))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out) // corrupt file = empty stats, never a boot failure
	return out
}

func saveUsage(dataDir string, u map[string]appUsage) {
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return
	}
	tmp := usagePath(dataDir) + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, usagePath(dataDir))
	}
}

func (s *Server) usageFile() string { return usagePath(s.cfg.DataDir) }

// recordAppStart bumps the app's run counter and last-started time. Called
// only for launches the launcher performed itself — an "already running"
// answer means someone else's instance and is not a run.
func (s *Server) recordAppStart(id string) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	u := loadUsage(s.cfg.DataDir)
	e := u[id]
	e.LastStarted = float64(time.Now().UnixNano()) / 1e9
	e.RunCount++
	u[id] = e
	saveUsage(s.cfg.DataDir, u)
}

// recordAppStop folds the just-ended run's wall time into the total. The
// startedAt comes from the state entry, so a stop without one (an external
// instance the launcher never started) records nothing.
func (s *Server) recordAppStop(id string, startedAt float64) {
	if startedAt <= 0 {
		return
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	u := loadUsage(s.cfg.DataDir)
	e := u[id]
	if elapsed := float64(time.Now().UnixNano())/1e9 - startedAt; elapsed > 0 {
		e.SecondsRun += elapsed
	}
	u[id] = e
	saveUsage(s.cfg.DataDir, u)
}

// readUsage returns one app's stats (zero values when never started).
func (s *Server) readUsage(id string) appUsage {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	return loadUsage(s.cfg.DataDir)[id]
}
