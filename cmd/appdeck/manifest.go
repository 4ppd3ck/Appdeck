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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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

// binaryChecksum computes the SHA-256 of the manifest's binary (hex, "" when
// no file matches — source-only imports build on first start and get their
// checksum on the next manifest write). Candidate order mirrors the
// launcher's resolution: the binary field's path relative to the app folder,
// its basename beside app.json, and the Windows .exe spelling.
func binaryChecksum(appDir, binaryField string) string {
	if binaryField == "" {
		return ""
	}
	rel := filepath.FromSlash(binaryField)
	candidates := []string{
		filepath.Join(appDir, rel),
	}
	if base := filepath.Base(rel); base != rel {
		candidates = append(candidates, filepath.Join(appDir, base))
	}
	if runtime.GOOS == "windows" {
		for _, c := range append([]string(nil), candidates...) {
			if !strings.HasSuffix(strings.ToLower(c), ".exe") {
				candidates = append(candidates, c+".exe")
			}
		}
	}
	for _, path := range candidates {
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			continue
		}
		f.Close()
		return hex.EncodeToString(h.Sum(nil))
	}
	return ""
}

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
	// Unknown privilege names are dropped, not fatal: a manifest written for
	// a newer launcher must still load here (the grant just does nothing).
	if len(a.Privileges) > 0 {
		var kept []string
		for _, p := range a.Privileges {
			if validPrivileges[p] {
				kept = append(kept, p)
			}
		}
		a.Privileges = kept
	}
	// Same forward-compat rule for declared permissions (permissions.go):
	// unknown names drop, known ones gate the first start and feed the
	// sandbox profiles.
	if len(a.Permissions) > 0 {
		var kept []string
		for _, p := range a.Permissions {
			p = strings.TrimSpace(p)
			if validPermissions[p] {
				kept = append(kept, p)
			}
		}
		a.Permissions = kept
	}
	// Unknown privacy classes fall back to the default guide, not fatal —
	// the same forward-compat rule as privileges above.
	if !validPrivacy[a.Privacy] {
		a.Privacy = ""
	}
	// Signature states this dashboard knows how to badge; a manifest
	// written by a newer launcher falls back to "" (unchecked), never a
	// broken badge.
	switch a.Signature {
	case "vendor", "adhoc", "invalid":
	default:
		a.Signature = ""
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
	// Health is a path appended to the loopback URL — same rules as urlPath,
	// plus no whitespace (it is interpolated into a request target verbatim).
	a.Health = strings.TrimSpace(a.Health)
	if a.Health != "" && (!strings.HasPrefix(a.Health, "/") || strings.ContainsAny(a.Health, " \t\r\n")) {
		return nil, fmt.Errorf("health %q must be a URL path starting with / (no spaces)", a.Health)
	}
	// TrayToggle (the manifest's tray quick action): both paths are
	// app-relative URL paths with the same shape rules as health, and it
	// needs an HTTP surface to flip — a desktop app (portless) or a
	// hosted-static app (no process at all) cannot honor it.
	if tt := a.TrayToggle; tt != nil {
		tt.Label = sanitizeIconText(tt.Label) // plain text, bounded
		tt.Status = strings.TrimSpace(tt.Status)
		tt.Action = strings.TrimSpace(tt.Action)
		if tt.Status == "" {
			return nil, fmt.Errorf("trayToggle: %q is required — a URL path answering {\"paused\": bool}", "status")
		}
		for name, p := range map[string]string{"status": tt.Status, "action": tt.Action} {
			if p != "" && (!strings.HasPrefix(p, "/") || strings.ContainsAny(p, " \t\r\n")) {
				return nil, fmt.Errorf("trayToggle: %s %q must be a URL path starting with / (no spaces)", name, p)
			}
		}
		if a.Desktop || a.ServedBy != "" {
			return nil, fmt.Errorf("trayToggle needs a web app with an HTTP surface (not a desktop or hosted-static app)")
		}
	}
	// StartWait (seconds) overrides the deck tools "app start timeout" for
	// this app — how long a start waits for the web UI to answer HTTP. 0
	// (absent) keeps the deck-wide setting.
	if a.StartWait < 0 || a.StartWait > startWaitMax {
		return nil, fmt.Errorf("startWait %d invalid — use 0 (deck default) or %d–%d seconds", a.StartWait, startWaitMin, startWaitMax)
	}
	a.Origin = strings.TrimSpace(a.Origin)
	// Lifecycle policy: a named mode, an idle budget in range, and — for the
	// scheduled mode — a schedule string the enforcement loop can actually
	// parse. Bad values are authoring errors: named, load-time, fatal.
	switch a.Lifecycle {
	case "", "manual", "always", "onDemand", "scheduled":
	default:
		return nil, fmt.Errorf("lifecycle %q is not one of manual, always, onDemand, scheduled", a.Lifecycle)
	}
	if a.StopAfterIdle < 0 || a.StopAfterIdle > 60*24*7 {
		return nil, fmt.Errorf("stopAfterIdle %d invalid — use 0 (default) or 1–10080 minutes", a.StopAfterIdle)
	}
	if a.Schedule != "" {
		if _, err := parseSchedule(a.Schedule); err != nil {
			return nil, err
		}
	}
	if a.Lifecycle == "scheduled" && a.Schedule == "" {
		return nil, fmt.Errorf("lifecycle \"scheduled\" needs a schedule field (e.g. \"Mon-Fri 09:00-17:00\")")
	}
	if a.KeepAliveLimit < 0 || a.KeepAliveLimit > 100 {
		return nil, fmt.Errorf("keepAliveLimit %d invalid — use 0 (default 5) or 1–100", a.KeepAliveLimit)
	}
	if a.KeepAliveDelay < 0 || a.KeepAliveDelay > 3600 {
		return nil, fmt.Errorf("keepAliveDelay %d invalid — use 0 (default 10 s) or 1–3600 seconds", a.KeepAliveDelay)
	}
	// Metadata the dashboard interpolates into its own HTML: a manifest is
	// untrusted input, so an icon is kept to plain glyphs and an accent to a
	// legal CSS colour. Both are also escaped at render time — belt and
	// braces, because a stale dashboard may still be open.
	a.Icon = sanitizeIconText(a.Icon)
	if a.Icon == "" {
		a.Icon = "📦"
	}
	a.Accent = safeAccent(a.Accent)
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
		if cmd != "" && binary != "" {
			return nil, fmt.Errorf("set binary or cmd, not both")
		}
		if binary == "" && cmd == "" {
			return nil, fmt.Errorf("desktop apps need a binary, or a cmd that launches one")
		}
		if binary == "" {
			// Desktop via cmd (a generated Python venv launch, a wrapper
			// script): nothing to normalize — the shell line is the app.
			break
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
		// ${VAR} is shell parameter expansion, not a launcher placeholder —
		// cmds legitimately carry ${APPDECK_PYTHON:-python3} and friends.
		// Only bare {port}/{host} braces are the contract.
		if i > 0 && s[i-1] == '$' {
			if k := strings.Index(s[i+1:], "{"); k >= 0 {
				s = s[i+1+k:]
				continue
			}
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
		// Match configuredDirs against both folder spellings. The registry
		// loop resolves symlinks when claiming (EvalSymlinks form), a
		// symlinked data directory (macOS /var → /private/var, a
		// LAUNCHER_DATA_DIR alias) makes that differ from the raw Abs form —
		// one spelling in the map and the other in the lookup misses, and
		// the app's own home gets flagged as a duplicate.
		dir := item.dir
		if absolute, err := filepath.Abs(dir); err == nil {
			dir = absolute
		}
		resolved := dir
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			resolved = r
		}
		// Keys are case-folded: APFS/HFS+/NTFS are case-insensitive, and the
		// registry's RawDir casing ("Apps/CargoScope") can differ from the
		// on-disk folder ("apps/cargoscope") from older installs — a literal
		// map lookup then misses and the folder is re-discovered as a
		// duplicate of its own registry app.
		if configuredDirs[foldPath(dir)] || configuredDirs[foldPath(resolved)] {
			continue // central registry (apps.json) owns this folder
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

// manifestDesktopFlag reports whether a folder's own app.json marks the app
// "desktop": true — a one-field peek that never fails its caller (missing
// file, broken JSON → false). loadApps uses it to lift the structural launch
// mode onto a central registry entry that omits the flag.
func manifestDesktopFlag(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, manifestFileName))
	if err != nil {
		return false
	}
	var probe struct {
		Desktop bool `json:"desktop"`
	}
	return json.Unmarshal(data, &probe) == nil && probe.Desktop
}

// manifestServedByLauncher reports whether the folder's own app.json hosts
// the app in-process (servedBy "launcher" — the static-app contract).
func manifestServedByLauncher(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, manifestFileName))
	if err != nil {
		return false
	}
	var probe struct {
		ServedBy string `json:"servedBy"`
	}
	return json.Unmarshal(data, &probe) == nil && probe.ServedBy == "launcher"
}

