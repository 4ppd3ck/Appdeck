// Launcher self-update, two ways. Both end in the same place: POST /api/update
// replaces the running launcher executable with the one inside a zip — only
// the executable file is swapped, in place, with no backup kept, and the
// running process keeps the old image until AppDeck is restarted. (On
// Windows the running exe must be renamed aside to free its path; that
// temporary .old cannot be deleted while the old image still runs and is
// removed at the next start — see removeStaleUpdateBackup.)
//
//   - GitHub channel (end clients): the launcher checks the project's
//     releases, picks the build for this platform, downloads and installs it.
//     GET /api/update/check previews what is available; POST with
//     {"source":"github"} applies it.
//   - Local channel (dev): POST with {"path": …} installs from a zip, a bare
//     built binary, a release folder (newest zip inside), or an AppDeck.app /
//     staging directory, which is zipped in memory — so the whole update flow
//     can be exercised without publishing anything. Drag-and-drop uploads and
//     ?url= keep working.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The repository the GitHub channel checks. One launcher, one home.
const (
	updateGitHubOwner = "4ppd3ck"
	updateGitHubRepo  = "Appdeck"
)

// spawnRestartLauncher starts the (already swapped-in) launcher executable
// as a detached child carrying --restart, then shuts this process down. The
// child's --restart flag makes it wait for the dashboard port to free before
// booting, so the hand-off never trips the "launcher already running" guard.
// Works for every platform's release shape: the exe path is whatever
// os.Executable reports (bare binary, Contents/MacOS/AppDeck, AppDeck.exe).
func (s *Server) spawnRestartLauncher() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot locate the launcher executable: %v", err)
	}
	if _, err := os.Stat(exePath); err != nil {
		return fmt.Errorf("the launcher executable %s is gone: %v", exePath, err)
	}
	handled, err := spawnRestartViaOpen(exePath)
	if err != nil {
		return err // no shutdown started: the launcher stays up to serve the error
	}
	if !handled {
		cmd := exec.Command(exePath, "--restart")
		cmd.SysProcAttr = processSysProcAttr()
		hideWindow(cmd)
		// Keep the child's output out of this process's pipes; windowsgui builds
		// already redirect to the log file, console builds inherit stderr.
		if f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
			cmd.Stdout, cmd.Stderr = f, f
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("starting the new launcher: %v", err)
		}
	}
	// The child holds the port-wait; the parent's clean shutdown stops the
	// managed apps and exits, releasing the port for the child to take over.
	go s.beginShutdown("restarting after launcher update")
	return nil
}

// spawnRestartViaOpen hands a macOS .app bundle's restart to LaunchServices
// (`open -n <bundle> … --args --restart`) instead of exec'ing the bundle
// binary directly. After a self-update replaced the executable inside the
// bundle, LaunchServices still holds the bundle's old identity — and the
// kernel's AppleSystemPolicy kills a directly spawned child of the running
// app at exec, silently, before its first log line (the unified log's only
// trace is "ASP: Security policy would not allow process"). The restart
// then looks like a quit, while a manual relaunch from Finder/Dock keeps
// working — because that goes through LaunchServices, which re-reads the
// changed bundle. `open -n` takes that same sanctioned path. Returns
// handled=false when this install is not a macOS bundle: a bare binary has
// no bundle seal to invalidate and direct exec stays correct.
//
// `open` answers as soon as LaunchServices has taken the request; the real
// spawn follows from launchd while this process is exiting, and the child's
// --restart port-wait absorbs the timing either way.
func spawnRestartViaOpen(exePath string) (bool, error) {
	if runtime.GOOS != "darwin" {
		return false, nil
	}
	i := strings.Index(exePath, ".app/Contents/")
	if i <= 0 {
		return false, nil
	}
	bundle := exePath[:i+4]
	args := []string{"-n", bundle}
	// LaunchServices launches get launchd's environment, not this process's —
	// forward the launcher's own overrides when set, so a sandboxed or
	// custom-configured bundle restarts with the same config it ran with.
	for _, key := range []string{"PORT", "HOST", "LAUNCHER_DATA_DIR", "LAUNCHER_ROOT",
		"LAUNCHER_TLS", "LAUNCHER_TLS_PORT", "LAUNCHER_LAN_TOKEN", "NO_OPEN"} {
		if v := os.Getenv(key); v != "" {
			args = append(args, "--env", key+"="+v)
		}
	}
	args = append(args, "--args", "--restart")
	// Bounded: `open` normally answers in well under a second; a hung
	// LaunchServices must not leave the restart request dangling forever.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "open", args...)
	cmd.SysProcAttr = processSysProcAttr()
	hideWindow(cmd)
	if f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		cmd.Stdout, cmd.Stderr = f, f
	}
	if err := cmd.Run(); err != nil {
		return true, fmt.Errorf("relaunching the updated launcher via LaunchServices failed (this instance keeps running): %v", err)
	}
	return true, nil
}

