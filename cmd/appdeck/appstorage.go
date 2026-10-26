// Machine-side per-app storage placement. A named location (an external
// drive, a second volume) can host an app's data home or its whole program
// folder; the choice is machine state — stored beside the privilege
// overrides, never in the authored app.json — and lands on disk as
// symlink/junction indirection: the canonical path stays where discovery,
// the binary resolver, imports and deck export look, and simply points
// elsewhere. The launcher's own bookkeeping (logs, elevation scripts,
// pidfiles) never moves: an unplugged drive must not cost the launcher its
// diagnostics. A previous placement's copy is kept under
// <data>/appstorage-keep/<id>/ until the user deletes it from the dialog —
// outside the Apps container, so manifest discovery never sees a duplicate.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// storageState is the persisted machine-local choice set. Placement values:
// "local" (default, also "" and "data:<name>" / "app:<name>".
type storageState struct {
	Locations map[string]string `json:"locations"` // name → absolute path
	Placement map[string]string `json:"placement"` // app id → placement
}

func (s *Server) appStorageFile() string {
	if s.cfg.DataDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.DataDir, "appstorage.json")
}

func loadAppStorage(path string) storageState {
	st := storageState{Locations: map[string]string{}, Placement: map[string]string{}}
	if path == "" {
		return st
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	if st.Locations == nil {
		st.Locations = map[string]string{}
	}
	if st.Placement == nil {
		st.Placement = map[string]string{}
	}
	return st
}

func saveAppStorage(path string, st storageState) error {
	if path == "" {
		return fmt.Errorf("no data directory for the storage state")
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// appDataHome is the app-facing data dir (exposed as APPDECK_DATA): a
// sibling of the launcher's own per-app plumbing inside appDataDir, so a
// data-only move never takes the logs or elevation scripts with it.
func (s *Server) appDataHome(a *App) string {
	base := s.appDataDir(a)
	if base == "" {
		return ""
	}
	home := filepath.Join(base, "appdata")
	_ = os.MkdirAll(home, 0o755)
	return home
}

// dirLinkTarget reports the path a directory link points at ("", false
// when path is not a link). Windows junctions surface as symlinks through
// Lstat, so both indirection kinds are covered.
func dirLinkTarget(path string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	target, err := os.Readlink(path)
	if err != nil {
		return "", false
	}
	return target, true
}

// danglingLink reports a link whose target is missing — the signature of an
// external drive that is not connected.
func danglingLink(path string) bool {
	if _, isLink := dirLinkTarget(path); !isLink {
		return false
	}
	_, err := os.Stat(path) // resolves the link
	return err != nil
}

// copyTreeVerify copies every regular file under src into dst, preserving
// relative layout and permission bits, then verifies the file count — the
// same discipline as the whole-deck move: a short copy never gets to swap
// anything. Reading through a link copies its content, so src may be the
// canonical path even when it already points at another location — but
// WalkDir does not descend into a root symlink, so it is resolved first.
func copyTreeVerify(src, dst string) error {
	if target, isLink := dirLinkTarget(src); isLink {
		src = target
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("creating %s failed: %w", dst, err)
	}
	var files int
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // links and specials never move
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files++
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			_ = in.Close()
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = in.Close()
			_ = out.Close()
			return err
		}
		_ = in.Close()
		_ = out.Close()
		return nil
	})
	if err != nil {
		return fmt.Errorf("copying %s to %s failed: %v (nothing was moved)", src, dst, err)
	}
	var got int
	_ = filepath.WalkDir(dst, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			got++
		}
		return nil
	})
	if got < files {
		return fmt.Errorf("verification failed: %d of %d files copied to %s (nothing was moved)", got, files, dst)
	}
	return nil
}

// appPlacement returns the app's current placement ("local" default).
func (s *Server) appPlacement(a *App) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := loadAppStorage(s.appStorageFile())
	if p, ok := st.Placement[a.ID]; ok && p != "" {
		return p
	}
	return "local"
}

func parsePlacement(p string) (mode, name string) {
	mode, name, _ = strings.Cut(p, ":")
	if mode != "data" && mode != "app" {
		return "local", ""
	}
	return mode, name
}

// storageProblem reports an external placement whose drive is not
// connected — Start must fail with the reason, not with a phantom
// "building from source" or a dead APPDECK_DATA.
func (s *Server) appStorageProblem(a *App) string {
	for _, p := range []string{a.Dir, s.appDataHome(a)} {
		if p == "" {
			continue
		}
		if target, isLink := dirLinkTarget(p); isLink && danglingLink(p) {
			return fmt.Sprintf("%s is on a drive that is not connected (%s) — plug it in, or use 💾 Storage on the app card to reset to local", a.Name, target)
		}
	}
	return ""
}

// savedCopiesList returns the kept copies of previous placements.
func (s *Server) savedCopiesList(a *App) []string {
	root := filepath.Join(s.cfg.DataDir, "appstorage-keep", a.ID)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, filepath.Join(root, e.Name()))
	}
	return out
}

