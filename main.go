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

	// Startup wait (Python: 50 rounds of probe + 0.3 s sleep, ~15 s budget).
	startWaitRounds = 50
	startWaitSleep  = 300 * time.Millisecond

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
}

// DefaultConfig resolves paths relative to the executable or working
// directory. LAUNCHER_ROOT overrides it for packaged deployments. In
// single-file builds everything lives under a per-user data directory
// (LAUNCHER_DATA_DIR overrides) so nothing is written next to the exe.
func DefaultConfig() Config {
	if embeddedBuild {
		dataDir := defaultDataDir()
		return Config{
			Root:      dataDir,
			DataDir:   dataDir,
			AppsFile:  "", // packed inside the executable
			StateFile: filepath.Join(dataDir, "state.json"),
			LogDir:    filepath.Join(dataDir, "logs"),
			IndexFile: "", // served from the embedded dashboard
			Host:      envOr("HOST", "0.0.0.0"),
			Port:      envInt("PORT", 9090),
			PortsFile: filepath.Join(dataDir, "ports.json"),
		}
	}
	root := sourceDir()
	if v := os.Getenv("LAUNCHER_ROOT"); v != "" {
		root = v
	}
	// state.json + ports.json live in the per-user data dir, never beside the
	// exe: replacing/updating the launcher must not touch deck state.
	dataDir := defaultDataDir()
	migrateUserState(root, dataDir)
	return Config{
		ProjectRoot: filepath.Dir(root),
		Root:        root,
		AppsFile:    filepath.Join(root, "apps.json"),
		StateFile:   filepath.Join(dataDir, "state.json"),
		LogDir:      filepath.Join(root, "logs"),
		IndexFile:   filepath.Join(root, "index.html"),
		Host:        envOr("HOST", "0.0.0.0"),
		Port:        envInt("PORT", 9090),
		LANToken:    os.Getenv("LAUNCHER_LAN_TOKEN"),
		// DataDir is the per-user store for app data and settings in every
		// build mode; folder mode never unpacks into it, but app data and
		// settings must not land inside the source tree.
		DataDir:   dataDir,
		PortsFile: filepath.Join(dataDir, "ports.json"),
	}
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
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Icon     string            `json:"icon"`
	Desc     string            `json:"desc"`
	Category string            `json:"category"`
	Port     int               `json:"port"`
	URLPath  string            `json:"urlPath"`
	Cmd      string            `json:"cmd"`
	Binary   string            `json:"binary,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Network  bool              `json:"network"`
	Env      map[string]string `json:"env,omitempty"`
	ServedBy string            `json:"servedBy,omitempty"` // "launcher" = HTML app hosted in-process (single-file build)
	RawDir   string            `json:"dir"`
	// Desktop marks a portless desktop app (GUI/CLI binary, no web server):
	// "running" is the state-tracked process being alive, not a port probe.
	Desktop bool `json:"desktop,omitempty"`
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
	// KeepAlive restarts the app's process when it dies (crash watchdog).
	KeepAlive bool   `json:"keepAlive,omitempty"`
	Source    string `json:"-"`
	Dir       string `json:"-"` // resolved absolute dir (Python "_dir")
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
	// accentCache memoizes per-app card colors derived from their icons.
	accentCache sync.Map
	// health caches the background liveness probe (does the app's port
	// actually answer HTTP?) — running ≠ serving. absent = unknown.
	health map[string]bool
	// keepAlive holds stop channels for the per-app crash watchers.
	keepAlive map[string]chan struct{}
}

// NewServer loads the app registry and returns a handler serving the
// launcher API.
func NewServer(cfg Config) (*Server, error) {
	apps, byID, warnings, err := loadApps(cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:             cfg,
		apps:            apps,
		byID:            byID,
		inflight:        make(map[string]struct{}),
		warnings:        warnings,
		hostedListeners: make(map[string]net.Listener),
		health:          make(map[string]bool),
		keepAlive:       make(map[string]chan struct{}),
	}
	s.applyNetworkOverrides(apps)
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
	byID := make(map[string]*App, len(parsed.Apps))
	kept := make([]*App, 0, len(parsed.Apps))
	configuredDirs := make(map[string]bool, len(parsed.Apps))
	var warnings []string
	for _, a := range parsed.Apps {
		if a == nil || a.ID == "" {
			return nil, nil, nil, fmt.Errorf("%s: app entry missing \"id\"", cfg.AppsFile)
		}
		if _, dup := byID[a.ID]; dup {
			return nil, nil, nil, fmt.Errorf("duplicate app id in apps.json: %s", a.ID)
		}
		byID[a.ID] = a
		a.Source = "registry"
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
		// no folder (assets come from the exe); in folder mode they serve
		// straight from their disk folder.
		if a.ServedBy == "launcher" && embeddedBuild {
			a.Dir = ""
			// An app with a RawDir may have assets on disk in the data
			// directory (disk-serve override, icons). That folder is
			// registry-owned: claim it so manifest discovery doesn't pick it
			// up as a duplicate third-party app. Embedded-only apps (RawDir
			// "") serve from the exe and own no folder — Join(DataDir, "")
			// would claim the whole data directory, so guard the empty case.
			if a.RawDir != "" {
				if abs, err := filepath.Abs(filepath.Join(cfg.DataDir, a.RawDir)); err == nil {
					if _, err := os.Stat(abs); err == nil {
						configuredDirs[foldPath(abs)] = true
					}
				}
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
		kept = append(kept, a)
	}

	// Third-party manifest apps. Folder mode scans the project tree (the
	// launcher's parent, including its Apps/ group folder); single-file
	// builds scan only the data directory — the one place an install
	// (POST /api/import) ever writes. rawDirPrefix makes each discovered
	// folder's RawDir registry-shaped ("../Apps/x" vs "Apps/x").
	if embeddedBuild {
		// The import endpoint installs into <data>/Apps (appsContainerDir,
		// capital A) — but on case-insensitive macOS filesystems a folder
		// created as "apps" by older builds stays lowercase on disk. Key the
		// RawDir prefix off the container's real on-disk name so the registry
		// path matches what was installed; the binary resolver only uses the
		// base name, so this only has to stay self-consistent.
		containerPrefix := "Apps/"
		if entries, err := os.ReadDir(cfg.DataDir); err == nil {
			for _, e := range entries {
				if e.IsDir() && strings.EqualFold(e.Name(), "Apps") {
					containerPrefix = e.Name() + "/"
					break
				}
			}
		}
		manifestApps := discoverManifestApps(
			[]string{cfg.DataDir}, containerPrefix, configuredDirs, byID, &warnings)
		kept = append(kept, manifestApps...)
	} else {
		manifestApps := discoverManifestApps(
			[]string{cfg.ProjectRoot}, "../", configuredDirs, byID, &warnings)
		kept = append(kept, manifestApps...)

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
// projects use the static server; Go modules use the launcher build logic.
func discoverableProject(dir string) (kind, entrypoint string) {
	if fileExists(filepath.Join(dir, "go.mod")) {
		if goHTTPEntrypoint(dir) {
			return "go", ""
		}
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
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "launcher: save_state: %v\n", err)
		return
	}
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
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), timeout)
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
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Icon         string   `json:"icon"`
	IconFile     bool     `json:"iconFile"`
	Accent       string   `json:"accent,omitempty"`
	Autostart    bool     `json:"autostart"`
	Desktop      bool     `json:"desktop,omitempty"`
	Desc         string   `json:"desc"`
	Category     string   `json:"category"`
	Port         int      `json:"port"`
	PortOverride int      `json:"portOverride,omitempty"`
	URL          string   `json:"url"`
	Cmd          string   `json:"cmd"`
	Network      bool     `json:"network"`
	Version      string   `json:"version,omitempty"`
	Healthy      *bool    `json:"healthy,omitempty"`
	LanURL       *string  `json:"lanUrl"`
	Running      bool     `json:"running"`
	Managed      bool     `json:"managed"`
	External     bool     `json:"external"`
	PID          *int     `json:"pid"`
	Proc         *string  `json:"proc"`
	StartedAt    *float64 `json:"startedAt"`
	Source       string   `json:"source,omitempty"` // registry | manifest | discovered
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

	out := appStatus{
		ID:           a.ID,
		Name:         a.Name,
		Icon:         a.Icon,
		IconFile:     appHasIconFile(a),
		Accent:       s.accentFor(a),
		Autostart:    a.Autostart,
		Desktop:      a.Desktop,
		Desc:         a.Desc,
		Category:     a.Category,
		Port:         a.Port,
		PortOverride: entry.PortOverride,
		URL:          url,
		Cmd:          a.Cmd,
		Network:      a.Network,
		Version:      a.Version,
		LanURL:       lanURL,
		Running:      running,
		Managed:      ours && running,
		External:     running && !ours,
		Proc:         proc,
		Source:       a.Source,
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
	Lan     *string `json:"lan"`
	LanURL  *string `json:"lanUrl"`
	Mode    string  `json:"mode,omitempty"`    // "single-file" in embed builds
	DataDir string  `json:"dataDir,omitempty"` // where extracted apps live (embed builds)
	Window  string  `json:"window,omitempty"`  // "native" | "app" | "browser" | "none"
	OS      string  `json:"os,omitempty"`      // runtime.GOOS, e.g. "windows"
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
	cmd.Stdout = logFile
	cmd.Stderr = logFile // Python: stderr=subprocess.STDOUT
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

	// Wait for the port to answer (max ~15 s, e.g. streamlit is slow).
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	for i := 0; i < startWaitRounds; i++ {
		if portOpen(a.Port, startProbeTimeout) {
			return &actionResp{OK: true, PID: &pid}
		}
		select {
		case <-exited:
			return &actionResp{OK: false, Error: "process exited during startup", PID: &pid}
		default:
		}
		time.Sleep(startWaitSleep)
	}
	note := "started, but port did not answer within 15 s — check logs"
	return &actionResp{OK: true, PID: &pid, Slow: true, Note: note}
}

// appCommand builds the exec.Cmd an app entry describes: a "binary"
// (auto-built when its platform binary is missing) with {port}/{host}-
// substituted args, else a shell "cmd" string. Shared by the web and desktop
// start paths.
func (s *Server) appCommand(a *App) (*exec.Cmd, error) {
	host := bindHostFor(a)
	if a.Binary != "" {
		binary, err := resolveBinary(s.cfg.Root, a.Binary)
		if err != nil {
			return nil, err
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
	var outBuf bytes.Buffer
	token := newToken()
	cmd, err := s.appCommand(a)
	if err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	cmd.Dir = a.Dir
	cmd.Env = s.childEnv(a, token)
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
			if len(out) > 300 {
				out = out[len(out)-300:] // the last lines carry the actual error
			}
			msg += ": " + out
		}
		return &actionResp{OK: false, Error: msg, PID: &pid}
	case <-time.After(time.Second):
	}
	return &actionResp{OK: true, PID: &pid}
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
		srv := &http.Server{Handler: hostedHandler(s, handler, a), ReadHeaderTimeout: 10 * time.Second}
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

func resolveBinary(root, configured string) (string, error) {
	// The app folder is the single home in every build mode: embedded
	// builds unpack binaries into <data>/<RawDir> (= apps/<id>/, the same
	// place imports extract to), and resolution tries the configured path,
	// its platform-suffixed form, then the legacy flat bin/ cache from
	// older installs. Matching the folder-mode rules keeps one mental model:
	// "the binary lives beside app.json".
	path := configured
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path, _ = filepath.Abs(path)
	platformPath := platformBinaryPath(path)
	// The plain configured path first: imported exes keep whatever name they
	// were imported with (it may already carry a platform suffix), while
	// repo-built apps follow the -<os>-<arch> convention.
	candidates := []string{path, platformPath}
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
	if built, err := buildGoBinary(platformPath); err == nil {
		return built, nil
	} else {
		return "", fmt.Errorf("Go app binary not found: %s (looked in %s; automatic build failed: %v)", configured, strings.Join(candidates, ", "), err)
	}
}

func platformBinaryPath(path string) string {
	suffixed := path + "-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		suffixed += ".exe" // Windows needs the extension to execute reliably
	}
	return suffixed
}

// buildGoBinary builds an app on first use when its platform binary is absent.
// Most apps have main.go at the module root; Monitor uses the conventional
// cmd/monitor entrypoint, which is discovered automatically.
func buildGoBinary(output string) (string, error) {
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
			if entry.IsDir() && fileExists(filepath.Join(moduleDir, "cmd", entry.Name(), "main.go")) {
				entrypoints = append(entrypoints, "./cmd/"+entry.Name())
			}
		}
		if len(entrypoints) != 1 {
			return "", fmt.Errorf("expected one Go entrypoint in %s/cmd, found %d", moduleDir, len(entrypoints))
		}
		buildTarget = entrypoints[0]
	}

	goTool, err := goToolPath()
	if err != nil {
		return "", fmt.Errorf("go toolchain not found — install Go (https://go.dev/dl/) or put the built binary next to app.json")
	}
	cmd := exec.Command(goTool, "build", "-o", output, buildTarget)
	hideWindow(cmd)
	cmd.Dir = moduleDir
	if outputText, err := cmd.CombinedOutput(); err != nil {
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

// goToolPath locates the go toolchain. Double-clicked .app launches get a
// minimal PATH (/usr/bin:/bin:/usr/sbin:/sbin) that excludes every usual Go
// install, so PATH alone is not enough — probe the common locations too.
func goToolPath() (string, error) {
	if p, err := exec.LookPath("go"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		"/opt/homebrew/bin/go",           // Homebrew (Apple silicon)
		"/usr/local/go/bin/go",           // official installer (macOS/Linux)
		"/usr/local/bin/go",              // Homebrew (Intel) / manual symlink
		"/opt/local/bin/go",              // MacPorts
		`C:\Program Files\Go\bin\go.exe`, // Windows official installer
		`C:\Go\bin\go.exe`,               // Windows custom install
	}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, "go", "bin", "go"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return c, nil
		}
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
	if a.Network {
		env["APPDECK_NETWORK"] = "1" // Wi-Fi sharing is on; Host checks may loosen
	}
	if token != "" {
		env["APPDECK_TOKEN"] = token
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
	killed := []int{}

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
	}

	// 2) anything else of ours still listening on the port (external start);
	// desktop apps have no port, so there is nothing external to find
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
		}
	}

	s.mu.Lock()
	st = loadState(s.cfg.StateFile)
	delete(st, a.ID)
	saveState(s.cfg.StateFile, st)
	s.mu.Unlock()

	still := portOpen(a.Port, startProbeTimeout)
	return &actionResp{OK: !still, Killed: &killed, StillRunning: &still}
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

// appIconNames are the icon files an app folder may provide, in priority
// order. Icons live in the app's own folder, so adding or changing one never
// touches Launcher/ — drop an icon.png next to the app and rebuild/restart.
// The apps.json emoji remains the fallback when no file is present.
var appIconNames = []struct {
	name  string
	ctype string
}{
	{"icon.png", "image/png"},
	{"icon.svg", "image/svg+xml"},
}

// appIcon loads the app's folder icon: from the packaged embedded assets in
// single-file builds, from the app folder on disk otherwise.
func appIcon(a *App) (data []byte, ctype string, ok bool) {
	if embeddedBuild {
		return embeddedIcon(a.ID)
	}
	if a.Dir == "" {
		return nil, "", false
	}
	for _, ic := range appIconNames {
		if data, err := os.ReadFile(filepath.Join(a.Dir, ic.name)); err == nil {
			return data, ic.ctype, true
		}
	}
	return nil, "", false
}

// appHasIconFile is the cheap existence check for the /api/apps badge.
func appHasIconFile(a *App) bool {
	if embeddedBuild {
		return embeddedIconExists(a.ID)
	}
	if a.Dir == "" {
		return false
	}
	for _, ic := range appIconNames {
		if fileExists(filepath.Join(a.Dir, ic.name)) {
			return true
		}
	}
	return false
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
	data, ctype, ok := appIcon(app)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "no icon file in the app folder (icon.png or icon.svg)"})
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
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

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
// serveUninstall removes a manifest/discovered app: stop it, delete its
// folder, drop its port assignment (via reload), and optionally its per-app
// data directory. Registry apps (apps.json) are refused — that file is
// hand-curated; remove the entry there first.
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
	if a.Source == "registry" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": a.ID + " is registered in apps.json — remove its entry there first"})
		return
	}
	if a.Dir == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": a.ID + " has no folder on disk"})
		return
	}
	if rootAbs, err := filepath.Abs(s.cfg.Root); err == nil && a.Dir == rootAbs {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "refusing to remove the launcher folder"})
		return
	}
	_ = s.stopApp(a) // a dead process must not block removal
	if err := os.RemoveAll(a.Dir); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "remove folder: " + err.Error()})
		return
	}
	note := a.ID + " uninstalled"
	dataDir := ""
	if s.cfg.DataDir != "" {
		dataDir = filepath.Join(s.cfg.DataDir, "appdata", a.ID)
	}
	if body.RemoveData && dataDir != "" {
		if os.RemoveAll(dataDir) == nil {
			note += " — data folder removed"
		}
	} else if dataDir != "" {
		note += " — data kept at " + dataDir
	}
	if err := s.reloadApps(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note})
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
	// Guard for state-changing calls. Browsers attach an Origin header to
	// every cross-site request, so one that names a different host is a
	// drive-by web page trying to drive the local API with a text/plain
	// POST — reject it. LAUNCHER_LAN_TOKEN additionally requires the
	// matching X-AppDeck-Token header from non-loopback callers when set.
	if r.Method == http.MethodPost && strings.HasPrefix(path, "/api/") {
		if origin := r.Header.Get("Origin"); origin != "" {
			if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
				s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin requests are not allowed"})
				return
			}
		}
		if !fromLocalMachine(r) && s.cfg.LANToken != "" && r.Header.Get("X-AppDeck-Token") != s.cfg.LANToken {
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
		// Read-only and LAN-visible by design; the open header lets deck
		// tools (Wi-Fi Guard-style pages on their own port) read it too.
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
		if embeddedBuild {
			out.Mode = "single-file"
			out.DataDir = s.cfg.DataDir
		}
		out.Window = launchWindow
		out.OS = runtime.GOOS
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
	case r.Method == http.MethodPost && path == "/api/heartbeat":
		// Dashboard keepalive; ?bye=1 is the pagehide beacon sent when the
		// last dashboard window/tab goes away (window mode shuts down then).
		noteActivity()
		if parseQuery(r.RequestURI)["bye"] == "1" {
			handleBye(s)
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
		if !strings.HasPrefix(u, "http://127.0.0.1") && !strings.HasPrefix(u, "http://[::1]") &&
			!strings.HasPrefix(u, "https://") {
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
	case r.Method == http.MethodGet && path == "/favicon.ico":
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && path == "/api/network":
		// Flip one web app's Wi-Fi sharing (the dashboard's drag between the
		// two sections). Writes machine-local state, so loopback only.
		s.serveNetworkSet(w, r)
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
		noteActivity()
		s.serveUninstall(w, r)
	case r.Method == http.MethodPost && path == "/api/datafolder":
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
	app, err := s.installArchiveBytes(body)
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
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
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

// isLoopback reports whether the request came from this computer (Host is
// 127.0.0.1[:port] or [::1][:port]).
func isLoopback(host string) bool {
	return strings.HasPrefix(host, "127.0.0.1") || strings.HasPrefix(host, "[::1]")
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
	if portOpen(cfg.Port, startProbeTimeout) {
		fail(fmt.Sprintf("Port %d is already in use - is the launcher already running?\n"+
			"Open http://127.0.0.1:%d or stop the other process first.", cfg.Port, cfg.Port))
	}
	srv, err := NewServer(cfg)
	if err != nil {
		fail(err.Error())
	}

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
	httpSrv := &http.Server{Handler: srv}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()

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
func hostedHandler(s *Server, app http.Handler, a *App) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/__tokenreport__/list" {
			s.serveTokenreportList(w, r)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/__tokenreport__/read" {
			s.serveTokenreportRead(w, r)
			return
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
				client := http.Client{Timeout: 2 * time.Second}
				resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, a.URLPath))
				ok := err == nil
				if resp != nil {
					resp.Body.Close()
				}
				s.mu.Lock()
				s.health[a.ID] = ok
				s.mu.Unlock()
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
	container, err := s.appsContainerDir()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="appdeck-deck-%s.zip"`, time.Now().Format("2006-01-02")))
	zw := zip.NewWriter(w)
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
		_, _ = io.Copy(out, f)
	}
	addDir := func(root, prefix string) {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil // zip entries imply their directories
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
	zw.Close()
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
		_, err = io.Copy(out, src)
		src.Close()
		out.Close()
		if err != nil {
			os.Remove(clean)
			continue
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
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "applied": applied, "skipped": skipped,
		"note": "apps merged — existing installs were not touched; registry apps rejoin via their Apps/ folders"})
}
