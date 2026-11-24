package main

// Menu bar companion model (the tray). Platform-neutral: the snapshot is
// rebuilt every few seconds from the launcher's own registry — the dashboard
// does the same work for its cards — and each platform bridges it to its
// native menu (tray_darwin.go, tray_windows.go, tray_linux.go). Menu opens
// serialize the last snapshot, never IO, so the UI thread never blocks on a
// probe.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// trayActiveFlag is the lifecycle gate: set only when the native window
// actually carries the tray. While it is set, a closed dashboard window
// hides instead of quitting (no heartbeat watchdog, no bye shutdown) — the
// menu bar owns the quit path (Quit item / ⌘Q), everything else keeps the
// deck running.
var trayActiveFlag atomic.Bool

// trayServer is installed by main before the native window opens.
var trayServer *Server

// trayRequested is the --no-tray-adjusted request for the menu bar
// companion; the platform tray files honor it (NSStatusItem on darwin, the
// notification area on Windows, StatusNotifierItem on Linux).
var trayRequested bool

// trayRunningOnly is the tray menu's display filter (◐ toggle): when set,
// the apps submenu shows the whole deck instead of only running apps.
// In-memory on purpose — the tray is a glanceable surface, the dashboard
// keeps the durable views.
var trayRunningOnly atomic.Bool

// trayDashboardURL is the loopback dashboard URL for the Open Dashboard
// action, installed by main.
var trayDashboardURL string

// trayShowDashboard surfaces the dashboard for this platform: the native
// window on darwin/Windows, an app window on Linux (where the launcher owns
// no window at all). Set by the platform tray files.
var trayShowDashboard func()

// traySearchApps is the tray's search entry: the tray menu itself stays
// compact (no text input, no long app list), so the item opens the
// dashboard with the #palette hash — the page auto-opens the ⌘K quick
// switcher when it loads with that hash, on every platform.
func traySearchApps() {
	if trayDashboardURL != "" && trayOpenURL != nil {
		trayOpenURL(trayDashboardURL + "#palette")
	}
}

// trayInstallLinux installs the session-bus tray; the no-op default covers
// every non-Linux build (tray_linux.go swaps in the real implementation).
var trayInstallLinux = func() bool { return false }

// trayOpenURL opens an app URL in a popup window the same way the
// dashboard's Open buttons do. Set by the platform tray files; nil when the
// platform has no popup mechanism.
var trayOpenURL func(url string)

// trayApplyVisible shows/hides the installed companion live (the General
// settings trayIcon toggle). Set by the platform tray files when the
// platform can do it in place; nil means the setting takes effect on the
// platform's next launch.
var trayApplyVisible func(on bool)

// trayQuit is the shared Quit action — every platform's Quit item lands
// here, running the same beginShutdown contract as the other quit paths
// (stop the started apps, then exit).
func trayQuit() {
	if s := trayServer; s != nil {
		s.beginShutdown("quit requested from the tray")
	}
}

// applyTraySetting flips the menu bar companion after a settings save.
// Turning it off also resurfaces the dashboard and restores the
// close-to-quit watchdog: with no tray icon, the dashboard window is the
// only door back in. Turning it on (re)installs where the platform supports
// it; where it does not, the flag stays off and the launch-time gate in
// main() decides.
func applyTraySetting(on bool) {
	if on {
		if trayApplyVisible == nil {
			return // this platform cannot install the companion live
		}
		trayApplyVisible(true)
		trayActiveFlag.Store(true)
		return
	}
	if trayApplyVisible != nil && trayEnabled() {
		trayApplyVisible(false)
	}
	trayActiveFlag.Store(false)
	if trayShowDashboard != nil {
		trayShowDashboard()
	}
	if s := trayServer; s != nil {
		startWindowWatchdog(s) // exits on its own once the tray returns
	}
}

// traySnapshotCopy hands the platform menus the cached model without the
// lock held. Menu building happens on UI/bus threads — never probe here.
func traySnapshotCopy() traySnapshot {
	trayMu.Lock()
	defer trayMu.Unlock()
	return trayModel
}

func trayEnabled() bool { return trayActiveFlag.Load() }

type trayApp struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url,omitempty"`
	Running bool   `json:"running"`
	Desktop bool   `json:"desktop"`
}

// trayToggle is one app's tray quick action ("Pause blocking" / "Resume
// blocking"), driven by the app manifest's trayToggle field. Err is set
// when the status probe or the toggle POST fails — the menu then renders a
// disabled "unreachable" row instead of a toggle that lies.
type trayToggle struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Running bool   `json:"running"`
	Paused  bool   `json:"paused"`
	Err     string `json:"err,omitempty"`
}

type traySnapshot struct {
	RunningCount int          `json:"running_count"`
	RunningOnly  bool         `json:"running_only"`  // the ◐ display filter's current state
	DashboardURL string       `json:"dashboard_url"` // for the menu's Search apps… entry
	Apps         []trayApp    `json:"apps"`
	Toggles      []trayToggle `json:"toggles,omitempty"`
}

// trayToggleTarget is where a quick-action click POSTs: the app's loopback
// base URL plus its manifest-declared paths. Rebuilt on every refresh; the
// click handlers read it under trayMu.
type trayToggleTarget struct {
	base string
	spec *TrayToggleSpec
}

var (
	trayMu            sync.Mutex
	trayModel         traySnapshot
	trayToggleTargets = map[string]trayToggleTarget{}
)

