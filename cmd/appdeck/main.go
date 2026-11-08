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
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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
type Config struct {
	// ProjectRoot is the parent of Root (Python PROJECT_ROOT). Kept for parity
	// with server.py, which defines it but never uses it.
	ProjectRoot string
	// Root is the directory holding apps.json, index.html, state.json and logs/
	// (Python ROOT — the directory containing server.py). In single-file builds
	// it is the data directory instead.
	Root      string
	AppsFile  string
	StateFile string
	LogDir    string
	IndexFile string
	Host      string
	Port      int
	// DataDir is where the single-file build unpacks app binaries, assets,
	// logs and state (empty in folder mode).
	DataDir string
	// PortsFile persists auto-assigned app ports (ports.json) so an app with
	// "port": 0 keeps the same URL across restarts.
	PortsFile string
	// LANToken (LAUNCHER_LAN_TOKEN): when set, state-changing requests from
	// non-loopback callers must carry the matching X-AppDeck-Token header.
	// Loopback requests never need it.
	LANToken string
	// TLSPort serves the same dashboard over HTTPS with a self-generated
	// certificate (LAUNCHER_TLS=0 disables, LAUNCHER_TLS_PORT overrides the
	// default 9443). The plain HTTP listener stays for loopback + the native
	// window — traffic that never crosses the network.
	TLSPort int
	// Warning carries a non-fatal boot problem (an unavailable configured
	// storage location) to the dashboard's warning list.
	Warning string
}

// DefaultConfig resolves paths relative to the executable or working
// directory. LAUNCHER_ROOT overrides it for packaged deployments. In
// single-file builds everything lives under a per-user data directory
// (LAUNCHER_DATA_DIR overrides) so nothing is written next to the exe.
func DefaultConfig() Config {
	dataDir := defaultDataDir()
	storageWarning := ""
	// A configured storage location (any drive/volume) overrides the
	// default; an unavailable one falls back with a warning so the deck
	// always boots.
	if pointed, problem := readStoragePointer(); pointed != "" {
		if problem == "" {
			dataDir = pointed
		} else {
			storageWarning = problem
		}
	}
	if embeddedBuild {
		return Config{
			Root:      dataDir,
			DataDir:   dataDir,
			AppsFile:  "", // packed inside the executable
			StateFile: filepath.Join(dataDir, "state.json"),
			LogDir:    filepath.Join(dataDir, "logs"),
			IndexFile: "", // served from the embedded dashboard
			Host:      envOr("HOST", "0.0.0.0"),
			Port:      envInt("PORT", 9090),
			TLSPort:   tlsPortFromEnv(),
			PortsFile: filepath.Join(dataDir, "ports.json"),
			Warning:   storageWarning,
		}
	}
	root := sourceDir()
	if v := os.Getenv("LAUNCHER_ROOT"); v != "" {
		root = v
	}
	// state.json + ports.json live in the per-user data dir, never beside the
	// exe: replacing/updating the launcher must not touch deck state. A
	// configured storage location (any drive/volume) overrides the default.
	if pointed, problem := readStoragePointer(); problem == "" && pointed != "" {
		dataDir = pointed
	}
	migrateUserState(root, dataDir)
	logDir := filepath.Join(root, "logs")
	appsFile := filepath.Join(root, "apps.json")
	if insideAppBundle(root) {
		// A bundled launcher never logs beside the exe: Contents/ is inside
		// the code-signature seal, and a growing log there invalidates it on
		// every write. Logs join the rest of deck state in the data dir.
		logDir = filepath.Join(dataDir, "logs")
		// The registry is deck state like state.json and ports.json: a
		// bundled launcher keeps it in the data dir too, seeded once from the
		// bundle's copy. Uninstalls then rewrite the data-dir file instead of
		// reaching into the seal, and a launcher update can never resurrect a
		// removed entry.
		migrateRegistry(filepath.Join(root, "apps.json"), dataDir)
		appsFile = filepath.Join(dataDir, "apps.json")
	}
	return Config{
		ProjectRoot: filepath.Dir(root),
		Root:        root,
		AppsFile:    appsFile,
		StateFile:   filepath.Join(dataDir, "state.json"),
		LogDir:      logDir,
		IndexFile:   filepath.Join(root, "index.html"),
		Host:        envOr("HOST", "0.0.0.0"),
		Port:        envInt("PORT", 9090),
		TLSPort:     tlsPortFromEnv(),
		LANToken:    os.Getenv("LAUNCHER_LAN_TOKEN"),
		// DataDir is the per-user store for app data and settings in every
		// build mode; folder mode never unpacks into it, but app data and
		// settings must not land inside the source tree.
		DataDir:   dataDir,
		Warning:   storageWarning,
		PortsFile: filepath.Join(dataDir, "ports.json"),
	}
}

// migrateRegistry seeds the data directory's apps.json from a bundled
// launcher's copy — once. The registry is deck state: once the data-dir
// copy exists it is the only one, so uninstalls rewrite it there instead of
// reaching into the .app seal, and a launcher update can never resurrect a
// removed entry. The bundle copy stays as a first-boot seed for fresh
// installs and is never touched again.
func migrateRegistry(bundleAppsFile, dataDir string) {
	if bundleAppsFile == "" || dataDir == "" {
		return
	}
	dst := filepath.Join(dataDir, "apps.json")
	if fileExists(dst) || !fileExists(bundleAppsFile) {
		return
	}
	data, err := os.ReadFile(bundleAppsFile)
	if err != nil {
		return
	}
	_ = os.WriteFile(dst, data, 0o644)
}

// migrateUserState moves pre-2026-10 state.json/ports.json from the launcher
// folder into the per-user data dir, once, so launcher updates never touch
// deck state. Cross-drive moves fall back to copy+delete.
func migrateUserState(fromDir, toDir string) {
	os.MkdirAll(toDir, 0o755)
	for _, name := range []string{"state.json", "ports.json"} {
		src := filepath.Join(fromDir, name)
		dst := filepath.Join(toDir, name)
		if !fileExists(src) || fileExists(dst) {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			if data, rerr := os.ReadFile(src); rerr == nil {
				if werr := os.WriteFile(dst, data, 0o644); werr == nil {
					os.Remove(src)
				}
			}
		}
	}
}

// sourceDir returns the runtime directory, so moving a complete app directory
// does not leave the launcher pointing at the build machine's source tree.
func sourceDir() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		exeDir := filepath.Dir(exe)
		if fileExists(filepath.Join(exeDir, "apps.json")) {
			return exeDir
		}
	}
	wd, _ := os.Getwd()
	return wd
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// -------------------------------------------------------------------- apps ----

