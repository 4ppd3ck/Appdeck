// Git import: POST /api/import?url=<repository url> clones a git repository
// (shallow, one branch) and installs it like a folder import. Repositories
// are code, not app packages: when the clone has no app.json one is
// generated from the project layout (Cargo workspace, Go module, or a root
// HTML page), and when there is no icon an image found in the repo is used
// or a lettered icon.svg is generated. Nothing is executed at import time —
// the clone only writes files.
//
// Monorepos: GitHub's web-UI folder URL —
// https://github.com/<owner>/<repo>/tree/<branch>/<folder> — imports only
// that folder as the app. The whole repo is still fetched (shallow), but
// only the folder is validated and installed.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// gitCloneTimeout bounds one clone; shallow single-branch clones of app-sized
// repositories finish well inside it.
const gitCloneTimeout = 10 * time.Minute

// gitSource is what an import URL points at: a repository (whole, or one
// folder inside it) and the suggested app name. owner/repo name the project
// for the prebuilt-release lookup (releases.go).
type gitSource struct {
	cloneURL string
	ref      string // branch or tag to check out ("" = remote default)
	subpath  string // folder inside the repo to install ("" = whole repo)
	name     string // suggested app name
	host     string // e.g. github.com ("" when unknown)
	owner    string // repository owner ("" when unknown)
	repo     string // repository name ("" when unknown)
}

// parseGitURL recognizes the URL shapes an import can clone: the plain
// repository form on the known hosts (github.com/gitlab.com/bitbucket.org/
// codeberg.org/sr.ht), the explicit .git form everywhere else, and GitHub's
// /tree/<ref>/<folder> subfolder form. It returns false for anything else —
// release-asset links and raw files keep flowing through the ordinary
// download path.
func parseGitURL(rawurl string) (gitSource, bool) {
	u, err := url.Parse(strings.TrimSpace(rawurl))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return gitSource{}, false
	}
	host := strings.ToLower(u.Hostname())
	segs := nonEmptySegments(u.Path)
	if len(segs) == 0 {
		return gitSource{}, false
	}
	for _, s := range segs {
		// Subfolder paths come from user-typed URLs; keep them inside the
		// clone and out of Windows drive syntax.
		if s == "." || s == ".." || strings.ContainsAny(s, "\\/:") {
			return gitSource{}, false
		}
	}
	base := u.Scheme + "://" + u.Host
	switch {
	case host == "github.com":
		if len(segs) >= 4 && strings.EqualFold(segs[2], "tree") {
			return gitSource{
				cloneURL: base + "/" + segs[0] + "/" + segs[1],
				ref:      segs[3],
				subpath:  strings.Join(segs[4:], "/"),
				name:     stripGitSuffix(segs[1]),
				host:     host, owner: segs[0], repo: stripGitSuffix(segs[1]),
			}, true
		}
		if len(segs) == 2 {
			return gitSource{
				cloneURL: base + "/" + segs[0] + "/" + segs[1],
				name:     stripGitSuffix(segs[1]),
				host:     host, owner: segs[0], repo: stripGitSuffix(segs[1]),
			}, true
		}
		return gitSource{}, false
	case host == "gitlab.com" || host == "bitbucket.org" || host == "codeberg.org" || strings.HasSuffix(host, ".sr.ht"):
		if len(segs) == 2 {
			return gitSource{
				cloneURL: base + "/" + segs[0] + "/" + segs[1],
				name:     stripGitSuffix(segs[1]),
				host:     host, owner: segs[0], repo: stripGitSuffix(segs[1]),
			}, true
		}
		return gitSource{}, false
	}
	// Any other host: only the explicit .git form.
	last := segs[len(segs)-1]
	if len(segs) >= 2 && strings.HasSuffix(strings.ToLower(last), ".git") {
		name := stripGitSuffix(last)
		return gitSource{cloneURL: base + "/" + strings.Join(segs, "/"), name: name}, true
	}
	return gitSource{}, false
}

func stripGitSuffix(name string) string {
	if strings.HasSuffix(strings.ToLower(name), ".git") {
		return name[:len(name)-4]
	}
	return name
}

