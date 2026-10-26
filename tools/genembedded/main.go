// genembedded — scans the project root and generates everything the release
// and dev builds need, so adding or removing an app folder is the ONLY step:
//
//  1. apps-embedded.json  the packaged app registry (generated, never edited)
//  2. a TSV build manifest for release.sh / build.sh:
//     kind \t id \t folder \t entrypoint \t binary-name \t has-assets \t desktop
//
// Classification per folder (the launcher's own checkout and dot/underscore
// folders are infrastructure and skipped — the checkout is recognised by
// path, not by name, so it works under any folder name):
//
//   - has go.mod            → Go app. Entrypoint: root package main, else the
//     single cmd/<name>/main.go. If a root .go file uses //go:embed the app
//     is self-contained (binary only); otherwise its web assets (html at the
//     root or asset dirs like templates/static/web/css/js/data) are packaged
//     beside the binary.
//   - has .html at the root → static app, hosted in-process by the launcher.
//   - anything else         → not an app, ignored.
//
// Metadata (id, name, icon, port, args, …) comes from apps.json when the
// folder is listed there ("dir": "../<Folder>"); unknown folders get sensible
// defaults (slug id, next free port from 9100). apps.json entries whose
// folder no longer exists are DROPPED from the generated config with a
// printed notice — remove the entry or restore the folder.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