// App mirrors one entry of the "apps" list in apps.json.
type App struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Icon     string `json:"icon"`
	Desc     string `json:"desc"`
	Category string `json:"category"`
	Port     int    `json:"port"`
	URLPath  string `json:"urlPath"`
	// Health is an optional probe path (e.g. "/healthz") polled once the
	// port answers: the card shows Starting… until it responds, then Ready.
	// Empty = the port answering is the whole truth.
	Health string `json:"health,omitempty"`
	// StartWait overrides the deck tools "app start timeout" for this app
	// (seconds): how long a start waits for the web UI to answer HTTP before
	// reporting "still starting". 0/unset = the deck-wide setting.
	StartWait int               `json:"startWait,omitempty"`
	Cmd       string            `json:"cmd"`
	Binary    string            `json:"binary,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Network   bool              `json:"network"`
	Env       map[string]string `json:"env,omitempty"`
	ServedBy  string            `json:"servedBy,omitempty"` // "launcher" = HTML app hosted in-process (single-file build)
	RawDir    string            `json:"dir"`
	// Desktop marks a portless desktop app (GUI/CLI binary, no web server):
	// "running" is the state-tracked process being alive, not a port probe.
	Desktop bool `json:"desktop,omitempty"`
	// Cli marks a headless terminal tool (an MCP stdio server, a CLI
	// toolkit): it rides the desktop lifecycle (PID liveness, no port) but
	// shows in the ⌨ CLI tools section with an open-a-terminal action
	// instead of Show — there is no window to focus.
	Cli bool `json:"cli,omitempty"`
	// Autostart boots the app with the launcher; Accent overrides the card
	// color (empty = derived from the app's icon.png).
	Autostart bool   `json:"autostart,omitempty"`
	Accent    string `json:"accent,omitempty"`
	// APIVersion is the app.json contract version (manifest apps only; the
	// central registry leaves it 0). Source records where the app came from:
	// "registry" (apps.json), "manifest" (app.json) or "discovered".
	APIVersion int `json:"apiVersion,omitempty"`
	// Hidden marks a manifest app as intentionally excluded from the deck:
	// the folder stays in Apps/, the dashboard skips it entirely.
	Hidden bool `json:"hidden,omitempty"`
	// Version is the app's own version string from app.json (display only).
	Version string `json:"version,omitempty"`
	// Origin is the URL the app was imported from (a git repository or a
	// download URL), recorded by the importer in the installed manifest. The
	// card's ↻ action re-imports from it in place; empty for local imports.
	Origin string `json:"origin,omitempty"`
	// Requires names prerequisite tool ids (go, rust, git, python, node,
	// docker, nginx) the app needs — the dashboard's 🧰 tab shows their status.
	Requires []string `json:"requires,omitempty"`
	// Privileges lists special access the app needs ("elevated" — macOS TCC
	// privacy grants like Full Disk Access today). A privileged app launches
	// from its own identity (see privileges.go); see AUTHORING.md.
	Privileges []string `json:"privileges,omitempty"`
	// Privacy names the macOS TCC category the elevation guide opens —
	// "accessibility", "screen", "input", "appManagement"; empty = Full Disk
	// Access, the default. Windows and Linux have no per-category privacy
	// model (admin/root already covers every class), so the field only
	// shapes the macOS guidance; their launch mechanics ignore it.
	Privacy string `json:"privacy,omitempty"`
	// KeepAlive restarts the app's process when it dies (crash watchdog).
	KeepAlive bool `json:"keepAlive,omitempty"`
	// Signature records what import learned about a macOS app's code seal —
	// "vendor" (developer signature verified), "adhoc" (import re-signed it
	// after the seal broke) or "invalid" (nothing verified; macOS may kill
	// it at launch). Empty on non-darwin decks and never-checked apps; shown
	// as a card badge so a later macOS security prompt has context.
	Signature string `json:"signature,omitempty"`
	// Security is the app's last malware-scan level — "warn" or "flagged"
	// (empty = clean/never scanned). Set from security/verdicts.json at
	// load and refreshed by the 🛡 scans; "flagged" makes Start ask first.
	Security string `json:"security,omitempty"`
	Source   string `json:"-"`
	Dir      string `json:"-"` // resolved absolute dir (Python "_dir")
}

// stateEntry is one value in state.json (Python: {"pid": …, "startedAt": …}).
// PortOverride records the custom port the app was last started on (""
// bodies in state.json stay absent); Stop clears it with the entry. Token is
// the per-start APPDECK_TOKEN the child received (see AUTHORING.md) — kept so
// diagnostics can confirm which instance is running.
type stateEntry struct {
	PID          int     `json:"pid"`
	StartedAt    float64 `json:"startedAt"`
	PortOverride int     `json:"portOverride,omitempty"`
	Token        string  `json:"token,omitempty"`
	Privileged   bool    `json:"privileged,omitempty"` // started elevated: Stop must re-raise the OS prompt to kill it
	LastError    string  `json:"lastError,omitempty"`  // last failed start's punchline — the card's ⚠ chip
}

type stateMap map[string]stateEntry

// Server implements http.Handler. One instance owns the app registry, the
// in-flight guard and the state-file lock (Python: APPS / INFLIGHT / MUTEX).
type Server struct {
	cfg      Config
	apps     []*App
	byID     map[string]*App
	mu       sync.Mutex
	inflight map[string]struct{}
	// warnings holds non-fatal registry problems (invalid third-party
	// manifests, exhausted port range) reported on /api/health.
	warnings []string
	// hostedListeners holds the in-process HTTP listeners of HTML apps hosted
	// inside the launcher itself (from the embed in single-file builds, from
	// the app's disk folder in dev).
	hostedListeners map[string]net.Listener
	// tlsProv serves the HTTPS listener's certificate; nil when TLS is off.
	// Regeneration swaps the certificate inside the provider, live.
	tlsProv *certProvider
	// accentCache memoizes per-app card colors derived from their icons.
	accentCache sync.Map
	// health caches the background liveness probe (does the app's port
	// actually answer HTTP?) — running ≠ serving. absent = unknown.
	health map[string]bool
	// lanExposed caches the contract-lint probe: an app the deck believes is
	// loopback-only (network: false) whose port answers on the LAN address
	// anyway — it ignored APPDECK_HOST and bound wider than promised.
	// absent = not probed / not applicable.
	lanExposed map[string]bool
	// keepAlive holds stop channels for the per-app crash watchers.
	keepAlive map[string]chan struct{}
	// jobs holds live import-progress logs, keyed by the dashboard's
	// ?jobid=. See joblog.go.
	jobs   map[string]*jobLog
	jobsMu sync.Mutex
	// usageMu serializes usage.json read-modify-write cycles (a start and a
	// stop of different apps can race).
	usageMu sync.Mutex
	// snapshotMu makes background deck snapshots one-at-a-time; TryLock
	// skips a snapshot when one is already running.
	snapshotMu sync.Mutex
}

// procStartedAt is this process's boot time (unix seconds). /api/health
// reports it so the dashboard can tell a launcher restart from a page
// reload even when the build version is identical (dev builds).
var procStartedAt = time.Now().Unix()

// NewServer loads the app registry and returns a handler serving the
// launcher API.
func NewServer(cfg Config) (*Server, error) {
	loadCustomToolPaths(cfg.DataDir)
	apps, byID, warnings, err := loadApps(cfg)
	if err != nil {
		return nil, err
	}
	seedBuiltinThemes(cfg.DataDir)
	seedBuiltinBackgrounds(cfg.DataDir)
	seedBuiltinIconPacks(cfg.DataDir)
	if cfg.Warning != "" {
		warnings = append([]string{cfg.Warning}, warnings...)
	}
	s := &Server{
		cfg:             cfg,
		apps:            apps,
		byID:            byID,
		inflight:        make(map[string]struct{}),
		warnings:        warnings,
		hostedListeners: make(map[string]net.Listener),
		health:          make(map[string]bool),
		lanExposed:      make(map[string]bool),
		keepAlive:       make(map[string]chan struct{}),
	}
	s.applyNetworkOverrides(apps)
	s.applyPrivilegeOverrides(apps)
	ls := s.loadLauncherSettings()
	// Re-assert the login item: an updated or moved executable would
	// otherwise leave the old path registered with the OS.
	if ls.LaunchAtLogin {
		if exe, err := os.Executable(); err == nil {
			_ = setLoginItem(exe, true)
		}
	}
	// The WKWebView download delegate needs the destination before the first
	// click, not only after a settings change.
	nativeConfigureDownloads(ls.DownloadDir, ls.DownloadAsk)
	s.startHealthProber()
	s.startWatchFolder()
	return s, nil
}

// launcherVersion is stamped by build.sh / release.sh via
// -X main.launcherVersion=<date>; "dev" means a plain `go build`. A var, not
// a const — -X only rewrites variables.
var launcherVersion = "dev"

// appByID returns the app with the given id (nil when unknown).
func (s *Server) appByID(id string) *App {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[id]
}

// appsList returns a snapshot of the app registry.
func (s *Server) appsList() []*App {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apps
}

// reloadApps re-reads the registry (after an install or scaffold) and swaps
// it in atomically. Existing *App pointers held by in-flight actions keep
// working — App fields are read-only after load.
func (s *Server) reloadApps() error {
	apps, byID, warnings, err := loadApps(s.cfg)
	if err != nil {
		return err
	}
	s.applyNetworkOverrides(apps)
	s.applyPrivilegeOverrides(apps)
	s.mu.Lock()
	s.apps, s.byID, s.warnings = apps, byID, warnings
	s.mu.Unlock()
	return nil
}

// loadApps merges the three app sources — the central apps.json registry,
// per-folder app.json manifests (third-party apps), and heuristic discovery
// of plain index.html / Go-module folders — assigns stable ports to
// portless web apps, and returns non-fatal problems as warnings. Only a
// broken central registry is fatal; one bad third-party manifest just
// becomes a warning.
// foldPath normalises a path for case-insensitive map lookups on filesystems
// where "Apps/CargoScope" and "apps/cargoscope" are the same directory.
// Case-sensitive platforms are unaffected (folding a lowercase path is a no-op
// there because both sides were written in matching case).
func foldPath(p string) string {
	return strings.ToLower(p)
}

func loadApps(cfg Config) ([]*App, map[string]*App, []string, error) {
	var parsed struct {
		Apps []*App `json:"apps"`
	}
	if embeddedBuild {
		data, err := embeddedConfigJSON()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, nil, nil, err
			}
			// Not staged (test binaries run with -tags embed but no
			// release.sh payload): an empty registry, not a fatal error.
			parsed.Apps = nil
			data = nil
		}
		if data != nil {
			if err := json.Unmarshal(data, &parsed); err != nil {
				return nil, nil, nil, fmt.Errorf("embedded apps.json: %w", err)
			}
		}
	} else if data, err := os.ReadFile(cfg.AppsFile); err == nil {
		if err := json.Unmarshal(data, &parsed); err != nil {
			return nil, nil, nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, nil, err
	}
	if parsed.Apps == nil {
		parsed.Apps = []*App{}
	}
	overriddenIDs := map[string]bool{}
	if embeddedBuild {
		for i, a := range parsed.Apps {
			if a == nil || a.ID == "" {
				continue
			}
			override, ok, err := embeddedOverrideApp(cfg.DataDir, a.ID)
			if err != nil {
				return nil, nil, nil, err
			}
			if ok {
				parsed.Apps[i] = override
				overriddenIDs[a.ID] = true
			}
		}
	}
	// Apps this machine uninstalled: the payload still carries them, so the
	// record is what keeps the card off the deck (and extraction skips them).
	// An imported copy sets the override marker and wins — a re-install always
	// brings the app back.
	removedIDs := readRemovedApps(cfg.DataDir)
	byID := make(map[string]*App, len(parsed.Apps))
	kept := make([]*App, 0, len(parsed.Apps))
	configuredDirs := make(map[string]bool, len(parsed.Apps))
	var warnings []string
	for _, a := range parsed.Apps {
		if a == nil || a.ID == "" {
			return nil, nil, nil, fmt.Errorf("%s: app entry missing \"id\"", cfg.AppsFile)
		}
		if embeddedBuild && removedIDs[a.ID] && !overriddenIDs[a.ID] {
			// Uninstalled on this machine. Claim the slug folder so a leftover
			// copy from an older boot cannot resurface as a discovered app.
			if abs, err := filepath.Abs(embeddedOverrideDir(cfg.DataDir, a.ID)); err == nil {
				configuredDirs[foldPath(abs)] = true
			}
			continue
		}
		if _, dup := byID[a.ID]; dup {
			return nil, nil, nil, fmt.Errorf("duplicate app id in apps.json: %s", a.ID)
		}
		byID[a.ID] = a
		if a.Source == "" {
			a.Source = "registry"
		}
		// In embed mode the generated registry carries the bare binary name
		// ("sentinel") plus dir "apps/<id>"; prefix it so Binary is
		// RawDir-relative exactly like manifest-discovered apps, and
		// resolveBinary finds it in the app folder. Folder mode keeps the
		// registry's own "../Apps/<Name>/<bin>" spelling as-is.
		if embeddedBuild && a.Binary != "" && !filepath.IsAbs(a.Binary) &&
			!strings.HasPrefix(a.Binary, "apps/") && !strings.HasPrefix(a.Binary, "Apps/") {
			a.Binary = a.RawDir + "/" + a.Binary
		}

		// Apps hosted inside the launcher: in the single-file build they have
		// no registry folder (assets come from the exe); in folder mode they
		// serve straight from their disk folder.
		if a.ServedBy == "launcher" && embeddedBuild && a.RawDir == "" {
			// Claim the app's data-dir slug folder (Apps/<id>) anyway when one
			// is present: a folder-mode episode (materializeFolderDeck) or an
			// older install leaves exactly that folder behind, complete with
			// app.json, and manifest discovery would otherwise flag the app's
			// own home as a duplicate id on every boot. Claiming uses the slug
			// folder directly — Join(DataDir, RawDir) cannot be used here,
			// RawDir is empty and would claim the whole data directory. A
			// genuine imported override replaces this entry before the loop
			// (embeddedOverrideApp sets RawDir), so overrides are unaffected.
			if abs, err := filepath.Abs(embeddedOverrideDir(cfg.DataDir, a.ID)); err == nil {
				configuredDirs[foldPath(abs)] = true
			}
			kept = append(kept, a)
			continue
		}

		// Python: a["_dir"] = (ROOT / a["dir"]).resolve()
		base := cfg.Root
		if embeddedBuild {
			base = cfg.DataDir // apps are unpacked into the data directory
		}
		dir := filepath.Join(base, a.RawDir)
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		// Cross-mode decks (a folder-mode launcher running apps that were
		// unpacked into the per-user data dir — e.g. after an update from an
		// all-in-one): the app folder lives in the data directory, not beside
		// the exe. resolveBinary already falls back to the data dir for the
		// binary; the folder itself needs the same fallback.
		if info, err := os.Stat(dir); (err != nil || !info.IsDir()) &&
			cfg.DataDir != "" && cfg.DataDir != base && a.RawDir != "" {
			if alt := filepath.Join(cfg.DataDir, a.RawDir); alt != dir {
				if altInfo, altErr := os.Stat(alt); altErr == nil && altInfo.IsDir() {
					dir = alt
				}
			}
		}
		a.Dir = dir
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			// Skip rather than fail: one missing folder should not take down
			// the whole launcher (matters in trimmed release bundles too).
			fmt.Fprintf(os.Stderr, "launcher: warning: skipping app %q — folder missing: %s\n", a.ID, dir)
			delete(byID, a.ID)
			continue
		}
		if absolute, err := filepath.Abs(a.Dir); err == nil {
			configuredDirs[foldPath(absolute)] = true
		}
		// A registry entry that omits "desktop" while the app's own app.json
		// sets it: the flag is structural (portless GUI launch), not
		// presentation metadata, and the entry may simply predate it — the
		// manifest's word wins. The same merge genembedded applies when
		// packing single-file builds.
		if !a.Desktop && manifestDesktopFlag(dir) {
			a.Desktop = true
			a.Port = 0
			a.Args = stripPortArgs(a.Args)
		}
		// A registry entry for a hosted HTML app: apps.json's static cards
		// carry no binary and no cmd — genembedded marks the same entries
		// servedBy "launcher" when packing single-file builds, but a plain
		// folder-mode registry never did, so Start exec'd an empty command
		// and the card died with "process exited during startup". The
		// folder's own app.json is the contract (the same lift genembedded
		// applies); root HTML is re-checked in case the folder changed
		// since the manifest was written.
		if a.ServedBy == "" && a.Binary == "" && strings.TrimSpace(a.Cmd) == "" && !a.Desktop &&
			manifestServedByLauncher(dir) && hasRootHTML(a.Dir) {
			a.ServedBy = "launcher"
		}
		kept = append(kept, a)
	}

	// Third-party manifest apps. Folder mode scans the project tree (the
	// launcher's parent, including its Apps/ group folder); single-file
	// builds scan only the data directory — the one place an install
	// (POST /api/import) ever writes. rawDirPrefix makes each discovered
	// folder's RawDir registry-shaped ("../Apps/x" vs "Apps/x").
	if embeddedBuild {
		// The import endpoint installs into <data>/Apps (appsContainerDir,
		// capital A). Scan the container itself, not the data directory: with
		// the data dir as the base, the Apps container matches the "Apps"
		// group-folder rule and every imported app's RawDir doubles to
		// "Apps/Apps/<id>" — a path that exists nowhere on disk, so the
		// binary resolver can only look in phantom folders.
		prefix, container := appsContainerPaths(cfg.DataDir)
		manifestApps := discoverManifestApps(
			[]string{container}, prefix, configuredDirs, byID, &warnings)
		kept = append(kept, manifestApps...)
	} else {
		manifestApps := discoverManifestApps(
			[]string{cfg.ProjectRoot}, "../", configuredDirs, byID, &warnings)
		kept = append(kept, manifestApps...)

		// Imports of a launcher installed inside a macOS .app bundle land in
		// the data directory's Apps container: appsContainerDir redirects
		// there because Contents/ is a sealed region no install may write
		// into. Discovery must look where installs go, or every imported app
		// vanishes from the deck on the next reload — the project tree holds
		// only the bundle. The prefix is DataDir-relative, the same spelling
		// the registry fallback above resolves for cross-mode decks.
		if base := cfg.ProjectRoot; (base == "" || insideAppBundle(base)) && cfg.DataDir != "" {
			prefix, container := appsContainerPaths(cfg.DataDir)
			manifestApps := discoverManifestApps(
				[]string{container}, prefix, configuredDirs, byID, &warnings)
			kept = append(kept, manifestApps...)
		}

		discovered, err := discoverApps(cfg, byID, kept)
		if err != nil {
			return nil, nil, nil, err
		}
		kept = append(kept, discovered...)
	}

	// Stable ports for portless web apps (manifest "port": 0, heuristic
	// discoveries). Assignments persist in ports.json so URLs survive
	// restarts; the launcher's own port and every configured port are taken.
	reg := newPortRegistry(cfg.PortsFile)
	taken := map[int]bool{cfg.Port: true}
	live := make(map[string]bool, len(kept))
	for _, a := range kept {
		live[a.ID] = true
		if a.Port != 0 {
			taken[a.Port] = true
		}
	}
	for _, a := range kept {
		reg.ensureAssigned(a, a.ID, taken)
		if a.Port != 0 {
			taken[a.Port] = true
		} else if !a.Desktop {
			// Desktop apps are portless by design; a web app still on 0 has
			// run out of auto-assign range.
			warnings = append(warnings, fmt.Sprintf(
				"app %q has no port — the auto-assign range %d–%d is exhausted", a.ID, autoPortLo, autoPortHi))
		}
	}
	reg.prune(live)

	applySecurityVerdicts(cfg.DataDir, kept)
	return kept, byID, warnings, nil
}

// discoverApps adds simple static web projects that are present beside the
// launcher but have not yet been added to apps.json. The registry remains the
// place for custom commands, ports, labels, and non-static applications.
func discoverApps(cfg Config, byID map[string]*App, configured []*App) ([]*App, error) {
	projectRoot := filepath.Dir(cfg.Root)
	launcherRoot, _ := filepath.Abs(cfg.Root)
	entries, err := os.ReadDir(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("scan project folders: %w", err)
	}

	// Candidate folders: root-level apps plus everything inside an "Apps"
	// group folder (mirrors tools/genembedded; see appsContainer there).
	type scanItem struct{ name, dir, prefix string }
	var items []scanItem
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "Launcher" ||
			strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dir := filepath.Join(projectRoot, entry.Name())
		if strings.EqualFold(entry.Name(), "Apps") {
			subs, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, sub := range subs {
				if !sub.IsDir() || strings.HasPrefix(sub.Name(), ".") ||
					strings.HasPrefix(sub.Name(), "_") ||
					strings.EqualFold(sub.Name(), "node_modules") {
					continue
				}
				items = append(items, scanItem{sub.Name(), filepath.Join(dir, sub.Name()), "../Apps/"})
			}
			continue
		}
		items = append(items, scanItem{entry.Name(), dir, "../"})
	}

	// Folders already claimed by the central registry or an app.json
	// manifest are skipped; ports are left 0 for the registry to assign
	// stably, and new apps default to loopback-only (network: false).
	configuredDirs := make(map[string]bool, len(configured))
	for _, a := range configured {
		if absolute, err := filepath.Abs(filepath.Join(cfg.Root, a.RawDir)); err == nil {
			configuredDirs[foldPath(absolute)] = true
		}
	}
	var discovered []*App
	for _, item := range items {
		if absolute, _ := filepath.Abs(item.dir); absolute == launcherRoot {
			continue
		}
		if absolute, _ := filepath.Abs(item.dir); configuredDirs[foldPath(absolute)] {
			continue
		}
		// A folder carrying app.json is manifest-managed (shown or hidden
		// there) — heuristic discovery must never resurrect it.
		if _, err := os.Stat(filepath.Join(item.dir, manifestFileName)); err == nil {
			continue
		}
		kind, entrypoint := discoverableProject(item.dir)
		if kind == "" {
			continue
		}
		id := appID(item.name)
		if _, exists := byID[id]; exists {
			continue
		}
		rawDir := item.prefix + item.name
		app := &App{
			ID: id, Name: item.name, Icon: "📦",
			Desc:     "Discovered static web app",
			Category: "Discovered", Port: 0, URLPath: "/",
			RawDir: rawDir, Network: false, Dir: item.dir,
			Source: "discovered",
		}
		if kind == "go" {
			app.Binary = rawDir + "/" + id + "-launcher"
			app.Args = []string{}
			app.Desc = "Discovered Go app"
		} else if kind == "rust" {
			// Desktop (portless) — the common case for Rust GUI apps. The
			// binary name is the folder id; the cargo builder maps it to the
			// workspace package producing that binary.
			app.Binary = rawDir + "/" + id
			app.Args = []string{}
			app.Desktop = true
			app.Desc = "Discovered Rust app"
		} else if kind == "rust-web" {
			app.Binary = rawDir + "/" + id
			app.Args = []string{}
			app.Desc = "Discovered Rust app"
		} else {
			app.ServedBy = "launcher" // hosted in-process, from its folder
			if entrypoint != "index.html" {
				app.URLPath = "/" + entrypoint
			}
		}
		if abs, err := filepath.Abs(item.dir); err == nil {
			app.Dir = abs
		}
		byID[id] = app
		discovered = append(discovered, app)
	}

	sort.Slice(discovered, func(i, j int) bool { return discovered[i].ID < discovered[j].ID })
	return discovered, nil
}

// discoverableProject identifies folders that contain a runnable app. HTML
// projects use the static server; Go modules use the launcher build logic;
// Cargo workspaces likewise build on first start — desktop (portless) unless
// the manifest depends on a known HTTP server crate.
func discoverableProject(dir string) (kind, entrypoint string) {
	if fileExists(filepath.Join(dir, "go.mod")) {
		if goHTTPEntrypoint(dir) {
			return "go", ""
		}
		return "", ""
	}
	if fileExists(filepath.Join(dir, "Cargo.toml")) {
		if rustHTTPProject(dir) {
			return "rust-web", ""
		}
		return "rust", ""
	}
	// A C/C++ source tree is not a static site even when a stray .html sits
	// at its root (ImageMagick ships a redirect index.html there) — the C
	// indicators outrank it so the import refuses honestly instead.
	if len(cProjectIndicators(dir)) > 0 {
		return "", ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", ""
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), ".html") {
			if entry.Name() == "index.html" {
				return "static", "index.html"
			}
			if entrypoint == "" {
				entrypoint = entry.Name()
			}
		}
	}
	if entrypoint != "" {
		return "static", entrypoint
	}
	return "", ""
}

// cProjectIndicators names the root files that mark a C/C++ source project:
// build files first, then root sources/headers. Used by the project
// classifiers (folder discovery and git import) to keep such repositories
// from being mistaken for anything AppDeck can run today.
func cProjectIndicators(dir string) []string {
	buildFiles := []string{"CMakeLists.txt", "Makefile", "Makefile.am", "configure", "configure.ac", "meson.build"}
	var found []string
	for _, n := range buildFiles {
		if fileExists(filepath.Join(dir, n)) {
			found = append(found, n)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return found
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".c", ".cc", ".cpp", ".cxx", ".h", ".hpp":
			found = append(found, e.Name())
		}
		if len(found) >= 4 {
			break
		}
	}
	return found
}

func goHTTPEntrypoint(dir string) bool {
	candidates := goSourceFiles(dir)
	if entries, err := os.ReadDir(filepath.Join(dir, "cmd")); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				candidates = append(candidates, filepath.Join(dir, "cmd", entry.Name(), "main.go"))
			}
		}
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		source := string(data)
		if strings.Contains(source, "net/http") ||
			strings.Contains(source, "ListenAndServe") ||
			strings.Contains(source, "http.Server") {
			return true
		}
	}
	return false
}

func goSourceFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".go" {
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}
	return files
}

func appID(name string) string {
	fields := strings.Fields(strings.ToLower(name))
	return strings.Join(fields, "-")
}

func loadState(path string) stateMap {
	data, err := os.ReadFile(path)
	if err != nil {
		return stateMap{}
	}
	st := stateMap{}
	if err := json.Unmarshal(data, &st); err != nil || st == nil {
		return stateMap{}
	}
	return st
}

// saveState writes state.json atomically (Python: tmp file + replace).
func saveState(path string, st stateMap) {
	tmp := strings.TrimSuffix(path, filepath.Ext(path)) + ".tmp"
	data, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "launcher: save_state: %v\n", err)
		return
	}
	// 0600: state.json carries each app's APPDECK_TOKEN. Chmod covers a temp
	// file left behind by an older build that wrote 0644.
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "launcher: save_state: %v\n", err)
		return
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "launcher: save_state: %v\n", err)
	}
}

// ------------------------------------------------------------------ probes ----

var lanCache struct {
	sync.Mutex
	ip string
	at time.Time
}

// lanIP returns this machine's Wi-Fi/LAN address ("" when offline / no
// interface). Cached for 30 s like the Python version.
func lanIP() string {
	lanCache.Lock()
	defer lanCache.Unlock()
	if time.Since(lanCache.at) < 30*time.Second {
		return lanCache.ip
	}
	ip := ""
	if runtime.GOOS == "windows" {
		// Prefer the adapter that actually has a default gateway (the real
		// LAN/Wi-Fi). "First IPv4" can pick a dead link-local 169.254.* or a
		// virtual adapter, which advertises an unreachable URL.
		out, err := runCommand(5*time.Second, "powershell", "-NoProfile", "-Command",
			"Get-NetIPConfiguration | Where-Object {$null -ne $_.IPv4DefaultGateway} | Select-Object -First 1 | ForEach-Object { $_.IPv4Address.IPAddress }")
		if err == nil {
			if trimmed := strings.TrimSpace(out); trimmed != "" {
				ip = strings.TrimSpace(strings.Split(trimmed, "\n")[0])
			}
		}
		if ip == "" {
			// Fall back: any IPv4 that is neither loopback nor link-local APIPA.
			out, err := runCommand(5*time.Second, "powershell", "-NoProfile", "-Command",
				"Get-NetIPAddress -AddressFamily IPv4 | Where-Object {$_.IPAddress -notlike '127.*' -and $_.IPAddress -notlike '169.254.*'} | Select-Object -First 1 -ExpandProperty IPAddress")
			if err == nil {
				if trimmed := strings.TrimSpace(out); trimmed != "" {
					ip = strings.TrimSpace(strings.Split(trimmed, "\n")[0])
				}
			}
		}
	}
	if ip == "" {
		for _, ifc := range []string{"en0", "en1"} { // macOS: primary Wi-Fi / ethernet
			out, err := runCommand(3*time.Second, "ipconfig", "getifaddr", ifc)
			if err != nil {
				continue
			}
			if trimmed := strings.TrimSpace(out); trimmed != "" {
				ip = trimmed
				break
			}
		}
	}
	if ip == "" {
		// UDP "connect" picks a source address without sending packets —
		// same trick as the Python socket fallback.
		if conn, err := net.DialTimeout("udp", "8.8.8.8:80", time.Second); err == nil {
			if host, _, splitErr := net.SplitHostPort(conn.LocalAddr().String()); splitErr == nil && !strings.HasPrefix(host, "127.") {
				ip = host
			}
			conn.Close()
		}
	}
	lanCache.ip, lanCache.at = ip, time.Now()
	return ip
}

// portOpen reports whether something accepts TCP connections on the port.
func portOpen(port int, timeout time.Duration) bool {
	return hostPortOpen("127.0.0.1", port, timeout)
}

// hostPortOpen reports whether host:port accepts a TCP connection.
func hostPortOpen(host string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// runCommand runs a helper command with a timeout and captures stdout.
// Mirrors subprocess.run(..., capture_output=True, text=True, timeout=N):
// on timeout or spawn failure the output is discarded.
func runCommand(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd) // no console flash for tasklist/netstat/powershell helpers
	out, err := cmd.Output()
	return string(out), err
}

// -------------------------------------------------------- app status types ----

// appStatus is the JSON object of GET /api/apps. Pointer fields are emitted as
// null when unset, matching the Python dict's explicit nulls.
type appStatus struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Icon         string `json:"icon"`
	IconFile     bool   `json:"iconFile"`
	Accent       string `json:"accent,omitempty"`
	Autostart    bool   `json:"autostart"`
	Desktop      bool   `json:"desktop,omitempty"`
	Cli          bool   `json:"cli,omitempty"` // headless terminal tool — ⌨ section, terminal action, no Show
	Desc         string `json:"desc"`
	Category     string `json:"category"`
	Port         int    `json:"port"`
	PortOverride int    `json:"portOverride,omitempty"`
	URL          string `json:"url"`
	Cmd          string `json:"cmd"`
	Network      bool   `json:"network"`
	Elevated     bool   `json:"elevated,omitempty"`
	Storage      string `json:"storage,omitempty"` // location name for an app living on another drive
	LastError    string `json:"lastError,omitempty"`
	// HasBinary marks apps the elevation toggle can apply to: elevating
	// wraps the app's own binary (macOS TCC bundle / Windows UAC / Linux
	// pkexec), and static or shell-cmd apps have none.
	HasBinary bool   `json:"hasBinary,omitempty"`
	Version   string `json:"version,omitempty"`
	// Signature mirrors App.Signature: "", "vendor", "adhoc" or "invalid" —
	// the macOS code-seal state import recorded (the card badge's data).
	Signature string `json:"signature,omitempty"`
	// Exposeable/Exposed drive the card's 🔗 button: whether the app carries
	// something the host OS can surface (a .app bundle on macOS, the binary
	// elsewhere) and whether that artifact currently exists (the
	// /Applications link, the Start-menu pin, the .desktop entry).
	Exposeable bool  `json:"exposeable,omitempty"`
	Exposed    bool  `json:"exposed,omitempty"`
	Healthy    *bool `json:"healthy,omitempty"`
	// LanExposed is the contract-lint verdict: true when a this-computer-
	// only app (network: false) still answers on the LAN address — it
	// ignored the launcher's APPDECK_HOST. Only set for running web apps.
	LanExposed *bool    `json:"lanExposed,omitempty"`
	LanURL     *string  `json:"lanUrl"`
	Running    bool     `json:"running"`
	Managed    bool     `json:"managed"`
	External   bool     `json:"external"`
	PID        *int     `json:"pid"`
	Proc       *string  `json:"proc"`
	StartedAt  *float64 `json:"startedAt"`
	Source     string   `json:"source,omitempty"` // registry | manifest | discovered
	// HealthPath mirrors App.Health so the card knows a Ready label is
	// meaningful; Origin is the import URL the ↻ update action re-fetches.
	HealthPath string `json:"health,omitempty"`
	Origin     string `json:"origin,omitempty"`
	// Usage stats (usage.json): last start, how many runs, total runtime.
	LastStarted *float64 `json:"lastStarted,omitempty"`
	RunCount    int      `json:"runCount,omitempty"`
	RunSeconds  float64  `json:"runSeconds,omitempty"`
}

func (s *Server) appStatus(a *App, st stateMap) appStatus {
	entry := st[a.ID]
	pid := entry.PID
	ours := pid != 0 && pidAlive(pid)
	// A custom-port start records its port in the state entry; every probe
	// and URL below must look at that port, not the configured one.
	port := a.Port
	if entry.PortOverride != 0 {
		port = entry.PortOverride
	}
	// Web apps announce themselves by listening on their port; a desktop app
	// has no port, so its process being alive is the whole truth. (Only this
	// launcher can start a desktop app, so "external" can never happen.)
	running := portOpen(port, defaultProbeTimeout)
	if a.Desktop {
		running = ours
	}

	var proc *string
	if running && !a.Desktop {
		if pids := listenerPIDs(port); len(pids) > 0 {
			if label := procCmdline(pids[0]); label != "" {
				proc = &label
			}
		}
	}

	var lanURL *string
	if a.Network && !a.Desktop {
		if lan := lanIP(); lan != "" {
			u := fmt.Sprintf("http://%s:%d%s", lan, port, a.URLPath)
			lanURL = &u
		}
	}

	url := ""
	if !a.Desktop {
		url = fmt.Sprintf("http://127.0.0.1:%d%s", port, a.URLPath)
	}

	exposeable, exposed := s.exposeInfo(a)

	out := appStatus{
		ID:           a.ID,
		Name:         a.Name,
		Icon:         a.Icon,
		IconFile:     appHasIconFile(a),
		Accent:       s.accentFor(a),
		Autostart:    a.Autostart,
		Desktop:      a.Desktop,
		Cli:          a.Cli,
		Desc:         a.Desc,
		Category:     a.Category,
		Port:         a.Port,
		PortOverride: entry.PortOverride,
		URL:          url,
		Cmd:          a.Cmd,
		Network:      a.Network,
		Elevated:     a.privilegeElevated(),
		Storage:      s.appStorageLocation(a),
		LastError:    entry.LastError,
		HasBinary:    a.Binary != "",
		Version:      a.Version,
		Signature:    a.Signature,
		Exposeable:   exposeable,
		Exposed:      exposed,
		LanURL:       lanURL,
		Running:      running,
		Managed:      ours && running,
		External:     running && !ours,
		Proc:         proc,
		Source:       a.Source,
		HealthPath:   a.Health,
		Origin:       a.Origin,
	}
	// Usage stats: "last run 2h ago" on the card and the Recently used sort.
	if u := s.readUsage(a.ID); u.LastStarted > 0 || u.RunCount > 0 {
		ls := u.LastStarted
		out.LastStarted = &ls
		out.RunCount = u.RunCount
		out.RunSeconds = u.SecondsRun
	}
	// Healthy: does the app's port actually answer? Only meaningful for a
	// running web app whose probe has come back at least once.
	if running && !a.Desktop {
		s.mu.Lock()
		healthy, probed := s.health[a.ID]
		s.mu.Unlock()
		if probed {
			out.Healthy = &healthy
		}
	}
	if running && !a.Desktop && !a.Network {
		s.mu.Lock()
		exposed, probed := s.lanExposed[a.ID]
		s.mu.Unlock()
		if probed {
			out.LanExposed = &exposed
		}
	}
	if ours {
		out.PID = &pid
		startedAt := entry.StartedAt
		out.StartedAt = &startedAt
	}
	return out
}

// ----------------------------------------------------------- start / stop ----

// actionResp is the JSON body of /api/start and /api/stop. Pointer/omitempty
// fields are present in the output exactly when the Python version includes
// them (e.g. "killed": [] stays visible, absent keys stay absent).
type actionResp struct {
	OK             bool       `json:"ok"`
	Busy           bool       `json:"busy,omitempty"`
	AlreadyRunning bool       `json:"alreadyRunning,omitempty"`
	Error          string     `json:"error,omitempty"`
	PID            *int       `json:"pid,omitempty"`
	Slow           bool       `json:"slow,omitempty"`
	Note           string     `json:"note,omitempty"`
	Killed         *[]int     `json:"killed,omitempty"`
	StillRunning   *bool      `json:"stillRunning,omitempty"`
	Status         *appStatus `json:"status,omitempty"`
	ID             string     `json:"id,omitempty"` // set on /api/all/* results
}

type allResp struct {
	OK      bool          `json:"ok"`
	Results []*actionResp `json:"results"`
}

type healthResp struct {
	OK      bool    `json:"ok"`
	Version string  `json:"version,omitempty"` // launcher build stamp
	Started int64   `json:"started,omitempty"` // this process's boot time (unix) — identifies the run, since Version is constant across a dev restart
	TLSPort int     `json:"tlsPort,omitempty"` // HTTPS listener port (0/omitted = TLS off)
	Lan     *string `json:"lan"`
	LanURL  *string `json:"lanUrl"`
	Mode    string  `json:"mode,omitempty"`    // "single-file" in embed builds
	DataDir string  `json:"dataDir,omitempty"` // where extracted apps live (embed builds)
	Window  string  `json:"window,omitempty"`  // "native" | "app" | "browser" | "none"
	OS      string  `json:"os,omitempty"`      // runtime.GOOS, e.g. "windows"
	Arch    string  `json:"arch,omitempty"`    // runtime.GOARCH, e.g. "arm64" — platform checks in the dashboard
	// Warnings are non-fatal registry problems (invalid third-party
	// manifests, exhausted port range) the dashboard surfaces once.
	Warnings []string `json:"warnings,omitempty"`
}

type logsResp struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Lines []string `json:"lines"`
}

func (s *Server) logPath(a *App) string {
	return filepath.Join(s.cfg.LogDir, a.ID+".log")
}

// conflictingAppName names another app that is currently holding port ("" when
// none can be identified). Used so a same-port clash reports the culprit
// instead of a bare bind error, and so a binary app whose port is taken by a
// *known other app* is not misreported as its own "already running".
func (s *Server) conflictingAppName(port int, excludeID string) string {
	st := loadState(s.cfg.StateFile)
	for _, other := range s.appsList() {
		if other.ID == excludeID || other.Desktop {
			continue
		}
		// Effective port: a custom-port override wins over the configured one.
		eport := other.Port
		if e := st[other.ID].PortOverride; e != 0 {
			eport = e
		}
		if eport != port {
			continue
		}
		if other.ServedBy == "launcher" {
			s.mu.Lock()
			_, ours := s.hostedListeners[other.ID]
			s.mu.Unlock()
			if ours {
				return other.Name
			}
			continue
		}
		if pid := st[other.ID].PID; pid != 0 && pidAlive(pid) {
			for _, p := range listenerPIDs(port) {
				if p == pid {
					return other.Name
				}
			}
		}
	}
	return ""
}

// startApp runs one start with the in-flight guard (Python start_app).
func (s *Server) startApp(a *App) *actionResp {
	s.mu.Lock()
	if _, busy := s.inflight[a.ID]; busy {
		s.mu.Unlock()
		return &actionResp{OK: false, Busy: true, Error: busyMessage}
	}
	s.inflight[a.ID] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, a.ID)
		s.mu.Unlock()
	}()
	resp := s.startAppLocked(a)
	if resp.OK {
		// A usage run is a launch the launcher performed — an "already
		// running" answer is someone else's instance, not a run.
		if !resp.AlreadyRunning {
			s.recordAppStart(a.ID)
		}
		s.startKeepAlive(a)
	}
	return resp
}

func (s *Server) startAppLocked(a *App) *actionResp {
	if a.ServedBy == "launcher" {
		return s.startHostedStatic(a)
	}
	if a.Desktop {
		return s.startDesktopApp(a)
	}
	if portOpen(a.Port, defaultProbeTimeout) {
		// Someone is on our port: our own instance started outside the
		// launcher reports as already running, but a *known other app* on the
		// same port is a clash worth naming.
		if culprit := s.conflictingAppName(a.Port, a.ID); culprit != "" {
			return &actionResp{OK: false, Error: fmt.Sprintf(
				"port %d is already used by %s — start with a custom port", a.Port, culprit)}
		}
		return &actionResp{OK: true, AlreadyRunning: true}
	}
	if err := os.MkdirAll(s.cfg.LogDir, 0o755); err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	logFile, err := os.OpenFile(s.logPath(a), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "\n===== started %s =====\n", time.Now().Format("2006-01-02 15:04:05"))

	token := newToken()
	cmd, err := s.appCommand(a)
	if err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	cmd.Dir = a.Dir
	cmd.Env = s.childEnv(a, token)
	// Privileged app: launch through the OS's own elevation mapping (TCC
	// identity bundle / UAC script / pkexec-sudo) so grants and the admin
	// token land on the app alone, never the launcher. The tail below is
	// unchanged: every platform's wrapper writes to the same log files.
	var exited chan struct{}
	pid := 0
	wasPrivileged := privilegedLaunch(a)
	if wasPrivileged {
		pid, exited, err = s.launchPrivileged(a, cmd.Env, cmd.Args[1:])
		if err != nil {
			logFile.WriteString("appdeck: privileged launch error: " + err.Error() + "\n")
			return &actionResp{OK: false, Error: err.Error()}
		}
	} else {
		cmd.Stdout = logFile
		cmd.Stderr = logFile // Python: stderr=subprocess.STDOUT
		cmd.SysProcAttr = processSysProcAttr()
		if err := cmd.Start(); err != nil {
			return &actionResp{OK: false, Error: err.Error()}
		}
		pid = cmd.Process.Pid
	}

	s.mu.Lock()
	st := loadState(s.cfg.StateFile)
	st[a.ID] = stateEntry{PID: pid, StartedAt: float64(time.Now().UnixNano()) / 1e9, Token: token, Privileged: wasPrivileged}
	saveState(s.cfg.StateFile, st)
	s.mu.Unlock()

	if exited == nil {
		// Wait for the app's web UI to answer HTTP (not just TCP open) within
		// the configured budget — deck tools "app start timeout", per-app
		// manifest startWait overrides it.
		exited = make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(exited)
		}()
	}
	return waitForAppReady(a, pid, startWaitBudget(a), exited,
		func(port int, timeout time.Duration) bool { return portOpen(port, timeout) },
		httpReady)
}

// startWaitBudget is the effective startup wait for one app: the manifest's
// startWait (seconds) when the author declared one, else the deck tools
// "app start timeout" (launcher.json startWaitSeconds, default 30 s).
func startWaitBudget(a *App) time.Duration {
	if a.StartWait > 0 {
		return time.Duration(a.StartWait) * time.Second
	}
	return time.Duration(startWaitSecondsCache.Load()) * time.Second
}

// httpReady reports whether the app's HTTP handler is actually serving —
// a TCP port can accept connections well before the handler runs (DB
// setup, first-request init), which used to open a blank page. Any HTTP
// response counts as serving, even 401/404/500: bytes on the wire mean a
// handler is there.
func httpReady(a *App) bool {
	path := a.Health
	if path == "" {
		path = a.URLPath
	}
	if path == "" {
		path = "/"
	}
	client := http.Client{Timeout: 600 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", a.Port, path))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// waitForAppReady is the startup wait loop: probe TCP, then HTTP once the
// port is open, until the budget expires or the process exits. The probe
// funcs are parameters so tests can fake both. Returns nil when the app is
// ready; tcpSeen distinguishes the two expiry notes.
func waitForAppReady(a *App, pid int, budget time.Duration, exited <-chan struct{},
	portProbe func(int, time.Duration) bool, httpProbe func(*App) bool,
) *actionResp {
	deadline := time.Now().Add(budget)
	tcpSeen := false
	for {
		if portProbe(a.Port, startProbeTimeout) {
			tcpSeen = true
			if httpProbe(a) {
				return &actionResp{OK: true, PID: &pid}
			}
		}
		select {
		case <-exited:
			return &actionResp{OK: false, Error: "process exited during startup", PID: &pid}
		default:
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(startWaitSleep)
	}
	secs := int(budget.Seconds())
	if tcpSeen {
		return &actionResp{OK: true, PID: &pid, Slow: true, Note: fmt.Sprintf(
			"port answered, but the web UI did not respond within %d s — still starting in the background", secs)}
	}
	return &actionResp{OK: true, PID: &pid, Slow: true, Note: fmt.Sprintf(
		"started, but the port did not answer within %d s — still starting in the background", secs)}
}

// appCommand builds the exec.Cmd an app entry describes: a "binary"
// (auto-built when its platform binary is missing) with {port}/{host}-
// substituted args, else a shell "cmd" string. Shared by the web and desktop
// start paths.
func (s *Server) appCommand(a *App) (*exec.Cmd, error) {
	// An app living on an unplugged drive must fail with the reason, not
	// with a phantom "building from source".
	if msg := s.appStorageProblem(a); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	host := bindHostFor(a)
	if a.Binary != "" {
		binary, err := s.resolveAppBinary(a)
		if err != nil {
			return nil, err
		}
		if why := appBinaryProblem(binary); why != "" {
			return nil, fmt.Errorf("%s cannot run on Windows: %s", binary, why)
		}
		args := make([]string, 0, len(a.Args))
		for _, arg := range a.Args {
			args = append(args, substitutePlaceholders(arg, a.Port, host))
		}
		return exec.Command(binary, args...), nil
	}
	cmdStr := normalizeAppCommand(substitutePlaceholders(a.Cmd, a.Port, host))
	parts := shellCommand(cmdStr)
	return exec.Command(parts[0], parts[1:]...), nil
}

// bindHostFor is the host an app should bind: loopback unless the app opts
// into Wi-Fi sharing (network: true). The loopback default is a security
// decision — an app that binds 0.0.0.0 is reachable from every device on
// the network, so sharing is always an explicit author choice.
func bindHostFor(a *App) string {
	if a.Network {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// substitutePlaceholders expands the app-contract placeholders in args, cmd
// strings and env values: {port} → the app's port, {host} → its bind host.
func substitutePlaceholders(s string, port int, host string) string {
	s = strings.ReplaceAll(s, "{port}", strconv.Itoa(port))
	return strings.ReplaceAll(s, "{host}", host)
}

// startDesktopApp launches a portless desktop app (a GUI/CLI binary with
// "desktop": true). There is no port to wait for, so success is the process
// still being alive shortly after spawn — long enough to catch an instant
// crash (missing binary, bad build); after that the app lives on its own and
// status polls track it through the state entry's PID.
func (s *Server) startDesktopApp(a *App) *actionResp {
	if pid := loadState(s.cfg.StateFile)[a.ID].PID; pid != 0 && pidAlive(pid) {
		return &actionResp{OK: true, AlreadyRunning: true}
	}
	if err := os.MkdirAll(s.cfg.LogDir, 0o755); err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	logFile, err := os.OpenFile(s.logPath(a), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "\n===== started %s =====\n", time.Now().Format("2006-01-02 15:04:05"))

	// A desktop app that dies instantly usually printed why (no GUI in this
	// build, bad build, …). Capture its output alongside the log file so
	// the failure can carry the reason back to the dashboard.
	token := newToken()
	cmd, err := s.appCommand(a)
	if err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	cmd.Dir = a.Dir
	cmd.Env = s.childEnv(a, token)
	if privilegedLaunch(a) {
		// Privileged: launch through the OS's own elevation mapping (TCC
		// identity bundle / UAC script / pkexec-sudo) so grants and the
		// admin token land on its identity. The wrapper writes to the same
		// log, so an instant crash is diagnosable from the Logs button.
		pid, exited, err := s.launchPrivileged(a, cmd.Env, cmd.Args[1:])
		if err != nil {
			logFile.WriteString("appdeck: privileged launch error: " + err.Error() + "\n")
			return &actionResp{OK: false, Error: err.Error()}
		}
		s.mu.Lock()
		st := loadState(s.cfg.StateFile)
		st[a.ID] = stateEntry{PID: pid, StartedAt: float64(time.Now().UnixNano()) / 1e9, Token: token, Privileged: true}
		saveState(s.cfg.StateFile, st)
		s.mu.Unlock()
		select {
		case <-exited:
			return &actionResp{OK: false, Error: "process exited during startup — check the app log", PID: &pid}
		case <-time.After(time.Second):
		}
		return &actionResp{OK: true, PID: &pid, Note: privilegedLaunchNote(a.Privacy)}
	}
	if bundle := s.launchableBundle(a); bundle != "" {
		// The app owns a .app bundle (a vendor bundle from the import, or
		// the elevation wrapper): launch it through Launch Services, exactly
		// like a double-click. Executing the in-bundle binary directly is
		// not equivalent — a CLI-first player like mpv finds no file and no
		// terminal and answers with its usage text, and the app never gets
		// its bundle context (Dock identity, GUI mode). Elevation keeps
		// precedence: the branch above wins. A generated wrapper runs
		// through its shim, which sources this start's environment and
		// writes to the same log, so the launch stays diagnosable.
		pid, exited, err := s.launchAppBundle(a, cmd.Env, bundle, cmd.Args[1:])
		if err != nil {
			logFile.WriteString("appdeck: bundle launch error: " + err.Error() + "\n")
			return &actionResp{OK: false, Error: err.Error()}
		}
		s.mu.Lock()
		st := loadState(s.cfg.StateFile)
		st[a.ID] = stateEntry{PID: pid, StartedAt: float64(time.Now().UnixNano()) / 1e9, Token: token}
		saveState(s.cfg.StateFile, st)
		s.mu.Unlock()
		select {
		case <-exited:
			return &actionResp{OK: false, Error: "process exited during startup — check the app log", PID: &pid}
		case <-time.After(time.Second):
		}
		return &actionResp{OK: true, PID: &pid}
	}
	var outBuf bytes.Buffer
	cmd.Stdout = io.MultiWriter(logFile, &outBuf)
	cmd.Stderr = io.MultiWriter(logFile, &outBuf)
	cmd.SysProcAttr = processSysProcAttr()
	if err := cmd.Start(); err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	pid := cmd.Process.Pid

	s.mu.Lock()
	st := loadState(s.cfg.StateFile)
	st[a.ID] = stateEntry{PID: pid, StartedAt: float64(time.Now().UnixNano()) / 1e9, Token: token}
	saveState(s.cfg.StateFile, st)
	s.mu.Unlock()

	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	select {
	case <-exited:
		msg := "process exited during startup"
		if out := strings.TrimSpace(outBuf.String()); out != "" {
			if usageTextExit(out) {
				msg += ": the program answered with its usage text — it was started " +
					"without the arguments (or the file) it needs. Set them in the " +
					"app's app.json \"args\" or re-import with Arguments filled in. " +
					"Last output: " + lastN(out, 300)
			} else {
				msg += ": " + lastN(out, 300) // the last lines carry the actual error
			}
		}
		return &actionResp{OK: false, Error: msg, PID: &pid}
	case <-time.After(time.Second):
	}
	return &actionResp{OK: true, PID: &pid}
}

// usageTextExit reports whether a child that died at startup answered with
// its usage/help block instead of an error. A CLI-first player (mpv) does
// exactly this when the card starts it with no arguments — no file to play,
// so it prints usage and exits, and the card's real failure is the missing
// arguments, not a crash. "usage:" alone is not enough (a program can log
// it incidentally); a listed --flag alongside it makes the block certain.
func usageTextExit(out string) bool {
	if len(out) > 4096 {
		out = out[len(out)-4096:]
	}
	lo := strings.ToLower(out)
	return strings.Contains(lo, "usage:") && strings.Contains(lo, "--")
}

// startHostedStatic hosts an HTML app inside the launcher process: no child
// process, the port is served from the embedded assets (single-file build)
// or straight from the app's disk folder (dev build).
func (s *Server) startHostedStatic(a *App) *actionResp {
	if ln, ours := s.hostedListeners[a.ID]; ours && ln != nil {
		return &actionResp{OK: true, AlreadyRunning: true}
	}
	if portOpen(a.Port, defaultProbeTimeout) {
		// Not our listener — either a foreign process or (the classic footgun)
		// another app configured on the same port. Name it if we can.
		msg := fmt.Sprintf("port %d is already in use", a.Port)
		if culprit := s.conflictingAppName(a.Port, a.ID); culprit != "" {
			msg += fmt.Sprintf(" by %s", culprit)
		}
		msg += " — start with a custom port"
		return &actionResp{OK: false, Error: msg}
	}
	handler, err := s.staticHandlerFor(a)
	if err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	if err := os.MkdirAll(s.cfg.LogDir, 0o755); err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	if logFile, err := os.OpenFile(s.logPath(a), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644); err == nil {
		fmt.Fprintf(logFile, "\n===== started %s (hosted inside launcher.exe) =====\n",
			time.Now().Format("2006-01-02 15:04:05"))
		logFile.Close()
	}
	host := "127.0.0.1"
	if a.Network {
		host = "0.0.0.0"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(a.Port)))
	if err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	pid := os.Getpid()
	s.mu.Lock()
	s.hostedListeners[a.ID] = ln
	st := loadState(s.cfg.StateFile)
	st[a.ID] = stateEntry{PID: pid, StartedAt: float64(time.Now().UnixNano()) / 1e9}
	saveState(s.cfg.StateFile, st)
	s.mu.Unlock()
	go func() {
		srv := &http.Server{Handler: guardHosts(hostedHandler(s, handler, a)), ReadHeaderTimeout: 10 * time.Second}
		_ = srv.Serve(ln) // returns when the app is stopped or the launcher exits
	}()
	return &actionResp{OK: true, PID: &pid}
}

// stopHostedStatic closes the in-process listener of a hosted HTML app.
func (s *Server) stopHostedStatic(a *App) *actionResp {
	s.mu.Lock()
	ln := s.hostedListeners[a.ID]
	delete(s.hostedListeners, a.ID)
	st := loadState(s.cfg.StateFile)
	delete(st, a.ID)
	saveState(s.cfg.StateFile, st)
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	for i := 0; i < 10; i++ {
		if !portOpen(a.Port, startProbeTimeout) {
			still := false
			killed := []int{}
			return &actionResp{OK: true, Killed: &killed, StillRunning: &still}
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Port still held after our listener closed → someone else's process; use
	// the ordinary external-kill path so Stop stays trustworthy.
	killed := []int{}
	for _, p := range listenerPIDs(a.Port) {
		if killProc(p) == nil {
			killed = append(killed, p)
		}
	}
	for i := 0; i < stopWaitRounds; i++ {
		if !portOpen(a.Port, startProbeTimeout) {
			break
		}
		time.Sleep(stopWaitSleep)
	}
	still := portOpen(a.Port, startProbeTimeout)
	return &actionResp{OK: !still, Killed: &killed, StillRunning: &still}
}

// resolveAppBinary resolves a.Binary with build progress streamed to the
// app's own log file — first starts of source apps (cargo/go builds) are
// long and silent otherwise, and the 📄 Logs view is where the operator
// looks.
func (s *Server) resolveAppBinary(a *App) (string, error) {
	binary, err := resolveBinary(s.cfg.Root, a.Binary)
	if err == nil {
		return s.rebuildIfImported(a, binary), nil
	}
	// The registry/discovery may express the binary Root-relative while the
	// app folder lives in the data directory (cross-mode decks); retry
	// there before giving up.
	if s.cfg.DataDir != "" && s.cfg.DataDir != s.cfg.Root {
		if binary2, err2 := resolveBinary(s.cfg.DataDir, a.Binary); err2 == nil {
			return s.rebuildIfImported(a, binary2), nil
		}
	}
	// The binary's home may be inside the app's .app bundle: an elevated
	// macOS launch moves it there (its one copy), and an app switched back
	// to inherited simply runs it from the bundle contents.
	if bundleBin := bundleBinaryPath(a.Dir, a.ID); bundleBin != "" && fileExists(bundleBin) {
		if err := ensureExecutable(bundleBin); err == nil {
			return s.rebuildIfImported(a, bundleBin), nil
		}
	}
	// Builds stream into the log only when one is actually attempted —
	// a missing binary for a manifest without sources stays a clean error.
	if a.Dir != "" {
		if logw, lerr := os.OpenFile(s.logPath(a), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644); lerr == nil {
			fmt.Fprintf(logw, "appdeck: no prebuilt binary found — building from source on first start (this can take minutes; live output below)\n")
			binary3, berr := resolveBinaryLogged(s.cfg.Root, a.Binary, logw)
			// Same cross-mode fallback as the prebuilt resolve above: a
			// bundled launcher's imported sources live in the data
			// directory, and Root-relative builds cannot see them.
			if berr != nil && s.cfg.DataDir != "" && s.cfg.DataDir != s.cfg.Root {
				binary3, berr = resolveBinaryLogged(s.cfg.DataDir, a.Binary, logw)
			}
			logw.Close()
			if berr == nil {
				// Built from the very sources an import just landed — the
				// rebuild marker is satisfied by construction; leaving it
				// would rebuild the fresh binary once more on the next start.
				os.Remove(filepath.Join(a.Dir, rebuildMarker))
				// The binary is generated: the build inputs are dead weight
				// now (the release build never ships them).
				s.pruneImportedSources(a)
			}
			return binary3, berr
		}
	}
	return "", err
}

func resolveBinary(root, configured string) (string, error) {
	return resolveBinaryLogged(root, configured, nil)
}

func resolveBinaryLogged(root, configured string, logw io.Writer) (string, error) {
	// The app folder is the single home in every build mode: embedded
	// builds unpack binaries into <data>/<RawDir> (= apps/<id>/, the same
	// place imports extract to), and resolution tries the configured path,
	// its platform-suffixed form, its plain .exe form (how embedded builds
	// unpack it on Windows), then the legacy flat bin/ cache from
	// older installs. Matching the folder-mode rules keeps one mental model:
	// "the binary lives beside app.json".
	path := configured
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path, _ = filepath.Abs(path)
	platformPath := platformBinaryPath(path)
	// The binary's one name is the plain configured one. A legacy
	// platform-suffixed binary (<name>-<os>-<arch>) on disk — older builds and
	// imports spelled it that way — is renamed to the plain name on first
	// resolve, so an app folder carries exactly one binary. A rename that
	// On Windows the canonical plain name carries .exe (embedded builds
	// unpack it there, go builds write it there). The extensionless spelling
	// is a legacy artifact: elevated starts go through Start-Process, which
	// cannot execute an extensionless file, and an extensionless Mach-O/ELF
	// that rode an asset copy would shadow the real binary entirely.
	exePath := filepath.Join(filepath.Dir(path), embeddedBinaryName(filepath.Base(configured)))
	windowsCanonical := runtime.GOOS == "windows" && exePath != path
	if windowsCanonical {
		if fileExists(platformPath) && !fileExists(exePath) {
			if err := os.Rename(platformPath, exePath); err == nil && logw != nil {
				fmt.Fprintf(logw, "appdeck: renamed %s → %s (the platform suffix is retired)\n",
					filepath.Base(platformPath), filepath.Base(exePath))
			}
		}
		if fileExists(platformPath) && fileExists(exePath) {
			if err := os.Remove(platformPath); err == nil && logw != nil {
				fmt.Fprintf(logw, "appdeck: removed %s (%s is the one that runs)\n",
					filepath.Base(platformPath), filepath.Base(exePath))
			}
		}
		// A stale foreign-platform binary (a Mach-O/ELF that rode an asset
		// copy) must never shadow the real .exe — drop it when the real
		// binary exists. An extensionless PE or script stays where it is:
		// resolution below simply prefers the .exe spelling.
		if fileExists(path) && fileExists(exePath) && foreignExecutable(path) {
			if err := os.Remove(path); err == nil && logw != nil {
				fmt.Fprintf(logw, "appdeck: removed %s (stale foreign-platform binary beside %s)\n",
					filepath.Base(path), filepath.Base(exePath))
			}
		}
	} else {
		// The binary's one name is the plain configured one. A legacy
		// platform-suffixed binary (<name>-<os>-<arch>) on disk — older
		// builds and imports spelled it that way — is renamed to the plain
		// name on first resolve, so an app folder carries exactly one
		// binary. A rename that cannot land (a running app's exe on Windows)
		// keeps resolving through the candidates below; the next resolve
		// retries.
		if fileExists(platformPath) && !fileExists(path) {
			if err := os.Rename(platformPath, path); err == nil && logw != nil {
				fmt.Fprintf(logw, "appdeck: renamed %s → %s (the platform suffix is retired)\n",
					filepath.Base(platformPath), filepath.Base(path))
			}
		}
		// Both present: the plain name is canonical (imports, extraction and
		// fresh builds all write it), so the suffixed sibling is dead weight
		// — drop it. Best effort: a running app's exe on Windows cannot be
		// removed; the next resolve retries.
		if fileExists(platformPath) && fileExists(path) {
			if err := os.Remove(platformPath); err == nil && logw != nil {
				fmt.Fprintf(logw, "appdeck: removed %s (the plain %s is the one that runs)\n",
					filepath.Base(platformPath), filepath.Base(path))
			}
		}
	}
	// The plain configured path first: imported exes keep whatever name they
	// were imported with (it may already carry a platform suffix), while
	// repo-built apps write the plain name. On Windows the .exe spelling
	// wins — see windowsCanonical above.
	var candidates []string
	if windowsCanonical {
		candidates = []string{exePath, platformPath, path}
	} else {
		candidates = []string{path, platformPath}
		if exePath != path {
			candidates = append(candidates, exePath)
		}
	}
	if embeddedBuild {
		// Legacy cache from older all-in-one installs: <data>/bin/<base>.
		candidates = append(candidates,
			filepath.Join(root, "bin", embeddedBinaryName(filepath.Base(configured))))
	}
	for _, candidate := range candidates {
		if fileExists(candidate) {
			if err := ensureExecutable(candidate); err != nil {
				return "", fmt.Errorf("make binary executable: %s: %v", candidate, err)
			}
			return candidate, nil
		}
	}
	// No prebuilt binary — build from source, the way the folder declares
	// itself: a Go module builds with go, a Cargo workspace with cargo.
	// Fresh builds take the binary's plain configured name (descale, not
	// descale-darwin-arm64); older suffixed binaries on disk keep resolving
	// through the candidates above.
	output := buildOutputPath(path)
	moduleDir := filepath.Dir(output)
	var buildErr error
	switch {
	case fileExists(filepath.Join(moduleDir, "go.mod")):
		_, buildErr = buildGoBinaryLogged(output, logw)
	case fileExists(filepath.Join(moduleDir, "Cargo.toml")):
		_, buildErr = buildRustBinary(output, logw)
	default:
		return "", fmt.Errorf("app binary not found: %s (looked in %s; the app folder has no go.mod or Cargo.toml — build the app and put the binary beside app.json)",
			configured, strings.Join(candidates, ", "))
	}
	if buildErr != nil {
		return "", fmt.Errorf("app binary not found: %s (looked in %s; automatic build failed: %v)",
			configured, strings.Join(candidates, ", "), buildErr)
	}
	return output, nil
}

// rebuildIfImported honors the .appdeck-rebuild marker an import leaves in a
// buildable app folder. Imports copy the source folder as-is — including any
// prebuilt binary it carries — and resolveBinary would run that binary
// forever: the build path only triggers when a binary is missing outright,
// never when the sources around it are newer. So a re-imported app ran its
// previous version. The marker says "sources just landed": rebuild from them
// before running. Failure (often a missing toolchain) keeps the marker and
// runs the existing binary — Start never breaks, the next Start retries.
func (s *Server) rebuildIfImported(a *App, binary string) string {
	if a.Binary == "" || a.Dir == "" {
		return binary
	}
	marker := filepath.Join(a.Dir, rebuildMarker)
	if _, err := os.Stat(marker); err != nil {
		return binary
	}
	// The rebuild writes beside app.json — the binary's canonical home —
	// wherever the running binary was found (an elevated macOS app's binary
	// lives inside its .app bundle; the fresh build lands in the folder and
	// the next privileged start moves it in).
	output := buildOutputPath(filepath.Join(a.Dir, filepath.Base(a.Binary)))
	logw, lerr := os.OpenFile(s.logPath(a), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if lerr != nil {
		logw = nil
	} else {
		defer logw.Close()
		fmt.Fprintf(logw, "appdeck: sources re-imported — rebuilding before start (the binary the folder carried may be stale)\n")
	}
	var berr error
	// go build -o refuses to overwrite a non-object file, and the stale
	// binary sits exactly at the output path — move it aside first. If the
	// build fails it goes back (it is the fallback that keeps Start working
	// until the next retry); on success it is deleted with the marker.
	fallback := ""
	if fileExists(output) {
		fallback = output + ".stale"
		if err := os.Rename(output, fallback); err != nil {
			fallback = ""
		}
	}
	switch {
	case fileExists(filepath.Join(a.Dir, "go.mod")):
		_, berr = buildGoBinaryLogged(output, logw)
	case fileExists(filepath.Join(a.Dir, "Cargo.toml")):
		_, berr = buildRustBinary(output, logw)
	default:
		if fallback != "" {
			_ = os.Rename(fallback, output)
		}
		os.Remove(marker) // folder is no longer buildable — drop the stale marker
		return binary
	}
	if berr != nil {
		if fallback != "" {
			_ = os.Rename(fallback, output)
		}
		if logw != nil {
			fmt.Fprintf(logw, "appdeck: rebuild failed — running the existing binary; the rebuild retries on the next start (%v)\n", berr)
		}
		return binary
	}
	if fallback != "" {
		_ = os.Remove(fallback)
	}
	os.Remove(marker)
	// The rebuild succeeded — the build inputs are dead weight now (the
	// release build never ships them).
	s.pruneImportedSources(a)
	if logw != nil {
		fmt.Fprintf(logw, "appdeck: rebuild finished → %s\n", filepath.Base(output))
	}
	return output
}

// pruneImportedSources strips an app's build inputs (see pruneBuildInputs)
// when its folder lives in the installed container — imported copies only:
// a dev-tree app's folder IS its live source tree and is never touched.
// Best-effort by design: a failed prune must never fail a start; it simply
// retries at the next successful build or boot sweep.
func (s *Server) pruneImportedSources(a *App) {
	if a == nil || a.Dir == "" {
		return
	}
	container, err := s.appsContainerDir()
	if err != nil {
		return
	}
	dirAbs, err := filepath.Abs(a.Dir)
	if err != nil {
		return
	}
	cAbs, err := filepath.Abs(container)
	if err != nil {
		return
	}
	if dirAbs != cAbs && !strings.HasPrefix(dirAbs, cAbs+string(filepath.Separator)) {
		return
	}
	if err := pruneBuildInputs(a.Dir); err != nil {
		fmt.Fprintf(os.Stderr, "launcher: could not prune build sources in %s: %v\n", a.Dir, err)
	}
}

// buildOutputPath names a fresh build after its configured binary — plain
// <name>, with the .exe Windows requires to execute. The legacy
// <name>-<os>-<arch> spelling stays readable through the candidates list.
func buildOutputPath(path string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(path, ".exe") {
		return path + ".exe"
	}
	return path
}

func platformBinaryPath(path string) string {
	suffixed := path + "-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		suffixed += ".exe" // Windows needs the extension to execute reliably
	}
	return suffixed
}

// foreignExecutable reports whether the file is a Mach-O or ELF binary —
// a foreign-platform executable that can never run on this machine. Such
// files usually arrive as stale prebuilts riding an asset copy, and on
// Windows they brick the app when the resolver picks them over the real
// .exe.
func foreignExecutable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	for _, m := range machoMagics {
		if bytes.Equal(head, m) {
			return true
		}
	}
	return bytes.Equal(head, []byte{0x7F, 'E', 'L', 'F'})
}

// buildGoBinary builds an app on first use when its platform binary is absent.
// Most apps have main.go at the module root; Monitor uses the conventional
// cmd/monitor entrypoint, which is discovered automatically.
func buildGoBinary(output string) (string, error) {
	return buildGoBinaryLogged(output, nil)
}

func buildGoBinaryLogged(output string, logw io.Writer) (string, error) {
	moduleDir := filepath.Dir(output)
	if !fileExists(filepath.Join(moduleDir, "go.mod")) {
		return "", fmt.Errorf("no go.mod in %s", moduleDir)
	}

	buildTarget := "."
	if !hasMainPackage(moduleDir) {
		entries, err := os.ReadDir(filepath.Join(moduleDir, "cmd"))
		if err != nil {
			return "", fmt.Errorf("no Go entrypoint in %s", moduleDir)
		}
		var entrypoints []string
		for _, entry := range entries {
			if entry.IsDir() && hasMainPackage(filepath.Join(moduleDir, "cmd", entry.Name())) {
				entrypoints = append(entrypoints, "./cmd/"+entry.Name())
			}
		}
		if len(entrypoints) == 0 {
			return "", fmt.Errorf("no Go entrypoint in %s", moduleDir)
		}
		// Multi-cmd modules (a main server beside converters/tools): the
		// entrypoint whose name matches the app's configured binary builds
		// it; a single entrypoint needs no name. Anything else is ambiguous.
		buildTarget = ""
		want := filepath.Base(output)
		for _, ep := range entrypoints {
			if strings.TrimPrefix(ep, "./cmd/") == want {
				buildTarget = ep
				break
			}
		}
		if buildTarget == "" {
			if len(entrypoints) == 1 {
				buildTarget = entrypoints[0]
			} else {
				return "", fmt.Errorf("module %s has several cmd entrypoints and none matches the binary %q", moduleDir, want)
			}
		}
	}

	goTool, err := goToolPath()
	if err != nil {
		return "", fmt.Errorf("go toolchain not found — install Go (https://go.dev/dl/) or put the built binary next to app.json")
	}
	cmd := exec.Command(goTool, "build", "-o", output, buildTarget)
	hideWindow(cmd)
	cmd.Dir = moduleDir
	var buf bytes.Buffer
	if logw != nil {
		fmt.Fprintf(logw, "appdeck: go build -o %s (in %s)\n", filepath.Base(output), moduleDir)
		cmd.Stdout = io.MultiWriter(&buf, logw)
		cmd.Stderr = io.MultiWriter(&buf, logw)
	} else {
		cmd.Stdout = &buf
		cmd.Stderr = &buf
	}
	if err := cmd.Run(); err != nil {
		outputText := buf.Bytes()
		message := strings.TrimSpace(string(outputText))
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("go build in %s: %s", moduleDir, message)
	}
	if !fileExists(output) {
		return "", fmt.Errorf("go build completed without creating %s", output)
	}
	if err := ensureExecutable(output); err != nil {
		return "", fmt.Errorf("make built binary executable: %s: %v", output, err)
	}
	return output, nil
}

// goToolPath locates the go toolchain through the shared lookup — a
// user-registered path (🧰 tab) wins; then PATH; then the well-known
// locations (double-clicked .app launches get a minimal PATH
// (/usr/bin:/bin:/usr/sbin:/sbin) that excludes every usual Go install, so
// PATH alone is not enough).
func goToolPath() (string, error) {
	if p := findToolPath("go", "go"); p != "" {
		return p, nil
	}
	return "", exec.ErrNotFound
}

func hasMainPackage(dir string) bool {
	for _, path := range goSourceFiles(dir) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), "package main") {
			return true
		}
	}
	return false
}

func ensureExecutable(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o111 == 0 {
		return os.Chmod(path, info.Mode().Perm()|0o755)
	}
	return nil
}

// childEnv builds the child environment — the app contract every app can
// rely on (see AUTHORING.md): the inherited environment, then
//
//	PORT, APPDECK_PORT   the port to serve on ({port} in args does the same)
//	APPDECK_HOST         the host to bind (loopback unless network: true)
//	APPDECK_APP_ID       the stable app id
//	APPDECK_DATA         the app's own writable data directory (created)
//	APPDECK_TOKEN        per-start secret; when set, requests must carry it
//	APPDECK_LAUNCHER_URL the dashboard URL (for apps that link back)
//
// then the app's own env entries with {port}/{host} substituted.
func (s *Server) childEnv(a *App, token string) []string {
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		if i := strings.Index(kv, "="); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	port := strconv.Itoa(a.Port)
	host := bindHostFor(a)
	env["PORT"] = port
	env["APPDECK_PORT"] = port
	env["APPDECK_HOST"] = host
	env["APPDECK_APP_ID"] = a.ID
	// A GUI launch inherits Launch Services' minimal PATH
	// (/usr/bin:/bin:/usr/sbin:/sbin); shell-cmd apps ("node server.js") and
	// builds' own subprocesses resolve their tools through it. Append the
	// well-known tool dirs the launcher already probes — additions only, so
	// nothing the operator put on PATH is ever shadowed.
	env["PATH"] = extendPath(env["PATH"], append(toolBinDirs(), systemBinDirs()...))
	// Best python3 on the machine (APPDECK_PYTHON): PATH extension alone
	// leaves the CLT 3.9 stub ahead of every newer install, and venv cmd
	// conventions (${APPDECK_PYTHON:-python3}) build on the newest.
	if py := appPythonPath(); py != "" {
		env["APPDECK_PYTHON"] = py
	}
	if a.Network {
		env["APPDECK_NETWORK"] = "1" // Wi-Fi sharing is on; Host checks may loosen
	}
	if token != "" {
		env["APPDECK_TOKEN"] = token
	}
	// The app-facing data home (a 💾 Storage data/app placement may link it
	// to another drive; the launcher's own files stay beside it, local).
	if home := s.appDataHome(a); home != "" {
		env["APPDECK_DATA"] = home
	}
	if dataDir := s.appDataDir(a); dataDir != "" {
		env["APPDECK_DATA"] = dataDir
	}
	env["APPDECK_LAUNCHER_URL"] = fmt.Sprintf("http://127.0.0.1:%d", s.cfg.Port)
	for k, v := range a.Env {
		env[k] = substitutePlaceholders(v, a.Port, host)
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// appDataDir returns the app's own writable directory under the launcher's
// per-user data dir (created on start): <data>/appdata/<app-id>. Apps must
// never write next to their binary — on macOS bundles and restricted
// Windows installs that fails, and updates would wipe user data.
func (s *Server) appDataDir(a *App) string {
	if s.cfg.DataDir == "" {
		return ""
	}
	dir := filepath.Join(s.cfg.DataDir, "appdata", a.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return dir
}

// newToken generates a fresh 256-bit hex token for one app start.
func newToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano()) // never expected; better than empty
	}
	return hex.EncodeToString(buf)
}

// stopApp runs one stop with the in-flight guard (Python stop_app).
func (s *Server) stopApp(a *App) *actionResp {
	s.stopKeepAlive(a.ID)
	// An app started on a custom port must be probed/killed on that port;
	// the override lives in the state entry until the stop removes it.
	if !a.Desktop {
		if e := loadState(s.cfg.StateFile)[a.ID].PortOverride; e != 0 {
			c := *a
			c.Port = e
			a = &c
		}
	}
	s.mu.Lock()
	if _, busy := s.inflight[a.ID]; busy {
		s.mu.Unlock()
		return &actionResp{OK: false, Busy: true, Error: busyMessage}
	}
	s.inflight[a.ID] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, a.ID)
		s.mu.Unlock()
	}()
	return s.stopAppLocked(a)
}

func (s *Server) stopAppLocked(a *App) *actionResp {
	if a.ServedBy == "launcher" {
		return s.stopHostedStatic(a)
	}
	st := loadState(s.cfg.StateFile)
	pid := st[a.ID].PID
	wasPrivileged := st[a.ID].Privileged
	killed := []int{}
	var elevatedErr error

	// 1) the instance this launcher started (whole process group)
	if pid != 0 && pidAlive(pid) {
		killTree(pid)
		killed = append(killed, pid)
		for i := 0; i < stopWaitRounds; i++ {
			if !pidAlive(pid) {
				break
			}
			time.Sleep(stopWaitSleep)
		}
		if pidAlive(pid) {
			killTreeForce(pid)
		}
		// A privileged start survives both kills (root/admin owns it, the
		// launcher's standard-user token cannot signal it): re-raise the
		// OS prompt and let an elevated taskkill/kill finish the job.
		if pidAlive(pid) && wasPrivileged {
			if err := stopPrivilegedElevated(pid); err == nil {
				for i := 0; i < stopWaitRounds; i++ {
					if !pidAlive(pid) {
						break
					}
					time.Sleep(stopWaitSleep)
				}
			}
		}
	}

	// 2) anything else of ours still listening on the port (external start);
	// desktop apps have no port, so there is nothing external to find
	elevatedTried := false
	if !a.Desktop && portOpen(a.Port, defaultProbeTimeout) {
		for _, p := range listenerPIDs(a.Port) {
			if killProc(p) == nil {
				killed = append(killed, p)
			}
		}
		for i := 0; i < stopWaitRounds; i++ {
			if !portOpen(a.Port, startProbeTimeout) {
				break
			}
			time.Sleep(stopWaitSleep)
		}
		if portOpen(a.Port, startProbeTimeout) {
			for _, p := range listenerPIDs(a.Port) {
				killProcForce(p)
			}
			for i := 0; i < stopWaitRounds; i++ {
				if !portOpen(a.Port, startProbeTimeout) {
					break
				}
				time.Sleep(stopWaitSleep)
			}
		}
		// An elevated instance survives every standard-token kill (Windows
		// integrity levels). That is not limited to the state-tracked PID
		// above: a UAC start by a previous launcher run — its state entry
		// long gone — lives on as an orphaned listener. Re-raise the OS
		// prompt for it too, the same elevated taskkill the privileged
		// start path uses.
		if portOpen(a.Port, startProbeTimeout) {
			for _, p := range listenerPIDs(a.Port) {
				elevatedTried = true
				if err := stopPrivilegedElevated(p); err != nil {
					elevatedErr = err
				}
			}
			for i := 0; i < stopWaitRounds; i++ {
				if !portOpen(a.Port, startProbeTimeout) {
					break
				}
				time.Sleep(stopWaitSleep)
			}
		}
	}

	s.mu.Lock()
	st = loadState(s.cfg.StateFile)
	if entry := st[a.ID]; entry.StartedAt > 0 {
		s.recordAppStop(a.ID, entry.StartedAt) // fold this run into the usage totals
	}
	delete(st, a.ID)
	saveState(s.cfg.StateFile, st)
	s.mu.Unlock()

	// Desktop apps have no port to probe: their liveness is the PID itself.
	still := !a.Desktop && portOpen(a.Port, startProbeTimeout)
	if a.Desktop && pid != 0 && pidAlive(pid) {
		still = true
	}
	resp := &actionResp{OK: !still, Killed: &killed, StillRunning: &still}
	if still && wasPrivileged {
		resp.Error = "still running — approve the elevation prompt (UAC on Windows, polkit/sudo on Linux) when Stop asks, then Stop again"
	} else if still && elevatedTried {
		if elevatedErr != nil && uacDeclined(elevatedErr.Error()) {
			resp.Error = "elevation declined — the app keeps running; Stop again and approve the prompt to stop it"
		} else {
			resp.Error = "still running — approve the elevation prompt (UAC on Windows) when Stop asks, then Stop again"
		}
	}
	return resp
}

// killAllApps stops every app this launcher started (state-tracked instances).
// It backs --kill-apps-on-exit: on Ctrl+C or closing the console the panel
// takes the apps it started down with it instead of leaving them detached.
// Apps running without a state entry (started outside the launcher) survive.
func (s *Server) killAllApps() {
	st := loadState(s.cfg.StateFile)
	for _, a := range s.appsList() {
		if _, ours := st[a.ID]; !ours {
			continue
		}
		if res := s.stopApp(a); !res.OK {
			fmt.Printf("  ! could not stop %s: %s\n", a.Name, res.Error)
		}
	}
}

// tailLog returns the last `lines` lines of the app's log ([] when missing).
func (s *Server) tailLog(a *App, lines int) []string {
	data, err := os.ReadFile(s.logPath(a))
	if err != nil {
		return []string{}
	}
	// Windows privileged starts split stderr into a sibling file (the
	// elevated PowerShell cannot merge two streams into one file); show it
	// as part of the same log.
	if errData, err := os.ReadFile(s.logPath(a) + ".err"); err == nil && len(errData) > 0 {
		data = append(data, append([]byte("\n--- stderr ---\n"), errData...)...)
	}
	all := splitLines(strings.ToValidUTF8(string(data), "\uFFFD"))
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	if all == nil {
		return []string{}
	}
	return all
}

// splitLines mirrors Python str.splitlines() for \n / \r\n / \r input
// (trailing newline does not produce a trailing empty line).
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// ---------------------------------------------------- per-app icon files ----

// appIconNames are the two conventional icon file names (used by the
// theme-pack overwrite path and the git import icon picker; the general
// lookup in iconfile.go covers these plus <id>.* / <name>.* and ico/icns).
var appIconNames = []struct {
	name  string
	ctype string
}{
	{"icon.png", "image/png"},
	{"icon.svg", "image/svg+xml"},
}

// appIcon loads the app's folder icon: from the packaged embedded assets in
// single-file builds, from the app folder on disk otherwise. Imported apps
// exist only on disk in single-file builds — the embedded payload has no
// icon for them — so the app folder is the fallback there too.
func appIcon(a *App) (data []byte, ctype string, ok bool) {
	// The app folder wins over the packaged payload: theme packs overwrite
	// installed apps' icon files (icons/<app-id>.png), and the dashboard
	// must show the overwrite, not the packaged original. The payload is
	// the fallback for apps whose folder carries no icon file.
	if a.Dir != "" {
		if data, ctype, ok := lookupIconDir(a.Dir, a, nil); ok {
			return data, ctype, true
		}
	}
	if embeddedBuild {
		if data, ctype, ok := embeddedIcon(a.ID); ok {
			return data, ctype, true
		}
	}
	if a.Dir == "" {
		return nil, "", false
	}
	return lookupIconDir(a.Dir, a, nil)
}

// appHasIconFile is the cheap existence check for the /api/apps badge.
func appHasIconFile(a *App) bool {
	if embeddedBuild && embeddedIconExists(a.ID) {
		return true
	}
	if a.Dir == "" {
		return false
	}
	_, _, ok := lookupIconDir(a.Dir, a, nil)
	return ok
}

// -------------------------------------------------------- per-app accents ----

// accentFor returns the card accent color for an app: the explicit apps.json
// "accent" if set, else derived once from the icon.png's dominant hue, else
// "" (the dashboard's default accent). Derived values are cached per app —
// decoding on every /api/apps poll would be wasteful; restart to re-derive
// after changing an icon.
func (s *Server) accentFor(a *App) string {
	if a.Accent != "" {
		return a.Accent
	}
	if v, ok := s.accentCache.Load(a.ID); ok {
		return v.(string)
	}
	var hex string
	if data, ctype, ok := appIcon(a); ok && ctype == "image/png" {
		hex = dominantAccent(data)
	}
	s.accentCache.Store(a.ID, hex)
	return hex
}

// dominantAccent picks a vivid color representing the icon. It samples the
// center box (where the subject sits, not the tile background) and weights
// pixels by saturation² × value so a colorful emoji outranks a large but
// medium-saturated background. Transparent, gray and near-black pixels are
// skipped. Returns "" when the icon is effectively colorless.
func dominantAccent(data []byte) string {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	// Background reference: four inset corner samples. Skipping pixels near
	// these removes the tile background whatever it looks like — the shared
	// deck gradient or any hand-made icon's own backdrop.
	refs := [4][3]float64{}
	for i, p := range [][2]int{
		{b.Min.X + w/6, b.Min.Y + h/6}, {b.Max.X - w/6, b.Min.Y + h/6},
		{b.Min.X + w/6, b.Max.Y - h/6}, {b.Max.X - w/6, b.Max.Y - h/6},
	} {
		r, g, bl, _ := img.At(p[0], p[1]).RGBA()
		refs[i] = [3]float64{float64(r) / 0xffff, float64(g) / 0xffff, float64(bl) / 0xffff}
	}
	nearRef := func(fr, fg, fb float64) bool {
		for _, r := range refs {
			dr, dg, db := fr-r[0], fg-r[1], fb-r[2]
			if dr*dr+dg*dg+db*db < 0.0225 { // ≈ 0.15 RGB distance
				return true
			}
		}
		return false
	}
	x0 := b.Min.X + w/4 // center box: middle half of the image
	x1 := b.Max.X - w/4
	y0 := b.Min.Y + h/4
	y1 := b.Max.Y - h/4
	var buckets [36]float64
	any := false
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			if a < 0x8000 {
				continue
			}
			fr, fg, fb := float64(r)/0xffff, float64(g)/0xffff, float64(bl)/0xffff
			if nearRef(fr, fg, fb) {
				continue
			}
			mx := max(fr, max(fg, fb))
			mn := min(fr, min(fg, fb))
			v := mx
			if v <= 0 {
				continue
			}
			s := (mx - mn) / v
			if s < 0.28 || v < 0.25 { // gray / near-black: not an identity color
				continue
			}
			h := 0.0
			switch mx {
			case fr:
				h = (fg - fb) / (mx - mn)
			case fg:
				h = 2 + (fb-fr)/(mx-mn)
			default:
				h = 4 + (fr-fg)/(mx-mn)
			}
			h = math.Mod(h*60+360, 360)
			buckets[int(h/10)%36] += s * s * v
			any = true
		}
	}
	if !any {
		return ""
	}
	best := 0
	for i, w := range buckets {
		if w > buckets[best] {
			best = i
		}
	}
	hue := float64(best)*10 + 5
	return hsvToHex(hue, 0.62, 0.95)
}

// hsvToHex renders HSV (hue in degrees) as a #rrggbb string.
func hsvToHex(h, s, v float64) string {
	i := int(math.Floor(h/60)) % 6
	f := h/60 - math.Floor(h/60)
	p := v * (1 - s)
	q := v * (1 - f*s)
	t := v * (1 - (1-f)*s)
	var r, g, b float64
	switch i {
	case 0:
		r, g, b = v, t, p
	case 1:
		r, g, b = q, v, p
	case 2:
		r, g, b = p, v, t
	case 3:
		r, g, b = p, q, v
	case 4:
		r, g, b = t, p, v
	default:
		r, g, b = v, p, q
	}
	return fmt.Sprintf("#%02x%02x%02x", int(r*255+0.5), int(g*255+0.5), int(b*255+0.5))
}

// serveIcon answers GET /api/icon?id=… with the app's icon file. The ETag
// (content hash) lets the dashboard's poll revalidate cheaply: unchanged
// icons get a 304 instead of a reload, so the grid never flickers.
func (s *Server) serveIcon(w http.ResponseWriter, r *http.Request) {
	qs := parseQuery(r.RequestURI)
	app := s.appByID(qs["id"])
	if app == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
		return
	}
	var data []byte
	var ctype string
	var ok bool
	// ?pack=<name> serves the pack's icon for the app (the 🧩 dialog's live
	// preview and per-app picker) without touching the installed files;
	// pack "original" previews the backed-up pre-pack artwork.
	if pack := slugify(qs["pack"]); pack != "" {
		data, ctype, ok = s.packIconBytes(pack, app.ID)
	}
	if !ok {
		data, ctype, ok = appIcon(app)
	}
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "no icon file in the app folder (icon.png/.svg/.ico/.icns or <app-id>.png)"})
		return
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", ctype)
	// An imported app can ship any bytes as icon.svg. Rendered via <img> the
	// script inside it never runs, but a page that frames or navigates to this
	// URL executes it on the launcher's origin — every guard passes for a
	// 127.0.0.1 iframe. "sandbox" makes the SVG a unique-origin document with
	// scripts disabled in those contexts; <img>/CSS usage is unaffected.
	if ctype == "image/svg+xml" {
		h.Set("Content-Security-Policy", "sandbox")
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// serveRebuildApp was removed with the 🛠 card action: imported sources are
// pruned after their binary is built (pruneBuildInputs), so there is nothing
// left to rebuild on demand — a re-import is the way to land new sources,
// and its marker flow rebuilds and prunes again.

// ------------------------------------------------------------------- http ----

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// ServeHTTP routes like the Python do_GET/do_POST pair: the raw request path
// is matched literally (no normalization), unmatched combinations get a JSON
// 404.
// serveUninstall removes an app: unregister it from apps.json when it is a
// registry app (folder mode), stop it, delete its folder, drop its port
// assignment (via reload), and optionally its per-app data directory. In
// embed mode, registry apps are uninstallable too: the payload inside the exe
// cannot be deleted, so the unpacked folder is removed and the app simply
// returns on the next start (extraction unpacks whatever the exe carries).
func (s *Server) serveUninstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID         string `json:"id"`
		RemoveData bool   `json:"removeData"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send {id, removeData}"})
		return
	}
	s.mu.Lock()
	var a *App
	for _, cand := range s.apps {
		if cand.ID == body.ID {
			a = cand
			break
		}
	}
	s.mu.Unlock()
	if a == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown app " + body.ID})
		return
	}
	note, err := s.uninstallOneApp(a, body.RemoveData)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Rescan now: without this the card lingers on the deck until the next
	// reload trigger (the 10s container watcher only notices new folders,
	// never removals) — the same rescan the batch uninstall performs.
	if err := s.reloadApps(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	go s.snapshotDeck()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note})
}