func nonEmptySegments(path string) []string {
	var segs []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

// gitToolPath locates the git binary (a user-registered path from the 🧰 tab
// wins; then PATH; then the well-known locations — double-clicked .app
// launches get a minimal PATH, the same problem goToolPath works around).
func gitToolPath() (string, error) {
	if p := registeredToolPath("git"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("git"); err == nil {
		return p, nil
	}
	candidates := []string{
		"/usr/bin/git",                     // macOS CLT shim
		"/opt/homebrew/bin/git",            // Homebrew (Apple silicon)
		"/usr/local/bin/git",               // Homebrew (Intel) / manual
		"/opt/local/bin/git",               // MacPorts
		`C:\Program Files\Git\cmd\git.exe`, // Windows Git for Windows
		`C:\Program Files (x86)\Git\cmd\git.exe`,
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return c, nil
		}
	}
	return "", exec.ErrNotFound
}

// importGit clones src and installs it: the whole repository, or — when the
// URL named a folder — just that folder. A missing app.json is generated and
// a missing icon backfilled before the folder-import validation runs, so a
// plain code repository imports with zero app-side changes.
//
// GitHub repositories try the prebuilt path first (releases.go): a binary the
// project already published installs instantly; ?source=1 opts out and always
// imports from source.
func (s *Server) importGit(w http.ResponseWriter, src gitSource, qs map[string]string) {
	releasesHint := ""
	if src.host == "github.com" && src.owner != "" && qs["source"] != "1" {
		rel, relErr := fetchBestGitHubRelease(src.owner, src.repo)
		if rel != nil {
			appname := strings.TrimSpace(qs["appname"])
			if appname == "" {
				appname = src.name
				if src.subpath != "" {
					appname = filepath.Base(src.subpath)
				}
			}
			if handled, why := s.importReleaseAssets(w, rel, src.owner+"/"+src.repo, appname, qs); handled {
				return
			} else if why != "" {
				releasesHint = why
			}
		} else if relErr != nil {
			releasesHint = relErr.Error()
		}
	}
	git, err := gitToolPath()
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "git toolchain not found — install git (https://git-scm.com) and try again"})
		return
	}
	tmp, err := os.MkdirTemp("", "appdeck-git-")
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer os.RemoveAll(tmp)
	if err := gitClone(git, src, tmp); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	appDir := tmp
	if src.subpath != "" {
		sub := filepath.Join(append([]string{tmp}, strings.Split(src.subpath, "/")...)...)
		clean, err := filepath.Abs(sub)
		tmpAbs, _ := filepath.Abs(tmp)
		if err != nil || !strings.HasPrefix(clean, tmpAbs+string(filepath.Separator)) {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid folder in URL"})
			return
		}
		if info, err := os.Stat(clean); err != nil || !info.IsDir() {
			where := "the repository"
			if src.ref != "" {
				where = "branch " + src.ref + " of the repository"
			}
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("%s has no folder %q — check the URL", where, src.subpath)})
			return
		}
		// Only the named folder becomes the app: move it into its own temp
		// dir (same volume, so this is a rename) and install from there.
		stage, err := os.MkdirTemp("", "appdeck-app-")
		if err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer os.RemoveAll(stage)
		if err := os.Rename(clean, filepath.Join(stage, "app")); err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		appDir = filepath.Join(stage, "app")
	} else {
		os.RemoveAll(filepath.Join(appDir, ".git")) // history never installs
	}
	name := strings.TrimSpace(qs["appname"])
	if name == "" {
		name = src.name
		if src.subpath != "" {
			name = filepath.Base(src.subpath)
		}
	}
	if err := prepareGeneratedApp(appDir, name); err != nil {
		msg := err.Error()
		if hint := releasesFailureHint(releasesHint, src); hint != "" {
			msg += "\n\n" + hint
		}
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	s.importFolder(w, appDir)
}

// releasesFailureHint turns a skipped prebuilt lookup into the actionable
// next step: the project's releases page, where a binary for this platform
// may exist even when the automated lookup found none.
func releasesFailureHint(reason string, src gitSource) string {
	if src.host != "github.com" || src.owner == "" {
		return ""
	}
	what := "no prebuilt release asset matched " + runtime.GOOS + "/" + runtime.GOARCH
	if reason != "" {
		what = reason
	}
	return "Prebuilt binaries, if the project ships any: " +
		"https://github.com/" + src.owner + "/" + src.repo + "/releases — " +
		"download one for this platform and drop it into the import dialog (" + what + ")"
}

// gitClone runs a shallow single-branch clone into dest.
func gitClone(git string, src gitSource, dest string) error {
	args := []string{"clone", "--depth", "1", "--single-branch", "--quiet"}
	if src.ref != "" {
		args = append(args, "--branch", src.ref)
	}
	args = append(args, src.cloneURL, dest)
	ctx, cancel := context.WithTimeout(context.Background(), gitCloneTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, args...)
	hideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("git clone timed out after %s", gitCloneTimeout)
		}
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("git clone failed: %s", lastLines(message, 8))
	}
	return nil
}

