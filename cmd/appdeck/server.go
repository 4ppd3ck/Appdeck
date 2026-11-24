// Config, App and Server — the launcher's core state types and accessors.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
	// KeepAliveLimit caps the crash watchdog's consecutive restart attempts
	// (0 = 5 — a crash loop must stay visible, not silent).
	KeepAliveLimit int `json:"keepAliveLimit,omitempty"`
	// KeepAliveDelay spaces the crash watchdog's poll/restart cycle in
	// seconds (0 = 10).
	KeepAliveDelay int `json:"keepAliveDelay,omitempty"`
	// Lifecycle policy: "always" (boot with the launcher + crash watchdog),
	// "onDemand" (stop once no client has held a connection for the idle
	// budget) or "scheduled" (run inside Schedule's window). "" = manual.
	Lifecycle string `json:"lifecycle,omitempty"`
	// StopAfterIdle is the On-Demand idle budget in minutes (0 = 5).
	StopAfterIdle int `json:"stopAfterIdle,omitempty"`
	// Schedule is the Scheduled window: "Mon-Fri 09:00-17:00" or plain
	// "09:00-17:00" (validated at load; see lifecycle.go).
	Schedule string `json:"schedule,omitempty"`
	// Permissions declares what the card needs beyond its own folder and
	// port — "network", "lan", "fs-read", "fs-write", "exec" (see
	// permissions.go). Declared permissions gate the first start behind an
	// operator prompt and feed the OS sandbox profiles.
	Permissions []string `json:"permissions,omitempty"`
	// Sandbox asks the launcher to run this app under the OS sandbox
	// (sandbox.go: seatbelt on darwin, bubblewrap on linux, a Job Object on
	// windows). The profile is shaped by the declared permissions; a
	// privileged (elevated) launch ignores the sandbox — the two wrappers
	// are exclusive.
	Sandbox bool `json:"sandbox,omitempty"`
	// BinaryChecksum is the SHA-256 the import recorded for the card's
	// binary (hex) — provenance: "is the file on disk still the one that was
	// imported?". Empty for source-only imports that build on first start.
	BinaryChecksum string `json:"binaryChecksum,omitempty"`
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
	// TrayToggle declares the app's quick action in the launcher's tray
	// menu: the item reads "Pause <label>" / "Resume <label>" and flips the
	// app's global enforcement state in one click (wTower's blocking pause
	// was the first). The contract: GET <status> answers {"paused": bool};
	// POST <action> takes {"action": "pause"|"resume"} and guards itself to
	// loopback callers. Web apps only — a desktop app has no HTTP surface
	// to toggle.
	TrayToggle *TrayToggleSpec `json:"trayToggle,omitempty"`
	Source     string          `json:"-"`
	Dir        string          `json:"-"` // resolved absolute dir (Python "_dir")
}

// TrayToggleSpec is the manifest side of App.TrayToggle: the tray quick
// action's noun and its two app-relative URL paths. Action falls back to
// Status (usually the same endpoint), and an empty label falls back to the
// app name — "Pause wTower" needs no label at all.
type TrayToggleSpec struct {
	Label  string `json:"label,omitempty"`
	Status string `json:"status"`
	Action string `json:"action,omitempty"`
}

// labelOr is the tray item noun: the declared label or the app name.
func (tt *TrayToggleSpec) labelOr(name string) string {
	if tt.Label != "" {
		return tt.Label
	}
	return name
}

