// Background packs: gradient or image backdrops painted on a fixed layer
// behind the dashboard (theme packs restyle the UI, backgrounds sit under
// it). A pack is a folder under <data>/backgrounds/<name>/ holding:
//
//	background.css   required — styles #bg-layer (the fixed backdrop div);
//	                 image files may sit beside it and are referenced as
//	                 url("/api/backgrounds/file/<name>/<file>")
//
// Packs arrive as a zip upload or a git repository URL (the same machinery
// as themes). The applied pack's name lives in <data>/background.choice.
// Beside the pack choice, <data>/background.conf stores the appearance
// knobs the dashboard's 🖼 dialog exposes: per-mode brightness (light mode
// glare taming), layer opacity, window transparency (native windows only),
// and two optional custom gradient colors that override every pack's own
// colors.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func (s *Server) backgroundsDir() string {
	return filepath.Join(s.cfg.DataDir, "backgrounds")
}

func backgroundChoicePath(dataDir string) string {
	return filepath.Join(dataDir, "background.choice")
}

// readBackgroundChoice returns the applied pack name ("" = flat theme bg).
func readBackgroundChoice(dataDir string) string {
	data, err := os.ReadFile(backgroundChoicePath(dataDir))
	if err != nil {
		return ""
	}
	name := slugify(string(data))
	if name == "" || !fileExists(filepath.Join(dataDir, "backgrounds", name, "background.css")) {
		return ""
	}
	return name
}

// backgroundCSS returns the applied pack's stylesheet ("" when none).
func (s *Server) backgroundCSS() string {
	name := readBackgroundChoice(s.cfg.DataDir)
	if name == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(s.backgroundsDir(), name, "background.css"))
	if err != nil {
		return ""
	}
	return string(data)
}

// ---- appearance settings (background.conf) ----

// backgroundConf holds the user-tunable knobs. Zero values mean "unset" in
// the JSON; clamping happens after the defaults are laid in.
type backgroundConf struct {
	BrightnessDark  int    `json:"brightnessDark"`  // % filter, 20..100
	BrightnessLight int    `json:"brightnessLight"` // % filter, 20..100
	Opacity         int    `json:"opacity"`         // % layer opacity, 20..100
	Transparency    int    `json:"transparency"`    // % window see-through, 0..60 (0 = opaque; native windows only)
	Color1          string `json:"color1"`          // #rrggbb or "" (both set = custom gradient)
	Color2          string `json:"color2"`
}

func defaultBackgroundConf() backgroundConf {
	return backgroundConf{BrightnessDark: 100, BrightnessLight: 100, Opacity: 100}
}

func backgroundConfPath(dataDir string) string {
	return filepath.Join(dataDir, "background.conf")
}

func clampPct(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// hexColor returns the lowercase #rrggbb form or "" when not a valid color.
func hexColor(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != 7 || s[0] != '#' {
		return ""
	}
	for _, c := range s[1:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return ""
		}
	}
	return s
}

// readBackgroundConf loads background.conf over the defaults; a missing or
// corrupt file is the default look, never an error.
func readBackgroundConf(dataDir string) backgroundConf {
	conf := defaultBackgroundConf()
	data, err := os.ReadFile(backgroundConfPath(dataDir))
	if err == nil {
		_ = json.Unmarshal(data, &conf)
	}
	if conf.BrightnessDark == 0 {
		conf.BrightnessDark = 100
	}
	if conf.BrightnessLight == 0 {
		conf.BrightnessLight = 100
	}
	if conf.Opacity == 0 {
		conf.Opacity = 100
	}
	conf.BrightnessDark = clampPct(conf.BrightnessDark, 20, 100)
	conf.BrightnessLight = clampPct(conf.BrightnessLight, 20, 100)
	conf.Opacity = clampPct(conf.Opacity, 20, 100)
	conf.Transparency = clampPct(conf.Transparency, 0, maxWindowTransparency)
	conf.Color1 = hexColor(conf.Color1)
	conf.Color2 = hexColor(conf.Color2)
	return conf
}

// writeBackgroundConf stores the settings atomically (tmp + rename).
func writeBackgroundConf(dataDir string, conf backgroundConf) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return err
	}
	p := backgroundConfPath(dataDir)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// varsCSS renders the settings as CSS variables riding on #bg-layer. The
// per-mode pair feeds :root[data-theme=…] rules in the base stylesheet, so
// one injected block covers both theme toggle states. Custom colors use
// the same specificity as the base light-mode fallback rule and appear
// later, so they win over it and over any pack's own background.
func (c backgroundConf) varsCSS() string {
	var b strings.Builder
	fmt.Fprintf(&b, ":root{--bg-opacity:%.2f;--bg-brightness-dark:%.2f;--bg-brightness-light:%.2f}\n",
		float64(c.Opacity)/100, float64(c.BrightnessDark)/100, float64(c.BrightnessLight)/100)
	if c.Color1 != "" && c.Color2 != "" {
		fmt.Fprintf(&b, ":root[data-theme=\"dark\"] #bg-layer,:root[data-theme=\"light\"] #bg-layer{background:linear-gradient(135deg,%s,%s)}\n",
			c.Color1, c.Color2)
	}
	return b.String()
}