// serveRestart answers POST /api/restart: hand the dashboard over to a fresh
// launcher process. Loopback only, like every endpoint that decides the fate
// of the launcher itself.
func (s *Server) serveRestart(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "restart is only allowed from this computer"})
		return
	}
	if err := s.spawnRestartLauncher(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"note": "restarting — the new launcher takes over in a moment"})
}

// waitForPortFree blocks until the launcher's dashboard port is free again
// or the budget expires. Used by the --restart child: the dying parent (and
// its apps) still hold the port right after the swap.
func waitForPortFree(port int, budget time.Duration) {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if !portOpen(port, startProbeTimeout) {
			return
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// serveUpdate answers POST /api/update. The body is either JSON
// ({"source":"github"} or {"path": …}), a raw zip upload, or ?url= where to
// download one (reusing the import fetch with its size cap).
func (s *Server) serveUpdate(w http.ResponseWriter, r *http.Request) {
	// Every update replaces the launcher itself — never trust this endpoint
	// from another machine, whatever the dashboard-reachability setting is.
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "launcher updates are only allowed from this computer"})
		return
	}
	qs := parseQuery(r.RequestURI)
	if v := qs["url"]; v != "" {
		if dec, err := url.PathUnescape(v); err == nil {
			qs["url"] = dec
		}
	}
	var body []byte
	var prefix string
	restartAfter := false
	switch {
	case qs["url"] != "":
		// A remote zip replaces this launcher binary: https only (a MITM on a
		// plain-HTTP fetch is a persistent backdoor), and when the caller
		// pins an expected digest it is verified before anything is installed.
		raw := qs["url"]
		if pu, perr := url.Parse(raw); perr != nil || pu.Scheme != "https" {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "update URLs must be https — the download replaces this launcher binary"})
			return
		}
		data, _, err := fetchForImport(raw)
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if want := strings.ToLower(strings.TrimSpace(qs["sha256"])); want != "" {
			sum := sha256.Sum256(data)
			if got := hex.EncodeToString(sum[:]); got != want {
				s.writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "sha256 mismatch — the downloaded bytes do not match the expected checksum (" + got + ")"})
				return
			}
		}
		body = data
	case strings.Contains(r.Header.Get("Content-Type"), "application/json"):
		var req struct {
			Source string `json:"source"`
			Path   string `json:"path"`
			// Stall bounds the GitHub download in minutes (0 = default cap);
			// slow connections pick a bigger number in the update dialog.
			Stall   int    `json:"stall"`
			Tag     string `json:"tag"`     // GitHub channel: a specific release (downgrades); empty = newest
			Restart bool   `json:"restart"` // relaunch the new launcher right after the swap
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading request: " + err.Error()})
			return
		}
		var err error
		if req.Source == "github" {
			body, prefix, err = s.updateGitHubZip(req.Tag, time.Duration(req.Stall)*time.Minute)
		} else {
			body, prefix, err = resolveLocalUpdateSource(req.Path)
		}
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		restartAfter = req.Restart
	default:
		data, err := io.ReadAll(io.LimitReader(r.Body, installMaxTotal+1))
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading upload: " + err.Error()})
			return
		}
		body = data
	}
	note, err := s.installLauncherUpdate(body)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if prefix != "" {
		note = prefix + " — " + note
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"note": note,
	})
	if restartAfter {
		// After the reply: the dashboard polls for the fresh launcher once
		// this process has handed the port over.
		go func() {
			time.Sleep(600 * time.Millisecond)
			if err := s.spawnRestartLauncher(); err != nil {
				fmt.Fprintf(os.Stderr, "[launcher] restart after update failed: %v\n", err)
			}
		}()
	}
}

