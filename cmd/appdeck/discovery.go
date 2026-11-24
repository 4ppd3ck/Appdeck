// App discovery: manifests, folder scanning and state persistence.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

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
		// Same lift for the tray quick action (App.TrayToggle): a central
		// registry entry predates the field while the folder's own app.json
		// declares it — the manifest is the author's contract for app-side
		// surfaces, exactly like the desktop flag above.
		if a.TrayToggle == nil {
			a.TrayToggle = manifestTrayToggle(dir)
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
