// Import router: the type-aware front door for bringing apps into the deck.
// One endpoint — POST /api/import — accepts any of these sources and normalizes
// every one of them into the same outcome: a validated app folder under the
// deck's Apps container.
//
//	archive (zip/tar.gz/tar)  → the existing archive installer (app.json at root)
//	portable .exe/.binary     → wrapped as an app in place — never executed
//	                            (.AppImage and ELF on a Linux deck)
//	installer .exe            → refused, or (Windows, explicit opt-in) a silent
//	                            install confined to the app folder
//	single .html file         → a servedBy-launcher static app
//	local folder              → validated + copied into the container
//	git repository URL        → shallow clone, then routed like a folder —
//	                            a missing app.json is generated from the
//	                            project layout, a missing icon is found in
//	                            the repo or generated (GitHub tree URLs
//	                            import just one folder of a monorepo)
//	URL                       → downloaded, then routed by sniffed content
//
// Import never executes the imported file: the only code that runs at import
// time is the installer the user explicitly opted into on Windows. Everything
// lands in the deck's app container — there is no install path to ask about,
// which is what keeps the deck portable.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// Same caps as the archive installer: a 512 MB upload ceiling and a
	// 20 MB ceiling for single-file HTML apps.
	importMaxBytes = 512 << 20
	importMaxHTML  = 20 << 20
	// A silent install gets one timeout for the whole run; installers that
	// need longer are the ones that pop UIs — exactly what we refuse.
	silentInstallTimeout = 15 * time.Minute
)

// installerSig holds the byte signatures of the common Windows installer
// frameworks. Detection is content-based (the bytes cannot lie about being
// an installer the way a file name can), scanned over the first 2 MB.
var installerSigs = []struct {
	kind string
	sig  string
}{
	{"nsis", "NullsoftInst"},
	{"inno", "Inno Setup"},
	{"installshield", "InstallShield"},
}

func isEmbeddedAppDir(dataDir, dest, id string) bool {
	if dataDir == "" || id == "" {
		return false
	}
	expected, err := filepath.Abs(embeddedOverrideDir(dataDir, id))
	if err != nil {
		return false
	}
	actual, err := filepath.Abs(dest)
	return err == nil && filepath.Clean(actual) == filepath.Clean(expected)
}

func (s *Server) stopAppBeforeUpdate(id string) error {
	app := s.appByID(id)
	if app == nil {
		return nil
	}
	st := loadState(s.cfg.StateFile)
	running := st[id].PID != 0 && pidAlive(st[id].PID)
	if !running {
		running = s.appStatus(app, st).Running
	}
	if app.ServedBy == "launcher" {
		s.mu.Lock()
		running = running || s.hostedListeners[id] != nil
		s.mu.Unlock()
	}
	if !running {
		return nil
	}
	res := s.stopApp(app)
	if !res.OK {
		return fmt.Errorf("stop %s before update: %s", id, res.Error)
	}
	return nil
}

// installerKind classifies an executable without running it: "" = portable
// program, otherwise the detected installer framework ("msi" comes from the
// OLE compound-file magic every .msi carries).
func installerKind(name string, body []byte) string {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".msi") ||
		(len(body) >= 8 && bytes.HasPrefix(body, []byte{0xD0, 0xCF, 0x11, 0xE0})) {
		return "msi"
	}
	scan := body
	if len(scan) > 2<<20 {
		scan = scan[:2<<20]
	}
	for _, sig := range installerSigs {
		if bytes.Contains(scan, []byte(sig.sig)) {
			return sig.kind
		}
	}
	base := strings.ToLower(filepath.Base(name))
	for _, word := range []string{"setup", "installer", "install_"} {
		if strings.Contains(base, word) {
			return "unknown-installer"
		}
	}
	return ""
}

// looksLikeHTML: the sniff for single-file app imports. Trimmed, lowercase
// prefix checks — enough to tell a page from a binary without a full parser.
func looksLikeHTML(body []byte) bool {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) < 6 {
		return false
	}
	head := trimmed
	if len(head) > 4096 {
		head = head[:4096]
	}
	lower := strings.ToLower(string(head))
	return strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html")
}

// serveImport is the /api/import entry point. Sources are picked from query
// parameters; a raw body is the file itself. Loopback is enforced by the
// route (imports write into the app container).
func (s *Server) serveImport(w http.ResponseWriter, r *http.Request) {
	qs := parseQuery(r.RequestURI)
	// parseQuery hands back raw query text; the frontend sends
	// encodeURIComponent values, so the human-facing parameters are decoded
	// here (PathUnescape keeps "+" literal, which file names may contain).
	for _, k := range []string{"name", "appname", "args", "folder", "url", "icon"} {
		if v := qs[k]; v != "" {
			if dec, err := url.PathUnescape(v); err == nil {
				qs[k] = dec
			}
		}
	}
	// The dashboard's floating progress dock watches a job handle it
	// generated; every milestone below streams into it. ?stall=<minutes>
	// is the no-progress timeout (0 = no limit, default 5): the job's
	// watchdog cancels an import that logs nothing for that long.
	job := s.job(qs["jobid"])
	// Name the job for the dashboard's dock: a hand-over from another page
	// (RepoLens) carries ?appname=, and the dock titles it with that once the
	// jobs list attaches to it.
	job.setLabel(qs["appname"])
	// A URL import remembers where it came from — the success tail stamps
	// it into the installed manifest (update-from-origin).
	job.setOrigin(qs["url"])
	if raw := qs["stall"]; raw != "" {
		if mins, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && mins >= 0 && mins <= 1440 {
			job.setStall(time.Duration(mins) * time.Minute)
		}
	}
	job.logf("import started")
	if importCanceled(w, job) {
		return
	}
	// The package-manager fallback: install a source-only project (C/C++
	// and friends) from the OS's own channel and add a ⌨ CLI card — the
	// dialog offers this after the plain import refused (see pkgmanager.go).
	if qs["install"] == "pkg" {
		s.installPackageApp(w, qs, job)
		return
	}
	if rawurl := qs["url"]; rawurl != "" {
		// A git repository URL clones instead of downloading a file: the
		// whole repo, or — GitHub tree URLs — one folder inside it.
		if src, ok := parseGitURL(rawurl); ok {
			job.logf("git repository detected: %s", src.repo)
			s.importGit(w, src, qs, job)
			return
		}
		job.logf("downloading %s", rawurl)
		body, name, err := fetchForImportCtx(job.Context(), rawurl, job)
		if err != nil {
			job.logf("download failed: %v", err)
			if job.Canceled() {
				importCanceled(w, job)
				return
			}
			job.finish(err.Error())
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		// Downloads land in memory (512 MB cap) — the per-format staging
		// below writes its own temp files and logs those paths.
		job.logf("downloaded %s (%d KB) into memory", name, len(body)/1024)
		if qs["name"] == "" {
			qs["name"] = name
		}
		s.importBytes(w, body, qs, job)
		return
	}
	if dir := qs["folder"]; dir != "" {
		job.logf("importing local folder %s", dir)
		s.importFolder(w, dir, job)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, importMaxBytes+1))
	if err != nil {
		job.logf("reading upload failed: %v", err)
		job.finish(err.Error())
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading upload: " + err.Error()})
		return
	}
	job.logf("upload received (%d KB)", len(body)/1024)
	s.importBytes(w, body, qs, job)
}

