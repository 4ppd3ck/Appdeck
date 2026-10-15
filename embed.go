// Single-file (embed) mode runtime support.
//
// The release launcher.exe carries every app: Go binaries under embedded/bin/
// and each app's web assets under embedded/assets/<app-id>/. At startup the
// binaries (and the assets of apps that read them from disk) are unpacked into
// a per-user data directory; the plain HTML apps are served straight out of
// memory by the launcher itself — no files beside the exe, no separate
// processes for them.
//
// Data directory layout (LAUNCHER_DATA_DIR overrides):
//
//	<data>/apps/<app-id>/       app folder: manifest, assets AND binaries —
//	                            the single home shared with zip imports
//	<data>/appdata/<app-id>/    per-app user data (never touched by updates)
//	<data>/logs/                launcher + per-app logs
//	<data>/state.json           which apps the launcher started
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Porting notes from the original server.py prototype:
//
// Mapping of the old Python routes is kept one-to-one in Server.route.
// App ids are stable identifiers; never rename one after a release.
// Never run two starts of the same app concurrently (inflight guard).
// Note: state.json files written by the prototype still load unchanged.
// Yield the 15 s startup budget before reporting an app as "slow".

// defaultDataDir is the per-user home for extracted binaries, logs and state.
// Nothing is ever written next to the executable.
func defaultDataDir() string {
	if v := os.Getenv("LAUNCHER_DATA_DIR"); v != "" {
		return v
	}
	var dir string
	switch runtime.GOOS {
	case "windows":
		dir = filepath.Join(os.Getenv("LocalAppData"), "AppDeck")
	case "darwin":
		dir = filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "AppDeck")
	default:
		dir = filepath.Join(os.Getenv("HOME"), ".local", "share", "AppDeck")
	}
	migrateLegacyDataDir(dir)
	return dir
}

// migrateLegacyDataDir renames the pre-rename "ProjectLauncher" data directory
// (if one exists) to its new home, so existing installs keep their unpacked
// apps and in-app user data. Best effort: on failure the old dir stays put and
// the launcher simply unpacks fresh.
func migrateLegacyDataDir(newDir string) {
	if _, err := os.Stat(newDir); err == nil {
		return // already migrated (or brand new)
	}
	parent := filepath.Dir(newDir)
	for _, legacy := range []string{"ProjectLauncher", "Project Launcher"} {
		old := filepath.Join(parent, legacy)
		if info, err := os.Stat(old); err == nil && info.IsDir() {
			if err := os.Rename(old, newDir); err == nil {
				return
			}
		}
	}
}

// embeddedConfigJSON reads the packaged apps.json.
func embeddedConfigJSON() ([]byte, error) {
	return fs.ReadFile(embeddedFS, "embedded/apps.json")
}

// embeddedAppIDs returns the ids packaged into this build's executable
// (empty in folder builds). Imports of these ids are refused: the embedded
// copy always wins — hosted apps serve from the exe's assets and app
// binaries are refreshed from it on every boot — so an imported folder with
// the same id would be a dead duplicate in the data directory.
func embeddedAppIDs() map[string]bool {
	ids := map[string]bool{}
	if !embeddedBuild {
		return ids
	}
	var parsed struct {
		Apps []*App `json:"apps"`
	}
	data, err := embeddedConfigJSON()
	if err != nil {
		return ids
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return ids
	}
	for _, a := range parsed.Apps {
		if a != nil && a.ID != "" {
			ids[a.ID] = true
		}
	}
	return ids
}

// embeddedDuplicateErr is the refusal every import flow returns for an app
// id already packaged in the executable, naming the way out for each intent.
func embeddedDuplicateErr(id string) error {
	return fmt.Errorf("%q is already packaged inside this launcher's executable — importing it "+
		"would only add a duplicate folder the launcher ignores (the embedded copy wins). "+
		"To update it, replace the launcher; to keep both, import it under a different app name", id)
}

