// The dashboard's HTTP surface: ServeHTTP, the route table and every handler.
package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

// serveIcon answers GET /api/icon?id=… with the app's icon file. The ETag
// (content hash) lets the dashboard's poll revalidate cheaply: unchanged
// icons get a 304 instead of a reload, so the grid never flickers.
func (s *Server) serveIcon(w http.ResponseWriter, r *http.Request) {
	qs := parseQuery(r.RequestURI)
	app := s.appByID(qs["id"])
	if app == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
		return
	}
	var data []byte
	var ctype string
	var ok bool
	// ?pack=<name> serves the pack's icon for the app (the 🧩 dialog's live
	// preview and per-app picker) without touching the installed files;
	// pack "original" previews the backed-up pre-pack artwork.
	if pack := slugify(qs["pack"]); pack != "" {
		data, ctype, ok = s.packIconBytes(pack, app.ID)
	}
	if !ok {
		data, ctype, ok = appIcon(app)
	}
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "no icon file in the app folder (icon.png/.svg/.ico/.icns or <app-id>.png)"})
		return
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", ctype)
	// An imported app can ship any bytes as icon.svg. Rendered via <img> the
	// script inside it never runs, but a page that frames or navigates to this
	// URL executes it on the launcher's origin — every guard passes for a
	// 127.0.0.1 iframe. "sandbox" makes the SVG a unique-origin document with
	// scripts disabled in those contexts; <img>/CSS usage is unaffected.
	if ctype == "image/svg+xml" {
		h.Set("Content-Security-Policy", "sandbox")
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// serveRebuildApp was removed with the 🛠 card action: imported sources are
// pruned after their binary is built (pruneBuildInputs), so there is nothing
// left to rebuild on demand — a re-import is the way to land new sources,
// and its marker flow rebuilds and prunes again.

// ------------------------------------------------------------------- http ----

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush passes through for streaming responses (the event bus's SSE) — a
// bare status recorder would make every handler believe flushing is
// unsupported.
func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ServeHTTP routes like the Python do_GET/do_POST pair: the raw request path
// is matched literally (no normalization), unmatched combinations get a JSON
// 404.
// serveUninstall removes an app: unregister it from apps.json when it is a
// registry app (folder mode), stop it, delete its folder, drop its port
// assignment (via reload), and optionally its per-app data directory. In
// embed mode, registry apps are uninstallable too: the payload inside the exe
// cannot be deleted, so the unpacked folder is removed and the app simply
// returns on the next start (extraction unpacks whatever the exe carries).
func (s *Server) serveUninstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID         string `json:"id"`
		RemoveData bool   `json:"removeData"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send {id, removeData}"})
		return
	}
	s.mu.Lock()
	var a *App
	for _, cand := range s.apps {
		if cand.ID == body.ID {
			a = cand
			break
		}
	}
	s.mu.Unlock()
	if a == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown app " + body.ID})
		return
	}
	note, err := s.uninstallOneApp(a, body.RemoveData)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Rescan now: without this the card lingers on the deck until the next
	// reload trigger (the 10s container watcher only notices new folders,
	// never removals) — the same rescan the batch uninstall performs.
	if err := s.reloadApps(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	go s.snapshotDeck()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note})
}

// removeRegistryEntry drops one app's entry from the central apps.json and
// rewrites the file in place (temp file + rename). Entries travel as raw JSON
// so every field the App struct does not model — and every sibling entry —
// survives the round trip untouched. An id the file does not list is not an
// error: the file already describes the deck without the app.
func removeRegistryEntry(appsFile, id string) error {
	if appsFile == "" {
		return fmt.Errorf("no apps.json to edit")
	}
	data, err := os.ReadFile(appsFile)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing %s: %w", appsFile, err)
	}
	var entries []json.RawMessage
	if raw, ok := doc["apps"]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("%s: the \"apps\" value is not a list: %w", appsFile, err)
		}
	}
	kept := make([]json.RawMessage, 0, len(entries))
	found := false
	for _, e := range entries {
		var probe struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(e, &probe) == nil && probe.ID == id {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		return nil // already absent — the file stays exactly as it is
	}
	appsRaw, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	doc["apps"] = appsRaw
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // do not rewrite untouched entries' <, >, &
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	tmp := appsFile + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, appsFile)
}

// uninstallOneApp removes one application: unregisters it from apps.json
// when it is a registry app of a folder build, stops it, resets its macOS
// privacy grants, deletes its folder (permanently — this is the launcher's
// long-standing uninstall semantic), and optionally drops the per-app data
// directory. Shared by the single-app route and /api/uninstall/batch. The
// caller reloads the app list. In an all-in-one build the payload still
// carries the app, so the removal is recorded in uninstalled.json: extraction
// skips it on every later boot and the deck stops advertising it, until the
// operator restores it from ⚙ Deck tools (or imports it again).
func (s *Server) uninstallOneApp(a *App, removeData bool) (string, error) {
	embeddedRegistry := a.Source == "registry"
	// Launcher-hosted embedded apps have no folder at all — their uninstall
	// is the data-dir slug folder removal below.
	if a.Dir == "" && !(embeddedRegistry && embeddedBuild) {
		return "", fmt.Errorf("%s has no folder on disk", a.ID)
	}
	if rootAbs, err := filepath.Abs(s.cfg.Root); err == nil && a.Dir == rootAbs {
		return "", fmt.Errorf("refusing to remove the launcher folder")
	}
	registryEntryRemoved := false
	if embeddedRegistry && !embeddedBuild {
		// Folder mode: apps.json is a plain editable file, so unregistering
		// is part of the uninstall — drop the entry first (a failed write
		// aborts here, before anything on disk is deleted), then fall through
		// to the normal stop-and-delete below.
		if err := removeRegistryEntry(s.cfg.AppsFile, a.ID); err != nil {
			return "", fmt.Errorf("%s is registered in apps.json — removing the entry failed: %w", a.ID, err)
		}
		registryEntryRemoved = true
	}
	_ = s.stopApp(a)        // a dead process must not block removal
	resetTccGrants(a.ID)    // the app's macOS privacy grants die with it (no-op off darwin)
	s.exposeRemoveForApp(a) // so does its /Applications link, Start-menu pin or .desktop entry
	if a.Dir == "" && embeddedRegistry && embeddedBuild {
		// Hosted apps have no registry folder; extraction materializes their
		// data-dir slug folder (app.json + icon) on every boot. Remove it so
		// the card is gone for this session — the next start unpacks it again,
		// like every other app in the payload. Overriding imports never reach
		// this branch (they set Source/Dir and delete their folder through the
		// a.Dir path above).
		_ = os.RemoveAll(filepath.Join(s.cfg.DataDir, "Apps", slugify(a.ID)))
	}
	if a.Dir != "" {
		if err := os.RemoveAll(a.Dir); err != nil {
			return "", fmt.Errorf("remove folder: %w", err)
		}
	}
	note := a.ID + " uninstalled"
	if registryEntryRemoved {
		note += " — apps.json entry removed"
	}
	if embeddedRegistry && embeddedBuild {
		// The payload still carries the app and the exe cannot be edited, so
		// the removal is what makes this stick: extraction skips the id from
		// now on, and the deck offers it back through ⚙ Deck tools.
		if err := markAppRemoved(s.cfg.DataDir, a.ID); err != nil {
			return "", fmt.Errorf("recording the removal of %s: %w", a.ID, err)
		}
		note += " — built into this launcher; restore it from ⚙ Deck tools"
	}
	dataDir := ""
	if s.cfg.DataDir != "" {
		dataDir = filepath.Join(s.cfg.DataDir, "appdata", a.ID)
	}
	if removeData && dataDir != "" {
		if os.RemoveAll(dataDir) == nil {
			note += " — data folder removed"
		}
	} else if dataDir != "" {
		note += " — data kept at " + dataDir
	}
	return note, nil
}

// serveUninstallBatch removes SEVERAL applications in one call. Per-id
// results follow the /api/all/* shape: a refusal (missing folder, in-flight
// start, a failed apps.json rewrite) skips that app and is reported —
// it never aborts the batch. Everything user-generated is deleted
// permanently, so the route is loopback-only, like the other destructive
// mutators.
func (s *Server) serveUninstallBatch(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "local only"})
		return
	}
	var body struct {
		IDs        []string `json:"ids"`
		RemoveData bool     `json:"removeData"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.IDs) == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send {ids: [...], removeData}"})
		return
	}
	if len(body.IDs) > 48 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "uninstall at most 48 apps at once"})
		return
	}
	type batchResult struct {
		ID    string `json:"id"`
		Name  string `json:"name,omitempty"`
		OK    bool   `json:"ok"`
		Note  string `json:"note,omitempty"`
		Error string `json:"error,omitempty"`
	}
	results := make([]batchResult, 0, len(body.IDs))
	ok, failed := 0, 0
	for _, id := range body.IDs {
		s.mu.Lock()
		var a *App
		for _, cand := range s.apps {
			if cand.ID == id {
				a = cand
				break
			}
		}
		name := ""
		if a != nil {
			name = a.Name
		}
		s.mu.Unlock()
		if a == nil {
			results = append(results, batchResult{ID: id, Error: "unknown app"})
			failed++
			continue
		}
		note, err := s.uninstallOneApp(a, body.RemoveData)
		if err != nil {
			results = append(results, batchResult{ID: id, Name: name, Error: err.Error()})
			failed++
			continue
		}
		results = append(results, batchResult{ID: id, Name: name, OK: true, Note: note})
		ok++
	}
	if err := s.reloadApps(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if ok > 0 {
		go s.snapshotDeck()
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": failed == 0, "uninstalled": ok, "failed": failed, "results": results})
}

// serveDataFolder opens (and creates) the app's per-user data directory in
// the OS file manager. Only dirs under the launcher's own DataDir can be
// opened — the path is derived from the id, never taken from the request.
func (s *Server) serveDataFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send {id}"})
		return
	}
	s.mu.Lock()
	var a *App
	for _, cand := range s.apps {
		if cand.ID == body.ID {
			a = cand
			break
		}
	}
	s.mu.Unlock()
	if a == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown app " + body.ID})
		return
	}
	if s.cfg.DataDir == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no data directory in this build"})
		return
	}
	dir := filepath.Join(s.cfg.DataDir, "appdata", a.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dir": dir})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", serverVersion)
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.route(rec, r)
	// Python log_message: "[HH:MM:SS] \"GET /api/apps HTTP/1.1\" 200 -"
	fmt.Fprintf(os.Stderr, "[%s] \"%s %s HTTP/1.1\" %d -\n",
		time.Now().Format("15:04:05"), r.Method, r.RequestURI, rec.status)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path, _, _ := strings.Cut(r.RequestURI, "?")
	// Dashboard loopback mode (launcher.json): the launcher's own UI and API
	// answer only this computer. Shared APPS keep their own ports and stay
	// LAN-reachable — this restricts the deck, not the apps.
	if settingsLoopback.Load() && !fromLocalMachine(r) && path != "/api/health" {
		s.writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "the dashboard is limited to this computer (launcher setting)"})
		return
	}
	// Guard for state-changing calls. Browsers attach an Origin header to
	// every cross-site request, so one that names a different host is a
	// drive-by web page trying to drive the local API with a text/plain
	// POST — reject it. The fence covers every state-changing method so a
	// future PUT/PATCH/DELETE route inherits it. LAUNCHER_LAN_TOKEN
	// additionally requires the matching X-AppDeck-Token header from
	// non-loopback callers when set.
	if mutatingMethod(r.Method) && strings.HasPrefix(path, "/api/") {
		// Sec-Fetch-Site is browser-supplied and still present where a page
		// cannot attach an Origin (some form posts, sandboxed frames).
		if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "same-site" && sfs != "none" {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site requests are not allowed"})
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
				s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin requests are not allowed"})
				return
			}
		}
		if !fromLocalMachine(r) && s.cfg.LANToken != "" &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("X-AppDeck-Token")), []byte(s.cfg.LANToken)) != 1 {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing or wrong X-AppDeck-Token"})
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && (path == "/" || path == "/index.html"):
		s.serveIndex(w)
	case strings.HasPrefix(path, embedPrefix):
		// Any method: embedded pages POST back through the shim too
		// (absolute-path apps' API calls ride the fallback below instead).
		s.serveEmbed(w, r)
	case r.Method == http.MethodGet && path == "/__tokenreport__/list":
		s.serveTokenreportList(w, r)
	case r.Method == http.MethodGet && path == "/__tokenreport__/read":
		s.serveTokenreportRead(w, r)
	case r.Method == http.MethodGet && path == "/api/apps":
		// Read-only and LAN-visible by design; the open CORS header lets
		// deck tools (Wi-Fi Guard-style pages on their own port — same site,
		// different port) read it too. A foreign web page is "cross-site"
		// and gets nothing, so the wildcard header never hands the
		// inventory (names, ports, git origins) to a drive-by reader. The
		// deck operator can flip visibility off entirely (⚙ Deck tools) —
		// then the inventory answers this machine only.
		if sfs := r.Header.Get("Sec-Fetch-Site"); sfs == "cross-site" {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site requests are not allowed"})
			return
		}
		if settingsHidden.Load() && !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "the app list is limited to this computer (launcher setting)"})
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		noteActivity()
		s.mu.Lock()
		st := loadState(s.cfg.StateFile)
		apps := s.apps
		s.mu.Unlock()
		statuses := make([]appStatus, 0, len(apps))
		for _, a := range apps {
			statuses = append(statuses, s.appStatus(a, st))
		}
		s.writeJSON(w, http.StatusOK, statuses)
	case r.Method == http.MethodGet && path == "/api/resources":
		// Read-only like /api/apps; serves the resmon cache (resmon.go) —
		// the ⚙ monitor table and any deck tool that wants the numbers.
		noteActivity()
		s.writeJSON(w, http.StatusOK, map[string]any{
			"apps":    s.resourcesSnapshot(),
			"updated": s.resUpdatedAt(),
		})
	case r.Method == http.MethodGet && path == "/api/lifecycle":
		noteActivity()
		s.serveLifecycleGet(w)
	case r.Method == http.MethodPost && path == "/api/lifecycle":
		// Central mutating-/api guard (Sec-Fetch-Site, Origin, LANToken)
		// already ran; body validation lives in serveLifecycleSet.
		s.serveLifecycleSet(w, r)
	case r.Method == http.MethodGet && path == "/api/permissions":
		noteActivity()
		s.servePermissionsGet(w)
	case r.Method == http.MethodPost && path == "/api/permissions":
		// The central mutating-/api guard already ran; body validation and
		// the grant store live in permissions.go.
		s.servePermissionsSet(w, r)
	case r.Method == http.MethodGet && path == "/appdeck-bus.js":
		// The one-tag bus client for card pages (eventbus.go) — served from
		// the launcher so the script derives the bus base from its own src.
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, appdeckBusJS)
	case r.Method == http.MethodOptions && strings.HasPrefix(path, "/api/bus/"):
		// CORS preflight for cross-origin card pages (the card's origin is
		// its own port, the bus lives on the deck's).
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && path == "/api/bus/events":
		// SSE stream — never a mutating request, no noteActivity (it never
		// returns until the client hangs up).
		s.serveBusEvents(w, r)
	case r.Method == http.MethodPost && path == "/api/bus/publish":
		s.serveBusPublish(w, r)
	case r.Method == http.MethodGet && path == "/api/bus/topics":
		noteActivity()
		s.serveBusTopics(w)
	case r.Method == http.MethodGet && path == "/api/featured":
		noteActivity()
		s.serveFeatured(w)
	case r.Method == http.MethodPost && path == "/api/featured/template":
		// Central mutating-/api guard already ran; scaffold validation lives
		// in featured.go.
		s.serveFeaturedTemplate(w, r)
	case r.Method == http.MethodGet && path == "/api/health":
		noteActivity()
		var out healthResp
		out.OK = true
		out.Version = launcherVersion
		out.Started = procStartedAt
		if embeddedBuild {
			out.Mode = "single-file"
			out.DataDir = s.cfg.DataDir
		}
		out.Window = launchWindow
		out.TLSPort = s.cfg.TLSPort
		out.OS = runtime.GOOS
		out.Arch = runtime.GOARCH
		s.mu.Lock()
		if len(s.warnings) > 0 {
			out.Warnings = append([]string(nil), s.warnings...)
		}
		s.mu.Unlock()
		if lan := lanIP(); lan != "" {
			out.Lan = &lan
			u := fmt.Sprintf("http://%s:%d", lan, s.cfg.Port)
			out.LanURL = &u
		}
		s.writeJSON(w, http.StatusOK, out)
	case r.Method == http.MethodGet && path == "/api/appstorage":
		s.serveAppStorageGet(w, r)
	case r.Method == http.MethodPost && path == "/api/appstorage":
		s.serveAppStoragePost(w, r)
	case r.Method == http.MethodPost && path == "/api/heartbeat":
		// Dashboard keepalive; ?bye=1 is the pagehide beacon sent when the
		// last dashboard window/tab goes away (window mode shuts down then).
		// The bye is the shutdown trigger, so it only counts from this
		// machine: one POST from the LAN must not kill the whole deck.
		if parseQuery(r.RequestURI)["bye"] == "1" {
			if fromLocalMachine(r) {
				handleBye(s)
			} else {
				s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "bye is only honored from this computer"})
				return
			}
		} else {
			noteActivity()
		}
		s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case r.Method == http.MethodPost && path == "/api/shutdown":
		// Explicit quit from the dashboard. Loopback only — a device on the
		// Wi-Fi may stop apps but not kill the launcher itself.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "shutdown is only allowed from this computer"})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		time.AfterFunc(300*time.Millisecond, func() { s.beginShutdown("quit requested from the dashboard") })
	case r.Method == http.MethodPost && path == "/api/open":
		// Open a URL on this computer (tab mode from the native window: the
		// WebView2 window has no tabs, so the app lands in the default
		// browser instead). Loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "open is only allowed from this computer"})
			return
		}
		var req openRequest
		readBodyInto(r, &req)
		u := strings.TrimSpace(req.URL)
		// Parse and compare the hostname exactly: a prefix match would accept
		// lookalikes like http://127.0.0.1.attacker.tld (or 127.0.0.10) and
		// aim the user's default browser at an attacker page.
		pu, perr := url.Parse(u)
		hn := ""
		if perr == nil {
			hn = strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(pu.Hostname()), "["), "]")
		}
		loopbackHTTP := perr == nil && pu.Scheme == "http" && (hn == "127.0.0.1" || hn == "::1")
		if !loopbackHTTP && (perr != nil || pu.Scheme != "https") {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported url"})
			return
		}
		openBrowser(u)
		s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case r.Method == http.MethodPost && path == "/api/firewall":
		// One-click fix for the per-app Windows Firewall "Allow/Cancel"
		// popups: adds allow-rules for the launcher and every network app via
		// a single UAC elevation. Windows only; loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "firewall fix is only allowed from this computer"})
			return
		}
		s.writeJSON(w, http.StatusOK, s.firewallFix())
	case r.Method == http.MethodPost && path == "/api/install":
		// Legacy alias of /api/import for archives: raw zip/tar.gz/tar bytes
		// (app.json at the root). Kept for scripts and older clients; the
		// dashboard's 📥 button uses /api/import.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "install is only allowed from this computer"})
			return
		}
		s.serveInstall(w, r)
	case r.Method == http.MethodPost && path == "/api/import":
		// Type-aware import: an app archive (zip/tar.gz), a portable .exe
		// (wrapped, never executed), a single .html page (hosted app), a
		// local app folder (?folder=), or a URL (?url=). Writes into the
		// deck's app container, so loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "import is only allowed from this computer"})
			return
		}
		s.serveImport(w, r)
	case r.Method == http.MethodGet && path == "/api/import/progress":
		// Live milestone log of a running import (?jobid= handle).
		s.serveJobProgress(w, r)
	case r.Method == http.MethodGet && path == "/api/import/jobs":
		// Every import running on this deck, with the label its starter gave
		// it — the dashboard's dock attaches to jobs it did not start itself.
		s.serveJobList(w, r)
	case r.Method == http.MethodGet && path == "/api/pkg/probe":
		// Read-only package-manager lookup behind the import offer.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "package lookup is only allowed from this computer"})
			return
		}
		s.servePkgProbe(w, r)
	case r.Method == http.MethodPost && path == "/api/import/cancel":
		// Abort a stuck import: cancels the job's context (git clone,
		// downloads) and flags every later step to bail out.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "import is only allowed from this computer"})
			return
		}
		s.serveImportCancel(w, r)
	case r.Method == http.MethodGet && path == "/api/presets":
		// Prerequisite toolchains: status of go/rust/git/python/node/docker/nginx,
		// install (default or custom dir), register an existing path.
		s.servePresets(w, r)
	case r.Method == http.MethodPost && path == "/api/presets/install":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "installs are only allowed from this computer"})
			return
		}
		s.servePresetInstall(w, r)
	case r.Method == http.MethodPost && path == "/api/presets/path":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "path registration is only allowed from this computer"})
			return
		}
		s.servePresetPath(w, r)
	case r.Method == http.MethodPost && path == "/api/presets/remove":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "removal is only allowed from this computer"})
			return
		}
		s.servePresetRemove(w, r)
	case r.Method == http.MethodPost && path == "/api/downloads/pick":
		// Choose where the native window's downloads land (OS folder picker).
		// Loopback only, like every other path-reading call.
		s.serveDownloadPick(w, r)
	case r.Method == http.MethodPost && path == "/api/pick-path":
		// Opens the OS folder picker on this machine and returns the chosen
		// path (browsers never hand out real paths). Loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "the folder picker is only allowed from this computer"})
			return
		}
		s.servePickPath(w, r)
	case r.Method == http.MethodGet && path == "/api/storage":
		// Read-only storage status; the launcher never blocks boot if the
		// configured volume is missing, it only warns instead.
		s.serveStorage(w, r)
	case r.Method == http.MethodPost && path == "/api/storage/move":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "storage moves are only allowed from this computer"})
			return
		}
		s.serveStorageMove(w, r)
	case r.Method == http.MethodGet && path == "/api/themes":
		s.serveThemes(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/themes/preview/"):
		// Read-only pack artwork, same reachability as /api/themes itself.
		s.serveThemePreview(w, strings.TrimPrefix(path, "/api/themes/preview/"))
	case r.Method == http.MethodPost && path == "/api/themes/apply":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "theme changes are only allowed from this computer"})
			return
		}
		s.serveThemeApply(w, r)
	case r.Method == http.MethodPost && path == "/api/themes/import":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "theme imports are only allowed from this computer"})
			return
		}
		s.serveThemeImport(w, r)
	case r.Method == http.MethodPost && path == "/api/themes/delete":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "theme deletion is only allowed from this computer"})
			return
		}
		s.serveThemeDelete(w, r)
	case r.Method == http.MethodGet && path == "/api/backgrounds":
		// Read-only pack list + appearance settings; the backdrop itself is
		// injected into the served page, so remote browsers need this too.
		s.serveBackgrounds(w, r)
	case r.Method == http.MethodGet && path == "/api/security":
		// Engine status + verdicts: readable like the pack lists; the
		// writes below are loopback-guarded.
		s.serveSecurityInfo(w, r)
	case r.Method == http.MethodPost && path == "/api/security/scan":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "security scans are only allowed from this computer"})
			return
		}
		s.serveSecurityScan(w, r)
	case r.Method == http.MethodPost && path == "/api/security/confirm":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "security confirmations are only allowed from this computer"})
			return
		}
		s.serveSecurityConfirm(w, r)
	case r.Method == http.MethodPost && path == "/api/security/selftest":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "the security self-test is only allowed from this computer"})
			return
		}
		s.serveSecuritySelftest(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/backgrounds/file/"):
		// One asset file inside a pack, referenced by pack CSS url(…).
		s.serveBackgroundFile(w, r, strings.TrimPrefix(path, "/api/backgrounds/file/"))
	case r.Method == http.MethodPost && path == "/api/backgrounds/apply":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "background changes are only allowed from this computer"})
			return
		}
		s.serveBackgroundApply(w, r)
	case r.Method == http.MethodPost && path == "/api/backgrounds/settings":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "background settings changes are only allowed from this computer"})
			return
		}
		s.serveBackgroundSettings(w, r)
	case r.Method == http.MethodPost && path == "/api/backgrounds/import":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "background imports are only allowed from this computer"})
			return
		}
		s.serveBackgroundImport(w, r)
	case r.Method == http.MethodPost && path == "/api/backgrounds/delete":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "background deletion is only allowed from this computer"})
			return
		}
		s.serveBackgroundDelete(w, r)
	case r.Method == http.MethodGet && path == "/api/iconpacks":
		s.serveIconPacks(w, r)
	case r.Method == http.MethodPost && path == "/api/iconpacks/apply":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "icon pack changes are only allowed from this computer"})
			return
		}
		s.serveIconPackApply(w, r)
	case r.Method == http.MethodPost && path == "/api/iconpacks/apply-one":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "icon pack changes are only allowed from this computer"})
			return
		}
		s.serveIconPackApplyOne(w, r)
	case r.Method == http.MethodPost && path == "/api/iconpacks/import":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "icon pack imports are only allowed from this computer"})
			return
		}
		s.serveIconPackImport(w, r)
	case r.Method == http.MethodPost && path == "/api/iconpacks/delete":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "icon pack deletion is only allowed from this computer"})
			return
		}
		s.serveIconPackDelete(w, r)
	case r.Method == http.MethodPost && path == "/api/tls/regenerate":
		// Fresh key + certificate for the HTTPS listener, swapped in live.
		// Loopback only — the compromise-response button must not be
		// reachable by whatever the caller is worried about.
		s.serveTLSRegenerate(w, r)
	case r.Method == http.MethodPost && path == "/api/show":
		// Bring a running desktop app's window to the front — the desktop
		// equivalent of Open. Loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "show is only allowed from this computer"})
			return
		}
		s.serveShow(w, r)
	case r.Method == http.MethodPost && path == "/api/restart":
		// Relaunch the launcher (used after a self-update): spawn the
		// swapped-in executable, then shut this process down cleanly.
		// Loopback only — this decides the fate of the launcher itself.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "restart is only allowed from this computer"})
			return
		}
		s.serveRestart(w, r)
	case r.Method == http.MethodPost && path == "/api/update":
		// Launcher self-update from an uploaded zip (or ?url=, or the JSON
		// github/local sources): swaps the running executable, keeps the
		// previous one as .old, asks for a restart. Loopback only — this
		// replaces the program itself.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "update is only allowed from this computer"})
			return
		}
		s.serveUpdate(w, r)
	case r.Method == http.MethodGet && path == "/api/update/check":
		// What could the GitHub channel install right now? Read-only: release
		// list (the downgrade dropdown), selected tag's asset, up-to-date.
		s.serveUpdateCheck(w, r)
	case r.Method == http.MethodPost && path == "/api/deck/rebuild":
		// Rebuild the deck from disk: generate app.json for installed app
		// folders that lack one (older all-in-one extractions never wrote
		// manifests, so an update to a smaller registry emptied the deck),
		// optionally importing app folders from a source path. Loopback only
		// — it mutates the data directory.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "rebuilding the deck is only allowed from this computer"})
			return
		}
		s.serveDeckRebuild(w, r)
	case r.Method == http.MethodGet && path == "/api/settings":
		qs := parseQuery(r.RequestURI)
		id := qs["id"]
		if s.appByID(id) == nil {
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
			return
		}
		doc, exists, err := s.readAppSettings(id)
		if err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out := appSettingsDoc{ID: id}
		if exists {
			out.Settings = doc
		}
		s.writeJSON(w, http.StatusOK, out)
	case r.Method == http.MethodPost && path == "/api/settings":
		// Store a per-app settings document. Writes machine-local state, so
		// loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "settings changes are only allowed from this computer"})
			return
		}
		var req appSettingsDoc
		readBodyInto(r, &req)
		if s.appByID(req.ID) == nil {
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
			return
		}
		if len(req.Settings) == 0 {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "settings document missing"})
			return
		}
		if err := s.writeAppSettings(req.ID, req.Settings); err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case r.Method == http.MethodGet && path == "/api/logs":
		s.serveLogs(w, r.RequestURI)
	case r.Method == http.MethodGet && path == "/api/icon":
		s.serveIcon(w, r)
	case r.Method == http.MethodPost && path == "/api/terminal":
		// ⌨ Open a terminal in a CLI app's folder. Launches a window on
		// this machine only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "opening a terminal is only allowed from this computer"})
			return
		}
		s.serveTerminal(w, r)
	case r.Method == http.MethodGet && path == "/favicon.svg":
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte(faviconSVG))
	case r.Method == http.MethodGet && path == "/site.webmanifest":
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Header().Set("Cache-Control", "max-age=86400")
		_, _ = w.Write([]byte(siteWebManifest))
	case r.Method == http.MethodGet && (path == "/dashboard.css" || path == "/dashboard.js"):
		s.serveDashboardAsset(w, path)
	case r.Method == http.MethodGet && path == "/favicon.ico":
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && path == "/api/network":
		// Flip one web app's Wi-Fi sharing (the dashboard's drag between the
		// two sections). Writes machine-local state, so loopback only.
		s.serveNetworkSet(w, r)
	case r.Method == http.MethodPost && path == "/api/privilege":
		// Flip one app's privileged launch (its own macOS TCC identity).
		// Machine-local security state, so loopback only.
		s.servePrivilegeToggle(w, r)
	case r.Method == http.MethodGet && path == "/api/launcher-settings":
		s.serveLauncherSettings(w, r)
	case r.Method == http.MethodGet && path == "/api/limits":
		s.serveLimits(w, r)
	case r.Method == http.MethodPost && path == "/api/launcher-settings":
		s.serveLauncherSettings(w, r)
	case r.Method == http.MethodGet && path == "/api/deck/export":
		// The whole user setup — Apps container, registry, Wi-Fi overrides —
		// as one zip. Machine-local data, so loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "deck export is only allowed from this computer"})
			return
		}
		s.serveDeckExport(w)
	case r.Method == http.MethodPost && path == "/api/deck/import":
		// Restore a deck export: apps not already installed are extracted,
		// Wi-Fi overrides merge. Machine-local data, so loopback only.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "deck import is only allowed from this computer"})
			return
		}
		s.serveDeckImport(w, r)
	case r.Method == http.MethodPost && path == "/api/start":
		s.serveStartStop(w, r, true)
	case r.Method == http.MethodPost && path == "/api/stop":
		s.serveStartStop(w, r, false)
	case r.Method == http.MethodPost && path == "/api/all/start":
		s.serveAll(w, true)
	case r.Method == http.MethodPost && path == "/api/all/stop":
		s.serveAll(w, false)
	case r.Method == http.MethodPost && path == "/api/uninstall":
		// Deleting an app folder is destructive machine-local state. A LAN
		// peer may start and stop apps (sharing is the point of the LAN
		// bind), but it may not remove them.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "uninstall is only allowed from this computer"})
			return
		}
		noteActivity()
		s.serveUninstall(w, r)
	case r.Method == http.MethodPost && path == "/api/expose":
		// Create/remove the app's OS artifact — the /Applications link, the
		// Start-menu pin, the .desktop entry (expose_<goos>.go). Loopback
		// only: it writes outside the deck container.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "expose is only allowed from this computer"})
			return
		}
		noteActivity()
		s.serveExpose(w, r)
	case r.Method == http.MethodPost && path == "/api/uninstall/batch":
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "uninstall is only allowed from this computer"})
			return
		}
		noteActivity()
		s.serveUninstallBatch(w, r)
	case r.Method == http.MethodGet && path == "/api/apps/removed":
		// What this machine uninstalled from the payload — the deck's restore
		// list. Loopback only (see serveRemovedApps).
		s.serveRemovedApps(w, r)
	case r.Method == http.MethodPost && path == "/api/apps/restore":
		noteActivity()
		s.serveRestoreApps(w, r)
	case r.Method == http.MethodPost && path == "/api/apps/forget":
		// Stop offering named removals back — the record stays for payload
		// apps (it is what keeps them off the deck), stale ones are dropped.
		// Loopback only (see serveForgetApps).
		noteActivity()
		s.serveForgetApps(w, r)
	case r.Method == http.MethodPost && path == "/api/datafolder":
		// Opens a Finder/Explorer window on this desktop — never on behalf of
		// a remote caller.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "opening a data folder is only allowed from this computer"})
			return
		}
		noteActivity()
		s.serveDataFolder(w, r)
	default:
		// An unmatched bare path is exactly what an embedded app's
		// root-absolute references produce — rescue it into that app's
		// embed shim before giving up.
		if s.embedFallback(w, r) {
			return
		}
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// serveInstall handles POST /api/install — the legacy archive-only installer,
// kept as an alias of /api/import for scripts and older clients: extract,
// validate, register, and report the installed app's id (the dashboard
// refreshes its list after).
func (s *Server) serveInstall(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, installMaxArchiveBytes+1))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading upload: " + err.Error()})
		return
	}
	app, err := s.installArchiveBytes(body, nil)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.reloadApps(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("installed %s, but rescanning the registry failed: %v", app.ID, err)})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"id":   app.ID,
		"name": app.Name,
		"note": "installed and discovered — it appears on the dashboard now",
	})
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "json encode error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (s *Server) serveIndex(w http.ResponseWriter) {
	body := []byte(nil)
	if embeddedBuild {
		body = embeddedIndexHTML
	} else {
		body, _ = os.ReadFile(s.cfg.IndexFile)
		if len(body) == 0 {
			body = dashboardHTML // bare exe: no index.html beside it — use the embedded dashboard
		}
	}
	if len(body) == 0 {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "index.html missing"})
		return
	}
	body = injectThemeCSS(body, s.themeCSS())
	// Background styles land after theme styles: the backdrop (and a pack's
	// panel overrides) win the cascade over both base and theme.
	body = injectBackgroundCSS(body, s.backgroundCSS(), readBackgroundConf(s.cfg.DataDir).varsCSS())
	// Boot-time frame info: the native darwin window needs its mac-frame
	// layout from the very first paint — /api/health answers too late, and
	// the traffic-lights zone would overlap the glass until it does.
	boot := fmt.Sprintf(`<script>window.__bootFrame={os:%q,window:%q};</script>`, runtime.GOOS, launchWindow)
	if n := strings.Count(string(body), "<body>"); n == 1 {
		body = []byte(strings.Replace(string(body), "<body>", "<body>"+boot, 1))
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	// no-store: a self-update swaps the binary under the page, and a cached
	// dashboard lags the launcher (missing buttons/routes) until a hard reload.
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) serveDashboardAsset(w http.ResponseWriter, path string) {
	name := strings.TrimPrefix(path, "/")
	var body []byte
	var err error
	if !embeddedBuild {
		body, err = os.ReadFile(filepath.Join(filepath.Dir(s.cfg.IndexFile), name))
	}
	if len(body) == 0 {
		body, err = readDashboardSource(name)
	}
	if err != nil || len(body) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if strings.HasSuffix(name, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) serveLogs(w http.ResponseWriter, requestURI string) {
	qs := parseQuery(requestURI)
	app := s.appByID(qs["id"])
	if app == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
		return
	}
	s.writeJSON(w, http.StatusOK, logsResp{
		ID:    app.ID,
		Name:  app.Name,
		Lines: s.tailLog(app, logTailLines),
	})
}

