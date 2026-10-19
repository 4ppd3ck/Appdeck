// Prerequisites: the launcher's own toolchains and services. An app can
// declare what it needs (app.json "requires": ["rust", …]); the dashboard's
// 🧰 tab shows each tool's status, installs the installable ones (default
// ~/.appdeck-tools location or a custom path), and accepts a path for tools
// already installed elsewhere — registered paths are what the toolchain
// lookups consult first, so a custom install is first-class, not a hint.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// customToolPaths maps a tool id to a user-registered binary path. It is
// loaded from <data>/toolpaths.json at startup and rewritten on change; the
// lookup helpers (goToolPath, cargoToolPath, gitToolPath, …) consult it
// before probing the well-known locations.
var (
	customToolMu     sync.RWMutex
	customToolPaths  = map[string]string{}
	customToolLoaded bool
)

// pickTimeout bounds a native folder-picker dialog — the user may wander
// off, but a wedged dialog must not hold the request forever.
const pickTimeout = 5 * time.Minute

func toolPathsFile(dataDir string) string {
	return filepath.Join(dataDir, "toolpaths.json")
}

func loadCustomToolPaths(dataDir string) { refreshCustomToolPaths(dataDir, false) }

// refreshCustomToolPaths re-reads toolpaths.json into memory. force bypasses
// the loaded-once guard: a fresh boot loads once, but the 🧰 tab re-reads on
// every view so a registry written by another process (or an install that
// landed after this launcher started) is never shown stale.
func refreshCustomToolPaths(dataDir string, force bool) {
	customToolMu.Lock()
	defer customToolMu.Unlock()
	if customToolLoaded && !force {
		return
	}
	data, err := os.ReadFile(toolPathsFile(dataDir))
	if err != nil {
		return // no file yet — keep whatever is in memory
	}
	var m map[string]string
	if json.Unmarshal(data, &m) == nil && m != nil {
		customToolPaths = m
		customToolLoaded = true
	}
}

func saveCustomToolPaths(dataDir string) error {
	customToolMu.RLock()
	m := customToolPaths
	customToolMu.RUnlock()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := toolPathsFile(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// setCustomToolPath registers (or clears, with an empty path) the binary
// path of one tool and persists the registry.
func setCustomToolPath(dataDir, tool, path string) error {
	if !knownToolID(tool) {
		return fmt.Errorf("unknown tool %q", tool)
	}
	customToolMu.Lock()
	if strings.TrimSpace(path) == "" {
		delete(customToolPaths, tool)
	} else {
		customToolPaths[tool] = strings.TrimSpace(path)
	}
	customToolMu.Unlock()
	return saveCustomToolPaths(dataDir)
}

// registeredToolPath returns the user-registered path of a tool (resolving a
// directory to the tool's binary inside it), or "".
func registeredToolPath(tool string) string {
	customToolMu.RLock()
	p := customToolPaths[tool]
	customToolMu.RUnlock()
	if p == "" {
		return ""
	}
	return resolveToolPath(tool, p)
}

// resolveToolPath accepts either the binary itself or a directory holding
// it — directly ("~/tools" with cargo inside → ~/tools/cargo), or under a
// bin/ or sbin/ subfolder ("~/.cargo" → ~/.cargo/bin/cargo, Homebrew's
// nginx → /opt/homebrew/sbin/nginx).
func resolveToolPath(tool, p string) string {
	p = expandHome(strings.TrimSpace(p))
	info, err := os.Stat(p)
	if err != nil {
		return ""
	}
	if !info.IsDir() {
		return p
	}
	for _, sub := range []string{"", "bin", "sbin"} {
		for _, name := range toolBinaryNames(tool) {
			cand := filepath.Join(p, sub, name)
			if st, err := os.Stat(cand); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
				return cand
			}
		}
	}
	return ""
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// toolBinaryNames lists the executable names a tool's binary may carry.
func toolBinaryNames(tool string) []string {
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	switch tool {
	case "go":
		return []string{"go" + exe}
	case "rust":
		return []string{"cargo" + exe}
	case "git":
		return []string{"git" + exe}
	case "python":
		return []string{"python3" + exe, "python" + exe}
	case "node":
		return []string{"node" + exe}
	case "nginx":
		return []string{"nginx" + exe}
	}
	return []string{tool + exe}
}

// knownToolID reports whether the id is one of the preset tools.
func knownToolID(id string) bool {
	for _, t := range toolPresets {
		if t.ID == id {
			return true
		}
	}
	return false
}

// toolPreset describes one prerequisite: how to detect it, how to install
// it (nil = not automatic), and where a default install lands. PointHint
// tells the user what to navigate to in the folder picker when registering
// an existing install — the answer differs per tool (Python wants its bin
// folder, Homebrew's nginx lives in sbin, …).
type toolPreset struct {
	ID          string
	Name        string
	Desc        string
	PointHint   string
	probe       func() (path, version string, ok bool)
	install     func(dir string) error // dir "" = default location; nil = manual
	defaultRoot string                 // default install parent for auto installs
	installNote string                 // shown when install is nil
}

func toolByID(id string) *toolPreset {
	for i := range toolPresets {
		if toolPresets[i].ID == id {
			return &toolPresets[i]
		}
	}
	return nil
}

// toolsRoot is the default parent for launcher-installed toolchains: a
// user-owned directory, so no sudo is involved anywhere.
func toolsRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".appdeck-tools")
}