// injectBackgroundCSS appends the pack stylesheet and the settings vars
// into the served dashboard after any theme pack styles (injected first,
// so a background can restyle on top of a theme when both are applied).
func injectBackgroundCSS(body []byte, packCSS, userCSS string) []byte {
	if packCSS == "" && userCSS == "" {
		return body
	}
	html := string(body)
	add := ""
	if packCSS != "" {
		add += "<style data-background-pack>\n" + packCSS + "\n</style>\n"
	}
	if userCSS != "" {
		add += "<style data-background-user>\n" + userCSS + "\n</style>\n"
	}
	if i := strings.LastIndex(html, "</head>"); i >= 0 {
		return []byte(html[:i] + add + html[i:])
	}
	return body
}

// backgroundPack describes one installed pack for the dashboard. CSS
// carries the pack's stylesheet so the dashboard can preview a switch live
// (rewrite the injected <style data-background-pack> slot) without a page
// reload; only Apply persists the choice server-side.
type backgroundPack struct {
	Name    string `json:"name"`
	Applied bool   `json:"applied"`
	Builtin bool   `json:"builtin"`
	CSS     string `json:"css"`
}

// isBuiltinBackground reports whether the name ships inside the executable
// (deleted built-ins re-seed on next boot, so they are not deletable).
func isBuiltinBackground(name string) bool {
	info, err := fs.Stat(builtinBackgroundFS, "background/"+name+"/background.css")
	return err == nil && !info.IsDir()
}

func (s *Server) listBackgroundPacks() []backgroundPack {
	applied := readBackgroundChoice(s.cfg.DataDir)
	var out []backgroundPack
	entries, err := os.ReadDir(s.backgroundsDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !fileExists(filepath.Join(s.backgroundsDir(), e.Name(), "background.css")) {
			continue
		}
		css, _ := os.ReadFile(filepath.Join(s.backgroundsDir(), e.Name(), "background.css"))
		out = append(out, backgroundPack{
			Name:    e.Name(),
			Applied: e.Name() == applied,
			Builtin: isBuiltinBackground(e.Name()),
			CSS:     string(css),
		})
	}
	return out
}

// serveBackgrounds answers GET /api/backgrounds: the installed packs plus
// the current appearance settings (readable from the Wi-Fi too — only the
// writes are loopback-guarded).
func (s *Server) serveBackgrounds(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"packs":    s.listBackgroundPacks(),
		"settings": readBackgroundConf(s.cfg.DataDir),
	})
}

// serveBackgroundApply answers POST /api/backgrounds/apply {name}: stores
// the choice. name "" clears the backdrop back to the flat theme look.
func (s *Server) serveBackgroundApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	readBodyInto(r, &req)
	name := slugify(req.Name)
	if name != "" && !fileExists(filepath.Join(s.backgroundsDir(), name, "background.css")) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("background %q is not installed", name)})
		return
	}
	tmp := backgroundChoicePath(s.cfg.DataDir) + ".tmp"
	if err := os.WriteFile(tmp, []byte(name), 0o644); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := os.Rename(tmp, backgroundChoicePath(s.cfg.DataDir)); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	note := "background applied"
	if name == "" {
		note = "back to the flat theme background"
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note})
}

// serveBackgroundDelete answers POST /api/backgrounds/delete {name}:
// removes a pack; if it was applied, the backdrop reverts to none.
func (s *Server) serveBackgroundDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	readBodyInto(r, &req)
	name := slugify(req.Name)
	if name == "" || !fileExists(filepath.Join(s.backgroundsDir(), name, "background.css")) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "background not installed"})
		return
	}
	if isBuiltinBackground(name) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this background is built into the launcher — it re-seeds on the next boot and cannot be deleted"})
		return
	}
	if err := os.RemoveAll(filepath.Join(s.backgroundsDir(), name)); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if readBackgroundChoice(s.cfg.DataDir) == name {
		_ = os.Remove(backgroundChoicePath(s.cfg.DataDir))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": fmt.Sprintf("background %q removed — the flat theme background is back", name)})
}

// serveBackgroundSettings answers POST /api/backgrounds/settings: stores
// the brightness/opacity/colors knobs. The dashboard applies them live and
// saves debounced while the sliders move.
func (s *Server) serveBackgroundSettings(w http.ResponseWriter, r *http.Request) {
	req := defaultBackgroundConf()
	readBodyInto(r, &req)
	req.BrightnessDark = clampPct(req.BrightnessDark, 20, 100)
	req.BrightnessLight = clampPct(req.BrightnessLight, 20, 100)
	req.Opacity = clampPct(req.Opacity, 20, 100)
	req.Transparency = clampPct(req.Transparency, 0, maxWindowTransparency)
	req.Color1 = hexColor(req.Color1)
	req.Color2 = hexColor(req.Color2)
	if err := writeBackgroundConf(s.cfg.DataDir, req); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": req})
}