// removeRegistryEntry drops one app's entry from the central apps.json and
// rewrites the file in place (temp file + rename). Entries travel as raw JSON
// so every field the App struct does not model — and every sibling entry —
// survives the round trip untouched. An id the file does not list is not an
// error: the file already describes the deck without the app.
func removeRegistryEntry(appsFile, id string) error {
	if appsFile == "" {
		return fmt.Errorf("no apps.json to edit")
	}
	data, err := os.ReadFile(appsFile)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing %s: %w", appsFile, err)
	}
	var entries []json.RawMessage
	if raw, ok := doc["apps"]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("%s: the \"apps\" value is not a list: %w", appsFile, err)
		}
	}
	kept := make([]json.RawMessage, 0, len(entries))
	found := false
	for _, e := range entries {
		var probe struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(e, &probe) == nil && probe.ID == id {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		return nil // already absent — the file stays exactly as it is
	}
	appsRaw, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	doc["apps"] = appsRaw
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // do not rewrite untouched entries' <, >, &
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	tmp := appsFile + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, appsFile)
}

// uninstallOneApp removes one application: unregisters it from apps.json
// when it is a registry app of a folder build, stops it, resets its macOS
// privacy grants, deletes its folder (permanently — this is the launcher's
// long-standing uninstall semantic), and optionally drops the per-app data
// directory. Shared by the single-app route and /api/uninstall/batch. The
// caller reloads the app list. In an all-in-one build the payload still
// carries the app, so the removal is recorded in uninstalled.json: extraction
// skips it on every later boot and the deck stops advertising it, until the
// operator restores it from ⚙ Deck tools (or imports it again).
func (s *Server) uninstallOneApp(a *App, removeData bool) (string, error) {
	embeddedRegistry := a.Source == "registry"
	// Launcher-hosted embedded apps have no folder at all — their uninstall
	// is the data-dir slug folder removal below.
	if a.Dir == "" && !(embeddedRegistry && embeddedBuild) {
		return "", fmt.Errorf("%s has no folder on disk", a.ID)
	}
	if rootAbs, err := filepath.Abs(s.cfg.Root); err == nil && a.Dir == rootAbs {
		return "", fmt.Errorf("refusing to remove the launcher folder")
	}
	registryEntryRemoved := false
	if embeddedRegistry && !embeddedBuild {
		// Folder mode: apps.json is a plain editable file, so unregistering
		// is part of the uninstall — drop the entry first (a failed write
		// aborts here, before anything on disk is deleted), then fall through
		// to the normal stop-and-delete below.
		if err := removeRegistryEntry(s.cfg.AppsFile, a.ID); err != nil {
			return "", fmt.Errorf("%s is registered in apps.json — removing the entry failed: %w", a.ID, err)
		}
		registryEntryRemoved = true
	}
	_ = s.stopApp(a)        // a dead process must not block removal
	resetTccGrants(a.ID)    // the app's macOS privacy grants die with it (no-op off darwin)
	s.exposeRemoveForApp(a) // so does its /Applications link, Start-menu pin or .desktop entry
	if a.Dir == "" && embeddedRegistry && embeddedBuild {
		// Hosted apps have no registry folder; extraction materializes their
		// data-dir slug folder (app.json + icon) on every boot. Remove it so
		// the card is gone for this session — the next start unpacks it again,
		// like every other app in the payload. Overriding imports never reach
		// this branch (they set Source/Dir and delete their folder through the
		// a.Dir path above).
		_ = os.RemoveAll(filepath.Join(s.cfg.DataDir, "Apps", slugify(a.ID)))
	}
	if a.Dir != "" {
		if err := os.RemoveAll(a.Dir); err != nil {
			return "", fmt.Errorf("remove folder: %w", err)
		}
	}
	note := a.ID + " uninstalled"
	if registryEntryRemoved {
		note += " — apps.json entry removed"
	}
	if embeddedRegistry && embeddedBuild {
		// The payload still carries the app and the exe cannot be edited, so
		// the removal is what makes this stick: extraction skips the id from
		// now on, and the deck offers it back through ⚙ Deck tools.
		if err := markAppRemoved(s.cfg.DataDir, a.ID); err != nil {
			return "", fmt.Errorf("recording the removal of %s: %w", a.ID, err)
		}
		note += " — built into this launcher; restore it from ⚙ Deck tools"
	}
	dataDir := ""
	if s.cfg.DataDir != "" {
		dataDir = filepath.Join(s.cfg.DataDir, "appdata", a.ID)
	}
	if removeData && dataDir != "" {
		if os.RemoveAll(dataDir) == nil {
			note += " — data folder removed"
		}
	} else if dataDir != "" {
		note += " — data kept at " + dataDir
	}
	return note, nil
}