// fetchForImport downloads a remote file (release asset, shared zip) with the
// same size cap as uploads.
func fetchForImport(rawurl string) ([]byte, string, error) {
	return fetchForImportCtx(context.Background(), rawurl, nil)
}

// fetchForImportCtx is fetchForImport under a cancellation context: the
// request dies with the context (the import's Cancel button or its
// no-progress watchdog). Progress is logged every 64 MB so a healthy
// download keeps the watchdog's clock alive — only a truly silent
// connection trips it. A nil job logs nothing.
func fetchForImportCtx(ctx context.Context, rawurl string, job *jobLog) ([]byte, string, error) {
	if !strings.HasPrefix(rawurl, "http://") && !strings.HasPrefix(rawurl, "https://") {
		return nil, "", fmt.Errorf("url must be http(s)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, "", fmt.Errorf("fetch: %v", err)
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", errImportCanceled
		}
		return nil, "", fmt.Errorf("fetch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch: HTTP %d", resp.StatusCode)
	}
	// Progress every 64 MB (the 512 MB cap bounds the line count), so big
	// downloads stream milestones into the import log and the watchdog.
	body, err := io.ReadAll(io.LimitReader(&progressReader{r: resp.Body, job: job, every: 64 << 20}, importMaxBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", errImportCanceled
		}
		return nil, "", fmt.Errorf("fetch: %v", err)
	}
	if len(body) > importMaxBytes {
		return nil, "", fmt.Errorf("download too large (limit %d bytes)", importMaxBytes)
	}
	name := ""
	if i := strings.LastIndexAny(rawurl, "/\\"); i >= 0 && i+1 < len(rawurl) {
		name = rawurl[i+1:]
	}
	if name == "" || strings.HasSuffix(name, "/") {
		name = "download.bin"
	}
	return body, name, nil
}