// actionPath is the POST endpoint (same endpoint as the status probe unless
// the manifest declares a separate one).
func (tt *TrayToggleSpec) actionPath() string {
	if tt.Action != "" {
		return tt.Action
	}
	return tt.Status
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
	// resmon cache (resmon.go): the sampler goroutine writes under resMu,
	// readers copy — sampling never runs on a request path.
	resMu      sync.Mutex
	resCache   map[string]appResources
	resCPULast map[int]float64
	resLast    time.Time
	// lifecycle policies (lifecycle.go): operator overrides ride s.mu;
	// the loop's runtime bookkeeping rides policyMu.
	policyOverrides   map[string]policyOverride
	policyMu          sync.Mutex
	policyIdle        map[string]time.Time
	policyLoopStarted map[string]bool
	// netConns is the TCP-table seam the On-Demand idle check reads
	// (gopsutil in production; tests stub it).
	netConns connFetcher
	// permission grants (permissions.go): persisted "always" verdicts under
	// s.mu, session-scoped "once" verdicts too.
	permGrants map[string]permGrant
	permOnce   map[string][]string
	// event bus (eventbus.go): one fan-out, a replay buffer per topic.
	busMu      sync.Mutex
	busSubs    []*busSub
	busHistory map[string][]busMessage
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
	// updateStage holds one validated package between the dashboard's review
	// and commit steps. Canceling it never touches the installed launcher.
	updateStageMu sync.Mutex
	updateStage   *stagedLauncherUpdate
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
		resCache:        make(map[string]appResources),
		resCPULast:      make(map[int]float64),
		policyOverrides: loadPolicyOverrides(filepath.Join(cfg.DataDir, "policies.json")),
	}
	s.policyIdle = make(map[string]time.Time)
	s.policyLoopStarted = make(map[string]bool)
	s.installNetFetcher()
	s.permGrants = loadPermGrants(filepath.Join(cfg.DataDir, "permissions.json"))
	s.permOnce = make(map[string][]string)
	s.busHistory = make(map[string][]busMessage)
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
	// Resources is the resmon sample for a running app (RSS/CPU of its
	// whole process tree) — nil when the sampler has not ticked yet or the
	// app is stopped.
	Resources *appResources `json:"resources,omitempty"`
	// Policy is the card's effective lifecycle policy (manifest defaults
	// with any operator override applied) — non-empty only when a policy
	// mode is actually set; the ⏱ dialog edits the override.
	Policy *appPolicy `json:"policy,omitempty"`
	// Permissions is the card's declared permission list (permissions.go) —
	// drives the 🔑 badge and the first-start prompt.
	Permissions []string `json:"permissions,omitempty"`
	// Sandboxed reports that the card runs under the OS sandbox (sandbox.go).
	Sandboxed bool `json:"sandboxed,omitempty"`
	// ServedBy mirrors App.ServedBy ("launcher" = a static page hosted
	// in-process) — the sidebar's Web pages section counts these.
	ServedBy string `json:"servedBy,omitempty"`
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
	// Resource chip data: the sampler's last snapshot, running apps only.
	if running {
		if r := s.appResFor(a.ID); r != nil {
			out.Resources = r
		}
	}
	// Lifecycle policy: only when a mode is set (or the operator overrode)
	// — keeps the /api/apps payload unchanged for policy-less decks.
	if p := s.policyFor(a); p.Mode != "" || p.Source == "override" {
		pp := p
		out.Policy = &pp
	}
	if len(a.Permissions) > 0 {
		out.Permissions = a.Permissions
	}
	out.Sandboxed = sandboxEnabled(a)
	out.ServedBy = a.ServedBy
	return out
}

// ----------------------------------------------------------- start / stop ----

// actionResp is the JSON body of /api/start and /api/stop. Pointer/omitempty
// fields are present in the output exactly when the Python version includes
// them (e.g. "killed": [] stays visible, absent keys stay absent).
type actionResp struct {
	OK             bool   `json:"ok"`
	Busy           bool   `json:"busy,omitempty"`
	AlreadyRunning bool   `json:"alreadyRunning,omitempty"`
	Error          string `json:"error,omitempty"`
	PID            *int   `json:"pid,omitempty"`
	Slow           bool   `json:"slow,omitempty"`
	Note           string `json:"note,omitempty"`
	// Removed marks a start that failed with a platform-incompatibility
	// announcement and uninstalled the app on the spot (removeIfIncompatible):
	// the card is gone from the deck and Note carries the reason.
	Removed      bool       `json:"removed,omitempty"`
	Killed       *[]int     `json:"killed,omitempty"`
	StillRunning *bool      `json:"stillRunning,omitempty"`
	Status       *appStatus `json:"status,omitempty"`
	ID           string     `json:"id,omitempty"` // set on /api/all/* results
	// Pending carries the permission gate: a start that refuses because the
	// card declares permissions nobody has granted yet (permissions.go).
	Pending *permPrompt `json:"pending,omitempty"`
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