// lastLines keeps the tail of a long command output — the part that carries
// the actual error.
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// prepareGeneratedApp backfills what a plain code repository lacks: an
// app.json generated from the project layout when none exists, and an icon
// (found in the repo, else generated) when there is none. An existing
// manifest is validated but never overwritten.
func prepareGeneratedApp(dir, name string) error {
	manifestPath := filepath.Join(dir, manifestFileName)
	if _, err := os.Stat(manifestPath); err == nil {
		if _, err := loadAppManifest(dir); err != nil {
			return fmt.Errorf("the repository has an app.json but it is not valid: %v", err)
		}
	} else if err := generateAppManifest(dir, name); err != nil {
		return err
	}
	return ensureAppIcon(dir, name)
}

// generateAppManifest writes an app.json for a repository that has none,
// deriving the launch mode from the layout: a Cargo workspace builds with
// cargo (desktop unless it serves HTTP), a Go module with go (same rule), a
// root HTML page is hosted in-process. Anything else is refused with what
// the author needs to add.
func generateAppManifest(dir, name string) error {
	id := slugify(name)
	if id == "" {
		return fmt.Errorf("%q produces no valid app id", name)
	}
	a := App{ID: id, Name: name, Category: "Tools", Icon: "📦", URLPath: "/"}
	switch {
	case fileExists(filepath.Join(dir, "Cargo.toml")):
		bin, err := pickRustBinary(dir, id)
		if err != nil {
			return err
		}
		a.Binary = bin
		if rustHTTPProject(dir) {
			a.Port = 0
		} else {
			a.Desktop = true
		}
	case fileExists(filepath.Join(dir, "go.mod")):
		a.Binary = id + "-launcher"
		if !goHTTPEntrypoint(dir) {
			a.Desktop = true
		}
	case isPythonProject(dir):
		cmdLine, web, err := generatePythonRun(dir)
		if err != nil {
			return err
		}
		a.Cmd = cmdLine
		a.Icon = "🐍"
		if web {
			a.Port = 0
		} else {
			a.Desktop = true
		}
	default:
		kind, entry := discoverableProject(dir)
		if kind == "static" {
			a.ServedBy = "launcher"
			if entry != "index.html" {
				a.URLPath = "/" + entry
			}
			break
		}
		// Name the layouts the launcher cannot run yet — a refusal that
		// says what was found is actionable, one that says "no layout" is
		// not (a Next.js site and an empty folder currently read the same).
		if files := cProjectIndicators(dir); len(files) > 0 {
			return fmt.Errorf("the repository is a C/C++ source project (%s) — AppDeck cannot "+
				"build C/C++ from source; import a prebuilt binary from the project's releases "+
				"instead, or add an app.json with a cmd field to run it yourself",
				strings.Join(files[:min(3, len(files))], ", "))
		}
		if fileExists(filepath.Join(dir, "package.json")) {
			return fmt.Errorf("the repository is a Node.js project (package.json) — AppDeck cannot " +
				"build or run Node apps yet; add an app.json with a cmd field to run it yourself")
		}
		return fmt.Errorf("the repository has no app.json and no recognizable layout " +
			"(Cargo.toml, go.mod, or a root .html page) — add an app.json describing the app")
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, manifestFileName), data, 0o644)
}

// isPythonProject reports whether the repository is a Python project by its
// dependency manifests (requirements.txt, pyproject.toml, setup.py).
func isPythonProject(dir string) bool {
	return fileExists(filepath.Join(dir, "requirements.txt")) ||
		fileExists(filepath.Join(dir, "pyproject.toml")) ||
		fileExists(filepath.Join(dir, "setup.py"))
}

// pythonWebCrates are the server frameworks whose presence in a Python
// project's dependency files marks it as a web app (the rest — CLIs, bots,
// scripts — become desktop cards).
var pythonWebCrates = []string{
	"flask", "fastapi", "uvicorn", "django", "tornado", "bottle",
	"sanic", "quart", "starlette", "gradio", "streamlit",
}