// cleanupSavedCopies deletes the kept copies after the user confirms the
// new placement works.
func (s *Server) cleanupSavedCopies(a *App) (int, error) {
	n := 0
	for _, p := range s.savedCopiesList(a) {
		if err := os.RemoveAll(p); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// moveAppStorage relocates one app: "data" (program folder stays local,
// the app's data home moves), "app" (whole program folder; its data home
// moves with it), or "local" (reset). A running app is stopped first; the
// copy-verify runs before anything is swapped; the previous local copy is
// kept under appstorage-keep until deleted from the dialog.
func (s *Server) moveAppStorage(a *App, mode, location string) (string, error) {
	if mode != "local" && mode != "data" && mode != "app" {
		return "", fmt.Errorf("unknown placement mode %q", mode)
	}
	if e := loadState(s.cfg.StateFile)[a.ID]; e.PID != 0 && pidAlive(e.PID) {
		if res := s.stopApp(a); res != nil && !res.OK {
			return "", fmt.Errorf("could not stop the app: %s", res.Error)
		}
	}
	s.mu.Lock()
	st := loadAppStorage(s.appStorageFile())
	if mode == "local" {
		delete(st.Placement, a.ID)
		err := saveAppStorage(s.appStorageFile(), st)
		s.mu.Unlock()
		if err != nil {
			return "", err
		}
		return s.resetAppToCanonical(a)
	}
	base, ok := st.Locations[location]
	st.Placement[a.ID] = mode + ":" + location
	err := saveAppStorage(s.appStorageFile(), st)
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("unknown location %q — add it in the 💾 Storage dialog first", location)
	}
	if err != nil {
		return "", err
	}

	// canonical path → target on the location
	var linkPath, target string
	switch mode {
	case "app":
		linkPath, target = a.Dir, filepath.Join(base, "apps", a.ID)
	default: // data
		home := s.appDataHome(a)
		if home == "" {
			return "", fmt.Errorf("no writable app data directory")
		}
		linkPath, target = home, filepath.Join(base, "data", a.ID)
	}
	if danglingLink(linkPath) {
		return "", fmt.Errorf("%s currently points at a drive that is not connected — plug it in, or reset to local first", linkPath)
	}
	if info, err := os.Stat(base); err != nil || !info.IsDir() {
		return "", fmt.Errorf("location %q is not connected (%s)", location, base)
	}
	if _, err := os.Stat(target); err == nil {
		return "", fmt.Errorf("%s already exists on %q — remove it (or pick another location) and try again", target, location)
	}
	if err := copyTreeVerify(linkPath, target); err != nil {
		return "", err
	}

	// Swap: a real canonical dir is kept aside (same volume, outside the
	// Apps container); a previous link is simply repointed.
	keepNote := ""
	if _, isLink := dirLinkTarget(linkPath); isLink {
		if err := os.Remove(linkPath); err != nil {
			return "", fmt.Errorf("removing the previous link: %v", err)
		}
	} else {
		keep := filepath.Join(s.cfg.DataDir, "appstorage-keep", a.ID,
			mode+"-"+strconv.FormatInt(time.Now().Unix(), 10))
		if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
			return "", err
		}
		if err := os.Rename(linkPath, keep); err != nil {
			return "", fmt.Errorf("setting aside the previous copy failed: %v (nothing was lost)", err)
		}
		keepNote = fmt.Sprintf(" — the previous copy is kept at %s until you delete it here", keep)
	}
	if err := makeDirLink(linkPath, target); err != nil {
		return "", fmt.Errorf("linking %s → %s failed: %v (the previous copy is intact)", linkPath, target, err)
	}
	return fmt.Sprintf("%s now lives on %q%s", a.Name, location, keepNote), nil
}