// parseQuery mirrors the Python handler's ad-hoc parser: split on "&", keep
// only "k=v" pairs (first "=" splits), last duplicate wins, no URL decoding.
func parseQuery(requestURI string) map[string]string {
	qs := map[string]string{}
	i := strings.Index(requestURI, "?")
	if i < 0 {
		return qs
	}
	for _, pair := range strings.Split(requestURI[i+1:], "&") {
		j := strings.Index(pair, "=")
		if j < 0 {
			continue
		}
		qs[pair[:j]] = pair[j+1:]
	}
	return qs
}

type idRequest struct {
	ID string `json:"id"`
}

type openRequest struct {
	URL string `json:"url"`
}

// ---- Wi-Fi sharing overrides (drag a card between the two sections) ----
// network.json in the data dir records explicit per-app user choices; the
// manifest's network field stays the default when no override exists. The
// endpoint is loopback-only: the dashboard itself is reachable from the
// Wi-Fi, so a LAN peer must never be able to expose local apps.

type networkRequest struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

func (s *Server) networkOverridesFile() string {
	return filepath.Join(s.cfg.DataDir, "network.json")
}

func (s *Server) loadNetworkOverrides() map[string]bool {
	data, err := os.ReadFile(s.networkOverridesFile())
	if err != nil {
		return map[string]bool{}
	}
	ov := map[string]bool{}
	if json.Unmarshal(data, &ov) != nil {
		return map[string]bool{}
	}
	return ov
}

