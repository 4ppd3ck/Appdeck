// Storage location: the whole app store (installed apps, their data dirs,
// state, settings) lives under one data directory — and that directory can
// live on any drive/volume. The location is a pointer file in the user's
// home (~/.appdeck-data-location), read at every boot, so a double-clicked
// launch follows it without environment variables.
//
// Resilience is the point ("no matter what"): when the pointer names a
// volume that is not mounted (an unplugged external drive), the launcher
// does not fail — it falls back to the default location, stamps a warning
// on the dashboard, and keeps booting. Moving is a verified copy: stop all
// apps, copy the tree preserving permissions, compare file counts, then
// flip the pointer. The old copy stays until the operator deletes it.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// storagePointerPath is the fixed, always-available location of the pointer
// (HOME survives unmounted volumes, launcher updates and data moves).
func storagePointerPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".appdeck-data-location")
}

// readStoragePointer returns the configured data directory, or "" when no
// pointer exists. A pointer that names a missing or read-only directory
// returns "" plus the reason — the caller falls back to the default and
// surfaces the problem instead of failing to boot.
func readStoragePointer() (string, string) {
	path := storagePointerPath()
	if path == "" {
		return "", ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "" // no pointer — the default location applies
	}
	dir := strings.TrimSpace(string(data))
	if dir == "" {
		return "", ""
	}
	info, err := os.Stat(dir)
	if err != nil {
		return dir, fmt.Sprintf("the configured data location %q is not available (volume not mounted?) — using the default location until it returns", dir)
	}
	if !info.IsDir() {
		return dir, fmt.Sprintf("the configured data location %q is not a folder — using the default location", dir)
	}
	if !writableDir(dir) {
		return dir, fmt.Sprintf("the configured data location %q is not writable — using the default location", dir)
	}
	return dir, ""
}

// writableDir reports whether a folder accepts file creation.
func writableDir(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	probe := filepath.Join(dir, ".appdeck-write-probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}

// writeStoragePointer records (or, with an empty path, clears) the data
// location. The write is atomic — a half-written pointer must never steer
// the next boot at a nonexistent path.
func writeStoragePointer(dir string) error {
	path := storagePointerPath()
	if path == "" {
		return fmt.Errorf("cannot determine the home directory for the location pointer")
	}
	if strings.TrimSpace(dir) == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%q must be an absolute path", dir)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.TrimSpace(dir)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// moveDataDir copies the whole data directory from src to dst, preserving
// file modes (exec bits matter — app binaries live here), and verifies the
// copy by comparing file counts. dst must be an empty or nonexistent folder
// on the target volume (a full copy, not a merge). Symlinks are skipped —
// nothing in the data tree needs them, and a dangling one must not abort a
// multi-gigabyte move.
func moveDataDir(src, dst string) error {
	if src == "" || dst == "" {
		return fmt.Errorf("both source and destination are required")
	}
	if src == dst {
		return fmt.Errorf("the data is already at %s", dst)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("creating %s failed: %w", dst, err)
	}
	if !writableDir(dst) {
		return fmt.Errorf("%s is not writable", dst)
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
			return nil // symlinks and specials never move
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
		return fmt.Errorf("copying %s to %s failed: %v (the original was not touched)", src, dst, err)
	}
	var gotFiles int
	_ = filepath.WalkDir(dst, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			gotFiles++
		}
		return nil
	})
	if gotFiles < files {
		return fmt.Errorf("verification failed: copied %d of %d files to %s — the original was not touched", gotFiles, files, dst)
	}
	return writeStoragePointer(dst)
}

// defaultStorageRoot is the classic per-user location the pointer overrides.
func defaultStorageRoot() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("LocalAppData"), "AppDeck")
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "AppDeck")
	}
	return filepath.Join(home, ".local", "share", "AppDeck")
}

// dirSizeBytes sums a tree (best effort — for the storage dialog display).
func dirSizeBytes(root string) int64 {
	if root == "" {
		return 0
	}
	var total int64
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// serveStorage answers GET /api/storage: where the app store lives now,
// whether a pointer file steers it, and how much space it holds.
func (s *Server) serveStorage(w http.ResponseWriter, r *http.Request) {
	pointed, problem := readStoragePointer()
	out := map[string]any{
		"ok":         true,
		"dataDir":    s.cfg.DataDir,
		"defaultDir": defaultStorageRoot(),
		"pointerSet": pointed != "",
		"bytes":      dirSizeBytes(s.cfg.DataDir),
	}
	if problem != "" {
		out["warning"] = problem
	}
	s.writeJSON(w, http.StatusOK, out)
}

// serveStorageMove answers POST /api/storage/move {path}: stops every
// running app, copies the whole data store to the chosen folder on the new
// volume, verifies the copy, and flips the pointer. The restart that
// follows boots from the new location; the old copy stays until deleted by
// hand (the safe side of a verified copy — nothing is removed for you).
func (s *Server) serveStorageMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	readBodyInto(r, &req)
	dst := strings.TrimSpace(req.Path)
	if dst == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "choose a folder first"})
		return
	}
	dst = expandHome(dst)
	if !filepath.IsAbs(dst) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("%q must be an absolute path", dst)})
		return
	}
	if info, err := os.Stat(dst); err == nil && !info.IsDir() {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("%q is not a folder", dst)})
		return
	}
	src := s.cfg.DataDir
	if src == "" {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "this build has no data directory to move"})
		return
	}
	if sameFile(src, dst) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the data already lives there"})
		return
	}
	if entries, err := os.ReadDir(dst); err == nil && len(entries) > 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("%s is not empty — pick (or create) an empty folder, e.g. a fresh \"AppDeck\" on that volume", dst)})
		return
	}
	for _, a := range s.appsList() {
		st := loadState(s.cfg.StateFile)
		if st[a.ID].PID == 0 || !pidAlive(st[a.ID].PID) {
			continue
		}
		if res := s.stopApp(a); !res.OK {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("stop %s before moving: %s", a.ID, res.Error)})
			return
		}
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := moveDataDir(src, dst); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"note": fmt.Sprintf("data store copied to %s and the location pointer updated — restart AppDeck to run from there. The old copy at %s stays until you delete it.", dst, src),
	})
}

// sameFile reports whether two paths name the same directory.
func sameFile(a, b string) bool {
	absA, _ := filepath.Abs(a)
	absB, _ := filepath.Abs(b)
	if filepath.Clean(absA) == filepath.Clean(absB) {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	if errA != nil || errB != nil || ia == nil || ib == nil {
		return false
	}
	return os.SameFile(ia, ib)
}