// serveUpdateCheck answers GET /api/update/check: what could the GitHub
// channel install right now? Read-only, no download — the dashboard shows
// the release list (newest first, the downgrade picker), the selected tag's
// best asset for this platform, and whether it equals the running version.
// ?tag=<tag> previews a specific (possibly older) release.
func (s *Server) serveUpdateCheck(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"ok": true, "current": launcherVersion, "repo": updateGitHubOwner + "/" + updateGitHubRepo}
	// The tag list doubles as the downgrade dropdown — newest first.
	list, listErr := fetchGitHubReleaseList(updateGitHubOwner, updateGitHubRepo)
	if listErr == nil {
		tags := make([]string, 0, len(list))
		for _, rel := range list {
			tags = append(tags, rel.Tag)
		}
		out["releases"] = tags
	}
	want := strings.TrimSpace(parseQuery(r.RequestURI)["tag"])
	if want == "" && len(list) > 0 {
		want = list[0].Tag // the newest
	}
	if want == "" {
		// No list at all (quota spent, page scrape empty): fall back to the
		// latest-release discovery chain before giving up.
		rel, err := fetchBestGitHubRelease(updateGitHubOwner, updateGitHubRepo)
		if err != nil {
			out["error"] = updateGitHubError(err)
			s.writeJSON(w, http.StatusOK, out)
			return
		}
		if rel == nil {
			out["found"] = false
			s.writeJSON(w, http.StatusOK, out)
			return
		}
		fillUpdateCheck(out, rel, "")
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	rel, err := resolveGitHubRelease(want, list)
	if err != nil {
		out["error"] = updateGitHubError(err)
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	fillUpdateCheck(out, rel, want)
	s.writeJSON(w, http.StatusOK, out)
}

// fillUpdateCheck adds the common "here is the release" fields to a check
// response: tag, whether it is what is already running, and the ranked asset
// for this platform (nil with an error note when the release carries none).
func fillUpdateCheck(out map[string]any, rel *ghReleaseInfo, _ string) {
	out["found"] = true
	out["tag"] = rel.Tag
	out["upToDate"] = updateUpToDate(rel.Tag)
	ranked := rankReleaseAssets(rel.Assets, runtime.GOOS, runtime.GOARCH)
	if len(ranked) == 0 {
		out["asset"] = nil
		out["error"] = fmt.Sprintf("release %s carries no build for %s/%s", rel.Tag, runtime.GOOS, runtime.GOARCH)
		return
	}
	a := ranked[0]
	out["asset"] = map[string]any{"name": a.Name, "size": a.Size, "url": a.URL}
}

// resolveGitHubRelease returns the release for tag: straight from the
// already-fetched list when it carries assets, else a dedicated fetch (API
// tags endpoint, expanded_assets scrape on quota). Empty tag = latest.
func resolveGitHubRelease(tag string, list []ghReleaseInfo) (*ghReleaseInfo, error) {
	tag = strings.TrimSpace(tag)
	for i := range list {
		if list[i].Tag == tag && len(list[i].Assets) > 0 {
			return &list[i], nil
		}
	}
	return fetchGitHubTag(updateGitHubOwner, updateGitHubRepo, tag)
}

// updateGitHubZip downloads the GitHub channel's selected release — the
// newest by default, or exactly the tag the user picked (downgrades are the
// point of the picker) — as update zip bytes. The prefix names the source
// for the response note and flags a downgrade against the list's newest.
func (s *Server) updateGitHubZip(tag string, stall time.Duration) ([]byte, string, error) {
	list, _ := fetchGitHubReleaseList(updateGitHubOwner, updateGitHubRepo)
	rel, err := resolveGitHubRelease(tag, list)
	if err != nil {
		return nil, "", fmt.Errorf("%s", updateGitHubError(err))
	}
	ranked := rankReleaseAssets(rel.Assets, runtime.GOOS, runtime.GOARCH)
	if len(ranked) == 0 {
		return nil, "", fmt.Errorf("release %s carries no build for %s/%s", rel.Tag, runtime.GOOS, runtime.GOARCH)
	}
	// stall bounds the whole download (the update dialog's "give up after"
	// selector; 0 = the HTTP client's own 30-minute cap). The import fetch
	// keeps its per-64MB progress lines for a future live view; here the
	// caller just wants bytes or an error naming the asset.
	fetchCtx := context.Background()
	if stall > 0 {
		var cancel context.CancelFunc
		fetchCtx, cancel = context.WithTimeout(fetchCtx, stall)
		defer cancel()
	}
	body, _, err := fetchForImportCtx(fetchCtx, ranked[0].URL, nil)
	if err != nil {
		if fetchCtx.Err() == context.DeadlineExceeded {
			return nil, "", fmt.Errorf("downloading %s timed out after %s — retry with a bigger timeout in the update dialog", ranked[0].Name, stall)
		}
		return nil, "", fmt.Errorf("downloading %s: %v", ranked[0].Name, err)
	}
	// Same verification imported prebuilts get: against the release's
	// checksum (per-asset .sha256 sidecar or the SHA256SUMS bundle
	// release.sh publishes) — the zip replaces the launcher binary.
	if err := verifyReleaseChecksum(rel, ranked[0], body, nil); err != nil {
		return nil, "", err
	}
	prefix := fmt.Sprintf("GitHub release %s (%s)", rel.Tag, ranked[0].Name)
	if len(list) > 0 && list[0].Tag != rel.Tag {
		prefix += " — downgrade from " + list[0].Tag
	}
	return body, prefix, nil
}

// updateGitHubError translates the shared GitHub client's failures into what
// they mean for the launcher channel — the importer's wording talks about
// apps, this one talks about updates.
func updateGitHubError(err error) string {
	if err == errGitHubRateLimited {
		return "GitHub's API rate limit is hit right now — try again later (the check falls back to the releases page, which also failed)"
	}
	return err.Error()
}

// updateUpToDate reports whether the running launcher already is the release
// tag. Versions here are build dates (or "dev" for a plain go build), so the
// honest comparison is exact-match equality — anything else is reported as
// "this release differs from what is running" and the user decides.
func updateUpToDate(tag string) bool {
	return tag == launcherVersion
}

// resolveLocalUpdateSource turns a dev-supplied path into update zip bytes.
// Accepted: a zip file; a bare launcher binary (zipped as a single entry);
// a directory holding zips (newest wins — point it at the release/ output);
// any other directory, including an AppDeck.app bundle or a staging folder,
// which is zipped in memory. The second return names the source for the
// response note.
func resolveLocalUpdateSource(path string) ([]byte, string, error) {
	p := expandHome(strings.TrimSpace(path))
	if p == "" {
		return nil, "", fmt.Errorf("give a path to update from — a zip, a built binary, or a folder (release output or an AppDeck.app)")
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, "", fmt.Errorf("cannot read %s: %v", p, err)
	}
	if !info.IsDir() {
		if strings.EqualFold(filepath.Ext(p), ".zip") {
			data, err := os.ReadFile(p)
			if err != nil {
				return nil, "", err
			}
			return data, filepath.Base(p), nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, "", err
		}
		if !looksExecutable(data) {
			return nil, "", fmt.Errorf("%s is neither a zip nor a launcher executable", filepath.Base(p))
		}
		zipped, err := zipUpdateFiles(map[string]zipUpdateFile{filepath.Base(p): {data: data, mode: 0o755}})
		if err != nil {
			return nil, "", err
		}
		return zipped, filepath.Base(p) + " (bare binary)", nil
	}
	// A directory: newest zip inside wins (the release/ output case).
	if newest, ok := newestZipIn(p); ok {
		data, err := os.ReadFile(newest)
		if err != nil {
			return nil, "", err
		}
		return data, filepath.Base(newest), nil
	}
	// Otherwise zip the tree. A single .app bundle inside (or the directory
	// being one) keeps its bundle name as the zip root, so the executable
	// picker sees the AppDeck.app shape it expects.
	root := ""
	if !strings.HasSuffix(p, ".app") {
		if entries, err := os.ReadDir(p); err == nil {
			var bundles []string
			for _, e := range entries {
				if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
					bundles = append(bundles, e.Name())
				}
			}
			if len(bundles) == 1 {
				root = bundles[0]
				p = filepath.Join(p, root)
			}
		}
	} else {
		root = filepath.Base(p)
	}
	zipped, err := zipUpdateDir(p, root)
	if err != nil {
		return nil, "", err
	}
	return zipped, filepath.Base(p) + " (folder zipped)", nil
}