func (s *Server) saveNetworkOverrides(ov map[string]bool) {
	// Prune ids that no longer exist so the file cannot grow forever.
	for id := range ov {
		if s.byID[id] == nil {
			delete(ov, id)
		}
	}
	data, err := json.MarshalIndent(ov, "", "  ")
	if err != nil {
		return
	}
	path := s.networkOverridesFile()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// applyNetworkOverrides layers saved user choices over the manifest
// defaults — the user's choice wins and survives re-imports and updates.
func (s *Server) applyNetworkOverrides(apps []*App) {
	ov := s.loadNetworkOverrides()
	if len(ov) == 0 {
		return
	}
	for _, a := range apps {
		if enabled, ok := ov[a.ID]; ok {
			a.Network = enabled
		}
	}
}

// serveNetworkSet flips one web app's Wi-Fi sharing. A running app restarts
// so the new bind host (0.0.0.0 vs 127.0.0.1) takes effect immediately.
func (s *Server) serveNetworkSet(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "Wi-Fi sharing changes are only allowed from this computer"})
		return
	}
	if s.cfg.DataDir == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no data directory"})
		return
	}
	var req networkRequest
	readBodyInto(r, &req)
	s.mu.Lock()
	app := s.byID[req.ID]
	if app == nil {
		s.mu.Unlock()
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
		return
	}
	if app.Desktop {
		s.mu.Unlock()
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "desktop apps have no web page to share — nothing to put on the Wi-Fi"})
		return
	}
	if _, busy := s.inflight[app.ID]; busy {
		s.mu.Unlock()
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": busyMessage})
		return
	}
	app.Network = req.Enabled
	ov := s.loadNetworkOverrides()
	ov[app.ID] = req.Enabled
	s.saveNetworkOverrides(ov)
	s.mu.Unlock()

	note := "will apply on next Start"
	if entry := loadState(s.cfg.StateFile)[app.ID]; entry.PID != 0 && pidAlive(entry.PID) {
		if resp := s.stopApp(app); resp != nil && !resp.OK {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "note": "sharing saved, but stopping the app failed: " + resp.Error})
			return
		}
		if resp := s.startApp(app); resp != nil && !resp.OK {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "note": "sharing saved, but restarting failed: " + resp.Error})
			return
		}
		note = "restarted with the new sharing setting"
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "note": note, "status": s.appStatus(app, loadState(s.cfg.StateFile))})
}

