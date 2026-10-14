// Per-app manifests (app.json) — the contract third-party developers target.
//
// An app folder becomes an AppDeck app by carrying an app.json next to its
// entrypoint (index.html for hosted static apps, a Go module + binary for
// process apps). Discovery scans the known app containers for that file,
// validates it with named, human errors, and merges it with the central
// apps.json registry — a central entry whose dir matches the manifest folder
// wins (it is the author's own metadata), so adding a manifest never breaks
// an existing configured app.
//
// Validation failures never take the launcher down: a broken third-party app
// is skipped and surfaces as a warning on /api/health and in the dashboard.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// manifestFileName is the per-app manifest file developers put in their
// app folder. See AUTHORING.md for the full field reference.
const manifestFileName = "app.json"

// appDeckAPIVersion is the manifest contract this launcher implements.
// A manifest declaring a different apiVersion is rejected with a named
// error instead of being guessed at.
const appDeckAPIVersion = 1

// loadAppManifest parses and validates <dir>/app.json. The returned App has
// ID/Name/… filled from the manifest; the caller supplies RawDir/Dir (the
// install location) afterwards. binary is interpreted relative to the app
// folder and rewritten Root-relative (the same form the central registry
// uses), so the launcher's build/resolve pipeline needs no special cases.
func loadAppManifest(dir string) (*App, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestFileName))
	if err != nil {
		return nil, err
	}
	var a App
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", manifestFileName, err)
	}
	if a.APIVersion == 0 {
		a.APIVersion = appDeckAPIVersion // absent = current contract
	}
	if a.APIVersion != appDeckAPIVersion {
		return nil, fmt.Errorf("apiVersion %d not supported — this launcher speaks version %d", a.APIVersion, appDeckAPIVersion)
	}
	a.ID = strings.TrimSpace(a.ID)
	if a.ID == "" {
		return nil, fmt.Errorf("%q is required", "id")
	}
	if !validAppID(a.ID) {
		return nil, fmt.Errorf("id %q must be lowercase letters, digits and dashes (start with a letter or digit)", a.ID)
	}
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" {
		return nil, fmt.Errorf("%q is required", "name")
	}
	if a.Port < 0 || a.Port > 65535 || (a.Port != 0 && a.Port < 1024) {
		return nil, fmt.Errorf("port %d invalid — use 0 (auto-assign) or 1024–65535", a.Port)
	}
	if a.Desktop && a.Port != 0 {
		return nil, fmt.Errorf("desktop apps are portless — drop the port field")
	}
	if a.URLPath == "" {
		a.URLPath = "/"
	}
	if !strings.HasPrefix(a.URLPath, "/") {
		return nil, fmt.Errorf("urlPath %q must start with /", a.URLPath)
	}
	if a.Icon == "" {
		a.Icon = "📦"
	}
	if a.Category == "" {
		a.Category = "Tools"
	}

	// Binary apps: the manifest's binary is relative to the app folder and
	// must stay inside it — a manifest that reaches outside its own folder
	// (absolute path or ..) is a zip-install attack vector, not an app.
	binary := strings.TrimSpace(a.Binary)
	cmd := strings.TrimSpace(a.Cmd)
	switch {
	case a.ServedBy == "launcher":
		if binary != "" || cmd != "" {
			return nil, fmt.Errorf("servedBy %q apps are hosted in-process — drop binary/cmd", "launcher")
		}
		if !hasRootHTML(dir) {
			return nil, fmt.Errorf("servedBy %q needs at least one .html file in the app folder", "launcher")
		}
	case a.Desktop, binary != "":
		if cmd != "" {
			return nil, fmt.Errorf("set binary or cmd, not both")
		}
		if binary == "" {
			return nil, fmt.Errorf("desktop apps need a binary")
		}
		rel, err := manifestRelPath(binary)
		if err != nil {
			return nil, fmt.Errorf("binary: %v", err)
		}
		// Keep the validated folder-relative form here; discoverManifestApps
		// rewrites it Root-relative (RawDir + binary) once the folder identity
		// is known, so the launcher's build/resolve pipeline sees the same
		// shape the central registry uses ("../Apps/<name>/<bin>").
		a.Binary = filepath.ToSlash(rel)
		a.Cmd = ""
	default:
		if cmd == "" {
			return nil, fmt.Errorf("need one of binary, cmd, or servedBy %q", "launcher")
		}
	}
	for _, arg := range a.Args {
		if bad := unknownPlaceholder(arg); bad != "" {
			return nil, fmt.Errorf("args: unknown placeholder %s (supported: {port}, {host})", bad)
		}
	}
	if bad := unknownPlaceholder(cmd); bad != "" {
		return nil, fmt.Errorf("cmd: unknown placeholder %s (supported: {port}, {host})", bad)
	}
	for k, v := range a.Env {
		if bad := unknownPlaceholder(v); bad != "" {
			return nil, fmt.Errorf("env[%q]: unknown placeholder %s (supported: {port}, {host})", k, bad)
		}
	}
	return &a, nil
}

// manifestRelPath validates a manifest-relative file path: relative, no
// traversal, non-empty. Returned cleaned and slash-normalized. Leading
// separators are rejected on every platform (on Windows "/x" would resolve
// against the drive root, and a contract that means different things per-OS
// is no contract).
func manifestRelPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("is empty")
	}
	slash := strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(slash, "/") || filepath.IsAbs(p) ||
		(len(p) > 1 && p[1] == ':') {
		return "", fmt.Errorf("%q must be relative to the app folder", p)
	}
	clean := filepath.Clean(filepath.FromSlash(slash))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q must stay inside the app folder", p)
	}
	return clean, nil
}

