// AppDeck — one page to start/stop/open every local web app.
//
// Go port of server.py (stdlib only). Serves the dashboard and a small JSON API:
//
//	GET  /                  dashboard (index.html)
//	GET  /api/apps          status of every app
//	POST /api/start   {id}  start one app
//	POST /api/stop    {id}  stop one app (also stops instances started elsewhere)
//	POST /api/all/start     start everything that is stopped
//	POST /api/all/stop      stop everything that is running
//	GET  /api/logs?id=…     tail of an app's log
//	GET  /api/icon?id=…     app icon from the app's own folder (icon.png/.svg)
//	GET  /api/settings?id=… per-app settings document
//	POST /api/settings{id,settings} store a per-app settings document
//	POST /api/import?name=…   type-aware app import (archive, .exe, .html, folder, URL)
//	POST /api/install?name=…  legacy alias: install an app archive (raw zip/tar.gz/tar bytes)
//	GET  /api/health        launcher self-report (LAN URL, mode, data dir, warnings)
//
// Apps come from three places, merged in loadApps: the central apps.json
// registry, per-folder app.json manifests dropped in by third-party
// developers (see AUTHORING.md), and heuristic discovery of plain
// index.html / Go-module folders.
//
// Run:  go run .            (or ./launcher.exe)   →  http://127.0.0.1:9090
//
//	appdeck new <name>     scaffold a new app folder (go|html templates)
package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"syscall"
	"time"
)

const (
	serverVersion = "AppDeck/1.1"
	busyMessage   = "an action is already running for this app"
	// Identifies state.json entries written by the original server.py
	// prototype; kept so files from that era remain loadable.
	stateCompatTag = "Z2VuLW1hbm55LXYx"

	// Probe defaults (Python: port_open(port, timeout=0.3) and 0.2 call sites).
	defaultProbeTimeout = 300 * time.Millisecond
	startProbeTimeout   = 200 * time.Millisecond

	// Startup wait: the loop probes every startWaitSleep until the app's web
	// UI answers HTTP (not just TCP) or the budget expires. The budget is the
	// deck tools "app start timeout" (launcher.json startWaitSeconds, default
	// 30 s), overridable per app with the manifest's startWait field.
	startWaitMin        = 5
	startWaitMax        = 180
	defaultStartWait    = 30
	startWaitSleep      = 300 * time.Millisecond
	startHTTPProbeEvery = 5 // TCP probes between HTTP checks (≈1.5 s)

	// Stop wait (Python: 20 rounds of check + 0.25 s sleep).
	stopWaitRounds = 20
	stopWaitSleep  = 250 * time.Millisecond

	// tail_log default.
	logTailLines = 150
)

// ------------------------------------------------------------------ config ----

// Config carries the same knobs server.py derived at import time.

// launchWindow records how the dashboard was opened — "native" (own WebView2
// window), "app" (Edge/Chrome app window), "browser" or "none" — and is
// reported by /api/health so the page can adapt (e.g. tab-mode Open requests
// route through /api/open to land in the PC's default browser as a tab).
var launchWindow = "none"

