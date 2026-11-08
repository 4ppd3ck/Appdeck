// Per-app privileges. Some apps need access the launcher's own user session
// cannot vouch for: on macOS that is TCC privacy grants (Full Disk Access),
// on Windows "run as administrator", on Linux sudo/polkit. One concept, three
// OS mechanisms — so the declaration here is platform-neutral
// (privileges: ["elevated"] in app.json, 🔓 Elevated / 🔒 Inherited on the
// card) and only the launch
// mechanics split per OS.
//
// The compliance rule this file enforces: the launcher itself never gains
// anything. Every OS maps the flag onto its own mechanism without touching
// the launcher's own privileges: macOS launches the app from its OWN
// generated .app bundle (tcc_darwin.go) so TCC grants land on that app
// alone; Windows starts it through Start-Process -Verb RunAs so the UAC
// consent prompt is the app's alone (priv_windows.go); Linux goes through
// pkexec (sudo -n as fallback) so the polkit agent authorizes that app alone
// (priv_linux.go). The launcher stays unprivileged and every other app keeps
// running exactly as before.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// validPrivileges is the closed set of privilege names the launcher knows.
// Unknown names are dropped at load time rather than silently misread.
var validPrivileges = map[string]bool{
	"elevated": true, // needs system-level/privacy-granted access (macOS: Full Disk Access class)
}

// validPrivacy is the closed set of privacy classes app.json may declare —
// the macOS TCC category the elevation guide opens (empty = Full Disk
// Access). Unknown values fall back to that default at load time, the same
// forward-compat rule privileges follow: a manifest written for a newer
// launcher must still load here.
var validPrivacy = map[string]bool{
	"":              true,
	"accessibility": true, // UI automation / synthetic input (CGEvent)
	"screen":        true, // Screen Recording / Screen & System Audio
	"input":         true, // Input Monitoring (global event taps)
	"appManagement": true, // modifying other apps' bundles
}

// privacyName is what System Settings calls the app's privacy category —
// the string the guidance and toasts show. Platform-neutral: the notes are
// built untagged and only the darwin branch quotes it.
func privacyName(privacy string) string {
	switch privacy {
	case "accessibility":
		return "Accessibility"
	case "screen":
		return "Screen Recording"
	case "input":
		return "Input Monitoring"
	case "appManagement":
		return "App Management"
	default:
		return "Full Disk Access"
	}
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
// path — every OS implements one (tcc_darwin.go, priv_windows.go,
// priv_linux.go). Shell-string cmd apps (no binary) launch as before: the
// elevation helpers take a binary, and the author's cmd string can already
// name gsudo/sudo itself.
func privilegedLaunch(a *App) bool {
	return a.privilegeElevated() && a.Binary != ""
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

// servePrivilegeToggle answers POST /api/privilege {id, enabled}: records
// the per-app choice and applies it right away — a running app is stopped
// and started again under its new identity, because a process keeps the
// elevation it was launched with. Loopback only, like every override.
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
	s.mu.Lock()
	app := s.byID[req.ID]
	if app == nil {
		s.mu.Unlock()
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
		return
	}
	if _, busy := s.inflight[app.ID]; busy {
		s.mu.Unlock()
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": busyMessage})
		return
	}
	ov := s.loadPrivilegeOverrides()
	ov[req.ID] = req.Enabled
	s.savePrivilegeOverrides(ov)
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

	note := privilegedStartNote(req.Enabled, app.Privacy)
	// Un-elevating retires the wrapper AND its grants: the binary moves back
	// beside app.json (its home while inherited) and the .app bundle goes
	// away with its stamp, while the TCC reset drops the System Settings
	// entry — a Full Disk Access row for a bundle that no longer launches
	// the app would sit enabled forever, doing nothing (the inherited app
	// runs outside the bundle, so TCC never attributes it there). Re-granting
	// on a later re-elevate is the guided flow's job. No-ops off macOS.
	if !req.Enabled {
		restoreBundleBinary(app)
		resetTccGrants(app.ID)
	}
	if entry := loadState(s.cfg.StateFile)[app.ID]; entry.PID != 0 && pidAlive(entry.PID) {
		// The restart is the point: the flag alone would only matter at the
		// app's next start. A failed stop usually means the app out-ranks the
		// launcher (Windows admin / Linux root) — the note says so.
		if resp := s.stopApp(app); resp != nil && !resp.OK {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "note": "privilege saved, but stopping the app failed: " + resp.Error + " — close it directly, then start again"})
			return
		}
		if resp := s.startApp(app); resp != nil && !resp.OK {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "note": "privilege saved, but restarting failed: " + resp.Error})
			return
		}
		note = "restarted with its new privilege"
	}
	// macOS asks nothing at start: the privacy grant is a manual one-time
	// add, so the elevation moment is when to show the app's own bundle and
	// the pane to add it to (no-op on Windows/Linux).
	if req.Enabled {
		if app.Binary == "" {
			// Elevation wraps the app's own binary (macOS identity bundle,
			// Windows UAC script, Linux pkexec argv); static and shell-cmd
			// apps run under the launcher and have nothing to elevate. The
			// choice still saves: an update that gives the app a binary
			// picks the override up on its next start.
			note += " — no effect: this app has no binary of its own (static and shell apps run under the launcher)"
		} else if privilegedLaunch(app) && s.guideTccGrant(app) {
			note += " — Finder shows the app's own .app; add THAT (not the raw binary) under " + privacyName(app.Privacy)
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note, "elevated": req.Enabled})
}