var toolPresets = []toolPreset{
	{
		ID:          "go",
		Name:        "Go",
		Desc:        "AppDeck itself and the Go apps build with it",
		PointHint:   "Point to the go binary, its bin folder, or the Go root (e.g. /usr/local/go).",
		probe:       probeVersion("go", "go", "version"),
		defaultRoot: toolsRoot(),
		install:     installGo,
	},
	{
		ID:          "rust",
		Name:        "Rust / Cargo",
		Desc:        "Rust apps build on first start",
		PointHint:   "Point to the cargo binary or the cargo home (e.g. ~/.cargo — bin/cargo inside is found).",
		probe:       probeVersion("cargo", "cargo", "--version"),
		defaultRoot: toolsRoot(),
		install:     installRust,
	},
	{
		ID:          "git",
		Name:        "Git",
		Desc:        "Imports repositories from the dashboard",
		PointHint:   "Point to the git binary or its bin folder (macOS: /usr/bin/git).",
		probe:       probeVersion("git", "git", "--version"),
		installNote: "macOS: run  xcode-select --install  in a terminal (or install Git for Windows on Windows)",
	},
	{
		ID:          "python",
		Name:        "Python 3",
		Desc:        "Needed by Python-based apps",
		PointHint:   "Point to the python3 binary or its bin folder (e.g. /Library/Frameworks/Python.framework/Versions/3.14/bin).",
		probe:       probeVersion("python", "python3", "--version"),
		defaultRoot: toolsRoot(),
		install:     installBrew("python3", "Python 3"),
	},
	{
		ID:          "node",
		Name:        "Node.js",
		Desc:        "Needed by Node-based apps",
		PointHint:   "Point to the node binary or its bin folder.",
		probe:       probeVersion("node", "node", "--version"),
		defaultRoot: toolsRoot(),
		install:     installBrew("node", "Node.js"),
	},
	{
		ID:          "nginx",
		Name:        "nginx",
		Desc:        "Reverse proxy for web apps that need one",
		PointHint:   "Point to the nginx binary or its sbin/bin folder (Homebrew: /opt/homebrew/sbin/nginx).",
		probe:       probeVersion("nginx", "nginx", "-v"),
		defaultRoot: toolsRoot(),
		install:     installBrew("nginx", "nginx"),
	},
}