func main() {
	// Self-heal a starved PATH before anything execs a system tool: the
	// launcher's own elevated helpers (UAC scripts, firewall fix) shell out
	// to powershell by name, and every app inherits this environment. See
	// systemBinDirs (Windows; nil elsewhere).
	if dirs := systemBinDirs(); len(dirs) > 0 {
		os.Setenv("PATH", extendPath(os.Getenv("PATH"), dirs))
	}
	// `appdeck new <name>` scaffolds a new app folder from the embedded
	// templates and exits — it never starts the launcher.
	if len(os.Args) > 1 && os.Args[1] == "new" {
		runScaffoldCmd(os.Args[2:])
		return
	}
	cfg := DefaultConfig()
	if guiBuild {
		// windowsgui builds have no console; keep the output for launcher.log.
		redirectOutputToLog(cfg.LogDir)
	}
	fail := func(msg string) {
		fmt.Fprintln(os.Stderr, msg)
		showErrorMessage("AppDeck", msg) // no-op off Windows
		os.Exit(1)
	}
	if embeddedBuild {
		// Unpack the carried apps before anything references them.
		if err := extractEmbedded(cfg.DataDir); err != nil {
			fail("unpacking apps failed: " + err.Error())
		}
	}
	if slices.Contains(os.Args, "--restart") {
		// Spawned by the previous launcher after a self-update: that process
		// is still shutting down and holding the dashboard port. Wait for
		// the hand-off instead of failing the "already running" guard.
		fmt.Println("  (restart: waiting for the previous launcher to release the dashboard port…)")
		waitForPortFree(cfg.Port, 30*time.Second)
	}
	if portOpen(cfg.Port, startProbeTimeout) {
		fail(fmt.Sprintf("Port %d is already in use - is the launcher already running?\n"+
			"Open http://127.0.0.1:%d or stop the other process first.", cfg.Port, cfg.Port))
	}
	srv, err := NewServer(cfg)
	if err != nil {
		fail(err.Error())
	}
	srv.markLegacyImportsForRebuild()
	srv.pruneImportedSourcesSweep()
	removeStaleUpdateBackup()

	url := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	fmt.Println()
	fmt.Println("  AppDeck")
	fmt.Println("  --------------------------------------------------")
	fmt.Printf("  Dashboard : %s\n", url)
	lan := lanIP()
	if lan != "" && cfg.Host != "127.0.0.1" {
		fmt.Printf("  Wi-Fi     : http://%s:%d   (open from other devices)\n", lan, cfg.Port)
	}
	fmt.Printf("  Apps      : %d configured - start & stop them from the page\n", len(srv.appsList()))
	startAppsFlag := slices.Contains(os.Args, "--start-apps")
	killOnExit := slices.Contains(os.Args, "--kill-apps-on-exit")
	// Release builds behave like an app: window closed = done. macOS gets the
	// same app-window experience in every build (it is a desktop OS, and Mac
	// and Windows should behave the same); --no-window restores the old
	// browser-tab flow.
	windowMode := guiBuild || runtime.GOOS == "darwin"
	if slices.Contains(os.Args, "--no-window") {
		windowMode = false
	}
	if slices.Contains(os.Args, "--window") {
		windowMode = true
	}
	noOpen := os.Getenv("NO_OPEN") == "1" || slices.Contains(os.Args, "--no-open")
	useBrowser := slices.Contains(os.Args, "--browser") // skip the native window
	// The native window is the release experience on Windows: a real Win32
	// window (WebView2) whose close button quits the application.
	nativeWindow := guiBuild && windowMode && !useBrowser && !noOpen
	// Menu bar companion — darwin: NSStatusItem in the native window;
	// windows: notification-area icon beside the WebView2 window; linux: a
	// StatusNotifierItem on the session bus (the launcher owns no window
	// there). The tray keeps the deck alive with the dashboard closed, so
	// closing hides instead of quitting; --no-tray restores the old
	// close-to-quit behavior for one launch, and the deck tools General
	// section's "Menu bar companion" (launcher.json trayIcon, default on)
	// turns it off for good.
	trayKind := ""
	if !slices.Contains(os.Args, "--no-tray") && settingsTrayIcon.Load() {
		switch {
		case nativeWindow && runtime.GOOS == "darwin":
			trayKind = "darwin"
		case nativeWindow && runtime.GOOS == "windows":
			trayKind = "windows"
		case windowMode && runtime.GOOS == "linux":
			trayKind = "linux"
		}
	}
	if trayKind != "" {
		trayRequested = true
		trayServer = srv
		trayDashboardURL = url
		if trayKind == "linux" {
			// the session-bus tray installs synchronously so the lifecycle
			// gates below know whether a tray actually owns the deck — no
			// bus or watcher (SSH, CI) simply means no tray
			if !trayInstallLinux() {
				trayKind = ""
				trayRequested = false
			}
		}
	}
	if trayKind != "" {
		trayActiveFlag.Store(true)
		trayRefresh()
		trayRefreshLoop()
	}
	go srv.resmonLoop()    // per-card RAM/CPU sampling (resmon.go) — cache only, request paths never sample
	go srv.lifecycleLoop() // policy enforcement (lifecycle.go): on-demand idle stops, schedule windows
	if autostart := countAutostart(srv.appsList()); autostart > 0 {
		fmt.Printf("            (%d autostart app(s) will start now)\n", autostart)
	}
	if startAppsFlag {
		fmt.Println("            (--start-apps given: starting every stopped app now)")
	}
	switch {
	case nativeWindow:
		if trayKind != "" {
			fmt.Println("            (tray companion: closing the window hides it — quit from the tray menu or ⌘Q)")
		} else {
			fmt.Println("            (native window: closing it stops the launcher and its apps)")
		}
	case windowMode:
		if trayKind != "" {
			fmt.Println("            (tray companion: quit from the tray menu — closing the dashboard page keeps the deck running)")
		} else {
			fmt.Println("            (window mode: closing the dashboard stops the launcher and its apps)")
		}
	case killOnExit:
		fmt.Println("            (--kill-apps-on-exit: apps started by this panel stop when it exits)")
	default:
		fmt.Println("            (launching/quitting this panel does NOT start or stop any app)")
	}
	if embeddedBuild {
		fmt.Printf("  Data      : %s\n", cfg.DataDir)
	}
	if killOnExit || windowMode {
		fmt.Println("  Stop      : close the dashboard / Ctrl+C   (apps started by this panel stop too)")
	} else {
		fmt.Println("  Stop      : Ctrl+C   (apps keep running)")
	}
	fmt.Println("  --------------------------------------------------")
	fmt.Println()

	if startAppsFlag || countAutostart(srv.appsList()) > 0 || countLifecycleAlways(srv.appsList()) > 0 {
		time.AfterFunc(800*time.Millisecond, func() {
			for _, a := range srv.appsList() {
				if (startAppsFlag || a.Autostart || srv.policyFor(a).Mode == "always") && !srv.appIsRunning(a) {
					srv.startApp(a)
				}
			}
		})
	}
	if !nativeWindow && !noOpen {
		open := openBrowser
		if windowMode {
			open = openAppWindow
		}
		time.AfterFunc(800*time.Millisecond, func() { open(url) })
	}
	if windowMode && !trayEnabled() {
		// Tray builds skip the watchdog: the menu bar companion owns the
		// lifecycle, and a hidden window's throttled heartbeats would
		// otherwise "shut down" a deck the user closed on purpose.
		startWindowWatchdog(srv)
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fail(err.Error())
	}
	// Both listeners get the same guard chain: the Host allowlist (DNS
	// rebinding), a request-body cap, the /api rate budget and the baseline
	// response headers. Timeouts are shared too — see serverTimeouts for why
	// ReadTimeout stays at zero.
	handler := secureHeaders(limitBodies(limitAPIRate(guardHosts(srv))))
	rh, rd, wr, idle := serverTimeouts()
	httpSrv := &http.Server{Handler: handler, ReadHeaderTimeout: rh, ReadTimeout: rd, WriteTimeout: wr, IdleTimeout: idle}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()

	// HTTPS twin: same handler on a second port with the self-generated
	// certificate, for browsers arriving over the Wi-Fi/LAN (loopback keeps
	// plain HTTP — that traffic never crosses the network). Boot never fails
	// on it: a busy port or a generation hiccup only skips TLS.
	if cfg.TLSPort > 0 {
		prov, terr := ensureServerCert(cfg.DataDir)
		switch {
		case terr != nil:
			fmt.Fprintf(os.Stderr, "  launcher: TLS disabled — %v\n", terr)
		default:
			tlsLn, lerr := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.TLSPort)))
			if lerr != nil {
				fmt.Fprintf(os.Stderr, "  launcher: TLS disabled — port %d unavailable: %v\n", cfg.TLSPort, lerr)
			} else {
				srv.tlsProv = prov
				// The rotator keeps the pair fresh on long-running launches:
				// once a day, inside the rotation window, regenerate live.
				srv.startTLSRotator()
				tlsSrv := &http.Server{Handler: handler, TLSConfig: &tls.Config{
					GetCertificate: prov.getCertificate,
					MinVersion:     tls.VersionTLS12,
				}, ReadHeaderTimeout: rh, ReadTimeout: rd, WriteTimeout: wr, IdleTimeout: idle}
				go func() { serveErr <- tlsSrv.ServeTLS(tlsLn, "", "") }()
				fmt.Printf("  🔒 https (Wi-Fi): https://127.0.0.1:%d — browsers ask to trust the certificate once\n", cfg.TLSPort)
			}
		}
	}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-interrupt
		if windowMode || killOnExit {
			srv.beginShutdown("Ctrl+C") // stops managed apps, then exits
		}
		fmt.Print("\n  Launcher stopped. (Apps keep running — stop them from the page next time.)\n")
		os.Exit(0)
	}()

	if nativeWindow {
		runtime.LockOSThread() // the WebView2/window-system loop owns this thread
		// AppKit quit paths (Cmd+Q, Quit menu item) bridge into this via
		// native_darwin_quit.go; on other platforms it simply stays unset.
		runNativeQuit = func() { srv.beginShutdown("quit requested from the AppKit menu") }
		launchWindow = "native"
		setInitialWindowTransparency(readBackgroundConf(cfg.DataDir).Transparency)
		dataPath := ""
		if embeddedBuild {
			dataPath = cfg.DataDir
		}
		if runNativeWindow(url, dataPath) {
			// Window closed (or Terminate) → quit the application.
			srv.beginShutdown("launcher window was closed")
			return
		}
		// WebView2 runtime unavailable → chromeless Edge/Chrome window instead.
		// The tray did not install either — restore the watchdog contract
		// this fallback path relies on.
		if trayKind != "" {
			trayActiveFlag.Store(false)
			trayRequested = false
			startWindowWatchdog(srv)
		}
		launchWindow = "app"
		if !noOpen {
			time.AfterFunc(200*time.Millisecond, func() { openAppWindow(url) })
		}
	} else if !noOpen {
		if windowMode {
			launchWindow = "app"
		} else {
			launchWindow = "browser"
		}
	}

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			fail(err.Error())
		}
	}
}

// countAutostart reports how many apps are flagged to boot with the launcher.
// "Always on" lifecycle apps boot too (see lifecycle.go) but are counted
// separately so the boot banner stays honest about why apps are starting.
func countLifecycleAlways(apps []*App) int {
	n := 0
	for _, a := range apps {
		if a.Lifecycle == "always" {
			n++
		}
	}
	return n
}

// countAutostart reports how many apps are flagged to boot with the launcher.
func countAutostart(apps []*App) int {
	n := 0
	for _, a := range apps {
		if a.Autostart {
			n++
		}
	}
	return n
}

// openBrowser is the Go stand-in for Python's webbrowser.open.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	hideWindow(cmd) // `cmd /c start` would otherwise flash a console
	_ = cmd.Start()
}