// ---- launcher settings (launcher.json) -------------------------------------

// launcherSettings is the small set of launcher-level toggles that live in
// the data dir (next to network.json / tcc.json). dashboardLoopback limits
// the launcher's own UI (dashboard + /api) to this computer — shared APPS on
// their own ports stay reachable from the LAN either way. UpdateChannel picks
// the ⬆ modal's default: "github" for end clients, "local" for dev testing;
// LocalUpdatePath remembers the dev's zip/folder between sessions.
type launcherSettings struct {
	DashboardLoopback bool `json:"dashboardLoopback"`
	// HideAppsInventory stops /api/apps from answering the network at all
	// (the dashboard's app list is the deck's inventory). Cross-port deck
	// tools like Wi-Fi Guard keep working on this machine; from the Wi-Fi
	// they lose the list, like everyone else.
	HideAppsInventory bool `json:"hideAppsInventory"`
	// VirusTotalKey opts the security scan into hash lookups on the free
	// VT API (hash-only — file contents never leave the machine).
	VirusTotalKey   string `json:"virusTotalKey,omitempty"`
	UpdateChannel   string `json:"updateChannel,omitempty"`
	LocalUpdatePath string `json:"localUpdatePath,omitempty"`
	// DownloadDir is where browser-mode downloads land in the native window
	// (empty = ~/Downloads). DownloadAsk makes every download open the OS
	// save panel instead — "ask me where to put it", like Safari:
	DownloadDir string `json:"downloadDir"`
	DownloadAsk bool   `json:"downloadAsk"`
	// LaunchAtLogin registers the launcher with the OS (macOS LaunchAgent,
	// Windows Run key, XDG autostart) so it starts at login; apps with
	// ⚡ auto then boot with it. Re-asserted on every boot so an updated
	// executable rewrites the OS record with the new path.
	LaunchAtLogin bool `json:"launchAtLogin"`
	// Import size limits, in bytes (see limits.go): 0 means no cap, nil
	// (absent) keeps the default. The deck tools modal edits these.
	ImportBodyBytes   *int64 `json:"importBodyBytes,omitempty"`
	ImportExpandBytes *int64 `json:"importExpandBytes,omitempty"`
	ImportHTMLBytes   *int64 `json:"importHTMLBytes,omitempty"`
	// StartWaitSeconds is the deck tools "app start timeout": how long a
	// start waits for the app's web UI to answer before reporting "still
	// starting" (5–180 s; default 30). Per-app manifest startWait overrides it.
	StartWaitSeconds *int `json:"startWaitSeconds,omitempty"`
}

// normalizeUpdateChannel collapses the channel to the two known values —
// the empty/unknown default is the GitHub channel end clients expect.
func normalizeUpdateChannel(ch string) string {
	if ch == "local" {
		return "local"
	}
	return "github"
}

