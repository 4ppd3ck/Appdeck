// Theme packs: custom dashboard skins (and icon sets) that any operator can
// drop in without touching the launcher. A pack is a folder under
// <data>/themes/<name>/ holding:
//
//	theme.css    required — a stylesheet appended after the base dashboard CSS
//	icons/       optional — app icons keyed by app id (icons/pdfcraft.png);
//	             applying the pack overwrites the installed apps' icons
//
// Packs arrive as a zip upload or a git repository URL (the same import
// machinery as apps). The applied pack's name is stored in
// <data>/theme.choice and its CSS is injected into the dashboard on serve —
// after the base styles, so a pack only needs to override what it changes.
package main

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) themesDir() string {
	return filepath.Join(s.cfg.DataDir, "themes")
}

func themeChoicePath(dataDir string) string {
	return filepath.Join(dataDir, "theme.choice")
}

// dirExists reports whether path is a directory (fileExists is files only).
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// readThemeChoice returns the applied pack name ("" = base dashboard only).
func readThemeChoice(dataDir string) string {
	data, err := os.ReadFile(themeChoicePath(dataDir))
	if err != nil {
		return ""
	}
	name := slugify(string(data))
	if name == "" || !fileExists(filepath.Join(dataDir, "themes", name, "theme.css")) {
		return "" // choice without a pack on disk falls back to the base
	}
	return name
}

// themeCSS returns the applied pack's stylesheet ("" when none).
func (s *Server) themeCSS() string {
	name := readThemeChoice(s.cfg.DataDir)
	if name == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(s.themesDir(), name, "theme.css"))
	if err != nil {
		return ""
	}
	return string(data)
}

// injectThemeCSS appends the applied pack's stylesheet into the served
// dashboard (before </head>, so pack rules win over the base stylesheet by
// order without needing !important for equal-specificity rules).
func injectThemeCSS(body []byte, css string) []byte {
	if css == "" {
		return body
	}
	html := string(body)
	if i := strings.LastIndex(html, "</head>"); i >= 0 {
		style := "<style data-theme-pack>\n" + css + "\n</style>\n"
		html = html[:i] + style + html[i:]
		return []byte(html)
	}
	return body
}

// themePack describes one installed pack for the dashboard. CSS carries the
// pack's stylesheet so the dashboard can preview a switch live (rewrite the
// injected <style data-theme-pack> slot) without a page reload; only Apply
// persists the choice server-side.
type themePack struct {
	Name     string `json:"name"`
	Applied  bool   `json:"applied"`
	HasIcons bool   `json:"hasIcons"`
	Builtin  bool   `json:"builtin"`
	CSS      string `json:"css"`
}

// packHasIcons reports whether the pack's icons/ folder holds at least one
// real icon file (.png/.svg) — a reference folder alone does not count.
func packHasIcons(packDir string) bool {
	entries, err := os.ReadDir(filepath.Join(packDir, "icons"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if !e.IsDir() && (ext == ".png" || ext == ".svg") {
			return true
		}
	}
	return false
}

// isBuiltinTheme reports whether the name ships inside the executable
// (deleted built-ins re-seed on next boot, so they are not deletable).
func isBuiltinTheme(name string) bool {
	info, err := fs.Stat(builtinThemeFS, "theme/"+name+"/theme.css")
	return err == nil && !info.IsDir()
}

func (s *Server) listThemePacks() []themePack {
	applied := readThemeChoice(s.cfg.DataDir)
	var out []themePack
	entries, err := os.ReadDir(s.themesDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !fileExists(filepath.Join(s.themesDir(), e.Name(), "theme.css")) {
			continue // an unusable half-pack is not listed
		}
		css, _ := os.ReadFile(filepath.Join(s.themesDir(), e.Name(), "theme.css"))
		out = append(out, themePack{
			Name:     e.Name(),
			Applied:  e.Name() == applied,
			HasIcons: packHasIcons(filepath.Join(s.themesDir(), e.Name())),
			Builtin:  isBuiltinTheme(e.Name()),
			CSS:      string(css),
		})
	}
	return out
}

// serveThemes answers GET /api/themes: the installed packs and which is
// applied.
func (s *Server) serveThemes(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, s.listThemePacks())
}

// serveThemeApply answers POST /api/themes/apply {name}: stores the choice
// and, when the pack carries icons/, overwrites the matching installed
// apps' icon files (the dashboard's icon poll picks the change up within
// seconds). name "" reverts to the base dashboard.
func (s *Server) serveThemeApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	readBodyInto(r, &req)
	name := slugify(req.Name)
	if name != "" && !fileExists(filepath.Join(s.themesDir(), name, "theme.css")) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("theme %q is not installed", name)})
		return
	}
	tmp := themeChoicePath(s.cfg.DataDir) + ".tmp"
	if err := os.WriteFile(tmp, []byte(name), 0o644); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := os.Rename(tmp, themeChoicePath(s.cfg.DataDir)); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	note := "theme applied"
	if name == "" {
		note = "back to the base dashboard theme"
	}
	if icons := s.applyThemeIcons(name); icons > 0 {
		note += fmt.Sprintf("; icons overwritten on %d app(s)", icons)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note})
}