// serveUninstallBatch removes SEVERAL applications in one call. Per-id
// results follow the /api/all/* shape: a refusal (missing folder, in-flight
// start, a failed apps.json rewrite) skips that app and is reported —
// it never aborts the batch. Everything user-generated is deleted
// permanently, so the route is loopback-only, like the other destructive
// mutators.
func (s *Server) serveUninstallBatch(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "local only"})
		return
	}
	var body struct {
		IDs        []string `json:"ids"`
		RemoveData bool     `json:"removeData"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.IDs) == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send {ids: [...], removeData}"})
		return
	}
	if len(body.IDs) > 48 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "uninstall at most 48 apps at once"})
		return
	}
	type batchResult struct {
		ID    string `json:"id"`
		Name  string `json:"name,omitempty"`
		OK    bool   `json:"ok"`
		Note  string `json:"note,omitempty"`
		Error string `json:"error,omitempty"`
	}
	results := make([]batchResult, 0, len(body.IDs))
	ok, failed := 0, 0
	for _, id := range body.IDs {
		s.mu.Lock()
		var a *App
		for _, cand := range s.apps {
			if cand.ID == id {
				a = cand
				break
			}
		}
		name := ""
		if a != nil {
			name = a.Name
		}
		s.mu.Unlock()
		if a == nil {
			results = append(results, batchResult{ID: id, Error: "unknown app"})
			failed++
			continue
		}
		note, err := s.uninstallOneApp(a, body.RemoveData)
		if err != nil {
			results = append(results, batchResult{ID: id, Name: name, Error: err.Error()})
			failed++
			continue
		}
		results = append(results, batchResult{ID: id, Name: name, OK: true, Note: note})
		ok++
	}
	if err := s.reloadApps(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if ok > 0 {
		go s.snapshotDeck()
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": failed == 0, "uninstalled": ok, "failed": failed, "results": results})
}

// serveDataFolder opens (and creates) the app's per-user data directory in
// the OS file manager. Only dirs under the launcher's own DataDir can be
// opened — the path is derived from the id, never taken from the request.
func (s *Server) serveDataFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send {id}"})
		return
	}
	s.mu.Lock()
	var a *App
	for _, cand := range s.apps {
		if cand.ID == body.ID {
			a = cand
			break
		}
	}
	s.mu.Unlock()
	if a == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown app " + body.ID})
		return
	}
	if s.cfg.DataDir == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no data directory in this build"})
		return
	}
	dir := filepath.Join(s.cfg.DataDir, "appdata", a.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dir": dir})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", serverVersion)
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.route(rec, r)
	// Python log_message: "[HH:MM:SS] \"GET /api/apps HTTP/1.1\" 200 -"
	fmt.Fprintf(os.Stderr, "[%s] \"%s %s HTTP/1.1\" %d -\n",
		time.Now().Format("15:04:05"), r.Method, r.RequestURI, rec.status)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path, _, _ := strings.Cut(r.RequestURI, "?")
	// Dashboard loopback mode (launcher.json): the launcher's own UI and API
	// answer only this computer. Shared APPS keep their own ports and stay
	// LAN-reachable — this restricts the deck, not the apps.
	if settingsLoopback.Load() && !fromLocalMachine(r) && path != "/api/health" {
		s.writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "the dashboard is limited to this computer (launcher setting)"})
		return
	}
	// Guard for state-changing calls. Browsers attach an Origin header to
	// every cross-site request, so one that names a different host is a
	// drive-by web page trying to drive the local API with a text/plain
	// POST — reject it. The fence covers every state-changing method so a
	// future PUT/PATCH/DELETE route inherits it. LAUNCHER_LAN_TOKEN
	// additionally requires the matching X-AppDeck-Token header from
	// non-loopback callers when set.
	if mutatingMethod(r.Method) && strings.HasPrefix(path, "/api/") {
		// Sec-Fetch-Site is browser-supplied and still present where a page
		// cannot attach an Origin (some form posts, sandboxed frames).
		if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "same-site" && sfs != "none" {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site requests are not allowed"})
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
				s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin requests are not allowed"})
				return
			}
		}
		if !fromLocalMachine(r) && s.cfg.LANToken != "" &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("X-AppDeck-Token")), []byte(s.cfg.LANToken)) != 1 {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing or wrong X-AppDeck-Token"})
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && (path == "/" || path == "/index.html"):
		s.serveIndex(w)
	case r.Method == http.MethodGet && path == "/__tokenreport__/list":
		s.serveTokenreportList(w, r)
	case r.Method == http.MethodGet && path == "/__tokenreport__/read":
		s.serveTokenreportRead(w, r)
	case r.Method == http.MethodGet && path == "/api/apps":
		// Read-only and LAN-visible by design; the open CORS header lets
		// deck tools (Wi-Fi Guard-style pages on their own port — same site,
		// different port) read it too. A foreign web page is "cross-site"
		// and gets nothing, so the wildcard header never hands the
		// inventory (names, ports, git origins) to a drive-by reader. The
		// deck operator can flip visibility off entirely (⚙ Deck tools) —
		// then the inventory answers this machine only.
		if sfs := r.Header.Get("Sec-Fetch-Site"); sfs == "cross-site" {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site requests are not allowed"})
			return
		}
		if settingsHidden.Load() && !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "the app list is limited to this computer (launcher setting)"})
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		noteActivity()
		s.mu.Lock()
		st := loadState(s.cfg.StateFile)
		apps := s.apps
		s.mu.Unlock()
		statuses := make([]appStatus, 0, len(apps))
		for _, a := range apps {
			statuses = append(statuses, s.appStatus(a, st))
		}
		s.writeJSON(w, http.StatusOK, statuses)
	case r.Method == http.MethodGet && path == "/api/health":
		noteActivity()
		var out healthResp
		out.OK = true
		out.Version = launcherVersion
		out.Started = procStartedAt
		if embeddedBuild {
			out.Mode = "single-file"
			out.DataDir = s.cfg.DataDir
		}
		out.Window = launchWindow
		out.TLSPort = s.cfg.TLSPort
		out.OS = runtime.GOOS
		out.Arch = runtime.GOARCH
		s.mu.Lock()
		if len(s.warnings) > 0 {
			out.Warnings = append([]string(nil), s.warnings...)
		}
		s.mu.Unlock()
		if lan := lanIP(); lan != "" {
			out.Lan = &lan
			u := fmt.Sprintf("http://%s:%d", lan, s.cfg.Port)
			out.LanURL = &u
		}
		s.writeJSON(w, http.StatusOK, out)
	case r.Method == http.MethodGet && path == "/api/appstorage":
		s.serveAppStorageGet(w, r)
	case r.Method == http.MethodPost && path == "/api/appstorage":
		s.serveAppStoragePost(w, r)
	case r.Method == http.MethodPost && path == "/api/heartbeat":
		// Dashboard keepalive; ?bye=1 is the pagehide beacon sent when the
		// last dashboard window/tab goes away (window mode shuts down then).
		// The bye is the shutdown trigger, so it only counts from this
		// machine: one POST from the LAN must not kill the whole deck.
		if parseQuery(r.RequestURI)["bye"] == "1" {
			if fromLocalMachine(r) {
				handleBye(s)
			} else {
				s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "bye is only honored from this computer"})
				return
			}
		} else {
			noteActivity()
		}
		s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case r.Method == http.MethodPost && path == "/api/shutdown":
		// Explicit quit from the dashboard. Loopback only — a device on the
		// Wi-Fi may stop apps but not kill the launcher itself.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "shutdown is only allowed from this computer"})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		time.AfterFunc(300*time.Millisecond, func() { s.beginShutdown("quit requested from the dashboard") })
	case r.Method == http.MethodPost && path == "/api/open":
		// Open a URL on this computer (tab mode from the native window: the
		// WebView2 window has no tabs, so the app lands in the default
		// browser instead). Loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "open is only allowed from this computer"})
			return
		}
		var req openRequest
		readBodyInto(r, &req)
		u := strings.TrimSpace(req.URL)
		// Parse and compare the hostname exactly: a prefix match would accept
		// lookalikes like http://127.0.0.1.attacker.tld (or 127.0.0.10) and
		// aim the user's default browser at an attacker page.
		pu, perr := url.Parse(u)
		hn := ""
		if perr == nil {
			hn = strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(pu.Hostname()), "["), "]")
		}
		loopbackHTTP := perr == nil && pu.Scheme == "http" && (hn == "127.0.0.1" || hn == "::1")
		if !loopbackHTTP && (perr != nil || pu.Scheme != "https") {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported url"})
			return
		}
		openBrowser(u)
		s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case r.Method == http.MethodPost && path == "/api/firewall":
		// One-click fix for the per-app Windows Firewall "Allow/Cancel"
		// popups: adds allow-rules for the launcher and every network app via
		// a single UAC elevation. Windows only; loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "firewall fix is only allowed from this computer"})
			return
		}
		s.writeJSON(w, http.StatusOK, s.firewallFix())
	case r.Method == http.MethodPost && path == "/api/install":
		// Legacy alias of /api/import for archives: raw zip/tar.gz/tar bytes
		// (app.json at the root). Kept for scripts and older clients; the
		// dashboard's 📥 button uses /api/import.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "install is only allowed from this computer"})
			return
		}
		s.serveInstall(w, r)
	case r.Method == http.MethodPost && path == "/api/import":
		// Type-aware import: an app archive (zip/tar.gz), a portable .exe
		// (wrapped, never executed), a single .html page (hosted app), a
		// local app folder (?folder=), or a URL (?url=). Writes into the
		// deck's app container, so loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "import is only allowed from this computer"})
			return
		}
		s.serveImport(w, r)
	case r.Method == http.MethodGet && path == "/api/import/progress":
		// Live milestone log of a running import (?jobid= handle).
		s.serveJobProgress(w, r)
	case r.Method == http.MethodGet && path == "/api/import/jobs":
		// Every import running on this deck, with the label its starter gave
		// it — the dashboard's dock attaches to jobs it did not start itself.
		s.serveJobList(w, r)
	case r.Method == http.MethodGet && path == "/api/pkg/probe":
		// Read-only package-manager lookup behind the import offer.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "package lookup is only allowed from this computer"})
			return
		}
		s.servePkgProbe(w, r)
	case r.Method == http.MethodPost && path == "/api/import/cancel":
		// Abort a stuck import: cancels the job's context (git clone,
		// downloads) and flags every later step to bail out.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "import is only allowed from this computer"})
			return
		}
		s.serveImportCancel(w, r)
	case r.Method == http.MethodGet && path == "/api/presets":
		// Prerequisite toolchains: status of go/rust/git/python/node/docker/nginx,
		// install (default or custom dir), register an existing path.
		s.servePresets(w, r)
	case r.Method == http.MethodPost && path == "/api/presets/install":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "installs are only allowed from this computer"})
			return
		}
		s.servePresetInstall(w, r)
	case r.Method == http.MethodPost && path == "/api/presets/path":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "path registration is only allowed from this computer"})
			return
		}
		s.servePresetPath(w, r)
	case r.Method == http.MethodPost && path == "/api/presets/remove":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "removal is only allowed from this computer"})
			return
		}
		s.servePresetRemove(w, r)
	case r.Method == http.MethodPost && path == "/api/downloads/pick":
		// Choose where the native window's downloads land (OS folder picker).
		// Loopback only, like every other path-reading call.
		s.serveDownloadPick(w, r)
	case r.Method == http.MethodPost && path == "/api/pick-path":
		// Opens the OS folder picker on this machine and returns the chosen
		// path (browsers never hand out real paths). Loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "the folder picker is only allowed from this computer"})
			return
		}
		s.servePickPath(w, r)
	case r.Method == http.MethodGet && path == "/api/storage":
		// Read-only storage status; the launcher never blocks boot if the
		// configured volume is missing, it only warns instead.
		s.serveStorage(w, r)
	case r.Method == http.MethodPost && path == "/api/storage/move":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "storage moves are only allowed from this computer"})
			return
		}
		s.serveStorageMove(w, r)
	case r.Method == http.MethodGet && path == "/api/themes":
		s.serveThemes(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/themes/preview/"):
		// Read-only pack artwork, same reachability as /api/themes itself.
		s.serveThemePreview(w, strings.TrimPrefix(path, "/api/themes/preview/"))
	case r.Method == http.MethodPost && path == "/api/themes/apply":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "theme changes are only allowed from this computer"})
			return
		}
		s.serveThemeApply(w, r)
	case r.Method == http.MethodPost && path == "/api/themes/import":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "theme imports are only allowed from this computer"})
			return
		}
		s.serveThemeImport(w, r)
	case r.Method == http.MethodPost && path == "/api/themes/delete":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "theme deletion is only allowed from this computer"})
			return
		}
		s.serveThemeDelete(w, r)
	case r.Method == http.MethodGet && path == "/api/backgrounds":
		// Read-only pack list + appearance settings; the backdrop itself is
		// injected into the served page, so remote browsers need this too.
		s.serveBackgrounds(w, r)
	case r.Method == http.MethodGet && path == "/api/security":
		// Engine status + verdicts: readable like the pack lists; the
		// writes below are loopback-guarded.
		s.serveSecurityInfo(w, r)
	case r.Method == http.MethodPost && path == "/api/security/scan":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "security scans are only allowed from this computer"})
			return
		}
		s.serveSecurityScan(w, r)
	case r.Method == http.MethodPost && path == "/api/security/confirm":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "security confirmations are only allowed from this computer"})
			return
		}
		s.serveSecurityConfirm(w, r)
	case r.Method == http.MethodPost && path == "/api/security/selftest":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "the security self-test is only allowed from this computer"})
			return
		}
		s.serveSecuritySelftest(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/backgrounds/file/"):
		// One asset file inside a pack, referenced by pack CSS url(…).
		s.serveBackgroundFile(w, r, strings.TrimPrefix(path, "/api/backgrounds/file/"))
	case r.Method == http.MethodPost && path == "/api/backgrounds/apply":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "background changes are only allowed from this computer"})
			return
		}
		s.serveBackgroundApply(w, r)
	case r.Method == http.MethodPost && path == "/api/backgrounds/settings":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "background settings changes are only allowed from this computer"})
			return
		}
		s.serveBackgroundSettings(w, r)
	case r.Method == http.MethodPost && path == "/api/backgrounds/import":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "background imports are only allowed from this computer"})
			return
		}
		s.serveBackgroundImport(w, r)
	case r.Method == http.MethodPost && path == "/api/backgrounds/delete":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "background deletion is only allowed from this computer"})
			return
		}
		s.serveBackgroundDelete(w, r)
	case r.Method == http.MethodGet && path == "/api/iconpacks":
		s.serveIconPacks(w, r)
	case r.Method == http.MethodPost && path == "/api/iconpacks/apply":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "icon pack changes are only allowed from this computer"})
			return
		}
		s.serveIconPackApply(w, r)
	case r.Method == http.MethodPost && path == "/api/iconpacks/apply-one":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "icon pack changes are only allowed from this computer"})
			return
		}
		s.serveIconPackApplyOne(w, r)
	case r.Method == http.MethodPost && path == "/api/iconpacks/import":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "icon pack imports are only allowed from this computer"})
			return
		}
		s.serveIconPackImport(w, r)
	case r.Method == http.MethodPost && path == "/api/iconpacks/delete":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "icon pack deletion is only allowed from this computer"})
			return
		}
		s.serveIconPackDelete(w, r)
	case r.Method == http.MethodPost && path == "/api/tls/regenerate":
		// Fresh key + certificate for the HTTPS listener, swapped in live.
		// Loopback only — the compromise-response button must not be
		// reachable by whatever the caller is worried about.
		s.serveTLSRegenerate(w, r)
	case r.Method == http.MethodPost && path == "/api/show":
		// Bring a running desktop app's window to the front — the desktop
		// equivalent of Open. Loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "show is only allowed from this computer"})
			return
		}
		s.serveShow(w, r)
	case r.Method == http.MethodPost && path == "/api/restart":
		// Relaunch the launcher (used after a self-update): spawn the
		// swapped-in executable, then shut this process down cleanly.
		// Loopback only — this decides the fate of the launcher itself.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "restart is only allowed from this computer"})
			return
		}
		s.serveRestart(w, r)
	case r.Method == http.MethodPost && path == "/api/update":
		// Launcher self-update from an uploaded zip (or ?url=, or the JSON
		// github/local sources): swaps the running executable, keeps the
		// previous one as .old, asks for a restart. Loopback only — this
		// replaces the program itself.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "update is only allowed from this computer"})
			return
		}
		s.serveUpdate(w, r)
	case r.Method == http.MethodGet && path == "/api/update/check":
		// What could the GitHub channel install right now? Read-only: release
		// list (the downgrade dropdown), selected tag's asset, up-to-date.
		s.serveUpdateCheck(w, r)
	case r.Method == http.MethodPost && path == "/api/deck/rebuild":
		// Rebuild the deck from disk: generate app.json for installed app
		// folders that lack one (older all-in-one extractions never wrote
		// manifests, so an update to a smaller registry emptied the deck),
		// optionally importing app folders from a source path. Loopback only
		// — it mutates the data directory.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "rebuilding the deck is only allowed from this computer"})
			return
		}
		s.serveDeckRebuild(w, r)
	case r.Method == http.MethodGet && path == "/api/settings":
		qs := parseQuery(r.RequestURI)
		id := qs["id"]
		if s.appByID(id) == nil {
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
			return
		}
		doc, exists, err := s.readAppSettings(id)
		if err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out := appSettingsDoc{ID: id}
		if exists {
			out.Settings = doc
		}
		s.writeJSON(w, http.StatusOK, out)
	case r.Method == http.MethodPost && path == "/api/settings":
		// Store a per-app settings document. Writes machine-local state, so
		// loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "settings changes are only allowed from this computer"})
			return
		}
		var req appSettingsDoc
		readBodyInto(r, &req)
		if s.appByID(req.ID) == nil {
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
			return
		}
		if len(req.Settings) == 0 {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "settings document missing"})
			return
		}
		if err := s.writeAppSettings(req.ID, req.Settings); err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case r.Method == http.MethodGet && path == "/api/logs":
		s.serveLogs(w, r.RequestURI)
	case r.Method == http.MethodGet && path == "/api/icon":
		s.serveIcon(w, r)
	case r.Method == http.MethodPost && path == "/api/terminal":
		// ⌨ Open a terminal in a CLI app's folder. Launches a window on
		// this machine only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "opening a terminal is only allowed from this computer"})
			return
		}
		s.serveTerminal(w, r)
	case r.Method == http.MethodGet && path == "/favicon.svg":
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "max-age=86400")
		_, _ = w.Write([]byte(faviconSVG))
	case r.Method == http.MethodGet && path == "/site.webmanifest":
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Header().Set("Cache-Control", "max-age=86400")
		_, _ = w.Write([]byte(siteWebManifest))
	case r.Method == http.MethodGet && path == "/favicon.ico":
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && path == "/api/network":
		// Flip one web app's Wi-Fi sharing (the dashboard's drag between the
		// two sections). Writes machine-local state, so loopback only.
		s.serveNetworkSet(w, r)
	case r.Method == http.MethodPost && path == "/api/privilege":
		// Flip one app's privileged launch (its own macOS TCC identity).
		// Machine-local security state, so loopback only.
		s.servePrivilegeToggle(w, r)
	case r.Method == http.MethodGet && path == "/api/launcher-settings":
		s.serveLauncherSettings(w, r)
	case r.Method == http.MethodGet && path == "/api/limits":
		s.serveLimits(w, r)
	case r.Method == http.MethodPost && path == "/api/launcher-settings":
		s.serveLauncherSettings(w, r)
	case r.Method == http.MethodGet && path == "/api/deck/export":
		// The whole user setup — Apps container, registry, Wi-Fi overrides —
		// as one zip. Machine-local data, so loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "deck export is only allowed from this computer"})
			return
		}
		s.serveDeckExport(w)
	case r.Method == http.MethodPost && path == "/api/deck/import":
		// Restore a deck export: apps not already installed are extracted,
		// Wi-Fi overrides merge. Machine-local data, so loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "deck import is only allowed from this computer"})
			return
		}
		s.serveDeckImport(w, r)
	case r.Method == http.MethodPost && path == "/api/start":
		s.serveStartStop(w, r, true)
	case r.Method == http.MethodPost && path == "/api/stop":
		s.serveStartStop(w, r, false)
	case r.Method == http.MethodPost && path == "/api/all/start":
		s.serveAll(w, true)
	case r.Method == http.MethodPost && path == "/api/all/stop":
		s.serveAll(w, false)
	case r.Method == http.MethodPost && path == "/api/uninstall":
		// Deleting an app folder is destructive machine-local state. A LAN
		// peer may start and stop apps (sharing is the point of the LAN
		// bind), but it may not remove them.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "uninstall is only allowed from this computer"})
			return
		}
		noteActivity()
		s.serveUninstall(w, r)
	case r.Method == http.MethodPost && path == "/api/expose":
		// Create/remove the app's OS artifact — the /Applications link, the
		// Start-menu pin, the .desktop entry (expose_<goos>.go). Loopback
		// only: it writes outside the deck container.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "expose is only allowed from this computer"})
			return
		}
		noteActivity()
		s.serveExpose(w, r)
	case r.Method == http.MethodPost && path == "/api/uninstall/batch":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "uninstall is only allowed from this computer"})
			return
		}
		noteActivity()
		s.serveUninstallBatch(w, r)
	case r.Method == http.MethodGet && path == "/api/apps/removed":
		// What this machine uninstalled from the payload — the deck's restore
		// list. Loopback only (see serveRemovedApps).
		s.serveRemovedApps(w, r)
	case r.Method == http.MethodPost && path == "/api/apps/restore":
		noteActivity()
		s.serveRestoreApps(w, r)
	case r.Method == http.MethodPost && path == "/api/apps/forget":
		// Stop offering named removals back — the record stays for payload
		// apps (it is what keeps them off the deck), stale ones are dropped.
		// Loopback only (see serveForgetApps).
		noteActivity()
		s.serveForgetApps(w, r)
	case r.Method == http.MethodPost && path == "/api/datafolder":
		// Opens a Finder/Explorer window on this desktop — never on behalf of
		// a remote caller.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "opening a data folder is only allowed from this computer"})
			return
		}
		noteActivity()
		s.serveDataFolder(w, r)
	default:
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// serveInstall handles POST /api/install — the legacy archive-only installer,
// kept as an alias of /api/import for scripts and older clients: extract,
// validate, register, and report the installed app's id (the dashboard
// refreshes its list after).
func (s *Server) serveInstall(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, installMaxArchiveBytes+1))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading upload: " + err.Error()})
		return
	}
	app, err := s.installArchiveBytes(body, nil)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.reloadApps(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("installed %s, but rescanning the registry failed: %v", app.ID, err)})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"id":   app.ID,
		"name": app.Name,
		"note": "installed and discovered — it appears on the dashboard now",
	})
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "json encode error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (s *Server) serveIndex(w http.ResponseWriter) {
	body := []byte(nil)
	if embeddedBuild {
		body = embeddedIndexHTML
	} else {
		body, _ = os.ReadFile(s.cfg.IndexFile)
		if len(body) == 0 {
			body = dashboardHTML // bare exe: no index.html beside it — use the embedded dashboard
		}
	}
	if len(body) == 0 {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "index.html missing"})
		return
	}
	body = injectThemeCSS(body, s.themeCSS())
	// Background styles land after theme styles: the backdrop (and a pack's
	// panel overrides) win the cascade over both base and theme.
	body = injectBackgroundCSS(body, s.backgroundCSS(), readBackgroundConf(s.cfg.DataDir).varsCSS())
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	// no-store: a self-update swaps the binary under the page, and a cached
	// dashboard lags the launcher (missing buttons/routes) until a hard reload.
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) serveLogs(w http.ResponseWriter, requestURI string) {
	qs := parseQuery(requestURI)
	app := s.appByID(qs["id"])
	if app == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
		return
	}
	s.writeJSON(w, http.StatusOK, logsResp{
		ID:    app.ID,
		Name:  app.Name,
		Lines: s.tailLog(app, logTailLines),
	})
}

// parseQuery mirrors the Python handler's ad-hoc parser: split on "&", keep
// only "k=v" pairs (first "=" splits), last duplicate wins, no URL decoding.
func parseQuery(requestURI string) map[string]string {
	qs := map[string]string{}
	i := strings.Index(requestURI, "?")
	if i < 0 {
		return qs
	}
	for _, pair := range strings.Split(requestURI[i+1:], "&") {
		j := strings.Index(pair, "=")
		if j < 0 {
			continue
		}
		qs[pair[:j]] = pair[j+1:]
	}
	return qs
}

type idRequest struct {
	ID string `json:"id"`
}

type openRequest struct {
	URL string `json:"url"`
}

// ---- Wi-Fi sharing overrides (drag a card between the two sections) ----
// network.json in the data dir records explicit per-app user choices; the
// manifest's network field stays the default when no override exists. The
// endpoint is loopback-only: the dashboard itself is reachable from the
// Wi-Fi, so a LAN peer must never be able to expose local apps.

type networkRequest struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

func (s *Server) networkOverridesFile() string {
	return filepath.Join(s.cfg.DataDir, "network.json")
}

func (s *Server) loadNetworkOverrides() map[string]bool {
	data, err := os.ReadFile(s.networkOverridesFile())
	if err != nil {
		return map[string]bool{}
	}
	ov := map[string]bool{}
	if json.Unmarshal(data, &ov) != nil {
		return map[string]bool{}
	}
	return ov
}

func (s *Server) saveNetworkOverrides(ov map[string]bool) {
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
	path := s.networkOverridesFile()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// applyNetworkOverrides layers saved user choices over the manifest
// defaults — the user's choice wins and survives re-imports and updates.
func (s *Server) applyNetworkOverrides(apps []*App) {
	ov := s.loadNetworkOverrides()
	if len(ov) == 0 {
		return
	}
	for _, a := range apps {
		if enabled, ok := ov[a.ID]; ok {
			a.Network = enabled
		}
	}
}

// serveNetworkSet flips one web app's Wi-Fi sharing. A running app restarts
// so the new bind host (0.0.0.0 vs 127.0.0.1) takes effect immediately.
func (s *Server) serveNetworkSet(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "Wi-Fi sharing changes are only allowed from this computer"})
		return
	}
	if s.cfg.DataDir == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no data directory"})
		return
	}
	var req networkRequest
	readBodyInto(r, &req)
	s.mu.Lock()
	app := s.byID[req.ID]
	if app == nil {
		s.mu.Unlock()
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
		return
	}
	if app.Desktop {
		s.mu.Unlock()
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "desktop apps have no web page to share — nothing to put on the Wi-Fi"})
		return
	}
	if _, busy := s.inflight[app.ID]; busy {
		s.mu.Unlock()
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": busyMessage})
		return
	}
	app.Network = req.Enabled
	ov := s.loadNetworkOverrides()
	ov[app.ID] = req.Enabled
	s.saveNetworkOverrides(ov)
	s.mu.Unlock()

	note := "will apply on next Start"
	if entry := loadState(s.cfg.StateFile)[app.ID]; entry.PID != 0 && pidAlive(entry.PID) {
		if resp := s.stopApp(app); resp != nil && !resp.OK {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "note": "sharing saved, but stopping the app failed: " + resp.Error})
			return
		}
		if resp := s.startApp(app); resp != nil && !resp.OK {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "note": "sharing saved, but restarting failed: " + resp.Error})
			return
		}
		note = "restarted with the new sharing setting"
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "note": note, "status": s.appStatus(app, loadState(s.cfg.StateFile))})
}

// fromLocalMachine reports whether the request's TCP peer is this computer.
// This is the connection's real source address — not the Host header, which
// any LAN client can set to "127.0.0.1" to impersonate a local caller.
func fromLocalMachine(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// readBodyInto mirrors Python _body(): no Content-Length or invalid JSON →
// leave v untouched.
func readBodyInto(r *http.Request, v any) {
	n, err := strconv.Atoi(strings.TrimSpace(r.Header.Get("Content-Length")))
	if err != nil || n <= 0 {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, int64(n)))
	if err != nil && len(data) == 0 {
		return
	}
	_ = json.Unmarshal(data, v)
}

// readBody mirrors the Python handler's ad-hoc parser for {id} bodies.
func readBody(r *http.Request) idRequest {
	var out idRequest
	readBodyInto(r, &out)
	return out
}

// startRequest is the /api/start body: the app id plus an optional custom
// port (1024–65535) to run this instance on instead of the configured one.
type startRequest struct {
	ID   string `json:"id"`
	Port int    `json:"port"`
	// SecurityOverride confirms the ⚠ start-gate for a flagged app. Only
	// honored from this machine, and only after the dashboard showed the
	// findings (the hold response carries them).
	SecurityOverride bool `json:"securityOverride"`
}

func (s *Server) serveStartStop(w http.ResponseWriter, r *http.Request, start bool) {
	var req startRequest
	readBodyInto(r, &req)
	app := s.appByID(req.ID)
	if app == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
		return
	}
	if !start {
		res := s.stopApp(app)
		st := s.appStatus(app, loadState(s.cfg.StateFile))
		res.Status = &st
		s.writeJSON(w, http.StatusOK, res)
		return
	}

	// Start-gate: an app flagged by the security scan asks once per launch
	// before it runs — the risk moment is Start, not the import. The
	// confirmation is loopback-only and remembered for this launcher run.
	if app.Security == "flagged" && !req.SecurityOverride {
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "this app is flagged by the security scan — confirm the start from this computer"})
			return
		}
		vd := loadSecurityVerdicts(s.securityDataDir())[app.ID]
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "securityHold": true, "id": app.ID, "name": app.Name,
			"findings": vd.Findings,
		})
		return
	}
	if req.SecurityOverride {
		// The override confirms the scan findings, which is a local decision.
		// Without this check the gate above is skipped entirely whenever the
		// field is set, so a remote caller could start a flagged app by
		// asserting the confirmation instead of being shown it.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "confirming a flagged app's start is only allowed from this computer"})
			return
		}
		secStartConfirmed.Store(app.ID, true)
	}

	// Custom port: run this start on the requested port instead of the
	// configured one. The override is recorded in the state entry so Open,
	// lanUrl and Stop keep working against it until the app is stopped.
	port := 0
	if req.Port != 0 {
		if req.Port < 1024 || req.Port > 65535 {
			s.writeJSON(w, http.StatusBadRequest,
				map[string]string{"error": "custom port must be between 1024 and 65535"})
			return
		}
		if req.Port == s.cfg.Port {
			s.writeJSON(w, http.StatusBadRequest,
				map[string]string{"error": fmt.Sprintf("port %d is the launcher's own port", req.Port)})
			return
		}
		if app.Desktop {
			s.writeJSON(w, http.StatusBadRequest,
				map[string]string{"error": "desktop apps have no port"})
			return
		}
		port = req.Port
	}

	effective := app
	if port != 0 {
		c := *app
		c.Port = port
		effective = &c
	}
	res := s.startApp(effective)
	// The card carries the last start failure until the next successful
	// start — one glance at the deck says which app needs attention.
	s.mu.Lock()
	stx := loadState(s.cfg.StateFile)
	e := stx[app.ID]
	if res.OK {
		e.LastError = ""
	} else if !res.Busy && res.Error != "" {
		e.LastError = lastN(res.Error, 160)
	}
	if e.LastError != "" || e.PID != 0 || e.PortOverride != 0 || e.Token != "" {
		stx[app.ID] = e
		saveState(s.cfg.StateFile, stx)
	}
	s.mu.Unlock()
	if port != 0 && res.OK && !res.AlreadyRunning {
		s.mu.Lock()
		st := loadState(s.cfg.StateFile)
		if e, ok := st[app.ID]; ok {
			e.PortOverride = port
			st[app.ID] = e
			saveState(s.cfg.StateFile, st)
		}
		s.mu.Unlock()
	}
	st := s.appStatus(effective, loadState(s.cfg.StateFile))
	res.Status = &st
	code := http.StatusOK
	if !res.OK && !res.Busy {
		code = http.StatusInternalServerError
	}
	s.writeJSON(w, code, res)
}

// appIsRunning is the running test used by /api/all/*: a port probe for web
// apps, the state PID's liveness for desktop apps (they have no port; the
// probe would read them as perpetually stopped and Start All would spawn
// duplicates).
func (s *Server) appIsRunning(a *App) bool {
	if a.Desktop {
		return pidAlive(loadState(s.cfg.StateFile)[a.ID].PID)
	}
	port := a.Port
	if e := loadState(s.cfg.StateFile)[a.ID].PortOverride; e != 0 {
		port = e
	}
	return portOpen(port, defaultProbeTimeout)
}

func (s *Server) serveAll(w http.ResponseWriter, start bool) {
	results := []*actionResp{}
	for _, a := range s.appsList() {
		if start && s.appIsRunning(a) {
			continue // already running → nothing to start
		}
		if !start && !s.appIsRunning(a) {
			continue // already stopped → nothing to stop
		}
		if start && a.Security == "flagged" && !securityStartOK(a) {
			// Flagged apps never ride Start All: their risk was accepted
			// (or not) one app at a time, from the card.
			results = append(results, &actionResp{OK: false, ID: a.ID,
				Error: "flagged by the security scan — start it individually to confirm"})
			continue
		}
		var res *actionResp
		if start {
			res = s.startApp(a)
		} else {
			res = s.stopApp(a)
		}
		res.ID = a.ID
		results = append(results, res)
	}
	s.writeJSON(w, http.StatusOK, allResp{OK: true, Results: results})
}

// ------------------------------------------------------------------- main ----

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
	if autostart := countAutostart(srv.appsList()); autostart > 0 {
		fmt.Printf("            (%d autostart app(s) will start now)\n", autostart)
	}
	if startAppsFlag {
		fmt.Println("            (--start-apps given: starting every stopped app now)")
	}
	switch {
	case nativeWindow:
		fmt.Println("            (native window: closing it stops the launcher and its apps)")
	case windowMode:
		fmt.Println("            (window mode: closing the dashboard stops the launcher and its apps)")
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

	if startAppsFlag || countAutostart(srv.appsList()) > 0 {
		time.AfterFunc(800*time.Millisecond, func() {
			for _, a := range srv.appsList() {
				if (startAppsFlag || a.Autostart) && !srv.appIsRunning(a) {
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
	if windowMode {
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

// hostedHandler wraps a hosted app's handler with the small slice of deck
// APIs its pages may need from their own origin: the TokenReport auto-scan
// (window.showDirectoryPicker is unavailable inside WKWebView, so the app
// reads the known agent-log homes through this loopback endpoint instead).
// The reader is mounted on the TokenReport origin only — on every hosted app
// it would hand any imported page a same-origin read of the agent-log homes,
// whose session transcripts routinely contain tokens and pasted secrets.
func hostedHandler(s *Server, app http.Handler, a *App) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.ID == "tokenreport" {
			if r.Method == http.MethodGet && r.URL.Path == "/__tokenreport__/list" {
				s.serveTokenreportList(w, r)
				return
			}
			if r.Method == http.MethodGet && r.URL.Path == "/__tokenreport__/read" {
				s.serveTokenreportRead(w, r)
				return
			}
		}
		app.ServeHTTP(w, r)
	})
}

// tokenreportAgentPath resolves one of the allow-listed agent-log homes to
// an absolute directory, or returns "" if the path is not allowed.
func tokenreportAgentPath(raw string) string {
	allowed := map[string]bool{
		"~/.zcode/cli/rollout":        true,
		"~/.claude/projects":          true,
		"~/.codex/sessions":           true,
		"~/.copilot":                  true,
		"~/.openclaw-autoclaw/agents": true,
	}
	if !allowed[raw] {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, raw[2:])
}

// serveTokenreportList backs the TokenReport auto-scan: GET
// /__tokenreport__/list?path=~/.zcode/cli/rollout (loopback only) counts —
// and names — the log files the app's parsers can read under path. The app
// cannot read the filesystem itself (browser sandbox), and a folder picker
// every start was the exact friction this removes.
func (s *Server) serveTokenreportList(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "local only"})
		return
	}
	qs := parseQuery(r.RequestURI)
	raw, err := url.QueryUnescape(strings.TrimSpace(qs["path"]))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad path"})
		return
	}
	raw = strings.TrimSpace(raw)
	root := tokenreportAgentPath(raw)
	if root == "" {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "path not allowed"})
		return
	}
	files := 0
	var names []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		ok := false
		for _, ext := range []string{".jsonl", ".json", ".db", ".db-wal"} {
			if strings.HasSuffix(d.Name(), ext) {
				ok = true
				break
			}
		}
		if ok {
			files++
			if names == nil {
				names = []string{}
			}
			names = append(names, rel)
		}
		return nil
	})
	if names == nil {
		names = []string{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": raw, "resolved": root, "files": files, "names": names})
}

// serveTokenreportRead streams ONE log file from an allow-listed agent home
// to the TokenReport app (loopback only): GET
// /__tokenreport__/read?path=~/.zcode/cli/rollout&file=<relative-path>.
// The relative path is cleaned and may not escape the root; only .jsonl,
// .json, .db and .db-wal files are served — the exact set the app's parsers
// consume. No directory listings outside the roots, no arbitrary reads.
func (s *Server) serveTokenreportRead(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "local only"})
		return
	}
	qs := parseQuery(r.RequestURI)
	raw, err := url.QueryUnescape(strings.TrimSpace(qs["path"]))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad path"})
		return
	}
	root := tokenreportAgentPath(raw)
	if root == "" {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "path not allowed"})
		return
	}
	rel, err := url.QueryUnescape(strings.TrimSpace(qs["file"]))
	if err != nil || rel == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file required"})
		return
	}
	rel = filepath.Clean("/" + rel) // forces relative, collapses ../
	if strings.Contains(rel, "..") {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "path not allowed"})
		return
	}
	switch {
	case strings.HasSuffix(rel, ".jsonl"), strings.HasSuffix(rel, ".json"),
		strings.HasSuffix(rel, ".db"), strings.HasSuffix(rel, ".db-wal"):
	default:
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "file type not allowed"})
		return
	}
	full := filepath.Join(root, rel)
	// A planted symlink inside an agent home must not pull content from
	// outside it: resolve the real paths of file and root and compare.
	// EvalSymlinks on a missing file just fails — os.Open below turns that
	// into the 404.
	if realFull, serr := filepath.EvalSymlinks(full); serr == nil {
		if realRoot, rerr := filepath.EvalSymlinks(root); rerr == nil &&
			realFull != realRoot && !strings.HasPrefix(realFull, realRoot+string(os.PathSeparator)) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "path not allowed"})
			return
		}
	}
	f, err := os.Open(full)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.Copy(w, f)
}

// ---- background watchers: liveness probe, crash watchdog, watch-folder ----

// startHealthProber refreshes the does-it-actually-answer cache every 20s.
// Running ≠ serving: a wedged process holds its pid but stops answering.
func (s *Server) startHealthProber() {
	go func() {
		for {
			st := loadState(s.cfg.StateFile)
			for _, a := range s.appsList() {
				if a.Desktop {
					continue
				}
				entry := st[a.ID]
				port := a.Port
				if entry.PortOverride != 0 {
					port = entry.PortOverride
				}
				if entry.PID == 0 || !pidAlive(entry.PID) {
					continue // stopped — the prober has no opinion
				}
				// A manifest with a health path is probed on that path: the
				// port can answer while the app is still booting, and Ready
				// should mean the app says it is ready.
				probePath := a.URLPath
				if a.Health != "" {
					probePath = a.Health
				}
				client := http.Client{Timeout: 2 * time.Second}
				resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, probePath))
				ok := err == nil
				if resp != nil {
					resp.Body.Close()
				}
				s.mu.Lock()
				s.health[a.ID] = ok
				s.mu.Unlock()
				// Contract lint: a this-computer-only app must not answer
				// on the LAN address. A refused/timeout dial means it bound
				// loopback as promised; anything else means the app ignored
				// APPDECK_HOST and the card gets a ⚠ exposed badge.
				if !a.Network {
					exposed := false
					if lan := lanIP(); lan != "" {
						exposed = hostPortOpen(lan, port, defaultProbeTimeout)
					}
					s.mu.Lock()
					s.lanExposed[a.ID] = exposed
					s.mu.Unlock()
				}
			}
			time.Sleep(20 * time.Second)
		}
	}()
}

// startWatchFolder rescans the app container every 10s; a folder dropped in
// with a parseable app.json joins the deck without touching the launcher.
func (s *Server) startWatchFolder() {
	go func() {
		known := map[string]bool{}
		for _, a := range s.appsList() {
			known[a.ID] = true
		}
		for range time.Tick(10 * time.Second) {
			container, err := s.appsContainerDir()
			if err != nil {
				continue
			}
			entries, err := os.ReadDir(container)
			if err != nil {
				continue
			}
			fresh := false
			for _, e := range entries {
				if !e.IsDir() || known[e.Name()] {
					continue
				}
				data, err := os.ReadFile(filepath.Join(container, e.Name(), manifestFileName))
				if err != nil {
					continue
				}
				var probe struct {
					ID string `json:"id"`
				}
				if json.Unmarshal(data, &probe) != nil || strings.TrimSpace(probe.ID) == "" {
					continue // half-copied folder — wait until it settles
				}
				fresh = true
			}
			if !fresh {
				continue
			}
			if err := s.reloadApps(); err != nil {
				continue
			}
			known = map[string]bool{}
			for _, a := range s.appsList() {
				known[a.ID] = true
			}
		}
	}()
}

// startKeepAlive spawns the crash watchdog for one app: poll its state pid
// every 10s and start it again when the process dies. Gives up after five
// consecutive failures — a crash loop must stay visible, not silent.
func (s *Server) startKeepAlive(a *App) {
	if !a.KeepAlive || a.Desktop || a.ServedBy == "launcher" {
		return
	}
	s.mu.Lock()
	if _, watching := s.keepAlive[a.ID]; watching {
		s.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	s.keepAlive[a.ID] = stop
	s.mu.Unlock()
	go func() {
		failures := 0
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Second):
			}
			st := loadState(s.cfg.StateFile)[a.ID]
			if st.PID != 0 && pidAlive(st.PID) {
				failures = 0
				continue
			}
			if failures >= 5 {
				return
			}
			failures++
			s.startApp(a)
		}
	}()
}

// stopKeepAlive retires the crash watcher before (and only before) a
// deliberate stop — the next start spawns a fresh one.
func (s *Server) stopKeepAlive(id string) {
	s.mu.Lock()
	stop, watching := s.keepAlive[id]
	delete(s.keepAlive, id)
	s.mu.Unlock()
	if watching {
		close(stop)
	}
}

// ---- deck export / import: the whole user setup (Apps container, registry,
// Wi-Fi overrides) as one zip, so moving to a new machine is a download and
// an upload instead of hand-copying folders. Loopback only — the deck is
// this machine's operator's data.

// deckImportMaxBytes caps an uploaded deck zip; decks carry app binaries and
// can be genuinely large, but not unbounded.
const deckImportMaxBytes = 4 << 30

func (s *Server) serveDeckExport(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="appdeck-deck-%s.zip"`, time.Now().Format("2006-01-02")))
	zw := zip.NewWriter(w)
	if err := s.writeDeckExport(zw, 0); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
}

