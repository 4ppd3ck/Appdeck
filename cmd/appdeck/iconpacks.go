// Icon packs: per-app icon sets the operator can apply over the deck — the
// icon counterpart of theme and background packs. A pack is a folder under
// <data>/iconpacks/<name>/ holding one icon per app, named by app id:
//
//	<app-id>.png   (or .svg — png wins, the same order app folders use)
//
// Packs arrive as a zip upload or a git repository URL (the same machinery
// as themes/backgrounds). Applying a pack first backs up every affected
// app's current icon to <data>/icon-originals/<app-id>.<ext> (only the
// first time — later applies never touch the backup), so "Original" always
// restores the pre-pack artwork, however many packs were applied since.
// The applied pack's name lives in <data>/iconpack.choice; name "" in the
// apply call restores every app's original icon.
package main

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func (s *Server) iconPacksDir() string {
	return filepath.Join(s.cfg.DataDir, "iconpacks")
}

func (s *Server) iconOriginalsDir() string {
	return filepath.Join(s.cfg.DataDir, "icon-originals")
}

func iconPackChoicePath(dataDir string) string {
	return filepath.Join(dataDir, "iconpack.choice")
}

// readIconPackChoice returns the applied pack name ("" = originals).
func readIconPackChoice(dataDir string) string {
	data, err := os.ReadFile(iconPackChoicePath(dataDir))
	if err != nil {
		return ""
	}
	name := slugify(string(data))
	if name == "" {
		return ""
	}
	if !dirExists(filepath.Join(dataDir, "iconpacks", name)) {
		return ""
	}
	return name
}

func writeIconPackChoice(dataDir, name string) {
	tmp := iconPackChoicePath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, []byte(name), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, iconPackChoicePath(dataDir))
}

// packIconPath returns the pack's icon file for id (png preferred, then
// svg), or "" when the pack has no icon for that app.
func packIconPath(packDir, id string) string {
	id = slugify(id)
	if id == "" {
		return ""
	}
	for _, ext := range []string{".png", ".svg"} {
		if p := filepath.Join(packDir, id+ext); fileExists(p) {
			return p
		}
	}
	return ""
}

// currentIconFile finds the icon file the resolver would serve for the app,
// as a folder path plus its extension ("" when the app has no icon file —
// it runs on its app.json emoji).
func currentIconFile(a *App) (string, string) {
	if a.Dir == "" {
		return "", ""
	}
	entries, err := os.ReadDir(a.Dir)
	if err != nil {
		return "", ""
	}
	index := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			index[strings.ToLower(e.Name())] = e.Name()
		}
	}
	for _, base := range iconBaseNames(a) {
		for _, x := range iconExts {
			if real, ok := index[strings.ToLower(base)+x.ext]; ok {
				return filepath.Join(a.Dir, real), x.ext
			}
		}
	}
	return "", ""
}

// backupAppIcon saves the app's current icon (or a "had no icon file"
// marker) to <data>/icon-originals/ — once. Later applies and other packs
// never overwrite the backup.
func (s *Server) backupAppIcon(a *App) {
	if a.Dir == "" || a.ID == "" {
		return
	}
	origDir := s.iconOriginalsDir()
	for _, ext := range []string{".png", ".svg", ".ico", ".icns", ".none"} {
		if fileExists(filepath.Join(origDir, a.ID+ext)) {
			return // already backed up
		}
	}
	src, ext := currentIconFile(a)
	if err := os.MkdirAll(origDir, 0o755); err != nil {
		return
	}
	if src == "" {
		_ = os.WriteFile(filepath.Join(origDir, a.ID+".none"), nil, 0o644)
		return
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(origDir, a.ID+ext), data, 0o644)
}

// applyPackIconToApp writes one pack icon over the app's icon file. The
// app's previous artwork must be backed up first (backupAppIcon).
func applyPackIconToApp(a *App, packFile string) error {
	data, err := os.ReadFile(packFile)
	if err != nil {
		return err
	}
	ext := strings.ToLower(filepath.Ext(packFile)) // .png or .svg
	if ext == ".svg" {
		// png wins in the resolver order — a pack's svg only takes effect
		// when no icon.png sits beside it.
		_ = os.Remove(filepath.Join(a.Dir, "icon.png"))
	}
	return os.WriteFile(filepath.Join(a.Dir, "icon"+ext), data, 0o644)
}