// fromLocalMachine reports whether the request's TCP peer is this computer.
// This is the connection's real source address — not the Host header, which
// any LAN client can set to "127.0.0.1" to impersonate a local caller.
func fromLocalMachine(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// readBodyInto mirrors Python _body(): no Content-Length or invalid JSON →
// leave v untouched.
func readBodyInto(r *http.Request, v any) {
	n, err := strconv.Atoi(strings.TrimSpace(r.Header.Get("Content-Length")))
	if err != nil || n <= 0 {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, int64(n)))
	if err != nil && len(data) == 0 {
		return
	}
	_ = json.Unmarshal(data, v)
}

// readBody mirrors the Python handler's ad-hoc parser for {id} bodies.
func readBody(r *http.Request) idRequest {
	var out idRequest
	readBodyInto(r, &out)
	return out
}

// startRequest is the /api/start body: the app id plus an optional custom
// port (1024–65535) to run this instance on instead of the configured one.
type startRequest struct {
	ID   string `json:"id"`
	Port int    `json:"port"`
	// SecurityOverride confirms the ⚠ start-gate for a flagged app. Only
	// honored from this machine, and only after the dashboard showed the
	// findings (the hold response carries them).
	SecurityOverride bool `json:"securityOverride"`
}

func (s *Server) serveStartStop(w http.ResponseWriter, r *http.Request, start bool) {
	var req startRequest
	readBodyInto(r, &req)
	app := s.appByID(req.ID)
	if app == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
		return
	}
	if !start {
		res := s.stopApp(app)
		st := s.appStatus(app, loadState(s.cfg.StateFile))
		res.Status = &st
		s.writeJSON(w, http.StatusOK, res)
		return
	}

	// Start-gate: an app flagged by the security scan asks once per launch
	// before it runs — the risk moment is Start, not the import. The
	// confirmation is loopback-only and remembered for this launcher run.
	if app.Security == "flagged" && !req.SecurityOverride {
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "this app is flagged by the security scan — confirm the start from this computer"})
			return
		}
		vd := loadSecurityVerdicts(s.securityDataDir())[app.ID]
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "securityHold": true, "id": app.ID, "name": app.Name,
			"findings": vd.Findings,
		})
		return
	}
	if req.SecurityOverride {
		// The override confirms the scan findings, which is a local decision.
		// Without this check the gate above is skipped entirely whenever the
		// field is set, so a remote caller could start a flagged app by
		// asserting the confirmation instead of being shown it.
		if !fromLocalMachine(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "confirming a flagged app's start is only allowed from this computer"})
			return
		}
		secStartConfirmed.Store(app.ID, true)
	}

	// Custom port: run this start on the requested port instead of the
	// configured one. The override is recorded in the state entry so Open,
	// lanUrl and Stop keep working against it until the app is stopped.
	port := 0
	if req.Port != 0 {
		if req.Port < 1024 || req.Port > 65535 {
			s.writeJSON(w, http.StatusBadRequest,
				map[string]string{"error": "custom port must be between 1024 and 65535"})
			return
		}
		if req.Port == s.cfg.Port {
			s.writeJSON(w, http.StatusBadRequest,
				map[string]string{"error": fmt.Sprintf("port %d is the launcher's own port", req.Port)})
			return
		}
		if app.Desktop {
			s.writeJSON(w, http.StatusBadRequest,
				map[string]string{"error": "desktop apps have no port"})
			return
		}
		port = req.Port
	}

	effective := app
	if port != 0 {
		c := *app
		c.Port = port
		effective = &c
	}
	res := s.startApp(effective)
	// The card carries the last start failure until the next successful
	// start — one glance at the deck says which app needs attention.
	s.mu.Lock()
	stx := loadState(s.cfg.StateFile)
	e := stx[app.ID]
	if res.OK {
		e.LastError = ""
	} else if !res.Busy && res.Error != "" {
		e.LastError = lastN(res.Error, 160)
	}
	if e.LastError != "" || e.PID != 0 || e.PortOverride != 0 || e.Token != "" {
		stx[app.ID] = e
		saveState(s.cfg.StateFile, stx)
	}
	s.mu.Unlock()
	// An app that died at startup announcing another OS uninstalls itself:
	// the card could only repeat the failure. Local starts only — a LAN
	// peer may start apps but not remove them, the uninstall route's rule.
	if !res.OK && !res.Busy && fromLocalMachine(r) {
		if note := s.removeIfIncompatible(app, res); note != "" {
			res.Removed = true
			res.Note = note
			// A reload error leaves a stale card until the next rescan; the
			// folder is already gone and nothing can start from it.
			_ = s.reloadApps()
			go s.snapshotDeck()
		}
	}
	if port != 0 && res.OK && !res.AlreadyRunning {
		s.mu.Lock()
		st := loadState(s.cfg.StateFile)
		if e, ok := st[app.ID]; ok {
			e.PortOverride = port
			st[app.ID] = e
			saveState(s.cfg.StateFile, st)
		}
		s.mu.Unlock()
	}
	st := s.appStatus(effective, loadState(s.cfg.StateFile))
	res.Status = &st
	code := http.StatusOK
	if !res.OK && !res.Busy {
		code = http.StatusInternalServerError
	}
	s.writeJSON(w, code, res)
}