// writeDeckExport packs the deck — every app folder, the registry and the
// network overrides — into zw. maxByte caps the uncompressed total (0 = no
// cap) so an automatic snapshot of a huge deck aborts instead of churning
// the disk; the manual export passes 0.
func (s *Server) writeDeckExport(zw *zip.Writer, maxByte int64) error {
	container, err := s.appsContainerDir()
	if err != nil {
		return err
	}
	var total int64
	addFile := func(name, path string) {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return
		}
		hdr.Name = name
		hdr.Method = zip.Deflate
		out, err := zw.CreateHeader(hdr)
		if err != nil {
			return
		}
		total += info.Size()
		_, _ = io.Copy(out, f)
	}
	addDir := func(root, prefix string) {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil // zip entries imply their directories
			}
			if maxByte > 0 && total > maxByte {
				return filepath.SkipAll
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if strings.HasSuffix(rel, ".DS_Store") || strings.HasSuffix(rel, "Thumbs.db") ||
				strings.Contains(rel, "__MACOSX") {
				return nil
			}
			addFile(prefix+rel, p)
			return nil
		})
	}
	addDir(container, "Apps/")
	addFile("apps.json", s.cfg.AppsFile)
	if s.cfg.DataDir != "" {
		addFile("network.json", filepath.Join(s.cfg.DataDir, "network.json"))
	}
	if maxByte > 0 && total > maxByte {
		return fmt.Errorf("deck exceeds the snapshot size cap (%d bytes) — skipped", maxByte)
	}
	return nil
}