// manifestTrayToggle reads the folder's own app.json tray quick action —
// the lift loadApps applies to central registry entries that predate the
// field (a central entry wins the merge, so its app.json is otherwise
// never parsed). nil when absent or shapeless.
func manifestTrayToggle(dir string) *TrayToggleSpec {
	data, err := os.ReadFile(filepath.Join(dir, manifestFileName))
	if err != nil {
		return nil
	}
	var probe struct {
		TrayToggle *TrayToggleSpec `json:"trayToggle"`
	}
	if json.Unmarshal(data, &probe) != nil || probe.TrayToggle == nil ||
		strings.TrimSpace(probe.TrayToggle.Status) == "" {
		return nil
	}
	return probe.TrayToggle
}

// stripPortArgs drops {port} arguments: a desktop app launches with no port
// machinery, and a leftover placeholder would substitute to a bogus "0".
func stripPortArgs(args []string) []string {
	kept := args[:0]
	for _, arg := range args {
		if strings.Contains(arg, "{port}") {
			continue
		}
		kept = append(kept, arg)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
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

// sanitizeIconText reduces a manifest icon to what a card can render as text:
// no markup characters, no control characters, at most 16 runes (enough for
// the longest ZWJ emoji sequences). Anything else and the default glyph is
// used — a card icon is never a place to smuggle HTML.
func sanitizeIconText(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', '&', '"', '\'', '`':
			return -1
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	cleaned = strings.TrimSpace(cleaned)
	if r := []rune(cleaned); len(r) > 16 {
		cleaned = string(r[:16])
	}
	return cleaned
}

// accentOK accepts the CSS colour forms the dashboard's --app-accent variable
// can carry: hex, rgb()/rgba()/hsl()/hsla() with plain numeric arguments, or a
// colour keyword. Quotes, semicolons, braces and url() are rejected, so the
// value cannot leave a style attribute.
var accentOK = regexp.MustCompile(`^(#[0-9a-fA-F]{3,8}|(rgb|hsl)a?\([0-9.,%/ ]+\)|[a-zA-Z]{3,20})$`)

// safeAccent returns the manifest accent when it is a plain CSS colour and ""
// otherwise (the card then falls back to the dominant colour of its icon).
func safeAccent(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 40 || !accentOK.MatchString(s) {
		return ""
	}
	return s
}