type appEntry struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Icon      string            `json:"icon"`
	Desc      string            `json:"desc"`
	Category  string            `json:"category"`
	Port      int               `json:"port"`
	URLPath   string            `json:"urlPath"`
	Cmd       string            `json:"cmd"`
	Binary    string            `json:"binary,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Network   bool              `json:"network"`
	Env       map[string]string `json:"env,omitempty"`
	ServedBy  string            `json:"servedBy,omitempty"`
	Desktop   bool              `json:"desktop,omitempty"`
	Dir       string            `json:"dir"`
	Autostart bool              `json:"autostart,omitempty"`
	Accent    string            `json:"accent,omitempty"`
	// Pack opts an app out of single-file builds ("pack": false): the folder
	// keeps working in folder/dev mode, but release.sh neither builds nor
	// ships it — how an author keeps a personal or license-restricted app
	// out of a distributed deck. Absent = pack it.
	Pack *bool `json:"pack,omitempty"`
}

type appsFile struct {
	Apps []*appEntry `json:"apps"`
}

// readManifest loads a folder's own app.json so manifest-authored apps that
// are not listed in the central registry still pack with their real name,
// icon, description and category instead of generic discovered defaults.
func readManifest(root, folder string) *appEntry {
	data, err := os.ReadFile(filepath.Join(root, folder, "app.json"))
	if err != nil {
		return nil
	}
	var e appEntry
	if json.Unmarshal(data, &e) != nil || strings.TrimSpace(e.ID) == "" {
		return nil
	}
	return &e
}

// mergeFolderDesktop lifts the folder's own "desktop" flag onto a registry
// entry that omits it. The central entry wins for presentation metadata
// (name, icon, desc), but "desktop" is structural — portless GUI launch, no
// {port} args, a native cgo build in release.sh — and the app.json beside
// the sources is the author's word for how the app runs. Without this merge
// a registry entry that predates the flag ships a GUI app configured as a
// web server (invented port + {port} argument) and it cannot start.
// Returns the entry unchanged when the folder has no manifest or the
// manifest does not set the flag.
func mergeFolderDesktop(cfg *appEntry, root, folder string) *appEntry {
	if cfg.Desktop {
		return cfg
	}
	fm := readManifest(root, folder)
	if fm == nil || !fm.Desktop {
		return cfg
	}
	c := *cfg
	c.Desktop = true
	return &c
}

type row struct {
	kind    string // "static" | "go"
	id      string
	folder  string
	entry   string // go entrypoint ("." or "./cmd/x")
	binName string
	assets  bool
	desktop bool // portless desktop app; needs a native cgo build for its GUI
}

var slugReplacer = strings.NewReplacer(" ", "-", "_", "-", "'", "", "\"", "")

// normalizeAppName folds an app name/id/folder to a comparison key:
// lowercase with dashes and underscores stripped, so devops-dojo,
// devopsdojo and DevopsDojo all match.
func normalizeAppName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if r == '-' || r == '_' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func slug(name string) string {
	s := strings.ToLower(slugReplacer.Replace(name))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "-")
}

func main() {
	root := flag.String("root", "..", "project root (parent of Launcher)")
	appsPath := flag.String("apps", "apps.json", "metadata registry (Launcher/apps.json)")
	out := flag.String("out", "", "path for the generated apps-embedded.json")
	manifest := flag.String("manifest", "", "path for the TSV build manifest")
	only := flag.String("only", "", "comma-separated app names/ids/folders to pack (everything else is skipped; names match case- and dash-insensitively: devops-dojo, devopsdojo and DevopsDojo are the same app)")
	drop := flag.String("drop", "", "comma-separated app names/ids/folders to exclude from this build (release.sh passes the apps that could not be cross-compiled for the target)")
	flag.Parse()

	if *out == "" || *manifest == "" {
		fmt.Fprintln(os.Stderr, "genembedded: -out and -manifest are required")
		os.Exit(1)
	}

	cfg := readApps(*appsPath)

	// Index the configured entries by folder name (dir looks like "../Name").
	byFolder := map[string]*appEntry{}
	usedIDs := map[string]bool{}
	var stale []*appEntry
	for _, a := range cfg.Apps {
		if a == nil || a.ID == "" {
			continue
		}
		usedIDs[a.ID] = true
		folder := strings.TrimPrefix(a.Dir, "../")
		folder = strings.TrimPrefix(folder, "..\\")
		if a.Dir != "" && !dirExists(filepath.Join(*root, folder)) {
			stale = append(stale, a)
			continue
		}
		if folder != "" && folder != "." {
			byFolder[folder] = a
		}
	}

	entries, dirs := scan(*root)
	sort.Strings(dirs)

	// onlySet, when -only is given, names the apps this build packs. Keys
	// are normalized (lowercase, dashes/underscores stripped) so
	// "devops-dojo" matches the folder DevopsDojo and its id devopsdojo.
	var onlySet map[string]bool
	if *only != "" {
		onlySet = map[string]bool{}
		for _, want := range strings.Split(*only, ",") {
			if key := normalizeAppName(strings.TrimSpace(want)); key != "" {
				onlySet[key] = true
			}
		}
		if len(onlySet) == 0 {
			fmt.Fprintln(os.Stderr, "genembedded: -only holds no usable app names")
			os.Exit(1)
		}
	}

	// dropSet, when -drop is given, names apps excluded from this build —
	// release.sh feeds it the apps that failed to cross-compile for the
	// target. Unlike -only these get a note: an app vanishing from a
	// release must be visible in the build log.
	var dropSet map[string]bool
	if *drop != "" {
		dropSet = map[string]bool{}
		for _, want := range strings.Split(*drop, ",") {
			if key := normalizeAppName(strings.TrimSpace(want)); key != "" {
				dropSet[key] = true
			}
		}
	}

	var gen []*appEntry
	var rows []row
	for _, folder := range dirs {
		info := entries[folder]
		cfgApp := byFolder[folder]
		if cfgApp == nil {
			cfgApp = readManifest(*root, folder)
		} else {
			cfgApp = mergeFolderDesktop(cfgApp, *root, folder)
		}

		if dropSet != nil {
			match := normalizeAppName(folder)
			if cfgApp != nil && cfgApp.ID != "" {
				match = normalizeAppName(cfgApp.ID)
			}
			if dropSet[match] {
				fmt.Printf("note: app %q (folder %s) dropped from this target — it could not be cross-compiled\n",
					match, folder)
				continue
			}
		}

		if onlySet != nil {
			match := normalizeAppName(folder)
			if cfgApp != nil && cfgApp.ID != "" {
				match = normalizeAppName(cfgApp.ID)
			}
			if !onlySet[match] {
				// Silently skipped — the -only list names what is wanted;
				// a note per unlisted app only floods the build log.
				continue
			}
		}

		if cfgApp != nil && cfgApp.Pack != nil && !*cfgApp.Pack {
			fmt.Printf("note: app %q (folder %s) excluded from this build (\"pack\": false in apps.json)\n",
				cfgApp.ID, folder)
			continue
		}

		base := filepath.Base(folder)
		id, name := base, base
		if cfgApp != nil {
			id, name = cfgApp.ID, cfgApp.Name
		} else {
			id = slug(base)
			if usedIDs[id] {
				id = id + "-app"
			}
			usedIDs[id] = true
		}

		if info.kind == "static" {
			e := &appEntry{
				ID: id, Name: name, Icon: "📦",
				Desc: "Static web app", Category: "Discovered",
				URLPath: "/", Network: true, ServedBy: "launcher",
			}
			if cfgApp != nil {
				e.Icon, e.Desc, e.Category = cfgApp.Icon, cfgApp.Desc, cfgApp.Category
				e.Port, e.URLPath, e.Network = cfgApp.Port, cfgApp.URLPath, cfgApp.Network
				e.Env, e.Autostart, e.Accent = cfgApp.Env, cfgApp.Autostart, cfgApp.Accent
			}
			if e.URLPath == "" {
				e.URLPath = "/"
			}
			if info.entryHTML != "" && info.entryHTML != "index.html" {
				e.URLPath = "/" + info.entryHTML
			}
			gen = append(gen, e)
			rows = append(rows, row{kind: "static", id: id, folder: folder})
			continue
		}

		// Go app
		binName := slug(base)
		if cfgApp != nil && cfgApp.Binary != "" {
			binName = filepath.Base(strings.ReplaceAll(cfgApp.Binary, "\\", "/"))
		}
		e := &appEntry{
			ID: id, Name: name, Icon: "📦",
			Desc: "Auto-discovered Go app", Category: "Discovered",
			URLPath: "/", Network: true, Binary: binName,
			Dir:  "Apps/" + id,
			Args: []string{"{port}"},
		}
		if cfgApp != nil {
			e.Icon, e.Desc, e.Category = cfgApp.Icon, cfgApp.Desc, cfgApp.Category
			e.URLPath, e.Network, e.Args, e.Env = cfgApp.URLPath, cfgApp.Network, cfgApp.Args, cfgApp.Env
			e.Autostart, e.Accent = cfgApp.Autostart, cfgApp.Accent
			e.Desktop = cfgApp.Desktop // portless GUI/CLI binary, no web server
			e.Port = cfgApp.Port
			if e.Desktop && cfgApp.Cmd != "" {
				// Desktop terminal apps launch through their own wrapper
				// script; the binary travels inside the app's data/ assets,
				// so nothing is packed into embedded/bin.
				e.Cmd = cfgApp.Cmd
				e.Binary = ""
				e.Args = nil
			}
			// Port stays 0 for portless web apps: the launcher assigns a
			// stable port from its reserved range (24500–25499) at start and
			// remembers it in ports.json — inventing one here would bypass
			// that contract and pin 91xx ports into every embedded build.
		}
		if e.URLPath == "" {
			e.URLPath = "/"
		}
		if len(e.Args) == 0 && !e.Desktop {
			e.Args = []string{"{port}"}
		}
		gen = append(gen, e)
		entry := goEntryFor(info, binName)
		if entry == "" {
			fmt.Printf("note: skipping %q — module has several cmd entrypoints and none matches the binary %q\n", id, binName)
			continue
		}
		rows = append(rows, row{
			kind: "go", id: id, folder: folder,
			entry: entry, binName: binName, assets: info.hasAssets,
			desktop: e.Desktop,
		})
	}

	for _, a := range stale {
		fmt.Printf("note: apps.json entry %q references missing folder %s — dropped from this release (remove the entry from apps.json or restore the folder)\n", a.ID, a.Dir)
	}

	// A -only list that matched nothing would ship a silently empty bundle;
	// refuse instead (a typo'd app name must not masquerade as a release).
	if onlySet != nil && len(gen) == 0 {
		fmt.Fprintln(os.Stderr, "genembedded: no app matched the -only list — check the names (ids, folder names, dash-insensitive)")
		os.Exit(1)
	}

	// Write the generated registry.
	outBytes, err := json.MarshalIndent(appsFile{Apps: gen}, "", "  ")
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*out, append(outBytes, '\n'), 0o644); err != nil {
		fail(err)
	}

	// Write the build manifest.
	var m strings.Builder
	for _, r := range rows {
		assets := "0"
		if r.assets {
			assets = "1"
		}
		desktop := "0"
		if r.desktop {
			desktop = "1"
		}
		entry := r.entry
		if entry == "" {
			entry = "-"
		}
		fmt.Fprintf(&m, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.kind, r.id, r.folder, entry, r.binName, assets, desktop)
	}
	if err := os.WriteFile(*manifest, []byte(m.String()), 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("genembedded: %d apps (%d static, %d go) → %s\n",
		len(gen), countKind(rows, "static"), countKind(rows, "go"), filepath.Base(*out))
}

type folderInfo struct {
	kind      string   // "static" | "go"
	entry     string   // go entrypoint (default pick; refined by binary name later)
	cmds      []string // every ./cmd/<name> entrypoint, for binary-name preference
	entryHTML string   // static entry page ("" for index.html)
	hasAssets bool
}

// appsContainer is the folder that groups app folders next to Launcher/
// ("../Apps/LawBuddy" …). It is scanned recursively so categories can be
// added later (Apps/Astrology/…). Root-level app folders keep working.
const appsContainer = "Apps"

// launcherDir locates the launcher's own checkout — the folder three levels
// above this file (tools/genembedded/main.go) — so the scan can skip it
// whatever the checkout folder is named ("Launcher" on the build machine,
// whatever a git clone produced). "" when it cannot be determined or lies
// outside root; the scan then classifies every folder on its own and a
// registry entry with "pack": false remains the manual override.
func launcherDir(root string) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	dir, err := filepath.Abs(filepath.Dir(filepath.Dir(filepath.Dir(file))))
	if err != nil || !strings.HasPrefix(dir, root+string(filepath.Separator)) {
		return ""
	}
	return dir
}

func scan(root string) (map[string]folderInfo, []string) {
	out := map[string]folderInfo{}
	var dirs []string
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	launcher := launcherDir(root)

	// classify checks dir as a possible app and records it under rel.
	classify := func(dir, rel string) {
		if _, dup := out[rel]; dup {
			return
		}
		if fileExists(filepath.Join(dir, "go.mod")) {
			info := folderInfo{kind: "go", entry: "."}
			if !rootHasMainPackage(dir) {
				info.cmds = cmdEntrypoints(dir)
				if len(info.cmds) == 0 {
					fmt.Printf("note: skipping %q — Go module without a root main package or a cmd/<name>/main.go\n", rel)
					return
				}
				info.entry = info.cmds[0]
			}
			info.hasAssets = goAppHasDiskAssets(dir)
			out[rel] = info
			dirs = append(dirs, rel)
			return
		}
		if html := firstRootHTML(dir); html != "" {
			out[rel] = folderInfo{kind: "static", entryHTML: html}
			dirs = append(dirs, rel)
		}
	}

	var scanDir func(dir, prefix string)
	scanDir = func(dir, prefix string) {
		dirEntries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, de := range dirEntries {
			name := de.Name()
			if !de.IsDir() || skipFolder(name) {
				continue
			}
			child := filepath.Join(dir, name)
			if prefix == "" && child == launcher {
				continue // the launcher's own checkout, whatever it is named
			}
			if prefix == "" && strings.EqualFold(name, appsContainer) {
				scanDir(child, name+"/") // app group folder — descend
				continue
			}
			classify(child, prefix+name)
		}
	}
	scanDir(root, "")
	return out, dirs
}

// skipFolder names that are never apps. The launcher's own checkout is
// skipped by path in scan — this list only excludes junk by name.
func skipFolder(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
		strings.EqualFold(name, "node_modules") || strings.EqualFold(name, "release")
}

func rootHasMainPackage(dir string) bool {
	goFiles, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, f := range goFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if strings.HasPrefix(string(data), "package main") ||
			strings.Contains(string(data), "\npackage main") {
			return true
		}
	}
	return false
}

// cmdEntrypoints lists every cmd/<name> package-main entrypoint of a
// module, sorted for determinism. The caller resolves which one builds the
// app's binary once the manifest's binary name is known (goEntryFor).
func cmdEntrypoints(dir string) []string {
	entries, err := os.ReadDir(filepath.Join(dir, "cmd"))
	if err != nil {
		return nil
	}
	var found []string
	for _, c := range entries {
		if c.IsDir() && rootHasMainPackage(filepath.Join(dir, "cmd", c.Name())) {
			found = append(found, "./cmd/"+c.Name())
		}
	}
	sort.Strings(found)
	return found
}

// goEntryFor picks the build entrypoint for a go app whose binary is
// binName: a root main package wins ("."), else cmd/<binName>, else a
// single cmd entrypoint. Multi-cmd modules without a matching name are
// ambiguous — the empty string says so.
func goEntryFor(info folderInfo, binName string) string {
	if len(info.cmds) == 0 {
		return info.entry // root main package
	}
	for _, e := range info.cmds {
		if strings.TrimPrefix(e, "./cmd/") == binName {
			return e
		}
	}
	if len(info.cmds) == 1 {
		return info.cmds[0]
	}
	return ""
}

// goAppHasDiskAssets: an app whose //go:embed directives cover its web UI
// (an asset dir or an .html page) is self-contained and ships binary-only;
// the rest ship their web assets when the folder actually has some (an
// index.html at the root or common asset directories). The directive is
// read, not just detected: an app embedding a data file — a tokenizer, a
// template — while serving its UI from disk (web/) must still get those
// assets packed, or its dashboard card opens to a 404.
func goAppHasDiskAssets(dir string) bool {
	if embedsWebAssets(dir) {
		return false
	}
	if hasRootHTML(dir) {
		return true
	}
	for _, d := range []string{"templates", "static", "web", "css", "js", "data"} {
		if dirExists(filepath.Join(dir, d)) {
			return true
		}
	}
	return false
}

// webAssetNames are the directory names a //go:embed pattern must reference
// for the binary to count as self-contained. A pattern naming anything else
// (tokens/, rules/, cmd/…) is data, not the UI.
var webAssetNames = []string{"web", "static", "templates", "css", "js", "ui", "assets", "dist"}

var embedDirectiveRe = regexp.MustCompile(`(?m)^//go:embed[ \t]+(.+)$`)