// generatePythonRun builds the shell command that sets up and runs a Python
// repository, plus whether it should be a web card. The per-app virtual
// environment lives under APPDECK_DATA — the app's writable directory, so
// re-imports keep the installed environment — and is created plus filled on
// first Start. Deliberately brace-free: the manifest validator treats {…}
// tokens as placeholders, and POSIX cmd strings run under /bin/sh -c.
func generatePythonRun(dir string) (string, bool, error) {
	entry := ""
	for _, c := range []string{"main.py", "app.py", "server.py", "run.py", "bot.py", "start.py"} {
		if fileExists(filepath.Join(dir, c)) {
			entry = c
			break
		}
	}
	if entry == "" {
		return "", false, fmt.Errorf("the repository is a Python project without a recognizable " +
			"entrypoint (looked for main.py, app.py, server.py, run.py, bot.py, start.py) — add an app.json with a cmd field")
	}
	spec := ""
	if data, err := os.ReadFile(filepath.Join(dir, "requirements.txt")); err == nil {
		spec += strings.ToLower(string(data)) + "\n"
	}
	if data, err := os.ReadFile(filepath.Join(dir, "pyproject.toml")); err == nil {
		spec += strings.ToLower(string(data)) + "\n"
	}
	web := false
	for _, crate := range pythonWebCrates {
		if strings.Contains(spec, crate) {
			web = true
			break
		}
	}
	install := ""
	switch {
	case fileExists(filepath.Join(dir, "requirements.txt")):
		install = `-r requirements.txt`
	case fileExists(filepath.Join(dir, "pyproject.toml")) || fileExists(filepath.Join(dir, "setup.py")):
		install = `-e .`
	}
	var cmdLine string
	if runtime.GOOS == "windows" {
		py := `%APPDECK_DATA%\.venv\Scripts`
		setup := `python -m venv "%APPDECK_DATA%\.venv"`
		if install != "" {
			setup += ` && "` + py + `\pip" install -q ` + install
		}
		cmdLine = `if not exist "` + py + `\python.exe" (` + setup + `) & "` + py + `\python" ` + entry
	} else {
		setup := `python3 -m venv "$V"`
		if install != "" {
			setup += ` && "$V/bin/pip" install -q ` + install
		}
		cmdLine = `V="$APPDECK_DATA/.venv"; if [ ! -x "$V/bin/python" ]; then ` + setup + `; fi; exec "$V/bin/python" ` + entry
	}
	return cmdLine, web, nil
}

// repoIconCandidates are the conventional places an icon hides in a
// repository, in priority order. Only well-named files are used — a random
// root screenshot would make a worse tile than a generated letter.
var repoIconCandidates = []string{
	"icon.png", "logo.png", "logo.svg", "icon.svg", "favicon.png",
	"assets/icon.png", "assets/logo.png", "assets/logo.svg",
	"docs/icon.png", "docs/logo.png", "images/logo.png", "images/icon.png",
	".github/logo.png", ".github/logo.svg", "resources/icon.png", "res/icon.png",
}

// ensureAppIcon gives an imported folder an icon: an existing icon.png/svg
// wins, else a suitably-named image from the repo is copied, else a lettered
// icon.svg is generated. Without any of these the dashboard falls back to an
// emoji.
func ensureAppIcon(dir, name string) error {
	for _, ic := range appIconNames {
		if fileExists(filepath.Join(dir, ic.name)) {
			return nil
		}
	}
	if src := findRepoIcon(dir); src != "" {
		if data, err := os.ReadFile(src); err == nil {
			dest := "icon.png"
			if strings.EqualFold(filepath.Ext(src), ".svg") {
				dest = "icon.svg"
			}
			return os.WriteFile(filepath.Join(dir, dest), data, 0o644)
		}
	}
	return generateIconSVG(dir, name)
}

// findRepoIcon returns the path of the first conventional icon file in the
// repository ("" when there is none).
func findRepoIcon(dir string) string {
	for _, rel := range repoIconCandidates {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// generateIconSVG writes a simple lettered tile — SVG is text, so no image
// toolchain is needed, and the deck serves icon.svg natively. The color is
// derived from the app id, so different apps get different tiles.
func generateIconSVG(dir, name string) error {
	id := slugify(name)
	letter := "?"
	for _, r := range strings.ToUpper(strings.TrimSpace(name)) {
		if r != ' ' {
			letter = string(r)
			break
		}
	}
	letter = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(letter)
	palette := []string{
		"#4A6FA5", "#5B8C5A", "#B0684C", "#7B6CA8",
		"#3E8C84", "#A85B63", "#8C7A3E", "#4C7A9B",
	}
	sum := sha256.Sum256([]byte(id))
	color := palette[hex.EncodeToString(sum[:])[0]%byte(len(palette))]
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="256" height="256" viewBox="0 0 256 256">
<rect width="256" height="256" rx="48" fill="%s"/>
<text x="128" y="132" font-family="system-ui,-apple-system,'Segoe UI',sans-serif" font-size="136" font-weight="600" fill="#ffffff" text-anchor="middle" dominant-baseline="central">%s</text>
</svg>
`, color, letter)
	return os.WriteFile(filepath.Join(dir, "icon.svg"), []byte(svg), 0o644)
}