// newestZipIn returns the most recently modified .zip directly inside dir.
func newestZipIn(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	newest := ""
	var newestAt int64
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".zip") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if newest == "" || fi.ModTime().UnixNano() > newestAt {
			newest = filepath.Join(dir, e.Name())
			newestAt = fi.ModTime().UnixNano()
		}
	}
	return newest, newest != ""
}

// zipUpdateFile is one in-memory zip entry under construction.
type zipUpdateFile struct {
	data []byte
	mode os.FileMode
	mod  time.Time
}

// zipUpdateFiles builds an update zip from the given entries (forward-slash
// names), capped at the install size limit.
func zipUpdateFiles(files map[string]zipUpdateFile) ([]byte, error) {
	var buf bytes.Buffer
	total := 0
	zw := zip.NewWriter(&buf)
	for name, f := range files {
		total += len(f.data)
		if total > installMaxTotal {
			zw.Close()
			return nil, fmt.Errorf("update source is larger than the %d byte cap", installMaxTotal)
		}
		mode := f.mode
		if mode == 0 {
			mode = 0o644
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: filepath.ToSlash(name), Method: zip.Deflate, Modified: f.mod})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// zipUpdateDir zips a directory tree as update source, skipping junk and
// non-regular files. root, when set, prefixes every entry (the .app bundle
// name, so the executable picker sees the bundle shape).
func zipUpdateDir(dir, root string) ([]byte, error) {
	files := map[string]zipUpdateFile{}
	total := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		name := filepath.ToSlash(rel)
		if root != "" {
			name = filepath.ToSlash(root) + "/" + name
		}
		if isJunkEntry(name) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			return nil // unreadable or special entry — skip, not fatal
		}
		total += int(info.Size())
		if total > installMaxTotal {
			return fmt.Errorf("update source is larger than the %d byte cap", installMaxTotal)
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil // skip unreadable files the way the zip reader would
		}
		files[name] = zipUpdateFile{data: data, mode: info.Mode().Perm(), mod: info.ModTime()}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s holds nothing to install", filepath.Base(dir))
	}
	return zipUpdateFiles(files)
}