// validAppID matches ^[a-z0-9][a-z0-9-]{0,62}$ — stable ids sort, slug and
// survive as folder names, registry keys and log file names on every OS.
func validAppID(id string) bool {
	if len(id) == 0 || len(id) > 63 {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0:
		default:
			return false
		}
	}
	return id[0] >= 'a' && id[0] <= 'z' || id[0] >= '0' && id[0] <= '9'
}

// unknownPlaceholder reports the first {…} token in s that is not a known
// substitution ("" when none).
func unknownPlaceholder(s string) string {
	for {
		i := strings.Index(s, "{")
		if i < 0 {
			return ""
		}
		j := strings.Index(s[i:], "}")
		if j < 0 {
			return ""
		}
		tok := s[i : i+j+1]
		switch tok {
		case "{port}", "{host}":
		default:
			return tok
		}
		s = s[i+j+1:]
	}
}

// manifestContainers lists the folder names a manifest scan descends into
// (besides scanning the base itself), mirroring the heuristic discovery's
// "Apps" group folder so both flows see the same layout.
var manifestContainers = []string{"Apps"}

// discoverManifestApps scans each base directory (one level: base/<app>,
// plus base/<container>/<app> for the Apps group folder) for folders carrying
// an app.json, and returns validated apps not already present in byID or
// configured by the central registry (configuredDirs holds resolved absolute
// folder paths). rawDirPrefix is how a discovered folder is written relative
// to the launcher root ("../" beside the launcher, "Apps/" inside a data
// directory). Problems are appended to warnings — one bad third-party
// folder must never keep the rest of the deck from loading.
func discoverManifestApps(bases []string, rawDirPrefix string, configuredDirs map[string]bool, byID map[string]*App, warnings *[]string) []*App {
	type candidate struct{ dir, prefix string }
	var items []candidate
	for _, base := range bases {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue // no such container — nothing installed there yet
		}
		for _, e := range entries {
			if !e.IsDir() || skipAppFolder(e.Name()) {
				continue
			}
			dir := filepath.Join(base, e.Name())
			if _, err := os.Stat(filepath.Join(dir, manifestFileName)); err == nil {
				items = append(items, candidate{dir, ""})
				continue
			}
			// One group level: base/<Apps>/<app>/app.json (not recursive).
			if !isManifestContainer(e.Name()) {
				continue
			}
			subs, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, sub := range subs {
				if !sub.IsDir() || skipAppFolder(sub.Name()) {
					continue
				}
				subDir := filepath.Join(dir, sub.Name())
				if _, err := os.Stat(filepath.Join(subDir, manifestFileName)); err == nil {
					items = append(items, candidate{subDir, e.Name() + "/"})
				}
			}
		}
	}

	var found []*App
	for _, item := range items {
		if absolute, err := filepath.Abs(item.dir); err == nil {
			// Case-folded: APFS/HFS+/NTFS are case-insensitive, and the
			// registry's RawDir casing ("Apps/CargoScope") can differ from the
			// on-disk folder ("apps/cargoscope") from older installs — a
			// literal map lookup then misses and the folder is re-discovered
			// as a duplicate of its own registry app.
			if configuredDirs[foldPath(absolute)] {
				continue // central registry (apps.json) owns this folder
			}
		}
		app, err := loadAppManifest(item.dir)
		if err != nil {
			*warnings = append(*warnings,
				fmt.Sprintf("skipping %s — invalid %s: %v", item.dir, manifestFileName, err))
			continue
		}
		if app.Hidden {
			continue // app.json marks it hidden — folder stays, deck skips it
		}
		if _, dup := byID[app.ID]; dup {
			*warnings = append(*warnings,
				fmt.Sprintf("skipping %s — app id %q is already used by another app", item.dir, app.ID))
			continue
		}
		// Normalize the folder identity into the registry's RawDir form
		// ("../Apps/<name>" folder mode, "Apps/<name>" data-dir mode) so dir
		// resolution, binary resolution and packaging all behave exactly like
		// centrally configured apps. The binary follows it: the launcher's
		// resolver expects Root-relative paths, and in single-file builds only
		// the binary's base name matters (binaries unpack into <data>/bin).
		app.RawDir = rawDirPrefix + item.prefix + filepath.Base(item.dir)
		if app.Binary != "" {
			app.Binary = app.RawDir + "/" + app.Binary
		}
		app.Dir = item.dir
		if abs, err := filepath.Abs(item.dir); err == nil {
			app.Dir = abs
		}
		if resolved, err := filepath.EvalSymlinks(item.dir); err == nil {
			app.Dir = resolved
		}
		app.Source = "manifest"
		byID[app.ID] = app
		found = append(found, app)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].ID < found[j].ID })
	return found
}

func isManifestContainer(name string) bool {
	for _, c := range manifestContainers {
		if strings.EqualFold(name, c) {
			return true
		}
	}
	return false
}

// skipAppFolder names that are never app folders (infrastructure, VCS,
// dependency trees), shared by the manifest and heuristic scans.
func skipAppFolder(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
		strings.EqualFold(name, "node_modules") || strings.EqualFold(name, "release") ||
		strings.EqualFold(name, "Launcher")
}

// hasRootHTML reports whether the folder carries at least one .html entry
// page (what servedBy-launcher apps serve).
func hasRootHTML(dir string) bool {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.html"))
	return len(matches) > 0
}