// importBytes sniffs the upload and routes it to the matching installer.
func (s *Server) importBytes(w http.ResponseWriter, body []byte, qs map[string]string, job *jobLog) {
	if importCanceled(w, job) {
		return
	}
	if len(body) == 0 {
		job.finish("empty upload")
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty upload"})
		return
	}
	if len(body) > importMaxBytes {
		msg := fmt.Sprintf("file too large (limit %d bytes)", importMaxBytes)
		job.finish(msg)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	name := qs["name"]
	if msg := importExtRefusal(name); msg != "" {
		job.finish(msg)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	// Security gate: every byte payload is scanned before it can land. A
	// critical verdict (ClamAV/VT detection, EICAR, an executable disguised
	// as a document) stages the payload and pauses — the dock shows the
	// findings with Install anyway / Cancel. Warnings flow through as amber
	// lines. The replay after "Install anyway" carries an approved jobid.
	jobID := qs["jobid"]
	if securityJobApproved(jobID) {
		// The operator already said Install anyway for this job: drop any
		// stale pause artifacts and let the replay through untouched.
		s.securityClearPendingForJob(jobID)
	} else {
		findings, sha := s.scanPayload(job, body, name)
		if hasCritical(findings) {
			if jobID == "" {
				// No dock to prompt in (a raw curl without a jobid):
				// refuse rather than install flagged bytes unattended.
				msg := "critical security findings — install refused: " + secFindingText(findings[0])
				job.finish(msg)
				s.writeJSON(w, http.StatusForbidden, map[string]string{"error": msg})
				return
			}
			scanID := s.securityStagePending(job, jobID, body, name, qs, findings, sha)
			job.logf("SECURITY: install paused — review the findings, then Install anyway or Cancel")
			s.writeJSON(w, http.StatusAccepted, map[string]any{
				"ok": false, "pendingSecurity": true, "scanId": scanID, "findings": findings,
			})
			return
		}
	}
	switch {
	case looksLikeDmg(name, body):
		job.logf("macOS disk image detected")
		s.installDmg(w, body, name, qs, job)
	case isDebBody(body):
		job.logf("Debian package detected")
		s.installDeb(w, body, name, qs, job)
	case detectArchiveFormat(body) != "":
		job.logf("archive detected (%s)", detectArchiveFormat(body))
		s.installUpload(w, body, qs, job)
	case len(body) >= 2 && body[0] == 'M' && body[1] == 'Z':
		job.logf("portable executable detected")
		s.importBinary(w, body, name, qs, job)
	case isELFBody(body):
		// AppImages and bare Linux release binaries are ELF; they only run on
		// a Linux deck — the same host policy the release-asset path and the
		// update checks enforce.
		if runtime.GOOS != "linux" {
			msg := "AppImage/ELF binaries run on Linux — this AppDeck is running on " + runtime.GOOS
			job.finish(msg)
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
			return
		}
		job.logf("Linux binary/AppImage detected")
		s.importBinary(w, body, name, qs, job)
	case looksLikeHTML(body):
		job.logf("single HTML page detected — hosted app")
		s.importHTML(w, body, name, qs, job)
	case installerKind(name, body) == "msi":
		// An MSI never reaches importBinary's installer refusal (its body
		// carries the OLE compound-file magic, not an MZ header), so without
		// this branch the operator got the generic "unsupported file" error
		// with no hint at the portable build. The refusal itself is policy:
		// no safe silent+directory flags exist for msiexec.
		msg := fmt.Sprintf("%s is a Windows Installer package (MSI) — no safe silent install into the "+
			"app folder exists for it — import the app's portable .exe build instead", name)
		job.finish(msg)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
	default:
		msg := "unsupported file — send an app archive (zip/tar.gz/7z/rar with app.json), a portable .exe, " +
			"a Linux .AppImage, a single .html page, a folder path (?folder=), a git repository URL " +
			"(?url=https://github.com/user/repo or …/tree/main/folder), or a URL (?url=)"
		job.finish(msg)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
	}
}

// installUpload routes a sniffed archive into the existing installer. A
// third-party release archive without app.json falls back to wrapping its
// single program as a desktop app.
func (s *Server) installUpload(w http.ResponseWriter, body []byte, qs map[string]string, job *jobLog) {
	// Release zips sometimes wrap the real payload in another archive —
	// mpv's macOS builds ship a zip whose only entry is mpv.tar.gz. When
	// the archive holds a single regular entry that is itself a readable
	// archive, peel it and install that; bounded, so nesting can't run
	// away.
	for peel := 0; peel < maxArchiveWrapping; peel++ {
		inner, innerName, ok := singleInnerArchive(body)
		if !ok {
			break
		}
		job.logf("the archive carries a single inner archive (%s) — unwrapping it", innerName)
		body = inner
	}
	job.logf("scanning archive (manifest, bomb guards)…")
	app, err := s.installArchiveBytes(body, job)
	if err != nil {
		if strings.Contains(err.Error(), "no app.json") && s.installBareArchive(w, body, qs, job) {
			return
		}
		job.finish(err.Error())
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	job.logf("app %s installed into the deck", app.ID)
	s.importDone(w, app, job)
}

// importDone is the shared success tail: rescan the registry so the new app
// appears on the dashboard, then report the id.
func (s *Server) importDone(w http.ResponseWriter, app *App, job *jobLog) {
	s.importDoneNote(w, app, "imported and discovered — it appears on the dashboard now", job)
}

// importDoneNote is importDone with a flow-specific message (the releases
// path names the asset and tag it installed).
func (s *Server) importDoneNote(w http.ResponseWriter, app *App, note string, job *jobLog) {
	if err := s.reloadApps(); err != nil {
		job.finish(fmt.Sprintf("imported %s, but rescanning the registry failed: %v", app.ID, err))
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("imported %s, but rescanning the registry failed: %v", app.ID, err)})
		return
	}
	// Remember where this app came from: an import started with ?url= (a
	// git repository, a release asset, a download) stamps the URL into the
	// installed manifest, and the card's ↻ action re-imports from it in
	// place later. Local imports have no origin and stay untouched.
	if origin := job.originURL(); origin != "" {
		s.stampOrigin(app.ID, origin)
	}
	// The deck just changed shape — refresh the automatic backup.
	go s.snapshotDeck()
	// Post-land security scan: heuristics over everything that just landed,
	// verdict persisted (the ⚠ badge), findings as amber lines. Warn-level
	// only — an install that passed the pre-land gate is never removed.
	s.securityScanInstalled(app, job)
	// The note is a log line and the job finishes CLEAN: finish's argument
	// is the failure reason, and a non-empty one renders the whole dock red
	// even on a successful import.
	job.succeed(note)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"id":   app.ID,
		"name": app.Name,
		"note": note,
	})
}

// stampOrigin writes origin into the installed app's manifest (and the
// in-memory copy) so /api/apps reports it and the next reload keeps it. The
// manifest is read as a generic map: fields the launcher does not model are
// preserved verbatim instead of being dropped by a struct round-trip.
func (s *Server) stampOrigin(id, origin string) {
	a := s.appByID(id)
	if a == nil || a.Dir == "" {
		return
	}
	path := filepath.Join(a.Dir, manifestFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return // no manifest to stamp (registry/discovered app) — nothing to remember
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return // a manifest that is not an object is left alone
	}
	doc["origin"] = origin
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return
	}
	s.mu.Lock()
	a.Origin = origin
	s.mu.Unlock()
}

// sniffBinaryKind guesses whether a portable binary serves a web page
// (→ "web") or is a plain GUI/CLI program (→ "desktop") from the server
// markers its compiler embeds — Go's net/http symbols and the Rust/Go web
// framework names survive in compiled binaries. Best effort by design: it
// only runs when the import explicitly asks for kind=auto, a false "web"
// merely gives the card a port it never uses, and an explicit desktop/web
// always wins.
func sniffBinaryKind(body []byte) string {
	scan := body
	if len(scan) > 2<<20 {
		scan = scan[:2<<20]
	}
	for _, marker := range []string{
		"ListenAndServe", "net/http", "http.Server", // Go standard library
		"axum", "actix-web", "gin-gonic", "rocket", "warp", // web frameworks
	} {
		if bytes.Contains(scan, []byte(marker)) {
			return "web"
		}
	}
	return "desktop"
}

// binaryNameNoise lists the packaging words release binary names carry:
// target OS/distro, architecture, release channel, build metadata. Mirrors
// the matchers in releases.go (assetOSKind, assetArchKind); "appimage"
// itself is noise too ("Brave-AppImage" is the packaging, not the app).
var binaryNameNoise = []string{
	"linux", "win", "windows", "win32", "win64", "mingw", "mingw64", "msvc",
	"mac", "macos", "macosx", "osx", "darwin", "apple", "ubuntu", "debian",
	"fedora", "arch", "centos", "opensuse", "manylinux", "appimage", "musl",
	"gnu", "glibc",
	"x86", "x64", "x86_64", "x86-64", "amd64", "amd", "arm", "arm64",
	"aarch64", "i386", "i686", "universal",
	"nightly", "stable", "beta", "alpha", "dev", "canary", "rc", "snapshot",
	"build", "release", "portable", "latest",
}