// deckSnapshotKeep/MaxBytes bound the automatic backups: the newest three,
// and a deck bigger than 512 MB (uncompressed) is skipped — snapshots must
// never turn an import into a disk-churning stall.
const (
	deckSnapshotKeep    = 3
	deckSnapshotMaxByte = 512 << 20
)

// snapshotDeck writes the deck export into <data>/backups/ (keeping the
// newest few) and runs in the background after deck changes: an import, an
// uninstall, a deck restore. One at a time; skipped when the previous one is
// still running.
func (s *Server) snapshotDeck() {
	if s.cfg.DataDir == "" {
		return
	}
	if !s.snapshotMu.TryLock() {
		return
	}
	defer s.snapshotMu.Unlock()
	dir := filepath.Join(s.cfg.DataDir, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "deck-"+time.Now().Format("2006-01-02-150405")+".zip")
	f, err := os.Create(path)
	if err != nil {
		return
	}
	zw := zip.NewWriter(f)
	err = s.writeDeckExport(zw, deckSnapshotMaxByte)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var snaps []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "deck-") && strings.HasSuffix(e.Name(), ".zip") {
			snaps = append(snaps, filepath.Join(dir, e.Name()))
		}
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i] > snaps[j] }) // newest name first
	if len(snaps) > deckSnapshotKeep {
		for _, old := range snaps[deckSnapshotKeep:] {
			os.Remove(old)
		}
	}
}