// embeddedIcon reads an app's packaged folder icon (the app folder's
// icon.png/icon.svg, staged under embedded/assets/<id>/ by release.sh — even
// for binary-only apps whose other assets live inside their binary). In
// folder builds the embedded FS is empty and this always misses; icons are
// read from the app folder on disk instead.
func embeddedIcon(id string) (data []byte, ctype string, ok bool) {
	for _, ic := range appIconNames {
		if data, err := fs.ReadFile(embeddedFS, "embedded/assets/"+id+"/"+ic.name); err == nil {
			return data, ic.ctype, true
		}
	}
	return nil, "", false
}

// embeddedIconExists is the stat-only check behind the iconFile status field.
func embeddedIconExists(id string) bool {
	for _, ic := range appIconNames {
		if _, err := fs.Stat(embeddedFS, "embedded/assets/"+id+"/"+ic.name); err == nil {
			return true
		}
	}
	return false
}

// extractEmbedded unpacks app binaries and disk-served assets into dataDir.
// Binaries are refreshed on every start (they are part of the exe); asset
// files are only written when missing so user data that lives inside an app
// folder (Investment's data.js, LawBuddy's db) survives launcher updates.
// Deleting the data directory resets everything.
func extractEmbedded(dataDir string) error {
	var parsed struct {
		Apps []*App `json:"apps"`
	}
	data, err := embeddedConfigJSON()
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("embedded apps.json: %w", err)
	}
	return extractApps(dataDir, parsed.Apps)
}

// extractApps does the actual unpacking for the given app list. Apps whose
// binary is not packaged in this build are skipped with a warning — that is
// the "app was removed from release.sh but still listed in apps-embedded.json"
// case, and it must never keep the launcher from starting.
func extractApps(dataDir string, apps []*App) error {
	for _, a := range apps {
		if a == nil || a.ServedBy == "launcher" {
			continue // hosted in-process, nothing to extract
		}
		// The app folder (<data>/<RawDir>, i.e. Apps/<id>/) is the single
		// home: binaries land beside the manifest and assets, exactly where
		// a zip import puts them — one layout for both origins. Older builds
		// extracted into lowercase apps/<id>; on case-sensitive filesystems
		// that folder is renamed to the canonical casing once, preserving
		// contents (case-insensitive systems share the same directory and
		// need nothing).
		appDir := filepath.Join(dataDir, a.RawDir)
		if err := migrateAppFolderCase(dataDir, a.RawDir); err != nil {
			return err
		}
		if err := os.MkdirAll(appDir, 0o755); err != nil {
			return err
		}
		if a.Binary != "" {
			if _, err := fs.Stat(embeddedFS, "embedded/bin/"+a.Binary); err != nil {
				fmt.Fprintf(os.Stderr, "launcher: warning: skipping app %q — %s is not packaged in this build\n",
					a.ID, a.Binary)
				continue
			}
			name := embeddedBinaryName(a.Binary)
			if err := extractFile(embeddedFS, "embedded/bin/"+a.Binary, filepath.Join(appDir, name), true); err != nil {
				return fmt.Errorf("extract %s: %w", a.Binary, err)
			}
		}
		// a.Binary empty → a Cmd-launched desktop app: its wrapper and binary
		// travel inside the app's own assets (data/) below.
		// Assets refresh on every boot: app updates in a rebuilt all-in-one
		// must reach existing installs. Only files shipped in the build are
		// written — user data lives in appdata/<id>, and anything else in the
		// app folder (databases, configs) is never touched by extraction.
		if err := extractTree(embeddedFS, "embedded/assets/"+a.ID, appDir, true); err != nil {
			return fmt.Errorf("extract assets for %s: %w", a.ID, err)
		}
	}
	pruneLegacyBinDir(filepath.Join(dataDir, "bin"), apps)
	return nil
}

// migrateAppFolderCase renames a legacy lowercase apps/<id> folder to the
// canonical Apps/<id> casing when both would otherwise diverge (Linux and
// other case-sensitive filesystems). Best effort: on failure the old folder
// stays and extraction proceeds — nothing is lost either way.
func migrateAppFolderCase(dataDir, rawDir string) error {
	container, rest, cut := strings.Cut(rawDir, "/")
	if !cut || strings.EqualFold(container, "Apps") && container == "Apps" {
		return nil // already canonical (or no container segment)
	}
	legacy := filepath.Join(dataDir, "apps", rest)
	canonical := filepath.Join(dataDir, "Apps", rest)
	_, legacyErr := os.Stat(legacy)
	_, canonicalErr := os.Stat(canonical)
	if legacyErr == nil && canonicalErr != nil {
		if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
			return err
		}
		return os.Rename(legacy, canonical)
	}
	return nil
}