// restoreAppIcon puts the backed-up artwork back (or removes the pack-added
// files when the app ran on its emoji). Reports whether anything changed.
func (s *Server) restoreAppIcon(a *App) bool {
	if a.Dir == "" || a.ID == "" {
		return false
	}
	origDir := s.iconOriginalsDir()
	managed := []string{"icon.png", "icon.svg"}
	for _, ext := range []string{".png", ".svg", ".ico", ".icns"} {
		p := filepath.Join(origDir, a.ID+ext)
		if !fileExists(p) {
			continue
		}
		if data, err := os.ReadFile(p); err == nil {
			if err := os.WriteFile(filepath.Join(a.Dir, "icon"+ext), data, 0o644); err != nil {
				return false
			}
			// Pack applies only ever add icon.png/icon.svg — drop the one
			// that is not the restored original's spelling.
			other := "icon.svg"
			if ext == ".svg" {
				other = "icon.png"
			}
			_ = os.Remove(filepath.Join(a.Dir, other))
			return true
		}
	}
	if fileExists(filepath.Join(origDir, a.ID+".none")) {
		for _, name := range managed {
			_ = os.Remove(filepath.Join(a.Dir, name))
		}
		return true
	}
	return false
}

// applyIconPack applies every matching icon of a pack ("" restores the
// originals deck-wide). Returns the number of apps touched.
func (s *Server) applyIconPack(name string) int {
	if name == "" {
		restored := 0
		for _, a := range s.appsList() {
			if s.restoreAppIcon(a) {
				restored++
			}
		}
		writeIconPackChoice(s.cfg.DataDir, "")
		return restored
	}
	packDir := filepath.Join(s.iconPacksDir(), name)
	if !dirExists(packDir) {
		return 0
	}
	applied := 0
	for _, a := range s.appsList() {
		if a.Dir == "" {
			continue
		}
		pf := packIconPath(packDir, a.ID)
		if pf == "" {
			continue
		}
		s.backupAppIcon(a)
		if err := applyPackIconToApp(a, pf); err == nil {
			applied++
		}
	}
	writeIconPackChoice(s.cfg.DataDir, name)
	return applied
}

// iconPack describes one installed pack for the dashboard. IDs lets the
// per-app picker ask "which of my apps does this pack restyle?".
type iconPack struct {
	Name    string   `json:"name"`
	Builtin bool     `json:"builtin"`
	Applied bool     `json:"applied"`
	Count   int      `json:"count"`
	IDs     []string `json:"ids,omitempty"`
}

func (s *Server) listIconPacks() []iconPack {
	applied := readIconPackChoice(s.cfg.DataDir)
	var out []iconPack
	entries, err := os.ReadDir(s.iconPacksDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		var ids []string
		files, err := os.ReadDir(filepath.Join(s.iconPacksDir(), e.Name()))
		if err == nil {
			for _, f := range files {
				if f.IsDir() {
					continue
				}
				ext := strings.ToLower(filepath.Ext(f.Name()))
				id := slugify(strings.TrimSuffix(f.Name(), filepath.Ext(f.Name())))
				if (ext == ".png" || ext == ".svg") && id != "" {
					ids = append(ids, id)
				}
			}
		}
		if len(ids) == 0 {
			continue // not an icon pack (no <app-id>.png/svg at its root)
		}
		out = append(out, iconPack{
			Name:    e.Name(),
			Builtin: isBuiltinIconPack(e.Name()),
			Applied: e.Name() == applied,
			Count:   len(ids),
			IDs:     ids,
		})
	}
	return out
}

// packIconBytes serves preview bytes: the pack's icon for id, or — for the
// reserved name "original" — the backed-up original artwork.
func (s *Server) packIconBytes(pack, id string) (data []byte, ctype string, ok bool) {
	id = slugify(id)
	if id == "" {
		return nil, "", false
	}
	var file string
	if pack == "original" {
		for _, ext := range []string{".png", ".svg"} {
			if p := filepath.Join(s.iconOriginalsDir(), id+ext); fileExists(p) {
				file = p
				break
			}
		}
	} else {
		file = packIconPath(filepath.Join(s.iconPacksDir(), slugify(pack)), id)
	}
	if file == "" {
		return nil, "", false
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, "", false
	}
	ctype = "image/png"
	if strings.EqualFold(filepath.Ext(file), ".svg") {
		ctype = "image/svg+xml"
	}
	return data, ctype, true
}