// isVersionToken reports whether a name token is a version/build counter:
// digit-led ("1.100.13", "20231001", "64") or a v-prefixed one ("v1", "v2.3").
func isVersionToken(tok string) bool {
	if tok == "" {
		return false
	}
	if tok[0] >= '0' && tok[0] <= '9' {
		return true
	}
	return (tok[0] == 'v' || tok[0] == 'V') && len(tok) > 1 && tok[1] >= '0' && tok[1] <= '9'
}

// cleanBinaryAppName turns a binary's file stem into a human display name by
// dropping the packaging noise release files carry — platform, architecture,
// version and channel tokens ("Brave-nightly-v1.100.13-x86_64" → "Brave").
// Tokens naming the program are kept verbatim; if every token is noise the
// stem is returned unchanged. Runs only when the import supplied no explicit
// ?appname= — a user-typed name is never rewritten.
func cleanBinaryAppName(stem string) string {
	tokens := strings.FieldsFunc(stem, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == '+' || r == ' '
	})
	kept := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if isVersionToken(tok) || slices.Contains(binaryNameNoise, strings.ToLower(tok)) {
			continue
		}
		if strings.ToLower(tok) == tok { // only all-lowercase words; "iPhone" keeps its case
			if b := tok[0]; b >= 'a' && b <= 'z' {
				tok = string(b-'a'+'A') + tok[1:]
			}
		}
		kept = append(kept, tok)
	}
	if len(kept) == 0 {
		return stem
	}
	return strings.Join(kept, " ")
}

// importBinary wraps a portable executable (a Windows .exe, a Linux
// .AppImage/ELF) into a deck app, or — for detected installers, on Windows,
// with &silent=1 — attempts a silent install confined to the app folder.
func (s *Server) importBinary(w http.ResponseWriter, body []byte, name string, qs map[string]string, job *jobLog) {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "" || name == "/" || name == "." {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "binary import needs the file name (?name=tool.exe / app.AppImage) so the binary keeps its name"})
		return
	}
	// The installer heuristics are Windows-installer lore (NSIS/Inno/MSI
	// signatures, "setup" file names): they only apply to PE bodies. An ELF
	// is never one of those installers, and a Linux binary whose name happens
	// to contain "setup" must not hit the installer refusal.
	isPE := len(body) >= 2 && body[0] == 'M' && body[1] == 'Z'
	if kind := installerKind(name, body); isPE && kind != "" && !strings.HasSuffix(name, "-portable") {
		if qs["silent"] != "1" {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("%s looks like an INSTALLER (%s), not a portable program — "+
					"import the app's own portable .exe instead, or re-send with &silent=1 to attempt "+
					"a silent install into the app folder (NSIS/Inno only, Windows only)", name, kind)})
			return
		}
		if runtime.GOOS != "windows" {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "silent installer runs are a Windows flow — import the app's portable binary instead"})
			return
		}
		if kind == "msi" || kind == "unknown-installer" {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("%s: no safe silent+directory flags are known for this installer kind (%s) — "+
					"import the app's portable .exe instead", name, kind)})
			return
		}
	}
	// Auto-detect runs when the import asks for it (the dialog's default):
	// a binary that carries web-server markers becomes a ported web card,
	// everything else a portless desktop card.
	if qs["kind"] == "auto" {
		qs["kind"] = sniffBinaryKind(body)
	}

	appname := strings.TrimSpace(qs["appname"])
	if appname == "" {
		appname = cleanBinaryAppName(strings.TrimSuffix(name, filepath.Ext(name)))
	}
	dest, finalize, err := s.importDest(appname, w)
	if err != nil {
		return // the response is already written
	}
	// Installer flow: the installer binary itself lands in the folder, runs
	// quietly with directory flags pointed at it, and the largest produced
	// exe becomes the app. The installer file is removed afterwards.
	if kind := installerKind(name, body); isPE && kind != "" {
		if err := runSilentInstall(dest, name, body, kind); err != nil {
			os.RemoveAll(dest)
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		bin, err := producedExe(dest, name)
		if err != nil {
			os.RemoveAll(dest)
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		os.Remove(filepath.Join(dest, name))
		app, err := writeWrapManifest(dest, bin, appname, qs, job, "")
		if err != nil {
			os.RemoveAll(dest)
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := s.finalizeImportDest(dest, app.ID, finalize, w, job); err != nil {
			return
		}
		s.importDone(w, app, job)
		return
	}

	// Portable flow: the file IS the app — write it untouched.
	if err := os.WriteFile(filepath.Join(dest, name), body, 0o755); err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	stampMotW(filepath.Join(dest, name), body) // Windows: SmartScreen sees downloaded executables
	// Windows: pull the program's own icon into the folder so the tile shows
	// the real icon (extracted via System.Drawing; skipped when it fails —
	// the emoji from ?icon= stays the fallback). AppImages carry their icon
	// inside (.DirIcon) — read it out without executing the image.
	if runtime.GOOS == "windows" {
		extractExeIconPNG(filepath.Join(dest, name), filepath.Join(dest, "icon.png"))
	} else if isAppImageBody(body) {
		extractAppImageIcon(body, filepath.Join(dest, name), dest)
	}
	// A Mach-O upload is verified where its seal can actually be checked
	// (darwin): byte-identical standalone binaries pass, everything else is
	// re-signed ad hoc. PE/ELF decks and PE/ELF uploads have no macOS seal —
	// "" state, no noise.
	sig := ""
	if isMachOBody(body) {
		sig = importSignatureState(filepath.Join(dest, name), false, job)
	}
	app, err := writeWrapManifest(dest, name, appname, qs, job, sig)
	if err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.finalizeImportDest(dest, app.ID, finalize, w, job); err != nil {
		return
	}
	s.importDone(w, app, job)
}

// finalizeImportDest runs the staged-import swap for flows that got a
// staging folder from importDest. A failed swap is reported and the staging
// folder removed — the previously installed app was never touched.
func (s *Server) finalizeImportDest(dest, id string, finalize func() error, w http.ResponseWriter, job *jobLog) error {
	if err := markEmbeddedOverride(dest, id); err != nil {
		os.RemoveAll(dest)
		job.logf("cleanup: removed the partial install at %s", dest)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return err
	}
	if finalize == nil {
		job.logf("install landed in %s", dest)
		return nil
	}
	job.logf("swapping the staged install into place…")
	if err := finalize(); err != nil {
		os.RemoveAll(dest)
		job.logf("cleanup: removed the staging folder at %s (the previous install is untouched)", dest)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return err
	}
	job.logf("staged install swapped in; set-aside copy removed")
	return nil
}

// extractExeIconPNG pulls the program's own icon out of a Windows exe and
// saves it as icon.png beside it (the tile then shows the real icon instead
// of the emoji fallback). Windows-only, best effort — failure keeps the emoji.
func extractExeIconPNG(exePath, pngPath string) {
	script := `Add-Type -AssemblyName System.Drawing
$i = [System.Drawing.Icon]::ExtractAssociatedIcon($env:ICON_EXE)
if ($i) { $i.ToBitmap().Save($env:ICON_PNG, [System.Drawing.Imaging.ImageFormat]::Png) }`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "ICON_EXE="+exePath, "ICON_PNG="+pngPath)
	_ = cmd.Run()
}

// extractAppImageIcon pulls the icon out of an installed AppImage without
// executing it. A type-2 AppImage embeds a SquashFS payload behind its ELF
// runtime stub; the payload is located by the "hsqs" superblock magic and
// read with the system unsquashfs (squashfs-tools), extracting the
// spec-mandated .DirIcon entry — followed into the icon tree when it is a
// symlink. Best effort like the Windows exe icon extraction: any failure
// (no SquashFS payload, e.g. a type-1 ISO AppImage; no unsquashfs on PATH)
// silently keeps the emoji fallback. The imported file is never executed.
func extractAppImageIcon(body []byte, appImagePath, destDir string) {
	payload := bytes.Index(body, []byte("hsqs"))
	if payload < 0 {
		return
	}
	unsquashfs, err := exec.LookPath("unsquashfs")
	if err != nil {
		return
	}
	tmp, err := os.MkdirTemp("", "appdeck-icon-")
	if err != nil {
		return
	}
	defer os.RemoveAll(tmp)
	extract := func(name string) bool {
		cmd := exec.Command(unsquashfs, "-o", strconv.Itoa(payload),
			"-n", "-f", "-d", tmp, appImagePath, name)
		return cmd.Run() == nil
	}
	src := filepath.Join(tmp, ".DirIcon")
	if !extract(".DirIcon") {
		return
	}
	if info, err := os.Lstat(src); err == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return
		}
		target = strings.TrimPrefix(filepath.ToSlash(target), "/")
		if target == "" || !extract(target) {
			return
		}
		src = filepath.Join(tmp, filepath.FromSlash(target))
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	name := "icon.png"
	if strings.EqualFold(filepath.Ext(src), ".svg") {
		name = "icon.svg"
	}
	_ = os.WriteFile(filepath.Join(destDir, name), data, 0o644)
}

