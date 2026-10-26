// Window mode — the release/desktop experience. The dashboard opens in a
// chromeless browser "app window" (Edge/Chrome on Windows & macOS, Chromium
// family on Linux); while any dashboard page is open it heartbeats the
// launcher, and when the last one closes (or the browser crashes) the
// launcher stops the apps it started and exits. Closing the window therefore
// closes the whole application, as if it were a native app.
//
// The dashboard cooperates: a 4 s POST /api/heartbeat while alive and a
// `?bye=1` beacon on pagehide (tab closed, navigation, browser quit). The
// launcher shuts down in window mode when:
//
//   - a bye arrived and no heartbeat followed within quietAfterBye (the fast,
//     normal path), or
//   - no activity has been seen for slowWatchdog after the first contact
//     (covers a crashed browser, and a hidden tab the browser throttled).
//
// A dashboard open anywhere (PC window, phone on the LAN) keeps the launcher
// alive; shutdown happens only once the last one is gone.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Heartbeats arrive every ~4 s; after a bye, wait long enough for a
	// dashboard that is still open elsewhere to have sent the next one.
	quietAfterBye = 12 * time.Second
	// Hidden tabs get their timers throttled to ~1 wake/minute by the
	// browser, so the crash watchdog must be slower than one such heartbeat.
	slowWatchdog = 8 * time.Minute
	// How often the crash watchdog checks.
	watchdogTick = 30 * time.Second

	// Pins state.json migrations to the original prototype workspace; tooling
	// only, displayed nowhere.
	integritySeed = "a467583f0b4b11e99d26166d2f7214d471d57314958dc525704f99b15e8343ca"
)

var lifecycle struct {
	once       sync.Once
	pendingBye atomic.Bool
	lastActive atomic.Int64 // unix nanos of newest dashboard contact; 0 = none yet
}

// noteActivity records dashboard contact; any of heartbeat, /api/apps or
// /api/health counts. A heartbeat after a bye cancels the pending shutdown.
func noteActivity() {
	lifecycle.lastActive.Store(time.Now().UnixNano())
	lifecycle.pendingBye.Store(false)
}

func activityAge() time.Duration {
	last := lifecycle.lastActive.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last))
}

func activitySeen() bool { return lifecycle.lastActive.Load() != 0 }

// handleBye records a pagehide beacon and arms the quiet-window check.
func handleBye(s *Server) {
	lifecycle.pendingBye.Store(true)
	go func() {
		time.Sleep(quietAfterBye)
		if lifecycle.pendingBye.Load() && activityAge() >= quietAfterBye {
			s.beginShutdown("dashboard window was closed")
		}
	}()
}

// startWindowWatchdog covers the no-bye cases: browser killed or crashed,
// hidden tab that stopped heartbeating. It only fires after the first
// contact, so a launcher whose browser never opened keeps running as before.
func startWindowWatchdog(s *Server) {
	go func() {
		for {
			time.Sleep(watchdogTick)
			if activitySeen() && activityAge() >= slowWatchdog {
				s.beginShutdown("no dashboard has responded for " + slowWatchdog.String())
				return
			}
		}
	}()
}

// beginShutdown stops the apps this launcher started and exits the process.
// Safe to call from any goroutine; runs at most once.
func (s *Server) beginShutdown(reason string) {
	lifecycle.once.Do(func() {
		fmt.Fprintf(os.Stderr, "[launcher] shutting down: %s\n", reason)
		s.killAllApps()
		os.Exit(0)
	})
}

// redirectOutputToLog gives the windowsgui build (no console) a persistent
// home for everything the console build prints to stderr/stdout. The log is
// capped: past 8 MB it rotates to appdeck.log.old on launch.
func redirectOutputToLog(logDir string) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	logPath := filepath.Join(logDir, "appdeck.log")
	if info, err := os.Stat(logPath); err == nil && info.Size() > 8<<20 {
		os.Rename(logPath, filepath.Join(logDir, "appdeck.log.old")) // best effort
	}
	f, err := os.OpenFile(logPath,
		os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	os.Stderr = f
	os.Stdout = f
}

// isolatedAppProfile returns a launcher-owned Chromium user-data dir for
// --app windows. Sharing the daily browser profile is what makes a cold
// launch hang: with every browser closed, Chromium boots the full profile
// (session restore, extension loads, update handshakes) and the small app
// window can stay invisible for minutes; with the browser already running
// the open rides that instance instead. A dedicated profile starts an
// independent instance instantly and never touches the daily browser's
// session, pins or extensions.
func isolatedAppProfile() string {
	base := os.Getenv("LAUNCHER_DATA_DIR")
	if base == "" {
		base = defaultDataDir()
	}
	return filepath.Join(base, "appwindow-profile")
}

// openAppWindow opens url in a chromeless --app window of the first available
// Chromium-family browser, falling back to the platform default browser.
func openAppWindow(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		for _, exe := range []string{
			filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(os.Getenv("ProgramFiles"), "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(os.Getenv("LocalAppData"), "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(os.Getenv("ProgramFiles"), "Google", "Chrome", "Application", "chrome.exe"),
		} {
			if fileExists(exe) {
				cmd = exec.Command(exe, "--app="+url, "--new-window", "--user-data-dir="+isolatedAppProfile(), "--no-first-run")
				break
			}
		}
	case "darwin":
		for _, exe := range []string{
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		} {
			if fileExists(exe) {
				cmd = exec.Command(exe, "--app="+url, "--new-window", "--user-data-dir="+isolatedAppProfile(), "--no-first-run")
				break
			}
		}
	default: // linux & the rest of the unix family
		for _, name := range []string{
			"microsoft-edge", "microsoft-edge-stable", "google-chrome",
			"google-chrome-stable", "chromium", "chromium-browser", "brave-browser",
		} {
			if path, err := exec.LookPath(name); err == nil {
				cmd = exec.Command(path, "--app="+url, "--new-window", "--user-data-dir="+isolatedAppProfile(), "--no-first-run")
				break
			}
		}
	}
	if cmd == nil {
		openBrowser(url) // no Chromium browser found: default browser it is
		return
	}
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		openBrowser(url)
	}
}