// probeVersion builds a probe that finds the tool (registered path first,
// then PATH) and runs its version command.
func probeVersion(tool, bin string, args ...string) func() (string, string, bool) {
	return func() (string, string, bool) {
		path := registeredToolPath(tool)
		if path == "" {
			p, err := exec.LookPath(bin)
			if err != nil {
				return "", "", false
			}
			path = p
		}
		out, err := exec.Command(path, args...).CombinedOutput()
		if err != nil && strings.TrimSpace(string(out)) == "" {
			return path, "", true // exists but would not report a version
		}
		version := strings.TrimSpace(string(out))
		if i := strings.Index(version, "\n"); i >= 0 {
			version = version[:i]
		}
		return path, version, true
	}
}

// servePresets answers GET /api/presets: every tool's status plus which
// installed apps declare it in their "requires" field.
func (s *Server) servePresets(w http.ResponseWriter, r *http.Request) {
	refreshCustomToolPaths(s.cfg.DataDir, true)
	type toolStatus struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Desc        string   `json:"desc"`
		PointHint   string   `json:"pointHint"`
		Installed   bool     `json:"installed"`
		Version     string   `json:"version,omitempty"`
		Path        string   `json:"path,omitempty"`
		Custom      bool     `json:"custom,omitempty"`
		DefaultDir  string   `json:"defaultDir,omitempty"`
		Installable bool     `json:"installable"`
		InstallNote string   `json:"installNote,omitempty"`
		Removable   bool     `json:"removable"`
		RequiredBy  []string `json:"requiredBy,omitempty"`
	}
	out := make([]toolStatus, 0, len(toolPresets))
	for _, t := range toolPresets {
		st := toolStatus{
			ID:          t.ID,
			Name:        t.Name,
			Desc:        t.Desc,
			PointHint:   t.PointHint,
			DefaultDir:  t.defaultRoot,
			Installable: t.install != nil,
			InstallNote: t.installNote,
		}
		if t.probe != nil {
			if path, version, ok := t.probe(); ok {
				st.Installed = true
				st.Path = path
				st.Version = version
				st.Custom = registeredToolPath(t.ID) != ""
			}
		}
		if reg := registeredToolPath(t.ID); reg != "" {
			if dir, ok := launcherInstalledDir(t.ID, reg); ok {
				st.Removable = true
				_ = dir
			}
		}
		for _, a := range s.appsList() {
			for _, want := range a.Requires {
				if want == t.ID {
					st.RequiredBy = append(st.RequiredBy, a.ID)
				}
			}
		}
		out = append(out, st)
	}
	s.writeJSON(w, http.StatusOK, out)
}

// servePresetRemove answers POST /api/presets/remove {tool}: deletes a
// toolchain the launcher itself installed (…/go, …/cargo plus its …/rustup)
// and clears the registration. Tools installed outside the launcher's tools
// root are refused — their own uninstaller owns them.
func (s *Server) servePresetRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool string `json:"tool"`
	}
	readBodyInto(r, &req)
	reg := registeredToolPath(req.Tool)
	if reg == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not registered with the launcher — nothing to remove"})
		return
	}
	dir, ok := launcherInstalledDir(req.Tool, reg)
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this copy lives outside the launcher's tools folder — remove it with its own uninstaller, or just clear the registered path"})
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("removing %s: %v", dir, err)})
		return
	}
	if req.Tool == "rust" {
		// rustup-init installs two sibling trees; both belong to us.
		_ = os.RemoveAll(filepath.Join(filepath.Dir(dir), "rustup"))
	}
	_ = setCustomToolPath(s.cfg.DataDir, req.Tool, "")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"note": fmt.Sprintf("removed %s and cleared its registered path", dir),
	})
}

// launcherInstalledDir reports the install root the launcher owns for a
// registered binary: …/go[/bin]/go → …/go, …/cargo[/bin]/cargo → …/cargo —
// and only under the launcher's tools root, so a hand-built or distro
// toolchain whose folder merely happens to share the name never matches.
func launcherInstalledDir(tool, bin string) (string, bool) {
	installDir := map[string]string{"go": "go", "rust": "cargo"}[tool]
	if installDir == "" {
		return "", false
	}
	d := filepath.Dir(bin)
	if base := filepath.Base(d); base == "bin" || base == "sbin" {
		d = filepath.Dir(d)
	}
	if filepath.Base(d) != installDir {
		return "", false
	}
	root := toolsRoot()
	if !strings.HasPrefix(d, root+string(filepath.Separator)) {
		return "", false
	}
	return d, true
}