// embedsWebAssets reports whether some //go:embed directive in the module
// references a web asset root or an .html page. Patterns are matched
// case-insensitively on their first path segment, so "all:web", "web/*" and
// the multi-pattern "index.html css js" all match.
func embedsWebAssets(dir string) bool {
	found := false
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipAll
		}
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "release" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range embedDirectiveRe.FindAllStringSubmatch(string(data), -1) {
			for _, pat := range strings.Fields(m[1]) {
				pat = strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(pat, "all:"), ":"))
				seg := strings.SplitN(pat, "/", 2)[0]
				if strings.HasSuffix(pat, ".html") {
					found = true
				}
				for _, a := range webAssetNames {
					if seg == a {
						found = true
					}
				}
			}
			if found {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}

func hasRootHTML(dir string) bool {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.html"))
	return len(matches) > 0
}

func firstRootHTML(dir string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.html"))
	sort.Strings(matches)
	for _, m := range matches {
		base := filepath.Base(m)
		if strings.EqualFold(base, "index.html") {
			return "index.html"
		}
	}
	if len(matches) > 0 {
		return filepath.Base(matches[0])
	}
	return ""
}

func readApps(path string) *appsFile {
	cfg := &appsFile{}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			fail(fmt.Errorf("%s: %w", path, err))
		}
	} else if !os.IsNotExist(err) {
		fail(err)
	}
	return cfg
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func countKind(rows []row, kind string) int {
	n := 0
	for _, r := range rows {
		if r.kind == kind {
			n++
		}
	}
	return n
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "genembedded:", err)
	os.Exit(1)
}