// serveDeckImport restores a deck export. Merge semantics: app files are
// written only where nothing exists yet (existing installs are never
// touched), Wi-Fi overrides merge with the imported values winning, and the
// central registry file is left alone — registry apps rejoin through
// discovery of their Apps/ folders.
func (s *Server) serveDeckImport(w http.ResponseWriter, r *http.Request) {
	container, err := s.appsContainerDir()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	tmp, err := os.CreateTemp("", "appdeck-import-*.zip")
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(r.Body, deckImportMaxBytes))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading upload: " + err.Error()})
		return
	}
	if n == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty upload — send a deck export zip"})
		return
	}
	zr, err := zip.NewReader(tmp, n)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a readable deck zip: " + err.Error()})
		return
	}

	// network.json is merged, not extracted: imported entries win, existing
	// local choices for other apps survive.
	var importedOverrides map[string]bool

	// Apps packaged inside this exe are never restored from the zip: the
	// embedded copy wins, so a restored folder would be a dead duplicate.
	embedded := embeddedAppIDs()

	applied, skipped := 0, 0
	var expanded int64
	capBlown := false
	for _, zf := range zr.File {
		name := filepath.ToSlash(zf.Name)
		if strings.HasSuffix(name, "/") || strings.Contains(name, "__MACOSX") ||
			strings.HasSuffix(name, ".DS_Store") || strings.HasSuffix(name, "Thumbs.db") {
			continue
		}
		var target, root string
		switch {
		case name == "network.json":
			if s.cfg.DataDir == "" {
				continue
			}
			src, err := zf.Open()
			if err != nil {
				continue
			}
			data, err := io.ReadAll(io.LimitReader(src, 1<<20))
			src.Close()
			if err != nil || json.Unmarshal(data, &importedOverrides) != nil {
				continue
			}
			continue // handled — never written to disk raw
		case name == "apps.json":
			continue // the registry stays local; Apps/ discovery brings apps in
		case strings.HasPrefix(name, "Apps/"):
			if embedded[strings.ToLower(strings.SplitN(strings.TrimPrefix(name, "Apps/"), "/", 2)[0])] {
				continue // packaged inside the exe — the embedded copy wins
			}
			root = container
			target = filepath.Join(container, strings.TrimPrefix(name, "Apps/"))
		default:
			continue // unknown entry — never written outside the known roots
		}
		clean := filepath.Clean(target)
		if clean != root && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
			continue // zip-slip attempt — ignored
		}
		if _, err := os.Lstat(clean); err == nil {
			skipped++ // existing installs are never overwritten
			continue
		}
		if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
			continue
		}
		src, err := zf.Open()
		if err != nil {
			continue
		}
		mode := os.FileMode(0o644)
		if zf.Mode()&0o111 != 0 {
			mode = 0o755 // keep executables executable (unix)
		}
		out, err := os.OpenFile(clean, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			src.Close()
			continue
		}
		n, cerr := io.Copy(out, src)
		src.Close()
		out.Close()
		if cerr != nil {
			os.Remove(clean)
			continue
		}
		expanded += n
		if expanded > installMaxTotal {
			// The same expanded-bytes cap an app import enforces — a deck
			// zip can fill the disk just as well. What already landed is
			// kept; the restore refuses to continue.
			os.Remove(clean)
			capBlown = true
			break
		}
		applied++
	}
	if importedOverrides != nil {
		ov := s.loadNetworkOverrides()
		for id, enabled := range importedOverrides {
			ov[id] = enabled
		}
		s.mu.Lock()
		s.saveNetworkOverrides(ov)
		s.mu.Unlock()
		s.applyNetworkOverrides(s.appsList())
	}
	s.reloadApps()
	if applied > 0 {
		go s.snapshotDeck()
	}
	if capBlown {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "applied": applied, "skipped": skipped,
			"error": "deck zip expands past the 1 GiB cap — refused; apps already extracted were kept"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "applied": applied, "skipped": skipped,
		"note": "apps merged — existing installs were not touched; registry apps rejoin via their Apps/ folders"})
}