// pruneLegacyBinDir deletes executables under the pre-standardization
// <data>/bin/ cache once this build no longer needs them: every binary it
// still holds is now extracted into the app folder instead. The folder
// itself is removed when empty, ending the two-home layout. Imports never
// write here anymore (they always landed in apps/<id>/).
func pruneLegacyBinDir(binDir string, apps []*App) {
	if _, err := os.Stat(binDir); err != nil {
		return // nothing to clean
	}
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue // leftover nests (e.g. bin/Apps) are handled below
		}
		if err := os.Remove(filepath.Join(binDir, e.Name())); err == nil {
			removed++
		}
	}
	// Subdirectories only appear from pre-fix import bugs; sweep them too.
	for _, e := range entries {
		if e.IsDir() {
			_ = os.RemoveAll(filepath.Join(binDir, e.Name()))
		}
	}
	if removed > 0 {
		fmt.Fprintf(os.Stderr, "launcher: retired legacy bin/ cache (%d files) — binaries now live in their app folders\n", removed)
	}
	_ = os.Remove(binDir) // succeeds only when empty
}

// embeddedBinaryName maps the packaged binary name to the on-disk name.
// Binaries are packaged without an extension; Windows needs .exe.
func embeddedBinaryName(binary string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(binary, ".exe") {
		return binary + ".exe"
	}
	return binary
}

// extractFile copies one file out of the embed. overwrite=false keeps an
// existing file on disk.
func extractFile(src embed.FS, name, dst string, overwrite bool) error {
	data, err := fs.ReadFile(src, name)
	if err != nil {
		return fmt.Errorf("%s is missing from this build (did release.sh stage it?)", name)
	}
	if !overwrite {
		if _, err := os.Stat(dst); err == nil {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}

// extractTree copies a whole directory from the embed.
func extractTree(src embed.FS, dir, dst string, overwrite bool) error {
	entries, err := fs.ReadDir(src, dir)
	if err != nil {
		return nil // no assets packaged for this app (embedded-asset apps) — fine
	}
	for _, e := range entries {
		s := dir + "/" + e.Name()
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := extractTree(src, s, d, overwrite); err != nil {
				return err
			}
			continue
		}
		if err := extractFile(src, s, d, overwrite); err != nil {
			return err
		}
	}
	return nil
}

// staticHandlerFor serves a hosted HTML app: from the exe's embedded assets
// in single-file builds, from the app's disk folder in dev builds. Apps
// imported (or watch-dropped) after an all-in-one exe was built have no
// packaged assets — those serve from their disk folder in the data
// directory's Apps container, so imports keep working without a rebuild.
// Both paths get no-store headers, the self-close watchdog, and a
// window.APPDECK constant naming the launcher URL + app id (apps that call
// back — Monitor-style dashboards, deck tools — read it instead of guessing
// addresses).
func (s *Server) staticHandlerFor(a *App) (http.Handler, error) {
	deckConsts := fmt.Sprintf(
		`<script>window.APPDECK={launcher:"http://127.0.0.1:%d",app:%q};</script>`,
		s.cfg.Port, a.ID)
	if embeddedBuild {
		sub, err := fs.Sub(embeddedFS, "embedded/assets/"+a.ID)
		if err != nil {
			return nil, err
		}
		if entries, err := fs.ReadDir(embeddedFS, "embedded/assets/"+a.ID); err == nil && len(entries) > 0 {
			return noCacheHeaders(injectScripts("head", deckConsts)(injectScripts("end", appWatchdogJS)(http.FileServer(http.FS(sub))))), nil
		}
		// No packaged assets for this id — fall through to the disk handler.
		// The data-dir Apps container is the only place an import ever
		// writes in this build mode, and loadApps has already resolved the
		// folder into a.Dir.
	}
	if a.Dir == "" {
		if embeddedBuild {
			return nil, fmt.Errorf("app %q has no packaged assets and no imported folder to serve", a.ID)
		}
		return nil, fmt.Errorf("app %q has no folder to serve", a.ID)
	}
	if entries, err := os.ReadDir(a.Dir); err != nil || len(entries) == 0 {
		return nil, fmt.Errorf("nothing to serve in %s", a.Dir)
	}
	return noCacheHeaders(injectScripts("head", deckConsts)(injectScripts("end", appWatchdogJS)(http.FileServer(http.Dir(a.Dir))))), nil
}

// appWatchdogJS: injected into hosted app pages so an open tab/window closes
// itself when the app's server goes away (Stop, Stop All, launcher quit) —
// even if the dashboard that opened it was reloaded and lost track of it.
// Browsers only honor window.close() for script-opened windows (exactly what
// the dashboard's Open button creates); a tab the user opened by hand just
// stays, which is the correct behavior anyway.
const appWatchdogJS = `<script>(function(){
var f=0;
window.setInterval(function(){
  fetch(location.origin+"/__launcher_watchdog__?t="+Date.now(),{cache:"no-store"})
    .then(function(){f=0})
    .catch(function(){if(++f>=2){window.setInterval(function(){try{window.close()}catch(e){}},400)}});
},2000);
})();</script>`

// injectScripts buffers 200 text/html responses and injects the given
// scripts — pos "end": before </body>; pos "head": right after <head>, so
// constants exist before any page script runs. One buffer pass per wrapper;
// everything else streams through untouched.
func injectScripts(pos string, scripts ...string) func(http.Handler) http.Handler {
	joined := strings.Join(scripts, "")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				next.ServeHTTP(w, r)
				return
			}
			wr := &watchResponse{ResponseWriter: w}
			defer func() {
				if !wr.hold {
					return
				}
				var body []byte
				if pos == "head" {
					body = injectAfterHead(wr.buf.Bytes(), joined)
				} else {
					body = injectBeforeBodyEnd(wr.buf.Bytes(), joined)
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.WriteHeader(wr.code)
				_, _ = w.Write(body)
			}()
			next.ServeHTTP(wr, r)
		})
	}
}

