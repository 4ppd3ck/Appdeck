package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

// deckrebuild.go — POST /api/deck/rebuild reconstructs the deck from disk.
//
// Older all-in-one builds never wrote app.json during extraction (the
// manifest lived only inside the exe's embedded registry), so updating to a
// launcher with a smaller registry emptied the deck down to the few folders
// an import had touched. This endpoint scans the apps container and generates
// a manifest for every folder that lacks one; an optional source path imports
// app folders from outside the data directory (dev recovery — end installs
// never need it). Both phases are best-effort: one unusable folder becomes a
// note, never a failure.

func (s *Server) serveDeckRebuild(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"` // optional: a folder of app folders to import
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading request: " + err.Error()})
			return
		}
	}
	res, err := s.rebuildDeck(req.Path)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Pick up the new manifests even when nothing changed — a reload is cheap
	// and keeps the dashboard in sync without a restart.
	if err := s.reloadApps(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) rebuildDeck(path string) (map[string]any, error) {
	container, err := s.appsContainerDir()
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, a := range s.appsList() {
		if a != nil && a.ID != "" {
			known[strings.ToLower(a.ID)] = true
		}
	}
	notes := []string{}
	generated, imported := 0, 0

	// Phase 1: installed folders without a manifest.
	entries, err := os.ReadDir(container)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || skipAppFolder(e.Name()) {
			continue
		}
		dir := filepath.Join(container, e.Name())
		if _, err := os.Stat(filepath.Join(dir, manifestFileName)); err == nil {
			continue // self-describing already
		}
		id := strings.ToLower(slugify(e.Name()))
		if id == "" || known[id] {
			continue
		}
		app, err := manifestForFolder(dir)
		if err != nil {
			notes = append(notes, "skipped "+e.Name()+": "+err.Error())
			continue
		}
		if err := writeAppManifest(app, dir); err != nil {
			notes = append(notes, "skipped "+e.Name()+": "+err.Error())
			continue
		}
		known[id] = true
		generated++
		notes = append(notes, "generated app.json for "+app.ID+" ("+app.Name+")")
	}

	// Phase 2: optional import from an external folder (same layout rules the
	// discovery uses: go.mod → binary app, root .html → hosted app).
	src := expandHome(strings.TrimSpace(path))
	if src != "" {
		info, err := os.Stat(src)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("import path is not a folder: %s", src)
		}
		sub, err := os.ReadDir(src)
		if err != nil {
			return nil, err
		}
		for _, e := range sub {
			if !e.IsDir() || skipAppFolder(e.Name()) {
				continue
			}
			dir := filepath.Join(src, e.Name())
			// The id comes from the folder's own manifest when it has one,
			// else from a generated one — a source folder "Itihasas" carrying
			// app.json id "mythology" must dedupe against the live app, not
			// import a duplicate.
			app, err := loadAppManifest(dir)
			if err != nil {
				app, err = manifestForFolder(dir)
				if err != nil {
					notes = append(notes, "skipped "+e.Name()+": "+err.Error())
					continue
				}
			}
			id := strings.ToLower(app.ID)
			if known[id] {
				notes = append(notes, "skipped "+e.Name()+" — already in the deck")
				continue
			}
			dest := filepath.Join(container, slugify(app.ID))
			if _, err := os.Stat(dest); err == nil {
				notes = append(notes, "skipped "+e.Name()+" — "+filepath.Base(dest)+" is already installed")
				continue
			}
			if err := copyAppFolder(dir, dest); err != nil {
				os.RemoveAll(dest)
				notes = append(notes, "skipped "+e.Name()+": "+err.Error())
				continue
			}
			// A copied folder carries its own app.json; generated manifests
			// are written only when the source had none.
			if err := writeAppManifest(app, dest); err != nil {
				os.RemoveAll(dest)
				notes = append(notes, "skipped "+e.Name()+": "+err.Error())
				continue
			}
			markRebuildOnImport(dest)
			if err := markEmbeddedOverride(dest, app.ID); err != nil {
				os.RemoveAll(dest)
				notes = append(notes, "skipped "+e.Name()+": "+err.Error())
				continue
			}
			known[id] = true
			imported++
			notes = append(notes, "imported "+app.ID+" ("+app.Name+")")
		}
	}

	return map[string]any{
		"ok":        true,
		"generated": generated,
		"imported":  imported,
		"notes":     notes,
	}, nil
}

// manifestForFolder describes an installed app folder that has no app.json:
// the binary is detected on disk (the folder-named file beats a generic
// executable scan), a go.mod makes it a buildable binary app, a root .html
// makes it a hosted app. Port 0 lets the port registry assign a stable one.
// The result is ready for writeAppManifest.
func manifestForFolder(dir string) (*App, error) {
	base := filepath.Base(dir)
	id := slugify(base)
	if id == "" || !validAppID(id) {
		return nil, fmt.Errorf("folder name %q is not a usable app id", base)
	}
	app := &App{
		ID:         id,
		Name:       displayName(base),
		Icon:       "📦",
		Category:   "Tools",
		URLPath:    "/",
		Network:    true,
		APIVersion: appDeckAPIVersion,
	}
	if bin := detectFolderBinary(dir, base); bin != "" {
		app.Binary = bin
		app.Args = []string{"{port}"}
		return app, nil
	}
	if fileExists(filepath.Join(dir, "go.mod")) {
		// Sources only — the launcher builds the binary on first start.
		app.Binary = id
		app.Args = []string{"{port}"}
		return app, nil
	}
	if hasRootHTML(dir) {
		app.ServedBy = "launcher"
		return app, nil
	}
	return nil, fmt.Errorf("no binary, go.mod or .html entry in the folder")
}

// detectFolderBinary returns the app folder's binary as a folder-relative
// path: the file named after the folder (plain and legacy platform-suffixed
// spellings) wins, else the first file in the folder root whose header sniffs
// as a Mach-O/PE/ELF binary (ReadDir is sorted, so the pick is stable).
func detectFolderBinary(dir, base string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	platform := base + "-" + runtime.GOOS + "-" + runtime.GOARCH
	named := map[string]bool{
		base:                     true,
		base + ".exe":            true,
		platform:                 true,
		platform + ".exe":        true,
		embeddedBinaryName(base): true,
	}
	fallback := ""
	for _, e := range entries {
		if e.IsDir() || !e.Type().IsRegular() || isJunkEntry(e.Name()) {
			continue
		}
		if named[e.Name()] {
			return e.Name()
		}
		if fallback == "" {
			if looksExecutable(readFileHeader(filepath.Join(dir, e.Name()), 4)) {
				fallback = e.Name()
			}
		}
	}
	return fallback
}

// readFileHeader reads at most n bytes from the start of a file (a zero-nil
// short read is fine — the magic sniff only needs the first four).
func readFileHeader(path string, n int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, n)
	read, _ := f.Read(buf)
	return buf[:read]
}

// displayName turns a folder slug into a human label: "ksana-ledger" →
// "Ksana Ledger", "chronostamp" → "Chronostamp". Compound words without a
// separator cannot be split — the dashboard's per-app edit covers those.
func displayName(base string) string {
	words := strings.FieldsFunc(base, func(r rune) bool {
		return r == '-' || r == '_' || r == ' ' || r == '.'
	})
	var b strings.Builder
	for _, w := range words {
		r := []rune(w)
		if len(r) == 0 {
			continue
		}
		b.WriteRune(unicode.ToUpper(r[0]))
		b.WriteString(string(r[1:]))
		b.WriteString(" ")
	}
	return strings.TrimSpace(b.String())
}