// installLauncherUpdate validates the zip, picks the launcher executable
// inside it, installs any runtime files that zip carries beside the
// executable, and swaps the running one. Returns the human note for the
// response. Both release shapes are accepted: the all-in-one zip (a bare
// binary, everything embedded) and the folder-mode release (an AppDeck.app
// whose Contents/MacOS/ carries index.html, apps.json and an Apps/ tree
// beside the launcher). Updating an embedded install with a folder-mode zip
// switches it to folder mode: the live deck is materialized into the
// registry beside the executable first (materializeFolderDeck), so the
// smaller release zip works without emptying the deck.
func (s *Server) installLauncherUpdate(body []byte) (string, error) {
	if detectArchiveFormat(body) != "zip" {
		return "", fmt.Errorf("send a zip containing the new launcher (a bare binary or an AppDeck.app bundle)")
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", fmt.Errorf("not a readable zip: %v", err)
	}
	exe, pick, err := launcherExecutableFromZip(zr)
	if err != nil {
		return "", err
	}
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot locate the running launcher: %v", err)
	}
	exePath, err = filepath.Abs(exePath)
	if err != nil {
		return "", err
	}
	// A folder-mode release zip carries its runtime files beside the
	// executable inside the bundle — index.html, apps.json, an Apps/
	// folder. They land next to the installed exe so the deck keeps working
	// when the update crosses build modes (all-in-one ↔ folder-mode).
	companions, err := installRuntimeCompanions(zr, pick, exePath)
	if err != nil {
		return "", fmt.Errorf("installing the launcher's runtime files: %v", err)
	}
	switched := ""
	if embeddedBuild && zipHasFolderRuntime(body) {
		kept, err := s.materializeFolderDeck(exePath)
		if err != nil {
			return "", fmt.Errorf("keeping the installed deck across the switch to folder mode: %v", err)
		}
		switched = fmt.Sprintf("; switched this install to folder mode with %d app(s) kept — the deck registry now lives beside the launcher (apps stay in the data directory)", kept)
	}
	newPath := exePath + ".new"
	// Windows cannot replace a running exe by rename alone: the aside-name
	// is only a structural step of the swap (freed again below and at the
	// next start), never a kept backup. Inside a .app bundle nothing extra
	// may land in Contents/MacOS anyway — it would break the signature.
	oldPath := exePath + ".old"
	if i := strings.Index(exePath, ".app/Contents/"); i > 0 {
		// Next to the bundle, not inside it: anything in the bundle root
		// invalidates the Gatekeeper signature ("unsealed contents").
		bundle := exePath[:i+4]
		oldPath = filepath.Join(filepath.Dir(bundle), filepath.Base(exePath)+".old")
	}
	if err := os.WriteFile(newPath, exe, 0o755); err != nil {
		return "", fmt.Errorf("staging the new launcher: %v", err)
	}
	if err := os.Chmod(newPath, 0o755); err != nil {
		os.RemoveAll(newPath)
		return "", err
	}
	if runtime.GOOS == "windows" {
		// Windows locks a running executable's path: replace-by-rename fails.
		// The dance that works: rename the running exe aside (allowed while
		// running), then rename the new one into the freed name.
		_ = os.Remove(oldPath)
		if err := os.Rename(exePath, oldPath); err != nil {
			os.RemoveAll(newPath)
			return "", fmt.Errorf("moving the running launcher aside: %v", err)
		}
		if err := os.Rename(newPath, exePath); err != nil {
			_ = os.Rename(oldPath, exePath) // put the running one back
			os.RemoveAll(newPath)
			return "", fmt.Errorf("swapping in the new launcher: %v", err)
		}
		// Usually refused while the old image still runs; the boot sweep
		// below finishes the job either way.
		_ = os.Remove(oldPath)
	} else {
		// POSIX: the atomic replace needs no aside step and no backup.
		if err := os.Rename(newPath, exePath); err != nil {
			os.RemoveAll(newPath)
			return "", fmt.Errorf("swapping in the new launcher: %v", err)
		}
	}
	if runtime.GOOS == "darwin" {
		// Best effort: the launcher is ad-hoc signed at release time, and
		// Gatekeeper checks the modified bundle on next start.
		_ = exec.Command("codesign", "--force", "-s", "-", exePath).Run()
	}
	note := "launcher updated — restart AppDeck to run the new version"
	if runtime.GOOS == "windows" {
		note += " (the previous binary is removed on restart)"
	}
	if companions > 0 {
		note += fmt.Sprintf("; installed %d runtime file(s) beside the launcher (folder-mode release)", companions)
	}
	note += switched
	return note, nil
}