// runSilentInstall executes a detected installer with silent + directory
// flags pointed inside the app folder. NSIS's /D= must be the LAST argument
// and unquoted; Inno takes /DIR= quoted. Both are run from the destination
// directory so relative extractions stay inside it.
func runSilentInstall(dest, name string, body []byte, kind string) error {
	if err := os.WriteFile(filepath.Join(dest, name), body, 0o755); err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch kind {
	case "nsis":
		cmd = exec.Command(filepath.Join(dest, name), "/S", "/D="+dest)
	case "inno":
		cmd = exec.Command(filepath.Join(dest, name),
			"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/DIR="+dest)
	default:
		return fmt.Errorf("%s: no safe silent install flags", kind)
	}
	cmd.Dir = dest
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("silent install could not start: %v", err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("silent install failed: %v", err)
		}
	case <-time.After(silentInstallTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return fmt.Errorf("silent install timed out after %s", silentInstallTimeout)
	}
	return nil
}

// producedExe finds the app an installer produced: the largest .exe in the
// folder that is not the installer itself.
func producedExe(dest, installerName string) (string, error) {
	var best string
	var bestSize int64
	entries, err := os.ReadDir(dest)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".exe") || e.Name() == installerName {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Size() > bestSize {
			best, bestSize = e.Name(), info.Size()
		}
	}
	if best == "" {
		return "", fmt.Errorf("the silent install produced no .exe in the app folder — " +
			"this installer likely needs its own wizard; import a portable build instead")
	}
	return best, nil
}

// writeWrapManifest creates app.json for a wrapped binary and validates the
// folder the same way every other app is validated. signature records the
// import's code-signature outcome for the dashboard badge (macOS decks;
// "" when there is nothing to report).
func writeWrapManifest(dest, binary, appname string, qs map[string]string, job *jobLog, signature string) (*App, error) {
	job.logf("app.json was missing — generated one (wrapped %s)", binary)
	kind := qs["kind"]
	if kind == "" {
		kind = "desktop"
	}
	app := App{
		ID:       slugify(appname),
		Name:     appname,
		Category: "Tools",
		Binary:   filepath.ToSlash(binary),
	}
	if signature != "" {
		app.Signature = signature
	}
	// Optional emoji icon from the import dialog (exe imports have no
	// icon.png — the emoji is what the deck can show).
	if ic := strings.TrimSpace(qs["icon"]); ic != "" {
		app.Icon = ic
	}
	switch kind {
	case "desktop":
		app.Desktop = true
	case "web":
		// Port is optional: omitted → port 0 and the port registry assigns a
		// stable free port (handed to the app via PORT/APPDECK_PORT env).
		// An explicit ?port= acts as an override.
		if p := strings.TrimSpace(qs["port"]); p != "" {
			port, err := strconv.Atoi(p)
			if err != nil || port < 1024 || port > 65535 {
				return nil, fmt.Errorf("web-UI apps: ?port= must be 1024–65535 when given (the port this program listens on)")
			}
			app.Port = port
		}
		app.URLPath = "/"
	default:
		return nil, fmt.Errorf("kind must be \"desktop\" or \"web\"")
	}
	if a := strings.TrimSpace(qs["args"]); a != "" {
		app.Args = strings.Fields(a)
	}
	data, err := json.MarshalIndent(app, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dest, manifestFileName), data, 0o644); err != nil {
		return nil, err
	}
	got, err := loadAppManifest(dest)
	if err != nil {
		return nil, err
	}
	got.Source = "manifest"
	return got, nil
}

