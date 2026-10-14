// Import router: the type-aware front door for bringing apps into the deck.
// One endpoint — POST /api/import — accepts any of these sources and normalizes
// every one of them into the same outcome: a validated app folder under the
// deck's Apps container.
//
//	archive (zip/tar.gz/tar)  → the existing archive installer (app.json at root)
//	portable .exe/.binary     → wrapped as an app in place — never executed
//	installer .exe            → refused, or (Windows, explicit opt-in) a silent
//	                            install confined to the app folder
//	single .html file         → a servedBy-launcher static app
//	local folder              → validated + copied into the container
//	URL                       → downloaded, then routed by sniffed content
//
// Import never executes the imported file: the only code that runs at import
// time is the installer the user explicitly opted into on Windows. Everything
// lands in the deck's app container — there is no install path to ask about,
// which is what keeps the deck portable.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	if rawurl := qs["url"]; rawurl != "" {
		body, name, err := fetchForImport(rawurl)
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if qs["name"] == "" {
			qs["name"] = name
		}
		s.importBytes(w, body, qs)
		return
	}
	if dir := qs["folder"]; dir != "" {
		s.importFolder(w, dir)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, importMaxBytes+1))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading upload: " + err.Error()})
		return
	}
	s.importBytes(w, body, qs)
}

// fetchForImport downloads a remote file (release asset, shared zip) with the
// same size cap as uploads.
func fetchForImport(rawurl string) ([]byte, string, error) {
	if !strings.HasPrefix(rawurl, "http://") && !strings.HasPrefix(rawurl, "https://") {
		return nil, "", fmt.Errorf("url must be http(s)")
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(rawurl)
	if err != nil {
		return nil, "", fmt.Errorf("fetch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, importMaxBytes+1))
	if err != nil {
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
func (s *Server) importBytes(w http.ResponseWriter, body []byte, qs map[string]string) {
	if len(body) == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty upload"})
		return
	}
	if len(body) > importMaxBytes {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("file too large (limit %d bytes)", importMaxBytes)})
		return
	}
	name := qs["name"]
	switch {
	case detectArchiveFormat(body) != "":
		s.installUpload(w, body, qs)
	case len(body) >= 2 && body[0] == 'M' && body[1] == 'Z':
		s.importBinary(w, body, name, qs)
	case looksLikeHTML(body):
		s.importHTML(w, body, name, qs)
	default:
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "unsupported file — send an app archive (zip/tar.gz with app.json), a portable .exe, " +
				"a single .html page, a folder path (?folder=), or a URL (?url=)"})
	}
}

// installUpload routes a sniffed archive into the existing installer.
func (s *Server) installUpload(w http.ResponseWriter, body []byte, qs map[string]string) {
	app, err := s.installArchiveBytes(body)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.importDone(w, app)
}

// importDone is the shared success tail: rescan the registry so the new app
// appears on the dashboard, then report the id.
func (s *Server) importDone(w http.ResponseWriter, app *App) {
	if err := s.reloadApps(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("imported %s, but rescanning the registry failed: %v", app.ID, err)})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"id":   app.ID,
		"name": app.Name,
		"note": "imported and discovered — it appears on the dashboard now",
	})
}