// ---- handlers ----

// serveIconPacks answers GET /api/iconpacks (public read, like themes).
func (s *Server) serveIconPacks(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"packs":  s.listIconPacks(),
		"choice": readIconPackChoice(s.cfg.DataDir),
	})
}

// serveIconPackApply answers POST /api/iconpacks/apply {name}: applies a
// pack deck-wide ("" restores every app's original icon).
func (s *Server) serveIconPackApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	readBodyInto(r, &req)
	name := slugify(req.Name)
	if name != "" && !dirExists(filepath.Join(s.iconPacksDir(), name)) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("icon pack %q is not installed", name)})
		return
	}
	n := s.applyIconPack(name)
	note := fmt.Sprintf("icon pack %q applied on %d app(s)", name, n)
	if name == "" {
		note = fmt.Sprintf("original icons restored on %d app(s)", n)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note})
}

// serveIconPackApplyOne answers POST /api/iconpacks/apply-one {id, pack}:
// one app gets one pack's icon (pack "" puts its original back) — the
// per-app picker behind the 🧩 dialog.
func (s *Server) serveIconPackApplyOne(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Pack string `json:"pack"`
	}
	readBodyInto(r, &req)
	a := s.appByID(req.ID)
	if a == nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown app id"})
		return
	}
	pack := slugify(req.Pack)
	if pack == "original" {
		pack = ""
	}
	if pack != "" {
		pf := packIconPath(filepath.Join(s.iconPacksDir(), pack), a.ID)
		if pf == "" {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("pack %q has no icon for %q", pack, a.ID)})
			return
		}
		s.backupAppIcon(a)
		if err := applyPackIconToApp(a, pf); err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": fmt.Sprintf("%s now wears the %s icon", a.Name, pack)})
		return
	}
	if !s.restoreAppIcon(a) {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": fmt.Sprintf("%s already wears its original icon", a.Name)})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": fmt.Sprintf("%s is back to its original icon", a.Name)})
}

// serveIconPackDelete answers POST /api/iconpacks/delete {name}. A deleted
// pack's applied icons stay in place; the choice just reverts to custom.
func (s *Server) serveIconPackDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	readBodyInto(r, &req)
	name := slugify(req.Name)
	if name == "" || !dirExists(filepath.Join(s.iconPacksDir(), name)) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "icon pack not installed"})
		return
	}
	if isBuiltinIconPack(name) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this icon pack is built into the launcher — it re-seeds on the next boot and cannot be deleted"})
		return
	}
	if err := os.RemoveAll(filepath.Join(s.iconPacksDir(), name)); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if readIconPackChoice(s.cfg.DataDir) == name {
		writeIconPackChoice(s.cfg.DataDir, "") // applied icons stay; no pack owns them now
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": fmt.Sprintf("icon pack %q removed (applied icons stay until you apply another)", name)})
}

// iconPackFileRe matches the only files a pack accepts at its root:
// <app-id>.png or <app-id>.svg, flat (sub-folders are pack baggage).
var iconPackFileRe = regexp.MustCompile(`(?i)^[a-z0-9][a-z0-9_-]*\.(png|svg)$`)

// serveIconPackImport answers POST /api/iconpacks/import: a zip body (or
// ?url= git repository) becomes a pack under iconpacks/<slug>/. Files that
// do not match an app-id naming are ignored; at least one must match.
func (s *Server) serveIconPackImport(w http.ResponseWriter, r *http.Request) {
	qs := parseQuery(r.RequestURI)
	if v := qs["name"]; v != "" {
		if dec, err := url.PathUnescape(v); err == nil {
			qs["name"] = dec
		}
	}
	var body []byte
	gitURL := ""
	if rawurl := qs["url"]; rawurl != "" {
		if src, ok := parseGitURL(rawurl); ok {
			gitURL = rawurl
			_ = src
		} else {
			data, _, err := fetchForImport(rawurl)
			if err != nil {
				s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			body = data
		}
	} else {
		data, err := io.ReadAll(io.LimitReader(r.Body, installMaxTotal+1))
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading upload: " + err.Error()})
			return
		}
		body = data
	}

	var name string
	if gitURL != "" {
		name = s.importIconPackFromGit(w, gitURL, qs)
	} else {
		var err error
		name, err = s.installIconPack(body, qs["name"])
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	if name == "" {
		return // response already written
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "name": name,
		"note": fmt.Sprintf("icon pack %q imported — Apply it from the 🧩 dialog", name),
	})
}