// importHTML turns a single .html file into a launcher-hosted static app.
func (s *Server) importHTML(w http.ResponseWriter, body []byte, name string, qs map[string]string, job *jobLog) {
	if len(body) > importMaxHTML {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("html too large (limit %d bytes)", importMaxHTML)})
		return
	}
	appname := strings.TrimSpace(qs["appname"])
	if appname == "" {
		appname = strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
		if appname == "" || appname == "/" {
			appname = "Imported page"
		}
	}
	dest, finalize, err := s.importDest(appname, w)
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(dest, "index.html"), body, 0o644); err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	app := App{
		ID:       slugify(appname),
		Name:     appname,
		Category: "Tools",
		ServedBy: "launcher",
		URLPath:  "/",
	}
	data, err := json.MarshalIndent(app, "", "  ")
	if err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := os.WriteFile(filepath.Join(dest, manifestFileName), data, 0o644); err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	got, err := loadAppManifest(dest)
	if err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	got.Source = "manifest"
	if err := s.finalizeImportDest(dest, got.ID, finalize, w, job); err != nil {
		return
	}
	s.importDone(w, got, job)
}

// rebuildMarker names the dot-file a source import leaves in a buildable app
// folder: sources just landed on disk while any binary the folder carried may
// be stale, so the next Start rebuilds before running (see rebuildIfImported).
const rebuildMarker = ".appdeck-rebuild"

// markRebuildOnImport flags a freshly imported folder for a rebuild at next
// Start. Imports copy the source folder as-is — including any prebuilt
// binary it contains — and resolveBinary would run that binary forever: the
// build path only triggers when a binary is missing outright, never when the
// sources around it are newer. Only buildable projects (go.mod / Cargo.toml)
// are marked; hosted apps and deliberate prebuilt drops never rebuild.
func markRebuildOnImport(dest string) {
	if !fileExists(filepath.Join(dest, "go.mod")) && !fileExists(filepath.Join(dest, "Cargo.toml")) {
		return
	}
	if err := os.WriteFile(filepath.Join(dest, rebuildMarker), []byte("sources re-imported — rebuild before start\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "launcher: could not write %s in %s: %v\n", rebuildMarker, dest, err)
	}
}

// rebuildSweepFile guards the one-time boot sweep below. Bump the suffix when
// a future change needs a fresh pass over installed apps.
const rebuildSweepFile = ".appdeck-rebuild-sweep-1"

// markLegacyImportsForRebuild retro-marks imported buildable apps that were
// installed before the rebuild marker existed: their import copied the
// source folder as-is (stale binary included) and nothing since has flagged
// it, so they keep launching the previous version forever. An imported app
// is one whose data-dir folder carries the import marker (.appdeck-override)
// or that manifest discovery found there — never plain registry apps
// (embedded builds refresh their unpacked binaries themselves; the deck a
// folder-mode switch materialized keeps its own copies) and never dev-tree
// apps beside the launcher (release.sh/build.sh keep those current). One
// pass per data directory, recorded in rebuildSweepFile; marking is
// idempotent anyway, the guard just keeps boots quiet afterwards.
func (s *Server) markLegacyImportsForRebuild() {
	if s.cfg.DataDir == "" {
		return
	}
	guard := filepath.Join(s.cfg.DataDir, rebuildSweepFile)
	if fileExists(guard) {
		return
	}
	dataAbs, err := filepath.Abs(s.cfg.DataDir)
	if err != nil {
		return
	}
	marked := 0
	for _, a := range s.apps {
		if a == nil || a.Dir == "" || a.Binary == "" {
			continue
		}
		imported := a.Source == "manifest" ||
			fileExists(filepath.Join(a.Dir, embeddedOverrideMarker))
		if !imported {
			continue
		}
		dirAbs, err := filepath.Abs(a.Dir)
		if err != nil || !strings.HasPrefix(dirAbs, dataAbs+string(filepath.Separator)) {
			continue
		}
		if fileExists(filepath.Join(a.Dir, rebuildMarker)) {
			continue
		}
		// Only apps carrying a binary — a missing one takes the normal
		// build-from-source path on Start already.
		bin := filepath.Join(a.Dir, filepath.Base(a.Binary))
		if !fileExists(bin) && !fileExists(platformBinaryPath(bin)) {
			continue
		}
		if !fileExists(filepath.Join(a.Dir, "go.mod")) && !fileExists(filepath.Join(a.Dir, "Cargo.toml")) {
			continue
		}
		markRebuildOnImport(a.Dir)
		marked++
	}
	if err := os.WriteFile(guard, []byte("pre-marker imports flagged for rebuild\n"), 0o644); err != nil {
		return // no guard → the sweep simply repeats next boot, still idempotent
	}
	if marked > 0 {
		fmt.Fprintf(os.Stderr, "launcher: flagged %d app(s) imported before the rebuild guard — they rebuild from their own sources at next start\n", marked)
	}
}

// buildInputFiles and buildInputDirs mirror what the all-in-one release
// never ships (release.sh copy_assets): the Go/Rust module files, the
// source tree, the cargo target dir, build/start scripts and repo docs.
// Everything else in an app folder is potential runtime surface — web
// assets apps serve from disk (index.html, css/, js/, rules/, data/),
// dist/, src/, Scripts/, app.json, icons — and stays.
var buildInputFiles = map[string]bool{
	"go.mod": true, "go.sum": true, "Cargo.toml": true, "Cargo.lock": true,
	"build.sh": true, "run.sh": true,
	"start.sh": true, "start.bat": true, "start.cmd": true,
	"README.md": true, "CHANGELOG.md": true, "LICENSE": true,
}

var buildInputDirs = map[string]bool{
	"cmd": true, "target": true, "docs": true, ".git": true, ".github": true, "node_modules": true,
}

// pruneBuildInputs strips an installed app's build-time-only files once its
// binary exists — imported sources are dead weight after the build, exactly
// the set the release build strips at pack time instead. Best-effort and
// idempotent; a pending .appdeck-rebuild marker means the sources are still
// needed and the folder is left alone. Directories the sweep empties (cmd/,
// an internal/ that held only .go files) go too — except runtime-named
// ones, which stay even when empty.
func pruneBuildInputs(dir string) error {
	if fileExists(filepath.Join(dir, rebuildMarker)) {
		return nil // a pending rebuild needs these sources
	}
	var files, dirs []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == dir {
			return nil // best-effort: unreadable entries are left in place
		}
		name := d.Name()
		if d.IsDir() {
			if buildInputDirs[name] {
				dirs = append(dirs, path)
			}
			return nil
		}
		// The named script/doc files apply at the app root only — a
		// Scripts/ subfolder holds operator scripts that stay.
		if filepath.Dir(path) == dir && buildInputFiles[name] {
			files = append(files, path)
			return nil
		}
		if strings.HasSuffix(name, ".go") {
			files = append(files, path)
		}
		return nil
	})
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			return err
		}
	}
	for _, d := range dirs {
		if err := os.RemoveAll(d); err != nil {
			return err
		}
	}
	removeEmptyDirs(dir)
	return nil
}