// removeStaleUpdateBackup deletes the <exe>.old a Windows self-update had
// to leave behind (the renamed-aside image could not be removed while it
// was still the running one) and any .new stage a failed update left.
// Best effort; POSIX updates leave nothing behind at all.
func removeStaleUpdateBackup() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	for _, p := range []string{exePath + ".new", exePath + ".old"} {
		_ = os.Remove(p)
	}
	if i := strings.Index(exePath, ".app/Contents/"); i > 0 {
		bundle := exePath[:i+4]
		_ = os.Remove(filepath.Join(filepath.Dir(bundle), filepath.Base(exePath)+".old"))
	}
}

// materializeFolderDeck carries the embedded deck across an update to a
// folder-mode launcher. Folder mode reads its registry from apps.json beside
// the executable and never scans the data directory's app store, so the swap
// alone would empty the deck. The live app list is written out with
// data-dir-relative paths — the folder-mode loader resolves app folders and
// binaries against the data directory when they are missing beside the exe —
// and hosted apps whose assets lived only inside the old exe get those
// assets unpacked to disk first. Returns how many apps the new registry
// keeps. The zip's own (empty) apps.json never overwrites this one:
// installRuntimeCompanions skips existing registry files.
func (s *Server) materializeFolderDeck(exePath string) (int, error) {
	apps := s.appsList()
	out := struct {
		Apps []*App `json:"apps"`
	}{Apps: make([]*App, 0, len(apps))}
	unpacked := 0
	for _, a := range apps {
		if a == nil || a.ID == "" {
			continue
		}
		c := *a
		// Hosted apps whose assets live only inside the running exe have no
		// folder on disk; a folder-mode launcher has no embedded FS to serve
		// them from. Unpack the packaged assets so they keep working.
		if c.ServedBy == "launcher" && c.Dir == "" {
			appDir := filepath.Join(s.cfg.DataDir, "Apps", slugify(c.ID))
			if err := extractTree(embeddedFS, "embedded/assets/"+c.ID, appDir, false); err != nil {
				return 0, fmt.Errorf("unpack assets for %s: %v", c.ID, err)
			}
			if entries, err := os.ReadDir(appDir); err != nil || len(entries) == 0 {
				continue // nothing was packaged for this id — cannot keep it
			}
			c.RawDir = filepath.ToSlash(filepath.Join("Apps", slugify(c.ID)))
			unpacked++
		}
		out.Apps = append(out.Apps, &c)
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return 0, err
	}
	regPath := filepath.Join(filepath.Dir(exePath), "apps.json")
	if err := os.WriteFile(regPath, append(data, '\n'), 0o644); err != nil {
		return 0, fmt.Errorf("writing %s: %v", filepath.Base(regPath), err)
	}
	fmt.Fprintf(os.Stderr, "launcher: folder-mode update: %d app(s) materialized beside the launcher (%d asset folder(s) unpacked)\n",
		len(out.Apps), unpacked)
	return len(out.Apps), nil
}