var (
	settingsLoopback      atomic.Bool  // cached; read on every request
	settingsHidden        atomic.Bool  // cached; /api/apps network visibility
	startWaitSecondsCache atomic.Int64 // cached; the start path reads it per launch
)

func init() { startWaitSecondsCache.Store(defaultStartWait) }

// resolveStartWait publishes the stored doc's timeout (nil keeps the
// default; an out-of-range hand-edited value falls back to the default,
// the sanitizeLimitsDoc pattern).
func resolveStartWait(v *int) {
	secs := defaultStartWait
	if v != nil && *v >= startWaitMin && *v <= startWaitMax {
		secs = *v
	}
	startWaitSecondsCache.Store(int64(secs))
}

func (s *Server) launcherSettingsFile() string {
	return filepath.Join(s.cfg.DataDir, "launcher.json")
}

func (s *Server) loadLauncherSettings() launcherSettings {
	var out launcherSettings
	if data, err := os.ReadFile(s.launcherSettingsFile()); err == nil {
		_ = json.Unmarshal(data, &out)
		// A hand-edited file with a non-number limit would decode to a
		// zeroed pointer — "no cap" — instead of the default (limits.go).
		sanitizeLimitsDoc(data, &out)
	}
	out.UpdateChannel = normalizeUpdateChannel(out.UpdateChannel)
	settingsLoopback.Store(out.DashboardLoopback)
	settingsHidden.Store(out.HideAppsInventory)
	resolveLimits(out.ImportBodyBytes, out.ImportExpandBytes, out.ImportHTMLBytes)
	resolveStartWait(out.StartWaitSeconds)
	return out
}

func (s *Server) saveLauncherSettings(in launcherSettings) {
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return
	}
	path := s.launcherSettingsFile()
	tmp := path + ".tmp"
	// 0600: the document holds the VirusTotal API key.
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Chmod(tmp, 0o600)
		_ = os.Rename(tmp, path)
	}
	settingsLoopback.Store(in.DashboardLoopback)
	settingsHidden.Store(in.HideAppsInventory)
	resolveLimits(in.ImportBodyBytes, in.ImportExpandBytes, in.ImportHTMLBytes)
	resolveStartWait(in.StartWaitSeconds)
}