// servePickPath answers POST /api/pick-path {tool, kind}: it opens the OS's
// native folder picker on the launcher's machine and returns the chosen
// path. The dashboard runs in a browser or embedded web view, which never
// hands out real filesystem paths — the launcher process, which lives on
// the same machine, has to ask the OS for both of us.
func (s *Server) servePickPath(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool string `json:"tool"`
		Kind string `json:"kind"` // "register" | "install"
	}
	readBodyInto(r, &req)
	prompt := "Choose a folder"
	switch req.Kind {
	case "install":
		prompt = "Choose where to install"
		if t := toolByID(req.Tool); t != nil {
			prompt = fmt.Sprintf("Choose where to install %s (a %s/ folder is created inside)", t.Name, installFolderName(req.Tool))
		}
	default:
		if t := toolByID(req.Tool); t != nil && t.PointHint != "" {
			prompt = t.PointHint
		}
	}
	path, err := nativeFolderPicker(prompt)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path})
}

// installFolderName is the folder an auto-install creates for a tool.
func installFolderName(tool string) string {
	if tool == "rust" {
		return "cargo"
	}
	return tool
}

// nativeFolderPicker blocks on the OS folder dialog (the user can take as
// long as they like) and returns the chosen POSIX path.
func nativeFolderPicker(prompt string) (string, error) {
	prompt = strings.NewReplacer(`\`, ``, `"`, "'").Replace(prompt)
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`POSIX path of (choose folder with prompt "%s")`, prompt)
		out, err := runWithTimeout(exec.Command("osascript", "-e", script), pickTimeout)
		if err != nil {
			if strings.Contains(strings.ToLower(out), "user canceled") {
				return "", fmt.Errorf("cancelled")
			}
			return "", fmt.Errorf("folder picker failed: %s", strings.TrimSpace(out))
		}
		return strings.TrimSpace(out), nil
	case "windows":
		script := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms; `+
			`$f = New-Object System.Windows.Forms.FolderBrowserDialog; `+
			`$f.Description = "%s"; if ($f.ShowDialog() -eq 'OK') { $f.SelectedPath }`, prompt)
		out, err := runWithTimeout(exec.Command("powershell", "-STA", "-NoProfile", "-NonInteractive", "-Command", script), pickTimeout)
		if err != nil {
			return "", fmt.Errorf("folder picker failed: %s", strings.TrimSpace(out))
		}
		path := strings.TrimSpace(out)
		if path == "" {
			return "", fmt.Errorf("cancelled")
		}
		return path, nil
	}
	for _, picker := range []struct {
		bin  string
		args []string
	}{
		{"zenity", []string{"--file-selection", "--directory", "--title", prompt}},
		{"kdialog", []string{"--getexistingdirectory", "~", "--title", prompt}},
	} {
		if _, err := exec.LookPath(picker.bin); err == nil {
			out, err := runWithTimeout(exec.Command(picker.bin, picker.args...), pickTimeout)
			if err != nil {
				if strings.TrimSpace(out) == "" {
					return "", fmt.Errorf("cancelled")
				}
				return "", fmt.Errorf("folder picker failed: %s", strings.TrimSpace(out))
			}
			return strings.TrimSpace(out), nil
		}
	}
	return "", fmt.Errorf("no folder picker available — install zenity, or type the path in your shell")
}

// servePresetInstall answers POST /api/presets/install {tool, dir?}: runs
// the tool's installer into the default location or the given directory.
func (s *Server) servePresetInstall(w http.ResponseWriter, r *http.Request) {
	refreshCustomToolPaths(s.cfg.DataDir, true)
	var req struct {
		Tool string `json:"tool"`
		Dir  string `json:"dir"`
	}
	readBodyInto(r, &req)
	for _, t := range toolPresets {
		if t.ID != req.Tool {
			continue
		}
		if t.install == nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": t.Name + " cannot be installed automatically — " + t.installNote})
			return
		}
		if err := t.install(strings.TrimSpace(req.Dir)); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		// Auto-installed toolchains live outside PATH (that is the point of
		// the default location) — register them so probes and the Go/Rust
		// build paths find them without a shell profile change.
		if bin := installedToolBinary(t.ID, strings.TrimSpace(req.Dir)); bin != "" {
			_ = setCustomToolPath(s.cfg.DataDir, t.ID, bin)
		}
		note := t.Name + " installed"
		if path, version, ok := t.probe(); ok {
			note = fmt.Sprintf("%s installed — %s at %s", t.Name, version, path)
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note})
		return
	}
	s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unknown tool %q", req.Tool)})
}

// installedToolBinary computes where an auto-install put the tool's binary
// ("" when the installer manages its own paths, like Homebrew).
func installedToolBinary(tool, dir string) string {
	if dir == "" {
		dir = toolsRoot()
	}
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	switch tool {
	case "go":
		return filepath.Join(dir, "go", "bin", "go"+exe)
	case "rust":
		return filepath.Join(dir, "cargo", "bin", "cargo"+exe)
	}
	return ""
}

// servePresetPath answers POST /api/presets/path {tool, path}: registers (or
// clears, with an empty path) where a tool's binary lives.
func (s *Server) servePresetPath(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool string `json:"tool"`
		Path string `json:"path"`
	}
	readBodyInto(r, &req)
	if err := setCustomToolPath(s.cfg.DataDir, req.Tool, req.Path); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	note := "path registered — the launcher will use it first"
	if strings.TrimSpace(req.Path) == "" {
		note = "registered path removed"
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note})
}

// installGo unpacks the official Go tarball into <dir>/go (or ~/.appdeck-tools/go).
func installGo(dir string) error {
	if dir == "" {
		dir = toolsRoot()
	}
	version, err := firstLine("https://go.dev/VERSION?m=text")
	if err != nil || !strings.HasPrefix(version, "go") {
		return fmt.Errorf("cannot determine the current Go version: %v", err)
	}
	// Go's official archives name the 32-bit linux build "armv6l", not
	// "arm" — every other GOARCH spells the same as runtime.GOARCH.
	arch := runtime.GOARCH
	if runtime.GOOS == "linux" && arch == "arm" {
		arch = "armv6l"
	}
	archiveURL := fmt.Sprintf("https://go.dev/dl/%s.%s-%s.%s", version, runtime.GOOS, arch, map[string]string{
		"darwin": "tar.gz", "linux": "tar.gz", "windows": "zip",
	}[runtime.GOOS])
	archive, err := downloadToTemp(archiveURL)
	if err != nil {
		return fmt.Errorf("downloading Go: %v", err)
	}
	defer os.Remove(archive)
	// The tarball expands to a "go/" folder — drop it into the target dir.
	if err := extractArchiveInto(archive, dir); err != nil {
		return err
	}
	bin := filepath.Join(dir, "go", "bin", "go")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if !fileExists(bin) {
		return fmt.Errorf("the Go archive did not produce %s", bin)
	}
	return nil
}

// installRust runs rustup-init non-interactively with CARGO_HOME/RUSTUP_HOME
// under the target dir, so cargo lands in <dir>/cargo/bin.
func installRust(dir string) error {
	if dir == "" {
		dir = toolsRoot()
	}
	triple := map[string]string{
		"darwin/arm64": "aarch64-apple-darwin",
		"darwin/amd64": "x86_64-apple-darwin",
		"linux/amd64":  "x86_64-unknown-linux-gnu",
		"linux/arm64":  "aarch64-unknown-linux-gnu",
	}[runtime.GOOS+"/"+runtime.GOARCH]
	exe := ""
	if runtime.GOOS == "windows" {
		triple = "x86_64-pc-windows-msvc"
		exe = ".exe"
	}
	if triple == "" {
		return fmt.Errorf("no rustup build for %s/%s — install Rust from https://rustup.rs", runtime.GOOS, runtime.GOARCH)
	}
	init, err := downloadToTemp("https://static.rust-lang.org/rustup/dist/" + triple + "/rustup-init" + exe)
	if err != nil {
		return fmt.Errorf("downloading rustup-init: %v", err)
	}
	defer os.Remove(init)
	stagedDir, stagedInit, err := stageRustupInit(init, exe)
	if err != nil {
		return err
	}
	defer os.RemoveAll(stagedDir)
	init = stagedInit
	if err := os.Chmod(init, 0o755); err != nil {
		return err
	}
	cmd := exec.Command(init, "-y", "--profile", "minimal", "--default-toolchain", "stable", "--no-modify-path")
	hideWindow(cmd)
	cmd.Env = append(os.Environ(),
		"CARGO_HOME="+filepath.Join(dir, "cargo"),
		"RUSTUP_HOME="+filepath.Join(dir, "rustup"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rustup-init failed: %s", lastLines(strings.TrimSpace(string(out)), 8))
	}
	return nil
}

// stageRustupInit gives the downloaded installer the canonical executable
// name rustup uses to distinguish its installer from installed proxies.
func stageRustupInit(downloaded, exe string) (string, string, error) {
	dir, err := os.MkdirTemp("", "appdeck-rustup-")
	if err != nil {
		return "", "", fmt.Errorf("staging rustup-init: %w", err)
	}
	init := filepath.Join(dir, "rustup-init"+exe)
	if err := os.Rename(downloaded, init); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("staging rustup-init: %w", err)
	}
	return dir, init, nil
}

// installBrew installs via Homebrew where it exists (macOS primarily); the
// launcher never installs brew itself.
func installBrew(formula, name string) func(dir string) error {
	return func(dir string) error {
		if dir != "" {
			return fmt.Errorf("%s installs through Homebrew, which manages its own paths — leave the path empty, or register an existing install below", name)
		}
		brew, err := exec.LookPath("brew")
		if err != nil {
			for _, c := range []string{"/opt/homebrew/bin/brew", "/usr/local/bin/brew"} {
				if st, err := os.Stat(c); err == nil && !st.IsDir() {
					brew = c
					break
				}
			}
		}
		if brew == "" {
			return fmt.Errorf("Homebrew not found — install it from https://brew.sh, or register an existing %s path below", name)
		}
		cmd := exec.Command(brew, "install", formula)
		hideWindow(cmd)
		if out, err := runWithTimeout(cmd, 15*time.Minute); err != nil {
			return fmt.Errorf("brew install %s failed: %s", formula, lastLines(strings.TrimSpace(out), 8))
		}
		return nil
	}
}

// runWithTimeout runs cmd and returns its combined output.
func runWithTimeout(cmd *exec.Cmd, timeout time.Duration) (string, error) {
	done := make(chan error, 1)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return buf.String(), err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.String(), err
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return buf.String(), fmt.Errorf("timed out after %s", timeout)
	}
}

// firstLine fetches a small text URL and returns its first line.
func firstLine(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(data))
	if i := strings.Index(line, "\n"); i >= 0 {
		line = line[:i]
	}
	return line, nil
}

// downloadToTemp fetches url into a temp file and returns its path.
func downloadToTemp(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", "appdeck-dl-")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := tmp.ReadFrom(resp.Body); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// extractArchiveInto unpacks a zip/tar.gz archive (whole tree) into dir.
func extractArchiveInto(archive, dir string) error {
	data, err := os.ReadFile(archive)
	if err != nil {
		return err
	}
	format := detectArchiveFormat(data)
	if format != "zip" && format != "tar" && format != "tar.gz" {
		return fmt.Errorf("unsupported tool archive format %q", format)
	}
	opener, err := archiveOpener(data)
	if err != nil {
		return err
	}
	entries, _, err := scanArchive(opener)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return extractArchiveTree(opener, "", dir, entries)
}
