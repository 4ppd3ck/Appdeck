// Per-app privileges. Some apps need access the launcher's own user session
// cannot vouch for: on macOS that is TCC privacy grants (Full Disk Access),
// on Windows "run as administrator", on Linux sudo/polkit. One concept, three
// OS mechanisms — so the declaration here is platform-neutral
// (privileges: ["elevated"] in app.json, 🔓 Elevated / 🔒 Inherited on the
// card) and only the launch
// mechanics split per OS.
//
// The compliance rule this file enforces: the launcher itself never gains
// anything. On macOS a privileged app launches from its OWN generated .app
// bundle (tcc_darwin.go) so TCC grants land on that app alone; the launcher
// stays unprivileged and every other app keeps running exactly as before.
// Windows/Linux launch unchanged for now (the manual escape hatch is an
// app.json cmd/args pointing at gsudo/sudo) — the runas/pkexec mappings slot
// into privilegedLaunch without touching this file.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
)

// validPrivileges is the closed set of privilege names the launcher knows.
// Unknown names are dropped at load time rather than silently misread.
var validPrivileges = map[string]bool{
	"elevated": true, // needs system-level/privacy-granted access (macOS: Full Disk Access class)
}

// privilegePrivileged reports whether the app carries the "elevated"
// privilege after validation and user overrides.
func (a *App) privilegeElevated() bool {
	for _, p := range a.Privileges {
		if p == "elevated" {
			return true
		}
	}
	return false
}

// privilegedLaunch reports whether this start must use the privileged launch
// path. macOS is the only implemented mapping; elsewhere the flag is recorded
// but the ordinary exec path runs (elevation there is the author's cmd to
// write until the runas/pkexec mappings land).
func privilegedLaunch(a *App) bool {
	return a.privilegeElevated() && runtime.GOOS == "darwin" && a.Binary != ""
}

// ---- user overrides (tcc.json) — the network.json pattern ------------------

// tcc.json in the data dir records explicit per-app user choices; the
// manifest's privileges stay the default when no override exists, and the
// user's choice survives re-imports and updates.
func (s *Server) privilegeOverridesFile() string {
	return filepath.Join(s.cfg.DataDir, "tcc.json")
}

func (s *Server) loadPrivilegeOverrides() map[string]bool {
	data, err := os.ReadFile(s.privilegeOverridesFile())
	if err != nil {
		return map[string]bool{}
	}
	ov := map[string]bool{}
	if json.Unmarshal(data, &ov) != nil {
		return map[string]bool{}
	}
	return ov
}

func (s *Server) savePrivilegeOverrides(ov map[string]bool) {
	// Prune ids that no longer exist so the file cannot grow forever.
	for id := range ov {
		if s.byID[id] == nil {
			delete(ov, id)
		}
	}
	data, err := json.MarshalIndent(ov, "", "  ")
	if err != nil {
		return
	}
	path := s.privilegeOverridesFile()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// applyPrivilegeOverrides layers saved user choices over the manifest
// defaults — the user's choice wins.
func (s *Server) applyPrivilegeOverrides(apps []*App) {
	ov := s.loadPrivilegeOverrides()
	if len(ov) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range apps {
		if enabled, ok := ov[a.ID]; ok {
			if enabled {
				if !a.privilegeElevated() {
					a.Privileges = append(a.Privileges, "elevated")
				}
			} else {
				a.Privileges = nil
			}
		}
	}
}

// servePrivilegeToggle answers POST /api/privilege {id, enabled}: records the
// per-app choice; it applies at the app's next start (a running process keeps
// the identity it was launched with). Loopback only, like every override.
func (s *Server) servePrivilegeToggle(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "privilege changes are only allowed from this computer"})
		return
	}
	var req struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	readBodyInto(r, &req)
	app := s.appByID(req.ID)
	if app == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
		return
	}
	ov := s.loadPrivilegeOverrides()
	ov[req.ID] = req.Enabled
	s.savePrivilegeOverrides(ov)
	s.mu.Lock()
	for _, a := range s.apps {
		if a.ID == req.ID {
			if req.Enabled {
				if !a.privilegeElevated() {
					a.Privileges = append(a.Privileges, "elevated")
				}
			} else {
				a.Privileges = nil
			}
		}
	}
	s.mu.Unlock()
	note := "app launches Elevated — its own macOS identity; grant it access in System Settings ▸ Privacy & Security when macOS asks (applies at next start)"
	if !req.Enabled {
		note = "app launches Inherited — under the launcher's identity again (applies at next start)"
	}
	if runtime.GOOS != "darwin" {
		note = "recorded — per-app privilege enforcement is macOS-only today; other platforms launch unchanged"
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note, "elevated": req.Enabled})
}

// ---- launcher settings (launcher.json) -------------------------------------

// launcherSettings is the small set of launcher-level toggles that live in
// the data dir (next to network.json / tcc.json). dashboardLoopback limits
// the launcher's own UI (dashboard + /api) to this computer — shared APPS on
// their own ports stay reachable from the LAN either way.
type launcherSettings struct {
	DashboardLoopback bool `json:"dashboardLoopback"`
}

var settingsLoopback atomic.Bool // cached; read on every request

func (s *Server) launcherSettingsFile() string {
	return filepath.Join(s.cfg.DataDir, "launcher.json")
}

func (s *Server) loadLauncherSettings() launcherSettings {
	var out launcherSettings
	if data, err := os.ReadFile(s.launcherSettingsFile()); err == nil {
		_ = json.Unmarshal(data, &out)
	}
	settingsLoopback.Store(out.DashboardLoopback)
	return out
}

func (s *Server) saveLauncherSettings(in launcherSettings) {
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return
	}
	path := s.launcherSettingsFile()
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
	settingsLoopback.Store(in.DashboardLoopback)
}

// serveLauncherSettings answers GET/POST /api/launcher-settings. Loopback
// only for writes; the GET is cheap enough to leave LAN-readable so a remote
// dashboard can explain why it just got 403'd.
func (s *Server) serveLauncherSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.writeJSON(w, http.StatusOK, s.loadLauncherSettings())
		return
	}
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "launcher settings are only allowed from this computer"})
		return
	}
	var req launcherSettings
	readBodyInto(r, &req)
	s.saveLauncherSettings(req)
	note := "dashboard limited to this computer — shared apps stay reachable from the Wi-Fi"
	if !req.DashboardLoopback {
		note = "dashboard reachable from the Wi-Fi again"
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note, "settings": req})
}