// importBinary wraps a portable executable into a deck app, or — for
// detected installers, on Windows, with &silent=1 — attempts a silent
// install confined to the app folder.
func (s *Server) importBinary(w http.ResponseWriter, body []byte, name string, qs map[string]string) {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "" || name == "/" || name == "." {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "exe import needs the file name (?name=tool.exe) so the binary keeps its name"})
		return
	}
	if kind := installerKind(name, body); kind != "" && !strings.HasSuffix(name, "-portable") {
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

	appname := strings.TrimSpace(qs["appname"])
	if appname == "" {
		appname = strings.TrimSuffix(name, filepath.Ext(name))
	}

	dest, finalize, err := s.importDest(appname, w)
	if err != nil {
		return // the response is already written
	}
	// Installer flow: the installer binary itself lands in the folder, runs
	// quietly with directory flags pointed at it, and the largest produced
	// exe becomes the app. The installer file is removed afterwards.
	if kind := installerKind(name, body); kind != "" {
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
		app, err := writeWrapManifest(dest, bin, appname, qs)
		if err != nil {
			os.RemoveAll(dest)
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := s.finalizeImportDest(dest, finalize, w); err != nil {
			return
		}
		s.importDone(w, app)
		return
	}

	// Portable flow: the file IS the app — write it untouched.
	if err := os.WriteFile(filepath.Join(dest, name), body, 0o755); err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Windows: pull the program's own icon into the folder so the tile shows
	// the real icon (extracted via System.Drawing; skipped when it fails —
	// the emoji from ?icon= stays the fallback).
	if runtime.GOOS == "windows" {
		extractExeIconPNG(filepath.Join(dest, name), filepath.Join(dest, "icon.png"))
	}
	app, err := writeWrapManifest(dest, name, appname, qs)
	if err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.finalizeImportDest(dest, finalize, w); err != nil {
		return
	}
	s.importDone(w, app)
}

// finalizeImportDest runs the staged-import swap for flows that got a
// staging folder from importDest. A failed swap is reported and the staging
// folder removed — the previously installed app was never touched.
func (s *Server) finalizeImportDest(dest string, finalize func() error, w http.ResponseWriter) error {
	if finalize == nil {
		return nil
	}
	if err := finalize(); err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return err
	}
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
// folder the same way every other app is validated.
func writeWrapManifest(dest, binary, appname string, qs map[string]string) (*App, error) {
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
func (s *Server) importHTML(w http.ResponseWriter, body []byte, name string, qs map[string]string) {
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
	if err := s.finalizeImportDest(dest, finalize, w); err != nil {
		return
	}
	s.importDone(w, got)
}

// importFolder copies an existing app folder (one carrying a valid app.json)
// into the container. The source must be outside the container — a folder
// already inside Apps/ is an installed app, not an import.
func (s *Server) importFolder(w http.ResponseWriter, src string) {
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
	got, err := loadAppManifest(dest)
	if err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	got.Source = "manifest"
	s.importDone(w, got)
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
	if err != nil {
		return fmt.Errorf("%q already holds a folder that is not a valid app — remove %s first", wantID, dest)
	}
	if existing.ID != wantID {
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
func (s *Server) importDest(appname string, w http.ResponseWriter) (string, func() error, error) {
	if strings.TrimSpace(appname) == "" {
		return "", nil, writeErrJSON(w, "exe import needs ?appname= (the app's display name)")
	}
	id := slugify(appname)
	if id == "" {
		return "", nil, writeErrJSON(w, fmt.Sprintf("%q produces no valid app id", appname))
	}
	container, err := s.appsContainerDir()
	if err != nil {
		return "", nil, writeErrJSON(w, err.Error())
	}
	dest := filepath.Join(container, id)
	var finalize func() error
	if _, err := os.Stat(dest); err == nil {
		// Already installed → build in staging; finalize swaps it in.
		existing, merr := loadAppManifest(dest)
		if merr != nil {
			return "", nil, writeErrJSON(w, fmt.Sprintf("%q already holds a folder that is not a valid app — remove %s first", id, dest))
		}
		if existing.ID != id {
			return "", nil, writeErrJSON(w, fmt.Sprintf("%q is already the folder of a different app (%q) — change the name", id, existing.ID))
		}
		stage := dest + ".importing"
		if err := os.RemoveAll(stage); err != nil {
			return "", nil, writeErrJSON(w, err.Error())
		}
		if err := os.MkdirAll(stage, 0o755); err != nil {
			return "", nil, writeErrJSON(w, err.Error())
		}
		finalize = func() error { return s.updateSwap(stage, filepath.Join(container, id)) }
		dest = stage
	} else if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", nil, writeErrJSON(w, err.Error())
	}
	return dest, finalize, nil
}

// updateSwap moves a staged folder over dest with the same set-aside-then-
// remove safety as updateInstalledApp (the old install goes to a hidden
// folder first, and a failed swap restores it).
func (s *Server) updateSwap(stage, dest string) error {
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