// installRuntimeCompanions writes a folder-mode zip's runtime files —
// everything sitting beside the picked executable inside Contents/MacOS/ —
// next to the installed launcher: plain files (index.html, apps.json,
// icons) overwrite; the Apps/ tree is written only where nothing exists
// yet, so an installed deck is never clobbered by an update. Returns how
// many files were written.
func installRuntimeCompanions(zr *zip.Reader, pick *zip.File, exePath string) (int, error) {
	prefix, _ := filepath.Split(filepath.ToSlash(pick.Name))
	if prefix == "" {
		return 0, nil // bare-binary zip: everything is inside the executable
	}
	destDir := filepath.Dir(exePath)
	written := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || f.Name == pick.Name {
			continue
		}
		name := filepath.ToSlash(f.Name)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(name, prefix)
		if rel == "" || isJunkEntry(rel) {
			continue
		}
		target := filepath.Join(destDir, filepath.FromSlash(rel))
		if strings.HasPrefix(rel, "Apps/") || rel == "apps.json" {
			if _, err := os.Stat(target); err == nil {
				continue // never overwrite the installed deck or its registry
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return written, err
		}
		data, err := readZipEntryRaw(f)
		if err != nil {
			return written, err
		}
		mode := f.Mode()
		if mode.IsRegular() && mode.Perm()&0o111 != 0 {
			mode = 0o755
		} else {
			mode = 0o644
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// readZipEntryRaw returns a file entry's contents without the executable
// sniff (runtime files are data, not programs).
func readZipEntryRaw(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, installMaxTotal+1))
}

// zipHasFolderRuntime reports whether an AppDeck.app zip carries the
// companion files used by folder-mode builds (index.html/apps.json beside
// the launcher) — the marker of a folder-mode release zip.
func zipHasFolderRuntime(body []byte) bool {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return false
	}
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		if strings.HasSuffix(name, "/Contents/MacOS/index.html") ||
			strings.HasSuffix(name, "/Contents/MacOS/apps.json") {
			return true
		}
	}
	return false
}

// launcherExecutableFromZip finds the launcher executable inside a zip: the
// Mach-O/PE/ELF file of an app bundle's MacOS folder, or a single bare file.
// Anything else is refused with what was actually found. Returns the bytes
// and the picked entry (the update flow extracts its neighbors).
func launcherExecutableFromZip(zr *zip.Reader) ([]byte, *zip.File, error) {
	var bare, bundle []string
	var bareFile, bundleFile *zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || isJunkEntry(f.Name) {
			continue
		}
		name := filepath.ToSlash(f.Name)
		isCandidate := false
		if i := strings.LastIndex(name, "Contents/MacOS/"); i >= 0 {
			rest := name[i+len("Contents/MacOS/"):]
			isCandidate = rest != "" && !strings.Contains(rest, "/")
		} else if !strings.Contains(name, "/") {
			isCandidate = true
		}
		if !isCandidate {
			continue
		}
		executable, err := zipEntryLooksExecutable(f)
		if err != nil {
			return nil, nil, fmt.Errorf("reading %s from zip: %w", name, err)
		}
		if !executable {
			continue
		}
		if strings.Contains(name, "Contents/MacOS/") {
			bundle = append(bundle, name)
			bundleFile = f
		} else {
			bare = append(bare, name)
			bareFile = f
		}
	}
	// An app bundle wins over stray files; exactly one bare file is the
	// fallback; anything else is ambiguous and refused.
	if len(bundle) > 1 {
		return nil, nil, fmt.Errorf("the zip holds several launcher executables (%s) — send the one launcher you want to install", strings.Join(bundle, ", "))
	}
	pick := bundleFile
	if pick == nil {
		if len(bare) == 1 {
			pick = bareFile
		} else if len(bare) > 1 {
			return nil, nil, fmt.Errorf("the zip holds several files (%s) — send a zip with just the launcher executable", strings.Join(bare, ", "))
		}
	}
	if pick == nil {
		return nil, nil, fmt.Errorf("the zip holds no launcher executable — include the launcher binary or an AppDeck.app bundle")
	}
	data, err := readZipEntryRaw(pick)
	if err != nil {
		return nil, nil, err
	}
	if !looksExecutable(data) {
		return nil, nil, fmt.Errorf("%s in the zip is not an executable", filepath.Base(pick.Name))
	}
	if why := foreignBinaryReason(data); why != "" {
		return nil, nil, fmt.Errorf("%s in the zip is a %s — this launcher runs on %s/%s; send the build for this platform",
			filepath.Base(pick.Name), why, runtime.GOOS, runtime.GOARCH)
	}
	if why := foreignArchReason(data); why != "" {
		return nil, nil, fmt.Errorf("%s in the zip is built for %s — this launcher runs on %s/%s; send the build for this platform",
			filepath.Base(pick.Name), why, runtime.GOOS, runtime.GOARCH)
	}
	return data, pick, nil
}