// appIsRunning is the running test used by /api/all/*: a port probe for web
// apps, the state PID's liveness for desktop apps (they have no port; the
// probe would read them as perpetually stopped and Start All would spawn
// duplicates).
func (s *Server) appIsRunning(a *App) bool {
	if a.Desktop {
		return pidAlive(loadState(s.cfg.StateFile)[a.ID].PID)
	}
	port := a.Port
	if e := loadState(s.cfg.StateFile)[a.ID].PortOverride; e != 0 {
		port = e
	}
	return portOpen(port, defaultProbeTimeout)
}

func (s *Server) serveAll(w http.ResponseWriter, start bool) {
	results := []*actionResp{}
	for _, a := range s.appsList() {
		if start && s.appIsRunning(a) {
			continue // already running → nothing to start
		}
		if !start && !s.appIsRunning(a) {
			continue // already stopped → nothing to stop
		}
		if start && a.Security == "flagged" && !securityStartOK(a) {
			// Flagged apps never ride Start All: their risk was accepted
			// (or not) one app at a time, from the card.
			results = append(results, &actionResp{OK: false, ID: a.ID,
				Error: "flagged by the security scan — start it individually to confirm"})
			continue
		}
		var res *actionResp
		if start {
			res = s.startApp(a)
		} else {
			res = s.stopApp(a)
		}
		res.ID = a.ID
		results = append(results, res)
	}
	s.writeJSON(w, http.StatusOK, allResp{OK: true, Results: results})
}