// injectAfterHead inserts inject right after the opening <head> tag (or at
// the very start when there is none).
func injectAfterHead(html []byte, inject string) []byte {
	lower := bytes.ToLower(html)
	idx := bytes.Index(lower, []byte("<head"))
	if idx < 0 {
		out := make([]byte, 0, len(html)+len(inject))
		out = append(out, inject...)
		return append(out, html...)
	}
	gt := bytes.IndexByte(lower[idx:], '>')
	if gt < 0 {
		out := make([]byte, 0, len(html)+len(inject))
		out = append(out, inject...)
		return append(out, html...)
	}
	at := idx + gt + 1
	out := make([]byte, 0, len(html)+len(inject))
	out = append(out, html[:at]...)
	out = append(out, inject...)
	return append(out, html[at:]...)
}

// injectBeforeBodyEnd inserts inject before the last "</body>" (case
// insensitive); without one it appends at the end.
func injectBeforeBodyEnd(html []byte, inject string) []byte {
	lower := bytes.ToLower(html)
	idx := bytes.LastIndex(lower, []byte("</body>"))
	out := make([]byte, 0, len(html)+len(inject))
	if idx < 0 {
		out = append(out, html...)
		return append(out, inject...)
	}
	out = append(out, html[:idx]...)
	out = append(out, inject...)
	return append(out, html[idx:]...)
}

// watchResponse decides on the first WriteHeader whether the response is a
// bufferable HTML body (inject) or passes straight through.
type watchResponse struct {
	http.ResponseWriter
	hold bool
	code int
	buf  bytes.Buffer
	done bool
}

func (w *watchResponse) WriteHeader(code int) {
	if w.done {
		return
	}
	w.done = true
	if code == http.StatusOK && strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		w.hold = true
		w.code = code
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *watchResponse) Write(p []byte) (int, error) {
	if !w.done { // Write implies 200 when WriteHeader wasn't called
		w.WriteHeader(http.StatusOK)
	}
	if w.hold {
		w.buf.Write(p)
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

func (w *watchResponse) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok && !w.hold {
		f.Flush()
	}
}

// noCacheHeaders keeps the browser honest while apps are being updated.
func noCacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