// applyThemeIcons copies icons/<app-id>.png|svg from a pack over the
// installed apps' icon files. Embedded apps pick the change up because the
// icon resolver prefers the disk copy over the packaged one.
func (s *Server) applyThemeIcons(name string) int {
	if name == "" {
		return 0
	}
	iconDir := filepath.Join(s.themesDir(), name, "icons")
	if !dirExists(iconDir) {
		return 0
	}
	applied := 0
	for _, a := range s.appsList() {
		if a.Dir == "" {
			continue
		}
		for _, ic := range appIconNames {
			pack := filepath.Join(iconDir, a.ID+filepath.Ext(ic.name))
			if !fileExists(pack) {
				continue
			}
			data, err := os.ReadFile(pack)
			if err != nil {
				continue
			}
			if err := os.WriteFile(filepath.Join(a.Dir, ic.name), data, 0o644); err == nil {
				applied++
			}
			break // one icon per app: png wins over svg by list order
		}
	}
	return applied
}

// serveThemeImport answers POST /api/themes/import: a zip body (or ?url=)
// becomes a pack under themes/<slug>/. The pack must carry a theme.css at
// its root (one wrapping folder is stripped, same rule as app archives).
func (s *Server) serveThemeImport(w http.ResponseWriter, r *http.Request) {
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
			_ = src // cloned below, after the target name is known
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

	if gitURL != "" {
		name := s.importThemeFromGit(w, gitURL, qs)
		if name == "" {
			return // response already written
		}
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "name": name,
			"note": fmt.Sprintf("theme %q imported — Apply it from the 🎨 dropdown", name),
		})
		return
	}

	if len(body) == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send a theme zip as the body, or a git URL (?url=)"})
		return
	}
	name, err := s.installThemePack(body, qs["name"])
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "name": name,
		"note": fmt.Sprintf("theme %q imported — Apply it from the 🎨 dropdown", name),
	})
}

// installThemePack extracts a zip into themes/<slug>/ and requires a
// theme.css at the pack root (one wrapping folder is stripped).
func (s *Server) installThemePack(body []byte, name string) (string, error) {
	if detectArchiveFormat(body) == "" {
		return "", fmt.Errorf("send a zip (or a git repository URL) containing theme.css")
	}
	opener, err := archiveOpener(body)
	if err != nil {
		return "", err
	}
	entries, manifests, err := scanArchive(opener)
	if err != nil {
		return "", err
	}
	_ = manifests
	// The stylesheet may sit at the root or inside one wrapping folder.
	prefix, _ := singleRootFolderNames(headerNames(entries))
	cssEntry := ""
	for _, h := range entries {
		rel := strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(h.name), prefix), "/")
		if rel == "theme.css" && !h.mode.IsDir() && h.mode&os.ModeType == 0 {
			cssEntry = h.name
			break
		}
	}
	if cssEntry == "" {
		return "", fmt.Errorf("the pack has no theme.css at its root — that file is the minimum a theme needs")
	}
	if strings.TrimSpace(name) == "" {
		// A wrapping folder names the pack; a root-level theme.css falls
		// back to "imported" (the dialog passes the zip's file name).
		name = strings.Trim(strings.TrimPrefix(filepath.ToSlash(prefix), "/"), "/")
		if name == "" || name == "." {
			name = "imported"
		}
	}
	slug := slugify(name)
	if slug == "" {
		return "", fmt.Errorf("%q produces no usable theme name", name)
	}
	dest := filepath.Join(s.themesDir(), slug)
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	if err := extractArchiveTree(opener, prefix, dest, entries); err != nil {
		os.RemoveAll(dest)
		return "", err
	}
	if !fileExists(filepath.Join(dest, "theme.css")) {
		os.RemoveAll(dest)
		return "", fmt.Errorf("theme extraction did not produce theme.css")
	}
	return slug, nil
}

// importThemeFromGit clones a repository (or one folder of it, via the same
// tree-URL rule as app imports) and installs it as a theme pack. Returns
// the pack name, or "" after writing the error response.
func (s *Server) importThemeFromGit(w http.ResponseWriter, rawurl string, qs map[string]string) string {
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
	tmp, err := os.MkdirTemp("", "appdeck-theme-")
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
	if !fileExists(filepath.Join(root, "theme.css")) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the repository (or folder) has no theme.css at its root — that file is the minimum a theme needs"})
		return ""
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
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("%q produces no usable theme name", name)})
		return ""
	}
	dest := filepath.Join(s.themesDir(), slug)
	os.RemoveAll(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return ""
	}
	// Copy theme.css, icons/, and anything else that is not repo baggage.
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if isJunkEntry(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info, ierr := d.Info(); ierr == nil && info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		return os.WriteFile(target, data, mode)
	})
	if err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return ""
	}
	if !fileExists(filepath.Join(dest, "theme.css")) {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "theme extraction did not produce theme.css"})
		return ""
	}
	return slug
}

// serveThemeDelete answers POST /api/themes/delete {name}: removes a pack;
// if it was applied, the dashboard reverts to the base theme.
func (s *Server) serveThemeDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	readBodyInto(r, &req)
	name := slugify(req.Name)
	if name == "" || !fileExists(filepath.Join(s.themesDir(), name, "theme.css")) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "theme not installed"})
		return
	}
	if isBuiltinTheme(name) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this theme is built into the launcher — it re-seeds on the next boot and cannot be deleted"})
		return
	}
	if err := os.RemoveAll(filepath.Join(s.themesDir(), name)); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if readThemeChoice(s.cfg.DataDir) == name {
		_ = os.Remove(themeChoicePath(s.cfg.DataDir))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": fmt.Sprintf("theme %q removed — the base theme is active", name)})
}