// ------------------------------------------------------------------- main ----

// hostedHandler wraps a hosted app's handler with the small slice of deck
// APIs its pages may need from their own origin: the TokenReport auto-scan
// (window.showDirectoryPicker is unavailable inside WKWebView, so the app
// reads the known agent-log homes through this loopback endpoint instead).
// The reader is mounted on the TokenReport origin only — on every hosted app
// it would hand any imported page a same-origin read of the agent-log homes,
// whose session transcripts routinely contain tokens and pasted secrets.
func hostedHandler(s *Server, app http.Handler, a *App) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.ID == "tokenreport" {
			if r.Method == http.MethodGet && r.URL.Path == "/__tokenreport__/list" {
				s.serveTokenreportList(w, r)
				return
			}
			if r.Method == http.MethodGet && r.URL.Path == "/__tokenreport__/read" {
				s.serveTokenreportRead(w, r)
				return
			}
		}
		app.ServeHTTP(w, r)
	})
}

// tokenreportAgentPath resolves one of the allow-listed agent-log homes to
// an absolute directory, or returns "" if the path is not allowed.
func tokenreportAgentPath(raw string) string {
	allowed := map[string]bool{
		"~/.zcode/cli/rollout":        true,
		"~/.claude/projects":          true,
		"~/.codex/sessions":           true,
		"~/.copilot":                  true,
		"~/.openclaw-autoclaw/agents": true,
	}
	if !allowed[raw] {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, raw[2:])
}