// keepEmptyDirs names folders apps may expect to exist even when empty.
var keepEmptyDirs = map[string]bool{
	"data": true, "logs": true, "cache": true, "assets": true, "rules": true,
	"css": true, "js": true, "web": true, "static": true, "dist": true,
}

// removeEmptyDirs deletes directories under dir that the prune left empty,
// deepest first — never dir itself, never dotted dirs, never keepEmptyDirs
// names.
func removeEmptyDirs(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || keepEmptyDirs[e.Name()] {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		removeEmptyDirs(sub)
		if again, err := os.ReadDir(sub); err == nil && len(again) == 0 {
			_ = os.Remove(sub)
		}
	}
}

// pruneSourcesSweepFile guards the one-per-boot pass below; bump the suffix
// when a change needs a fresh pass over installed apps.
const pruneSourcesSweepFile = ".appdeck-prune-sources-sweep-1"

// pruneImportedSourcesSweep trims imported apps built before source pruning
// existed: an imported folder with a working binary beside app.json has no
// use for its build inputs anymore — the release build never ships them.
// Same identification as markLegacyImportsForRebuild (imported manifests /
// .appdeck-override folders under the data directory only, never dev-tree
// apps) and the same skips: a pending rebuild marker or a missing binary
// waits for the build path, which prunes on success.
func (s *Server) pruneImportedSourcesSweep() {
	if s.cfg.DataDir == "" {
		return
	}
	guard := filepath.Join(s.cfg.DataDir, pruneSourcesSweepFile)
	if fileExists(guard) {
		return
	}
	dataAbs, err := filepath.Abs(s.cfg.DataDir)
	if err != nil {
		return
	}
	pruned := 0
	for _, a := range s.apps {
		if a == nil || a.Dir == "" || a.Binary == "" {
			continue
		}
		imported := a.Source == "manifest" ||
			fileExists(filepath.Join(a.Dir, embeddedOverrideMarker))
		if !imported {
			continue
		}
		dirAbs, err := filepath.Abs(a.Dir)
		if err != nil || !strings.HasPrefix(dirAbs, dataAbs+string(filepath.Separator)) {
			continue
		}
		if fileExists(filepath.Join(a.Dir, rebuildMarker)) {
			continue // rebuild pending: the sources are still needed
		}
		bin := filepath.Join(a.Dir, filepath.Base(a.Binary))
		if !fileExists(bin) && !fileExists(platformBinaryPath(bin)) {
			continue // never built: the first-start build prunes on success
		}
		if !fileExists(filepath.Join(a.Dir, "go.mod")) &&
			!fileExists(filepath.Join(a.Dir, "Cargo.toml")) &&
			!fileExists(filepath.Join(a.Dir, "cmd")) {
			continue // nothing build-input-shaped to remove
		}
		if err := pruneBuildInputs(a.Dir); err == nil {
			pruned++
		}
	}
	if err := os.WriteFile(guard, []byte("imported app sources pruned post-build\n"), 0o644); err != nil {
		return // no guard → the sweep repeats next boot, still idempotent
	}
	if pruned > 0 {
		fmt.Fprintf(os.Stderr, "launcher: pruned build sources from %d imported app(s) — the release build never ships them\n", pruned)
	}
}

// importFolder copies an existing app folder (one carrying a valid app.json)
// into the container. The source must be outside the container — a folder
// already inside Apps/ is an installed app, not an import.
func (s *Server) importFolder(w http.ResponseWriter, src string, job *jobLog) {
	abs, err := filepath.Abs(filepath.Clean(src))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("%q is not a folder", src)})
		return
	}
	app, err := loadAppManifest(abs)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("the folder has no valid %s: %v", manifestFileName, err)})
		return
	}
	container, err := s.appsContainerDir()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	containerAbs, err := filepath.Abs(container)
	if err == nil && (abs == containerAbs || strings.HasPrefix(abs, containerAbs+string(filepath.Separator))) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "that folder is already inside the deck's Apps container — nothing to import"})
		return
	}
	dest := filepath.Join(container, slugify(app.ID))
	if _, err := os.Stat(dest); err == nil {
		// Already installed → update in place: copy into staging, swap only
		// after the staged folder validates. Same app id required.
		if err := s.updateInstalledApp(dest, app.ID, func(stage string) error {
			return copyAppFolder(abs, stage)
		}); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	} else {
		if err := copyAppFolder(abs, dest); err != nil {
			os.RemoveAll(dest)
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	markRebuildOnImport(dest)
	if err := markEmbeddedOverride(dest, app.ID); err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	got, err := loadAppManifest(dest)
	if err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	got.Source = "manifest"
	s.importDone(w, got, job)
}

// copyAppFolder deep-copies src → dest with the same guards as archive
// extraction: symlinks are skipped, file count and total size are capped.
func copyAppFolder(src, dest string) error {
	var files int
	var total int64
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dest, 0o755)
		}
		if isJunkEntry(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !d.Type().IsRegular() { // symlinks and specials never copy
			return nil
		}
		files++
		if files > installMaxFiles {
			return fmt.Errorf("folder holds more than %d files — refusing", installMaxFiles)
		}
		total += info.Size()
		if total > installMaxTotal {
			return fmt.Errorf("folder is larger than %d bytes — refusing", installMaxTotal)
		}
		target := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			return err
		}
		return nil
	})
}