// resetAppToCanonical moves content from the external location back to the
// canonical local paths. The external copy stays on its drive — it is the
// backup until the user removes it.
func (s *Server) resetAppToCanonical(a *App) (string, error) {
	reset := func(canonical string) (moved bool, note string, err error) {
		target, isLink := dirLinkTarget(canonical)
		if !isLink {
			return false, "", nil
		}
		if danglingLink(canonical) {
			if err := os.Remove(canonical); err != nil {
				return false, "", err
			}
			if err := os.MkdirAll(canonical, 0o755); err != nil {
				return false, "", err
			}
			return true, fmt.Sprintf("%s was not connected — recreated an empty local folder; the external copy at %s is untouched", a.Name, target), nil
		}
		if err := os.Remove(canonical); err != nil {
			return false, "", err
		}
		if err := copyTreeVerify(target, canonical); err != nil {
			return false, "", err
		}
		return true, "", nil
	}
	notes := []string{}
	for _, p := range []string{a.Dir, s.appDataHome(a)} {
		if p == "" {
			continue
		}
		_, note, err := reset(p)
		if err != nil {
			return "", fmt.Errorf("resetting %s failed: %v — the external copy is intact", p, err)
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	msg := a.Name + " is back on the local disk (the external copy stays on its drive)"
	if len(notes) > 0 {
		msg += " — " + strings.Join(notes, "; ")
	}
	return msg, nil
}

// makeDirLink points linkPath at target: a symlink where the platform
// allows one, a junction on Windows (junctions need no developer mode).
// The link path must not exist.
func makeDirLink(linkPath, target string) error {
	err := os.Symlink(target, linkPath)
	if err == nil {
		return nil
	}
	return junctionFallback(linkPath, target, err)
}

// appStorageLocation names the location an app lives on ("" when local) —
// the dashboard's 💾 card badge.
func (s *Server) appStorageLocation(a *App) string {
	_, name := parsePlacement(s.appPlacement(a))
	return name
}

// serveAppStorageGet answers GET /api/appstorage?id=: the app's placement,
// the registered locations, the live sizes and the kept copies — the
// dialog renders its choices from this.
func (s *Server) serveAppStorageGet(w http.ResponseWriter, r *http.Request) {
	a := s.appByID(r.URL.Query().Get("id"))
	if a == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
		return
	}
	s.mu.Lock()
	st := loadAppStorage(s.appStorageFile())
	s.mu.Unlock()
	placement := "local"
	if p, ok := st.Placement[a.ID]; ok && p != "" {
		placement = p
	}
	mode, name := parsePlacement(placement)
	type loc struct{ Name, Path string }
	locs := make([]loc, 0, len(st.Locations))
	for n, p := range st.Locations {
		locs = append(locs, loc{n, p})
	}
	sort.Slice(locs, func(i, j int) bool { return locs[i].Name < locs[j].Name })
	dataBytes := int64(0)
	if home := s.appDataHome(a); home != "" {
		dataBytes = dirSizeBytes(home)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"placement":    placement,
		"mode":         mode,
		"location":     name,
		"locations":    locs,
		"programBytes": dirSizeBytes(a.Dir),
		"dataBytes":    dataBytes,
		"savedCopies":  s.savedCopiesList(a),
		"appDataHome":  s.appDataHome(a),
	})
}

// serveAppStoragePost answers POST /api/appstorage — machine-local storage
// choices, so loopback only, like the privilege toggle.
func (s *Server) serveAppStoragePost(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "storage changes are only allowed from this computer"})
		return
	}
	var req struct {
		Op       string `json:"op"` // location | location-delete | move | reset | cleanup
		ID       string `json:"id"`
		Mode     string `json:"mode"`
		Location string `json:"location"`
		Name     string `json:"name"`
		Path     string `json:"path"`
	}
	readBodyInto(r, &req)
	switch req.Op {
	case "location":
		name := strings.TrimSpace(req.Name)
		path, err := filepath.Abs(strings.TrimSpace(req.Path))
		if name == "" || strings.ContainsAny(name, ":/\\") {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "give the location a short name (no : or slashes)"})
			return
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": path + " is not a folder (mount the drive first)"})
			return
		}
		if !writableDir(path) {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": path + " is not writable"})
			return
		}
		s.mu.Lock()
		st := loadAppStorage(s.appStorageFile())
		st.Locations[name] = path
		err = saveAppStorage(s.appStorageFile(), st)
		s.mu.Unlock()
		if err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": "location " + name + " added"})
	case "location-delete":
		s.mu.Lock()
		st := loadAppStorage(s.appStorageFile())
		if _, used := st.Locations[req.Name]; !used {
			s.mu.Unlock()
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown location"})
			return
		}
		for id, p := range st.Placement {
			if _, name := parsePlacement(p); name == req.Name {
				s.mu.Unlock()
				s.writeJSON(w, http.StatusConflict, map[string]string{"error": "app " + id + " still lives on " + req.Name + " — reset it first"})
				return
			}
		}
		delete(st.Locations, req.Name)
		err := saveAppStorage(s.appStorageFile(), st)
		s.mu.Unlock()
		if err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": "location " + req.Name + " removed"})
	case "move":
		a := s.appByID(req.ID)
		if a == nil {
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
			return
		}
		note, err := s.moveAppStorage(a, req.Mode, req.Location)
		if err != nil {
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note})
	case "reset":
		a := s.appByID(req.ID)
		if a == nil {
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
			return
		}
		note, err := s.moveAppStorage(a, "local", "")
		if err != nil {
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note})
	case "cleanup":
		a := s.appByID(req.ID)
		if a == nil {
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
			return
		}
		n, err := s.cleanupSavedCopies(a)
		if err != nil {
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": fmt.Sprintf("%d saved cop%s deleted", n, map[bool]string{true: "y", false: "ies"}[n == 1])})
	default:
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown op " + req.Op})
	}
}