// foreignBinaryReason names a loader magic that cannot run on this machine:
// a PE on macOS/Linux, an ELF on macOS/Windows, a Mach-O elsewhere — the
// class of mistake where a cross-platform release zip installs a launcher
// that can never boot on the machine that downloaded it.
func foreignBinaryReason(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	pe := bytes.HasPrefix(b, []byte("MZ"))
	elf := bytes.HasPrefix(b, []byte{0x7F, 'E', 'L', 'F'})
	var macho bool
	if !pe && !elf {
		be := binary.BigEndian.Uint32(b)
		le := binary.LittleEndian.Uint32(b)
		const (
			macho32 = 0xFEEDFACE
			macho64 = 0xFEEDFACF
			fatBe   = 0xCAFEBABE
			fatLe   = 0xBEBAFECA
		)
		macho = be == macho32 || be == macho64 || be == fatBe ||
			le == macho32 || le == macho64 || le == fatLe
	}
	other := ""
	switch {
	case pe:
		other = "a Windows PE binary"
	case elf:
		other = "a Linux ELF binary"
	case macho:
		other = "a macOS Mach-O binary"
	}
	if other != "" && ((pe && runtime.GOOS != "windows") ||
		(elf && runtime.GOOS != "linux") || (macho && runtime.GOOS != "darwin")) {
		return other
	}
	return ""
}

// foreignArchReason reads the CPU architecture out of a same-OS binary
// header and names it when this machine could never run it. Fail-open: an
// unparseable or multi-arch (fat) header returns "" — today's behavior.
// Rosetta is respected on macOS (an amd64 binary on arm64 is fine), where
// Linux and Windows have no such translation layer.
func foreignArchReason(b []byte) string {
	le16 := binary.LittleEndian.Uint16
	le32 := binary.LittleEndian.Uint32
	host := runtime.GOARCH
	if bytes.HasPrefix(b, []byte{0x7F, 'E', 'L', 'F'}) && len(b) >= 20 {
		switch le16(b[18:20]) {
		case 62: // EM_X86_64
			if host == "arm64" || host == "386" {
				return "linux/amd64"
			}
		case 183: // EM_AARCH64
			if host != "arm64" {
				return "linux/arm64"
			}
		case 3: // EM_386
			if host == "amd64" || host == "arm64" {
				return "linux/386"
			}
		}
		return ""
	}
	if bytes.HasPrefix(b, []byte("MZ")) && len(b) >= 0x40 {
		if off := int(le32(b[0x3C:0x40])); off > 0 && off+6 <= len(b) &&
			bytes.HasPrefix(b[off:off+4], []byte("PE\x00\x00")) {
			switch le16(b[off+4 : off+6]) {
			case 0x8664: // IMAGE_FILE_MACHINE_AMD64
				if host == "arm64" || host == "386" {
					return "windows/amd64"
				}
			case 0xAA64: // ARM64
				if host != "arm64" {
					return "windows/arm64"
				}
			case 0x014C: // I386
				if host == "amd64" || host == "arm64" {
					return "windows/386"
				}
			}
		}
		return ""
	}
	const (
		macho64LE = 0xFEEDFACF // little-endian MH_MAGIC_64
		macho64BE = 0xCFFAEDFE
		cpuX86_64 = 0x01000007
		cpuARM64  = 0x0100000C
		cpuABI64  = 0x01000000
		cpuI386   = 7
	)
	be32 := binary.BigEndian.Uint32
	if len(b) >= 8 && (le32(b[0:4]) == macho64LE || be32(b[0:4]) == macho64BE) {
		cputype := le32(b[4:8])
		switch cputype {
		case cpuX86_64:
			if host == "arm64" {
				return "" // Rosetta runs it
			}
			if host != "amd64" {
				return "macOS x86_64"
			}
		case cpuARM64:
			if host != "arm64" {
				return "macOS arm64"
			}
		case cpuI386:
			if host == "arm64" {
				return "macOS i386"
			}
		}
	}
	// 32-bit thin Mach-O and fat binaries: fat carries every arch, 32-bit
	// Mach-O is a legacy shape — both stay fail-open.
	return ""
}

func zipEntryLooksExecutable(f *zip.File) (bool, error) {
	rc, err := f.Open()
	if err != nil {
		return false, err
	}
	defer rc.Close()
	header, err := io.ReadAll(io.LimitReader(rc, 4))
	if err != nil {
		return false, err
	}
	return looksExecutable(header), nil
}

// looksExecutable sniffs the loader magics the launcher ships as: Mach-O
// (thin big/little-endian and fat), PE (MZ), ELF.
func looksExecutable(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	if bytes.HasPrefix(b, []byte{0x7F, 'E', 'L', 'F'}) || bytes.HasPrefix(b, []byte("MZ")) {
		return true
	}
	be := binary.BigEndian.Uint32(b)
	le := binary.LittleEndian.Uint32(b)
	const (
		macho32 = 0xFEEDFACE
		macho64 = 0xFEEDFACF
		fatBe   = 0xCAFEBABE
		fatLe   = 0xBEBAFECA
	)
	return be == macho32 || be == macho64 || be == fatBe ||
		le == macho32 || le == macho64 || le == fatLe
}