// updateInstalledApp replaces an already-installed app folder with new
// content. The copy runs into a staging folder; only after it validates does
// the old install move aside — into a dot-folder the app scanner ignores —
// and the staging folder take its name. The old copy is removed last, so a
// failure at any point leaves the previous install untouched. The guard
// refuses when the folder is not a valid app (unknown data is never
// clobbered) or when it belongs to a different app id.
func (s *Server) updateInstalledApp(dest, wantID string, fill func(stage string) error) error {
	existing, err := loadAppManifest(dest)
	embeddedTarget := embeddedBuild &&
		embeddedAppIDs()[slugify(wantID)] &&
		isEmbeddedAppDir(s.cfg.DataDir, dest, wantID)
	if err != nil && !embeddedTarget {
		return fmt.Errorf("%q already holds a folder that is not a valid app — remove %s first", wantID, dest)
	}
	if err == nil && existing.ID != wantID {
		return fmt.Errorf("%q is already the folder of a different app (%q) — change the app id", wantID, existing.ID)
	}
	stage := dest + ".importing"
	previous := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".previous")
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	if err := fill(stage); err != nil {
		os.RemoveAll(stage)
		return err
	}
	if _, err := loadAppManifest(stage); err != nil {
		os.RemoveAll(stage)
		return fmt.Errorf("the new app folder carries an invalid manifest: %v", err)
	}
	if err := markEmbeddedOverride(stage, wantID); err != nil {
		os.RemoveAll(stage)
		return err
	}
	if err := s.stopAppBeforeUpdate(wantID); err != nil {
		os.RemoveAll(stage)
		return err
	}
	if err := os.RemoveAll(previous); err != nil {
		return err
	}
	if err := os.Rename(dest, previous); err != nil {
		return err
	}
	if err := os.Rename(stage, dest); err != nil {
		_ = os.Rename(previous, dest) // put the old install back
		return err
	}
	_ = os.RemoveAll(previous) // success — the old copy goes away
	return nil
}

// importDest computes and reserves the destination folder for a new import
// from the app's display name. Writes the error response itself. When the
// app is already installed the import builds in a staging folder and the
// returned finalize func swaps it in (update-in-place); on a fresh import
// finalize is nil.
// importDest picks (and on re-imports, stages) the install folder, writing
// any error to w. See stageImportDest for the mechanics.
func (s *Server) importDest(appname string, w http.ResponseWriter) (string, func() error, error) {
	dest, finalize, err := s.stageImportDest(appname)
	if err != nil {
		return "", nil, writeErrJSON(w, err.Error())
	}
	return dest, finalize, nil
}

// stageImportDest is importDest's core: a fresh import creates Apps/<id>
// directly; an existing install is never touched until the caller's finalize
// swaps a validated staging copy in. Flows that retry (the releases path
// tries up to three candidates) use this form so a failed attempt can be
// discarded without a half-written response.
func (s *Server) stageImportDest(appname string) (string, func() error, error) {
	if strings.TrimSpace(appname) == "" {
		return "", nil, fmt.Errorf("exe import needs ?appname= (the app's display name)")
	}
	id := slugify(appname)
	if id == "" {
		return "", nil, fmt.Errorf("%q produces no valid app id", appname)
	}
	container, err := s.appsContainerDir()
	if err != nil {
		return "", nil, err
	}
	dest := filepath.Join(container, id)
	var finalize func() error
	if _, err := os.Stat(dest); err == nil {
		// Already installed → build in staging; finalize swaps it in.
		existing, merr := loadAppManifest(dest)
		if merr != nil {
			return "", nil, fmt.Errorf("%q already holds a folder that is not a valid app — remove %s first", id, dest)
		}
		if existing.ID != id {
			return "", nil, fmt.Errorf("%q is already the folder of a different app (%q) — change the name", id, existing.ID)
		}
		stage := dest + ".importing"
		if err := os.RemoveAll(stage); err != nil {
			return "", nil, err
		}
		if err := os.MkdirAll(stage, 0o755); err != nil {
			return "", nil, err
		}
		finalize = func() error { return s.updateSwap(stage, filepath.Join(container, id)) }
		dest = stage
	} else if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", nil, err
	}
	return dest, finalize, nil
}

// updateSwap moves a staged folder over dest with the same set-aside-then-
// remove safety as updateInstalledApp (the old install goes to a hidden
// folder first, and a failed swap restores it).
func (s *Server) updateSwap(stage, dest string) error {
	id := filepath.Base(dest)
	if err := markEmbeddedOverride(stage, id); err != nil {
		return err
	}
	if err := s.stopAppBeforeUpdate(id); err != nil {
		return err
	}
	previous := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".previous")
	if err := os.RemoveAll(previous); err != nil {
		return err
	}
	if err := os.Rename(dest, previous); err != nil {
		return err
	}
	if err := os.Rename(stage, dest); err != nil {
		_ = os.Rename(previous, dest)
		return err
	}
	_ = os.RemoveAll(previous)
	return nil
}

// writeErrJSON is writeJSON's one-response form for helpers that return.
func writeErrJSON(w http.ResponseWriter, msg string) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"error":` + mustJSON(msg) + `}`))
	return fmt.Errorf("%s", msg)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `"` + strings.ReplaceAll(fmt.Sprint(v), `"`, `\"`) + `"`
	}
	return string(b)
}

// progressReader counts bytes through a download and logs a milestone every
// `every` bytes — the import log shows a big download moving, and the job's
// no-progress watchdog sees it is alive. nil job = counting only.
type progressReader struct {
	r     io.Reader
	job   *jobLog
	every int64
	seen  int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.seen += int64(n)
	if p.every > 0 && p.seen/p.every != (p.seen-int64(n))/p.every {
		p.job.logf("downloading… %d MB", p.seen>>20)
	}
	return n, err
}