// serveTokenreportList backs the TokenReport auto-scan: GET
// /__tokenreport__/list?path=~/.zcode/cli/rollout (loopback only) counts —
// and names — the log files the app's parsers can read under path. The app
// cannot read the filesystem itself (browser sandbox), and a folder picker
// every start was the exact friction this removes.
func (s *Server) serveTokenreportList(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "local only"})
		return
	}
	qs := parseQuery(r.RequestURI)
	raw, err := url.QueryUnescape(strings.TrimSpace(qs["path"]))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad path"})
		return
	}
	raw = strings.TrimSpace(raw)
	root := tokenreportAgentPath(raw)
	if root == "" {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "path not allowed"})
		return
	}
	files := 0
	var names []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		ok := false
		for _, ext := range []string{".jsonl", ".json", ".db", ".db-wal"} {
			if strings.HasSuffix(d.Name(), ext) {
				ok = true
				break
			}
		}
		if ok {
			files++
			if names == nil {
				names = []string{}
			}
			names = append(names, rel)
		}
		return nil
	})
	if names == nil {
		names = []string{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": raw, "resolved": root, "files": files, "names": names})
}

// serveTokenreportRead streams ONE log file from an allow-listed agent home
// to the TokenReport app (loopback only): GET
// /__tokenreport__/read?path=~/.zcode/cli/rollout&file=<relative-path>.
// The relative path is cleaned and may not escape the root; only .jsonl,
// .json, .db and .db-wal files are served — the exact set the app's parsers
// consume. No directory listings outside the roots, no arbitrary reads.
func (s *Server) serveTokenreportRead(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "local only"})
		return
	}
	qs := parseQuery(r.RequestURI)
	raw, err := url.QueryUnescape(strings.TrimSpace(qs["path"]))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad path"})
		return
	}
	root := tokenreportAgentPath(raw)
	if root == "" {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "path not allowed"})
		return
	}
	rel, err := url.QueryUnescape(strings.TrimSpace(qs["file"]))
	if err != nil || rel == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file required"})
		return
	}
	rel = filepath.Clean("/" + rel) // forces relative, collapses ../
	if strings.Contains(rel, "..") {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "path not allowed"})
		return
	}
	switch {
	case strings.HasSuffix(rel, ".jsonl"), strings.HasSuffix(rel, ".json"),
		strings.HasSuffix(rel, ".db"), strings.HasSuffix(rel, ".db-wal"):
	default:
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "file type not allowed"})
		return
	}
	full := filepath.Join(root, rel)
	// A planted symlink inside an agent home must not pull content from
	// outside it: resolve the real paths of file and root and compare.
	// EvalSymlinks on a missing file just fails — os.Open below turns that
	// into the 404.
	if realFull, serr := filepath.EvalSymlinks(full); serr == nil {
		if realRoot, rerr := filepath.EvalSymlinks(root); rerr == nil &&
			realFull != realRoot && !strings.HasPrefix(realFull, realRoot+string(os.PathSeparator)) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "path not allowed"})
			return
		}
	}
	f, err := os.Open(full)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.Copy(w, f)
}

// ---- background watchers: liveness probe, crash watchdog, watch-folder ----