// serveLauncherSettings answers GET/POST /api/launcher-settings. Writes are
// loopback only; the GET stays LAN-readable so a remote dashboard can explain
// why it just got 403'd — but it never echoes a stored secret back.
func (s *Server) serveLauncherSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		out := s.loadLauncherSettings()
		// The VirusTotal key is a credential, and this endpoint answers the
		// Wi-Fi. The dashboard never prefills the field (it saves what the
		// operator types) and /api/security reports "key saved" as a boolean,
		// so dropping the value costs the UI nothing.
		out.VirusTotalKey = ""
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "launcher settings are only allowed from this computer"})
		return
	}
	// Patch semantics: only the fields the client actually sent change —
	// a menu toggle must not wipe the saved update channel or path.
	var patch struct {
		DashboardLoopback *bool   `json:"dashboardLoopback"`
		HideAppsInventory *bool   `json:"hideAppsInventory"`
		VirusTotalKey     *string `json:"virusTotalKey"`
		UpdateChannel     *string `json:"updateChannel"`
		LocalUpdatePath   *string `json:"localUpdatePath"`
		DownloadDir       *string `json:"downloadDir"`
		DownloadAsk       *bool   `json:"downloadAsk"`
		LaunchAtLogin     *bool   `json:"launchAtLogin"`
		ImportBodyBytes   *int64  `json:"importBodyBytes"`
		ImportExpandBytes *int64  `json:"importExpandBytes"`
		ImportHTMLBytes   *int64  `json:"importHTMLBytes"`
		StartWaitSeconds  *int    `json:"startWaitSeconds"`
	}
	readBodyInto(r, &patch)
	req := s.loadLauncherSettings()
	if patch.DashboardLoopback != nil {
		req.DashboardLoopback = *patch.DashboardLoopback
	}
	if patch.HideAppsInventory != nil {
		req.HideAppsInventory = *patch.HideAppsInventory
	}
	if patch.VirusTotalKey != nil {
		req.VirusTotalKey = strings.TrimSpace(*patch.VirusTotalKey)
	}
	if patch.UpdateChannel != nil {
		req.UpdateChannel = normalizeUpdateChannel(*patch.UpdateChannel)
	}
	if patch.LocalUpdatePath != nil {
		req.LocalUpdatePath = *patch.LocalUpdatePath
	}
	if patch.DownloadDir != nil {
		req.DownloadDir = strings.TrimSpace(*patch.DownloadDir)
		req.DownloadAsk = false // an explicit folder wins over "ask each time"
	}
	if patch.DownloadAsk != nil {
		req.DownloadAsk = *patch.DownloadAsk
	}
	if patch.ImportBodyBytes != nil {
		req.ImportBodyBytes = patch.ImportBodyBytes
	}
	if patch.ImportExpandBytes != nil {
		req.ImportExpandBytes = patch.ImportExpandBytes
	}
	if patch.ImportHTMLBytes != nil {
		req.ImportHTMLBytes = patch.ImportHTMLBytes
	}
	if patch.StartWaitSeconds != nil {
		if *patch.StartWaitSeconds < startWaitMin || *patch.StartWaitSeconds > startWaitMax {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("startWaitSeconds %d invalid — use %d–%d",
					*patch.StartWaitSeconds, startWaitMin, startWaitMax)})
			return
		}
		req.StartWaitSeconds = patch.StartWaitSeconds
	}
	if patch.ImportBodyBytes != nil || patch.ImportExpandBytes != nil || patch.ImportHTMLBytes != nil {
		if err := validateLimits(
			orDefaultLimit(req.ImportBodyBytes, defaultImportBody),
			orDefaultLimit(req.ImportExpandBytes, defaultExpand),
			orDefaultLimit(req.ImportHTMLBytes, defaultImportHTML)); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	// The OS registration is applied before the flag is saved: a failing
	// setLoginItem answers with the reason and leaves the setting untouched,
	// so the toggle never lies.
	if patch.LaunchAtLogin != nil {
		exe, err := os.Executable()
		if err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot locate the running executable: " + err.Error()})
			return
		}
		if err := setLoginItem(exe, *patch.LaunchAtLogin); err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not update the login item: " + err.Error()})
			return
		}
		req.LaunchAtLogin = *patch.LaunchAtLogin
	}
	s.saveLauncherSettings(req)
	// The WKWebView download delegate reads these from native globals.
	nativeConfigureDownloads(req.DownloadDir, req.DownloadAsk)
	note := "saved"
	switch {
	case patch.HideAppsInventory != nil:
		if req.HideAppsInventory {
			note = "app list limited to this computer — Wi-Fi clients no longer see the deck's inventory"
		} else {
			note = "app list visible on the Wi-Fi again"
		}
	case patch.DashboardLoopback != nil:
		if req.DashboardLoopback {
			note = "dashboard limited to this computer — shared apps stay reachable from the Wi-Fi"
		} else {
			note = "dashboard reachable from the Wi-Fi again"
		}
	case patch.LaunchAtLogin != nil:
		if req.LaunchAtLogin {
			note = "AppDeck will start when you log in (takes effect next login)"
		} else {
			note = "launch-at-login removed"
		}
	case patch.StartWaitSeconds != nil:
		if req.StartWaitSeconds != nil {
			note = fmt.Sprintf("app start timeout set to %d s", *req.StartWaitSeconds)
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note, "settings": req})
}

// serveDownloadPick answers POST /api/downloads/pick: the OS folder picker,
// then the chosen folder becomes the download destination (and "ask each
// time" turns off — an explicit folder answers the question for good).
func (s *Server) serveDownloadPick(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "the folder picker is only allowed from this computer"})
		return
	}
	dir, err := nativeFolderPicker("Choose where downloads go")
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "cancel") {
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": false, "canceled": true})
			return
		}
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	req := s.loadLauncherSettings()
	req.DownloadDir = dir
	req.DownloadAsk = false
	s.saveLauncherSettings(req)
	nativeConfigureDownloads(req.DownloadDir, req.DownloadAsk)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dir": dir, "settings": req})
}
