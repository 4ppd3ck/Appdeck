// Deck export/import and snapshot persistence.
package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// deckImportMaxBytes caps an uploaded deck zip; decks carry app binaries and
// can be genuinely large, but not unbounded.
const deckImportMaxBytes = 4 << 30

func (s *Server) serveDeckExport(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="appdeck-deck-%s.zip"`, time.Now().Format("2006-01-02")))
	zw := zip.NewWriter(w)
	if err := s.writeDeckExport(zw, 0); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
}

// writeDeckExport packs the deck — every app folder, the registry and the
// network overrides — into zw. maxByte caps the uncompressed total (0 = no
// cap) so an automatic snapshot of a huge deck aborts instead of churning
// the disk; the manual export passes 0.
func (s *Server) writeDeckExport(zw *zip.Writer, maxByte int64) error {
	container, err := s.appsContainerDir()
	if err != nil {
		return err
	}
	var total int64
	addFile := func(name, path string) {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return
		}
		hdr.Name = name
		hdr.Method = zip.Deflate
		out, err := zw.CreateHeader(hdr)
		if err != nil {
			return
		}
		total += info.Size()
		_, _ = io.Copy(out, f)
	}
	addDir := func(root, prefix string) {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil // zip entries imply their directories
			}
			if maxByte > 0 && total > maxByte {
				return filepath.SkipAll
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if strings.HasSuffix(rel, ".DS_Store") || strings.HasSuffix(rel, "Thumbs.db") ||
				strings.Contains(rel, "__MACOSX") {
				return nil
			}
			addFile(prefix+rel, p)
			return nil
		})
	}
	addDir(container, "Apps/")
	addFile("apps.json", s.cfg.AppsFile)
	if s.cfg.DataDir != "" {
		addFile("network.json", filepath.Join(s.cfg.DataDir, "network.json"))
	}
	if maxByte > 0 && total > maxByte {
		return fmt.Errorf("deck exceeds the snapshot size cap (%d bytes) — skipped", maxByte)
	}
	return nil
}

// deckSnapshotKeep/MaxBytes bound the automatic backups: the newest three,
// and a deck bigger than 512 MB (uncompressed) is skipped — snapshots must
// never turn an import into a disk-churning stall.
const (
	deckSnapshotKeep    = 3
	deckSnapshotMaxByte = 512 << 20
)

// snapshotDeck writes the deck export into <data>/backups/ (keeping the
// newest few) and runs in the background after deck changes: an import, an
// uninstall, a deck restore. One at a time; skipped when the previous one is
// still running.
func (s *Server) snapshotDeck() {
	if s.cfg.DataDir == "" {
		return
	}
	if !s.snapshotMu.TryLock() {
		return
	}
	defer s.snapshotMu.Unlock()
	dir := filepath.Join(s.cfg.DataDir, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "deck-"+time.Now().Format("2006-01-02-150405")+".zip")
	f, err := os.Create(path)
	if err != nil {
		return
	}
	zw := zip.NewWriter(f)
	err = s.writeDeckExport(zw, deckSnapshotMaxByte)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var snaps []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "deck-") && strings.HasSuffix(e.Name(), ".zip") {
			snaps = append(snaps, filepath.Join(dir, e.Name()))
		}
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i] > snaps[j] }) // newest name first
	if len(snaps) > deckSnapshotKeep {
		for _, old := range snaps[deckSnapshotKeep:] {
			os.Remove(old)
		}
	}
}

// serveDeckImport restores a deck export. Merge semantics: app files are
// written only where nothing exists yet (existing installs are never
// touched), Wi-Fi overrides merge with the imported values winning, and the
// central registry file is left alone — registry apps rejoin through
// discovery of their Apps/ folders.
func (s *Server) serveDeckImport(w http.ResponseWriter, r *http.Request) {
	container, err := s.appsContainerDir()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	tmp, err := os.CreateTemp("", "appdeck-import-*.zip")
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(r.Body, deckImportMaxBytes))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading upload: " + err.Error()})
		return
	}
	if n == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty upload — send a deck export zip"})
		return
	}
	zr, err := zip.NewReader(tmp, n)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a readable deck zip: " + err.Error()})
		return
	}

	// network.json is merged, not extracted: imported entries win, existing
	// local choices for other apps survive.
	var importedOverrides map[string]bool

	// Apps packaged inside this exe are never restored from the zip: the
	// embedded copy wins, so a restored folder would be a dead duplicate.
	embedded := embeddedAppIDs()

	applied, skipped := 0, 0
	var expanded int64
	capBlown := false
	for _, zf := range zr.File {
		name := filepath.ToSlash(zf.Name)
		if strings.HasSuffix(name, "/") || strings.Contains(name, "__MACOSX") ||
			strings.HasSuffix(name, ".DS_Store") || strings.HasSuffix(name, "Thumbs.db") {
			continue
		}
		var target, root string
		switch {
		case name == "network.json":
			if s.cfg.DataDir == "" {
				continue
			}
			src, err := zf.Open()
			if err != nil {
				continue
			}
			data, err := io.ReadAll(io.LimitReader(src, 1<<20))
			src.Close()
			if err != nil || json.Unmarshal(data, &importedOverrides) != nil {
				continue
			}
			continue // handled — never written to disk raw
		case name == "apps.json":
			continue // the registry stays local; Apps/ discovery brings apps in
		case strings.HasPrefix(name, "Apps/"):
			if embedded[strings.ToLower(strings.SplitN(strings.TrimPrefix(name, "Apps/"), "/", 2)[0])] {
				continue // packaged inside the exe — the embedded copy wins
			}
			root = container
			target = filepath.Join(container, strings.TrimPrefix(name, "Apps/"))
		default:
			continue // unknown entry — never written outside the known roots
		}
		clean := filepath.Clean(target)
		if clean != root && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
			continue // zip-slip attempt — ignored
		}
		if _, err := os.Lstat(clean); err == nil {
			skipped++ // existing installs are never overwritten
			continue
		}
		if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
			continue
		}
		src, err := zf.Open()
		if err != nil {
			continue
		}
		mode := os.FileMode(0o644)
		if zf.Mode()&0o111 != 0 {
			mode = 0o755 // keep executables executable (unix)
		}
		out, err := os.OpenFile(clean, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			src.Close()
			continue
		}
		n, cerr := io.Copy(out, src)
		src.Close()
		out.Close()
		if cerr != nil {
			os.Remove(clean)
			continue
		}
		expanded += n
		if expanded > installMaxTotal {
			// The same expanded-bytes cap an app import enforces — a deck
			// zip can fill the disk just as well. What already landed is
			// kept; the restore refuses to continue.
			os.Remove(clean)
			capBlown = true
			break
		}
		applied++
	}
	if importedOverrides != nil {
		ov := s.loadNetworkOverrides()
		for id, enabled := range importedOverrides {
			ov[id] = enabled
		}
		s.mu.Lock()
		s.saveNetworkOverrides(ov)
		s.mu.Unlock()
		s.applyNetworkOverrides(s.appsList())
	}
	s.reloadApps()
	if applied > 0 {
		go s.snapshotDeck()
	}
	if capBlown {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "applied": applied, "skipped": skipped,
			"error": "deck zip expands past the 1 GiB cap — refused; apps already extracted were kept"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "applied": applied, "skipped": skipped,
		"note": "apps merged — existing installs were not touched; registry apps rejoin via their Apps/ folders"})
}