// trayRefresh rebuilds the tray snapshot from the registry. Heavy-ish on
// purpose: it reuses appStatus (the /api/apps card data — port probes and
// all) so the tray never disagrees with the dashboard, and probes each
// toggle app's status endpoint so the items never lie about paused state.
func trayRefresh() {
	s := trayServer
	if s == nil {
		return
	}
	st := loadState(s.cfg.StateFile)
	snap := traySnapshot{Apps: []trayApp{}, DashboardURL: trayDashboardURL, RunningOnly: trayRunningOnly.Load()}
	targets := map[string]trayToggleTarget{}
	for _, a := range s.appsList() {
		as := s.appStatus(a, st)
		ta := trayApp{ID: as.ID, Name: as.Name, URL: as.URL, Running: as.Running,
			Desktop: as.Desktop || as.Cli}
		snap.Apps = append(snap.Apps, ta)
		if ta.Running {
			snap.RunningCount++
		}
		if tt := a.TrayToggle; tt != nil && as.Running && as.URL != "" {
			tg := trayToggle{ID: a.ID, Label: tt.labelOr(a.Name), Running: true}
			tg.Paused, tg.Err = probeTogglePaused(trimURLSlashes(as.URL) + tt.Status)
			snap.Toggles = append(snap.Toggles, tg)
			targets[a.ID] = trayToggleTarget{base: as.URL, spec: tt}
		}
	}
	trayMu.Lock()
	trayModel = snap
	trayToggleTargets = targets
	trayMu.Unlock()
}

// probeTogglePaused reads a toggle app's status endpoint for the paused
// flag. A failure is reported as Err, never as a silently unpaused state.
func probeTogglePaused(url string) (bool, string) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false, "unreachable"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, "status " + resp.Status
	}
	var out struct {
		Paused bool `json:"paused"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, "bad status"
	}
	return out.Paused, ""
}

// trayRefreshLoop keeps the snapshot warm so menu opens are instant.
func trayRefreshLoop() {
	go func() {
		for {
			trayRefresh()
			time.Sleep(5 * time.Second)
		}
	}()
}

// trayLaunchApp starts the app when stopped, then opens its page in a popup
// window (web apps). Runs on a goroutine — the menu click returns at once.
func trayLaunchApp(id, url string, running bool) {
	s := trayServer
	if s == nil {
		return
	}
	var a *App
	for _, cand := range s.appsList() {
		if cand.ID == id {
			a = cand
			break
		}
	}
	if a == nil {
		return
	}
	if !running {
		if res := s.startApp(a); res == nil || !res.OK {
			return // the card's error chip explains; no dead popup
		}
		trayRefresh()
	}
	if url != "" && trayOpenURL != nil {
		trayOpenURL(url)
	}
}

// trayRunToggle flips one app's global enforcement state (the manifest
// trayToggle contract): POST {"action":"pause"|"resume"} to the app's
// action endpoint, which guards itself to loopback callers — the launcher
// always calls from this machine. Optimistic: the snapshot's paused flag
// flips before the POST lands so the next menu open is already right; the
// follow-up refresh re-probes and corrects (with Err) when the POST fails.
// Runs on a goroutine — the menu click returns at once.
func trayRunToggle(id string, pause bool) {
	trayMu.Lock()
	target, ok := trayToggleTargets[id]
	if ok {
		for i := range trayModel.Toggles {
			if trayModel.Toggles[i].ID == id {
				trayModel.Toggles[i].Paused = pause
				trayModel.Toggles[i].Err = ""
			}
		}
	}
	trayMu.Unlock()
	if !ok {
		return
	}
	action := "resume"
	if pause {
		action = "pause"
	}
	body, _ := json.Marshal(map[string]string{"action": action})
	req, err := http.NewRequest(http.MethodPost,
		trimURLSlashes(target.base)+target.spec.actionPath(),
		bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			err = &toggleStatusError{resp.Status}
		}
	}
	if err != nil {
		trayMu.Lock()
		for i := range trayModel.Toggles {
			if trayModel.Toggles[i].ID == id {
				// Revert the optimistic flip and own the failure: the next
				// menu open shows the truth even before the refresh runs.
				trayModel.Toggles[i].Paused = !pause
				trayModel.Toggles[i].Err = "toggle failed"
			}
		}
		trayMu.Unlock()
	}
	time.Sleep(300 * time.Millisecond) // let the enforcer settle, then re-read
	trayRefresh()
}

// toggleStatusError marks a non-2xx answer from a toggle app's endpoint.
type toggleStatusError struct{ status string }

func (e *toggleStatusError) Error() string { return "toggle refused: " + e.status }

// trayAppsPool is the Apps submenu's source: every app when the ◐ filter is
// on, only running ones otherwise — running first so the cap keeps the
// useful set.
func trayAppsPool(snap traySnapshot) []trayApp {
	var running, stopped []trayApp
	for _, ta := range snap.Apps {
		if ta.Running {
			running = append(running, ta)
		} else {
			stopped = append(stopped, ta)
		}
	}
	if !snap.RunningOnly {
		return running
	}
	return append(running, stopped...)
}

// trayRowLabel is one Apps-submenu row: a filled dot for running, hollow for
// stopped, so the state survives grayscale menus.
func trayRowLabel(ta trayApp) string {
	if ta.Running {
		return "● " + ta.Name
	}
	return "○ " + ta.Name
}

func trimURLSlashes(u string) string {
	for len(u) > 0 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	return u
}