// serveBackgroundImport answers POST /api/backgrounds/import: a zip body
// (or ?url=) becomes a pack under backgrounds/<slug>/. The pack must carry
// a background.css at its root (one wrapping folder is stripped).
func (s *Server) serveBackgroundImport(w http.ResponseWriter, r *http.Request) {
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
		name := s.importBackgroundFromGit(w, gitURL, qs)
		if name == "" {
			return // response already written
		}
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "name": name,
			"note": fmt.Sprintf("background %q imported — Apply it from the 🖼 dialog", name),
		})
		return
	}

	if len(body) == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send a background zip as the body, or a git URL (?url=)"})
		return
	}
	name, err := s.installBackgroundPack(body, qs["name"])
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "name": name,
		"note": fmt.Sprintf("background %q imported — Apply it from the 🖼 dialog", name),
	})
}

// installBackgroundPack extracts a zip into backgrounds/<slug>/ and
// requires a background.css at the pack root (one wrapping folder is
// stripped, same rule as theme packs).
func (s *Server) installBackgroundPack(body []byte, name string) (string, error) {
	if detectArchiveFormat(body) == "" {
		return "", fmt.Errorf("send a zip (or a git repository URL) containing background.css")
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
	prefix, _ := singleRootFolderNames(headerNames(entries))
	cssEntry := ""
	for _, h := range entries {
		rel := strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(h.name), prefix), "/")
		if rel == "background.css" && !h.mode.IsDir() && h.mode&os.ModeType == 0 {
			cssEntry = h.name
			break
		}
	}
	if cssEntry == "" {
		return "", fmt.Errorf("the pack has no background.css at its root — that file is the minimum a background needs")
	}
	if strings.TrimSpace(name) == "" {
		name = strings.Trim(strings.TrimPrefix(filepath.ToSlash(prefix), "/"), "/")
		if name == "" || name == "." {
			name = "imported"
		}
	}
	slug := slugify(name)
	if slug == "" {
		return "", fmt.Errorf("%q produces no usable background name", name)
	}
	dest := filepath.Join(s.backgroundsDir(), slug)
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
	if !fileExists(filepath.Join(dest, "background.css")) {
		os.RemoveAll(dest)
		return "", fmt.Errorf("background extraction did not produce background.css")
	}
	return slug, nil
}

// importBackgroundFromGit clones a repository (or one folder of it) and
// installs it as a background pack. Returns the pack name, or "" after
// writing the error response.
func (s *Server) importBackgroundFromGit(w http.ResponseWriter, rawurl string, qs map[string]string) string {
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
	tmp, err := os.MkdirTemp("", "appdeck-background-")
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
	if !fileExists(filepath.Join(root, "background.css")) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the repository (or folder) has no background.css at its root — that file is the minimum a background needs"})
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
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("%q produces no usable background name", name)})
		return ""
	}
	dest := filepath.Join(s.backgroundsDir(), slug)
	os.RemoveAll(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return ""
	}
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
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
		data, err := os.ReadFile(p)
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
	if !fileExists(filepath.Join(dest, "background.css")) {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "background extraction did not produce background.css"})
		return ""
	}
	return slug
}

// serveBackgroundFile answers GET /api/backgrounds/file/<pack>/<file>: one
// asset inside a pack, referenced by pack CSS as url("/api/backgrounds/
// file/<pack>/<file>"). Only image extensions resolve; the name is
// slugified and the file path normalized, so nothing escapes the pack
// folder. Public like /api/icon — a backdrop is not secret.
func (s *Server) serveBackgroundFile(w http.ResponseWriter, r *http.Request, rest string) {
	name, file, _ := strings.Cut(strings.TrimPrefix(rest, "/"), "/")
	name = slugify(name)
	file = strings.TrimPrefix(path.Clean("/"+file), "/")
	if name == "" || file == "" || file == "." {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	ct := ""
	switch strings.ToLower(path.Ext(file)) {
	case ".jpg", ".jpeg":
		ct = "image/jpeg"
	case ".png":
		ct = "image/png"
	case ".webp":
		ct = "image/webp"
	case ".avif":
		ct = "image/avif"
	case ".gif":
		ct = "image/gif"
	case ".svg":
		ct = "image/svg+xml"
	default:
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	full := filepath.Join(s.backgroundsDir(), name, filepath.FromSlash(file))
	if !fileExists(full) {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "max-age=60")
	http.ServeFile(w, r, full)
}