// installIconPack extracts <app-id>.png/svg entries from a zip into
// iconpacks/<slug>/ (one wrapping folder is stripped, same rule as the
// other pack kinds).
func (s *Server) installIconPack(body []byte, name string) (string, error) {
	if detectArchiveFormat(body) == "" {
		return "", fmt.Errorf("send a zip (or a git repository URL) containing <app-id>.png icons")
	}
	opener, err := archiveOpener(body)
	if err != nil {
		return "", err
	}
	entries, _, err := scanArchive(opener)
	if err != nil {
		return "", err
	}
	prefix, _ := singleRootFolderNames(headerNames(entries))
	if strings.TrimSpace(name) == "" {
		name = strings.Trim(strings.TrimPrefix(filepath.ToSlash(prefix), "/"), "/")
		if name == "" || name == "." {
			name = "imported"
		}
	}
	slug := slugify(name)
	if slug == "" {
		return "", fmt.Errorf("%q produces no usable icon pack name", name)
	}
	dest := filepath.Join(s.iconPacksDir(), slug)
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	count := 0
	if err := extractArchiveTree(opener, prefix, dest, entries); err != nil {
		os.RemoveAll(dest)
		return "", err
	}
	// Packs are flat <app-id>.png/svg folders — drop everything the tree
	// extraction wrote that is not one, and slugify what remains.
	kept, err := os.ReadDir(dest)
	if err == nil {
		for _, e := range kept {
			if e.IsDir() {
				os.RemoveAll(filepath.Join(dest, e.Name()))
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			id := slugify(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
			if !iconPackFileRe.MatchString(id + ext) {
				_ = os.Remove(filepath.Join(dest, e.Name()))
				continue
			}
			if id+ext != e.Name() {
				_ = os.Rename(filepath.Join(dest, e.Name()), filepath.Join(dest, id+ext))
			}
			count++
		}
	}
	if count == 0 {
		os.RemoveAll(dest)
		return "", fmt.Errorf("no <app-id>.png/.svg files found at the archive root — name icons after the apps they replace (e.g. descale.png)")
	}
	return slug, nil
}

// importIconPackFromGit clones a repository and installs its top-level
// <app-id>.png/svg files as a pack. Returns "" after writing the error.
func (s *Server) importIconPackFromGit(w http.ResponseWriter, rawurl string, qs map[string]string) string {
	git, err := gitToolPath()
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "git toolchain not found — install git (https://git-scm.com)"})
		return ""
	}
	src, ok := parseGitURL(rawurl)
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a git repository URL — use https://github.com/user/repo (or …/tree/main/Folder)"})
		return ""
	}
	tmp, err := os.MkdirTemp("", "appdeck-iconpack-")
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return ""
	}
	defer os.RemoveAll(tmp)
	if err := gitClone(git, src, tmp, nil); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return ""
	}
	root := tmp
	if src.subpath != "" {
		root = filepath.Join(tmp, filepath.FromSlash(src.subpath))
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("the repository has no folder %q", src.subpath)})
			return ""
		}
	}
	name := strings.TrimSpace(qs["name"])
	if name == "" {
		name = src.name
		if src.subpath != "" {
			name = filepath.Base(src.subpath)
		}
	}
	slug := slugify(name)
	if slug == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("%q produces no usable icon pack name", name)})
		return ""
	}
	dest := filepath.Join(s.iconPacksDir(), slug)
	os.RemoveAll(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return ""
	}
	count := 0
	entries, err := os.ReadDir(root)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !iconPackFileRe.MatchString(strings.ToLower(e.Name())) {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, e.Name()))
			if err != nil {
				continue
			}
			base := slugify(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))) + strings.ToLower(filepath.Ext(e.Name()))
			if err := os.WriteFile(filepath.Join(dest, base), data, 0o644); err == nil {
				count++
			}
		}
	}
	if count == 0 {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no <app-id>.png/.svg files found at the repository root — name icons after the apps they replace"})
		return ""
	}
	return slug
}

// isBuiltinIconPack reports whether the name ships inside the executable
// (deleted built-ins re-seed on next boot, so they are not deletable).
func isBuiltinIconPack(name string) bool {
	entries, err := fs.ReadDir(builtinIconPackFS, "iconpack/"+name)
	return err == nil && len(entries) > 0
}
