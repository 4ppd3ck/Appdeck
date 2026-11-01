// Git import: POST /api/import?url=<repository url> clones a git repository
// (shallow, one branch) and installs it like a folder import. Repositories
// are code, not app packages: when the clone has no app.json one is
// generated from the project layout (Cargo workspace, Go module, Python or
// Node project, compose stack, or a root HTML page), and when there is no
// icon an image found in the repo is used
// or a lettered icon.svg is generated. Nothing is executed at import time —
// the clone only writes files.
//
// Monorepos: GitHub's web-UI folder URL —
// https://github.com/<owner>/<repo>/tree/<branch>/<folder> — imports only
// that folder as the app. The whole repo is still fetched (shallow), but
// only the folder is validated and installed.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
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

// gitToolPath locates the git binary through the shared lookup (a
// user-registered path from the 🧰 tab wins; then PATH; then the well-known
// locations — double-clicked .app launches get a minimal PATH, the same
// problem goToolPath works around).
func gitToolPath() (string, error) {
	if p := findToolPath("git", "git"); p != "" {
		return p, nil
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
func (s *Server) importGit(w http.ResponseWriter, src gitSource, qs map[string]string, job *jobLog) {
	if importCanceled(w, job) {
		return
	}
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
			if handled, why := s.importReleaseAssets(w, rel, src.owner+"/"+src.repo, appname, qs, job); handled {
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
	job.logf("cloning %s…", src.name)
	job.logf("$ git clone --depth 1 --single-branch %s", src.cloneURL)
	if err := gitClone(git, src, tmp, job); err != nil {
		job.finish(err.Error())
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if importCanceled(w, job) {
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
	s.importFolder(w, appDir, job)
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

// gitClone runs a shallow single-branch clone into dest. The job's context
// is the parent (Cancel button, no-progress watchdog) with the clone's own
// hard timeout on top; --progress streams git's own counters (counting,
// receiving) into the import log, which both shows movement for big repos
// and keeps the watchdog's clock alive.
func gitClone(git string, src gitSource, dest string, job *jobLog) error {
	args := []string{"clone", "--depth", "1", "--single-branch", "--progress"}
	if src.ref != "" {
		args = append(args, "--branch", src.ref)
	}
	args = append(args, src.cloneURL, dest)
	baseCtx := context.Background()
	if job != nil {
		baseCtx = job.Context()
	}
	ctx, cancel := context.WithTimeout(baseCtx, gitCloneTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, args...)
	// A canceled clone kills git itself, but its helper (git-remote-https)
	// can keep the output pipe open and stall Run() past the kill. WaitDelay
	// makes Run return once the process is dead even if the pipe lingers.
	cmd.WaitDelay = 5 * time.Second
	hideWindow(cmd)
	var out bytes.Buffer
	if job != nil {
		cmd.Stdout = &out
		cmd.Stderr = io.MultiWriter(&out, &cloneProgress{job: job})
	} else {
		cmd.Stdout = &out
		cmd.Stderr = &out
	}
	if err := cmd.Run(); err != nil {
		if job != nil && job.Canceled() {
			return errImportCanceled
		}
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("git clone timed out after %s", gitCloneTimeout)
		}
		message := strings.TrimSpace(out.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("git clone failed: %s", lastLines(message, 8))
	}
	return nil
}

// cloneProgress turns git's --progress stderr stream into import-log lines:
// git rewrites one line per tick ("Receiving objects:  47% (123/261)"), so
// only the 10% milestones of each counter are logged — enough movement in
// the dock (and for the no-progress watchdog) without flooding it. Anything
// that is not a progress line (warnings, errors) logs once, verbatim.
type cloneProgress struct {
	job     *jobLog
	lastPct map[string]int
}

func (c *cloneProgress) Write(p []byte) (int, error) {
	n := len(p)
	// git rewrites progress lines with \r; treat both as line breaks.
	for _, line := range strings.Split(strings.ReplaceAll(string(p), "\r", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		counter, pct, ok := strings.Cut(line, ":")
		if !ok || !strings.Contains(line, "%") {
			c.job.logf("git: %s", line)
			continue
		}
		pctVal := progressPercent(pct)
		if pctVal < 0 {
			continue // an intermediate rewrite of the same counter
		}
		if c.lastPct == nil {
			c.lastPct = map[string]int{}
		}
		if last, seen := c.lastPct[counter]; !seen || pctVal/10 != last/10 || pctVal == 100 {
			c.lastPct[counter] = pctVal
			c.job.logf("git: %s", line)
		}
	}
	return n, nil
}

// progressPercent pulls the integer percentage out of a git progress tail
// ("  47% (123/261), 900 KiB" → 47; -1 when there is none).
func progressPercent(rest string) int {
	j := strings.Index(rest, "%")
	if j <= 0 {
		return -1
	}
	pct := 0
	for _, r := range rest[:j] {
		if r < '0' || r > '9' {
			return -1
		}
		pct = pct*10 + int(r-'0')
	}
	return pct
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
// Python project installs a venv on first start, a Node project runs
// npm install + the package entrypoint, a root HTML page is hosted
// in-process, a compose-only repository runs through the docker CLI.
// Anything else is refused with what the author needs to add.
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
	case fileExists(filepath.Join(dir, "package.json")):
		cmdLine, web, err := generateNodeRun(dir)
		if err != nil {
			return err
		}
		a.Cmd = cmdLine
		a.Icon = "⬢"
		a.Requires = []string{"node"}
		if web {
			a.Port = 0
		} else {
			a.Desktop = true
			// An MCP stdio server / CLI toolkit (the npm registry's
			// server.json is its manifest) has no service mode and no
			// window: it belongs with the ⌨ CLI tools, not among windowed
			// desktop apps.
			if fileExists(filepath.Join(dir, "server.json")) {
				a.Cli = true
				a.Icon = "⌨"
			}
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
		// An agent-skill repository (a root SKILL.md — the Claude Code,
		// Codex, OpenCode, Antigravity and MiniMax Code convention) is
		// instructions plus example pages, not a process: host it
		// in-process and generate a front page that links the pages and
		// docs the repository ships.
		if skillPath := skillManifestPath(dir); skillPath != "" {
			skillName, skillDesc := skillFrontmatter(skillPath)
			if skillName != "" {
				a.Name = skillName
			}
			if skillDesc != "" {
				a.Desc = truncateRunes(skillDesc, 180)
			}
			a.Icon = "🧩"
			a.ServedBy = "launcher"
			a.URLPath = "/"
			if err := generateSkillIndex(dir, skillName, skillDesc); err != nil {
				return err
			}
			break
		}
		// A compose-only repository (no other recognized layout) runs
		// through the docker CLI: the tracked process is `compose up`
		// itself, so the card stops when the stack stops. Config comes from
		// the project's own .env template when the repo ships one.
		if composeFile(dir) != "" {
			a.Cmd = "docker compose up"
			a.Icon = "🐳"
			a.Desktop = true
			a.Requires = []string{"docker"}
			copyComposeEnv(dir)
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
		return fmt.Errorf("the repository has no app.json and no recognizable layout " +
			"(Cargo.toml, go.mod, package.json, or a root .html page) — add an app.json describing the app")
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
		setup := `${APPDECK_PYTHON:-python3} -m venv "$V"`
		if install != "" {
			setup += ` && "$V/bin/pip" install -q ` + install
		}
		cmdLine = `V="$APPDECK_DATA/.venv"; if [ ! -x "$V/bin/python" ]; then ` + setup + `; fi; exec "$V/bin/python" ` + entry
	}
	return cmdLine, web, nil
}

// nodeWebCrates are the npm packages whose presence in package.json marks a
// project as a web app (the rest — CLIs, MCP servers, bots — become desktop
// cards).
var nodeWebCrates = []string{
	"express", "fastify", "koa", "hapi", "next", "nuxt", "hono",
	"polka", "restify", "@nestjs/core", "astro", "remix",
}

// nodeEntryFiles are the conventional entrypoints tried, in order, when
// package.json names none.
var nodeEntryFiles = []string{
	"server.js", "app.js", "index.js", "src/index.js", "src/server.js",
	"src/main.js", "dist/index.js", "dist/server.js", "lib/index.js",
}

// packageJSON is the slice of package.json the generator needs.
type packageJSON struct {
	Name    string            `json:"name"`
	Scripts map[string]string `json:"scripts"`
	Main    string            `json:"main"`
	Bin     json.RawMessage   `json:"bin"` // npm allows a string or a {"name": "file"} map
	Deps    map[string]string `json:"dependencies"`
	DevDeps map[string]string `json:"devDependencies"`
}

// generateNodeRun builds the shell command that installs dependencies on
// first start and runs a Node repository, plus whether it should be a web
// card. Dependencies install into the app folder itself (node_modules), so
// unlike Python there is no per-app environment to recreate — re-imports
// keep it. Deliberately brace-free: the manifest validator treats {…}
// tokens as placeholders, and POSIX cmd strings run under /bin/sh -c.
func generateNodeRun(dir string) (string, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", false, err
	}
	var p packageJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return "", false, fmt.Errorf("package.json is not valid JSON: %v", err)
	}
	entry := ""
	if p.Scripts["start"] != "" {
		entry = "start" // npm start runs the author's own script
	}
	if entry == "" {
		var bin string
		if err := json.Unmarshal(p.Bin, &bin); err == nil && bin != "" &&
			fileExists(filepath.Join(dir, bin)) {
			entry = bin
		}
	}
	if entry == "" {
		var bins map[string]string
		if err := json.Unmarshal(p.Bin, &bins); err == nil {
			for _, bin := range bins {
				if bin != "" && fileExists(filepath.Join(dir, bin)) {
					entry = bin
					break
				}
			}
		}
	}
	if entry == "" && p.Main != "" && fileExists(filepath.Join(dir, p.Main)) {
		entry = p.Main
	}
	if entry == "" {
		for _, c := range nodeEntryFiles {
			if fileExists(filepath.Join(dir, c)) {
				entry = c
				break
			}
		}
	}
	if entry == "" {
		return "", false, fmt.Errorf("the repository is a Node.js project without a recognizable " +
			"entrypoint (looked for scripts.start, bin and main in package.json, and server.js, " +
			"app.js, index.js in the folder) — add an app.json with a cmd field")
	}
	isWebCrate := func(name string) bool {
		for _, c := range nodeWebCrates {
			if name == c || strings.HasPrefix(name, c+"/") {
				return true
			}
		}
		return false
	}
	web := false
	for _, names := range []map[string]string{p.Deps, p.DevDeps} {
		for name := range names {
			if isWebCrate(name) {
				web = true
				break
			}
		}
		if web {
			break
		}
	}
	run := "node " + entry
	if entry == "start" {
		run = "npm start"
	}
	var cmdLine string
	if runtime.GOOS == "windows" {
		cmdLine = `if not exist node_modules npm install --omit=dev & ` + run
	} else {
		cmdLine = `if [ ! -d node_modules ]; then npm install --omit=dev; fi; exec ` + run
	}
	return cmdLine, web, nil
}

// composeManifests are the root file names the docker CLI auto-detects.
var composeManifests = []string{
	"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml",
}

// composeFile returns the compose manifest at the repository root, or "".
func composeFile(dir string) string {
	for _, name := range composeManifests {
		if fileExists(filepath.Join(dir, name)) {
			return name
		}
	}
	return ""
}

// copyComposeEnv seeds .env from .env.example when the repository ships the
// example but no real env file: compose stacks (pentagi-style) keep their
// whole configuration there and refuse to start without it, and the example
// is the project's own template.
func copyComposeEnv(dir string) {
	if fileExists(filepath.Join(dir, ".env")) {
		return
	}
	if data, err := os.ReadFile(filepath.Join(dir, ".env.example")); err == nil {
		_ = os.WriteFile(filepath.Join(dir, ".env"), data, 0o644)
	}
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

// skillPage is one relative link on a generated skill front page.
type skillPage struct{ label, href string }

// skillCard is one tile on that page: a figure and, when the repository
// ships both, its dark and light builds.
type skillCard struct {
	title string
	pages []skillPage
}

// skillManifestPath returns the agent-skill manifest at the repository root
// ("" when there is none). SKILL.md is the convention shared by Claude
// Code, Codex, OpenCode, Antigravity and MiniMax Code skills.
func skillManifestPath(dir string) string {
	for _, name := range []string{"SKILL.md", "skill.md"} {
		if fileExists(filepath.Join(dir, name)) {
			return filepath.Join(dir, name)
		}
	}
	return ""
}

// skillFrontmatter reads the small YAML header an agent skill carries
// between --- lines: the skill's name and description. Anything the parser
// does not understand is skipped — the repository name stays the fallback.
func skillFrontmatter(path string) (name, desc string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	if len(data) > 32*1024 {
		data = data[:32*1024]
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", ""
	}
	for _, line := range lines[1:] {
		line = strings.TrimRight(line, "\r")
		if t := strings.TrimSpace(line); t == "---" || t == "..." {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(key, " ") || strings.HasPrefix(key, "\t") {
			continue // a nested key, not ours
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "name":
			if name == "" {
				name = value
			}
		case "description":
			if desc == "" {
				desc = value
			}
		}
	}
	return name, desc
}

// truncateRunes shortens s to max runes, cutting at the last space and
// adding an ellipsis when something was dropped.
func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := string(r[:max])
	if i := strings.LastIndexAny(cut, " \t"); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}

// generateSkillIndex writes the front page a skill repository lacks: the
// name and description from SKILL.md, a tile per HTML page the repository
// ships (a skill's built examples; a "-light" build shares its dark twin's
// tile) and links to the docs. The page is self-contained inline HTML, so
// it works in every hosting mode.
func generateSkillIndex(dir, name, desc string) error {
	if name == "" {
		name = "Agent skill"
	}
	cards := skillCards(collectSkillPages(dir))
	docs := collectSkillDocs(dir)
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(fmt.Sprintf(skillIndexHead, esc(name), esc(name), esc(desc)))
	if len(cards) > 0 {
		b.WriteString("<h2>Pages</h2>" + "\n" + `<div class="grid">` + "\n")
		for _, c := range cards {
			b.WriteString(`<div class="card"><h3>` + esc(c.title) + `</h3>`)
			for _, p := range c.pages {
				b.WriteString(`<a href="` + skillHref(p.href) + `">` + esc(p.label) + `</a>`)
			}
			b.WriteString("</div>\n")
		}
		b.WriteString("</div>\n")
	}
	if len(docs) > 0 {
		b.WriteString("<h2>Docs</h2>\n" + `<ul class="docs">` + "\n")
		for _, d := range docs {
			b.WriteString(`<li><a href="` + skillHref(d.href) + `">` + esc(d.label) + `</a></li>` + "\n")
		}
		b.WriteString("</ul>\n")
	}
	b.WriteString(skillIndexFoot)
	return os.WriteFile(filepath.Join(dir, "index.html"), []byte(b.String()), 0o644)
}

// skillIndexHead is the fixed head and style of a generated skill front
// page: name in the title, name in the h1, description in the lead. Dark
// and quiet to sit beside the figures the skill builds.
const skillIndexHead = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>%s — agent skill</title>
<style>
:root{color-scheme:dark}
body{margin:0;background:#0b0b0b;color:#a3a3a3;font:15px/1.6 system-ui,-apple-system,'Segoe UI',sans-serif}
main{max-width:880px;margin:0 auto;padding:48px 24px 64px}
h1{color:#e6e6e6;font-size:26px;margin:0 0 10px}
.lead{margin:0 0 8px;max-width:66em}
h2{color:#e6e6e6;font-size:12px;letter-spacing:.14em;text-transform:uppercase;margin:36px 0 14px}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(230px,1fr));gap:12px}
.card{background:#131313;border:1px solid #242424;border-radius:10px;padding:13px 16px}
.card h3{margin:0 0 8px;color:#e6e6e6;font-size:14px;font-weight:600}
.card a{display:inline-block;margin:0 14px 0 0;color:#7ab0ff;text-decoration:none;font-size:13px}
.card a:hover{text-decoration:underline}
ul.docs{list-style:none;margin:0;padding:0;columns:2;column-gap:32px}
ul.docs li{margin:3px 0;break-inside:avoid}
ul.docs a{color:#7ab0ff;text-decoration:none;font-size:13px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
ul.docs a:hover{text-decoration:underline}
footer{margin-top:44px;color:#6e6e6e;font-size:12.5px;border-top:1px solid #1e1e1e;padding-top:16px}
code{background:#191919;border-radius:4px;padding:1px 5px}
</style></head><body><main><h1>%s</h1><p class="lead">%s</p>`

const skillIndexFoot = `<footer>This repository is an AI agent skill: <code>SKILL.md</code> is the instruction file a coding agent follows. The tiles above are the pages the repository ships — the skill's built examples.</footer></main></body></html>`

// collectSkillPages gathers every HTML page the repository ships (a
// skill's built examples), excluding the root front page itself, hidden
// folders and node_modules; the walk stops after 48 pages so a stray tree
// cannot slow the import.
func collectSkillPages(dir string) []skillPage {
	var pages []skillPage
	n := 0
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != dir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(d.Name()), ".html") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		if strings.EqualFold(rel, "index.html") {
			return nil // the front page this generator writes
		}
		pages = append(pages, skillPage{label: rel, href: rel})
		if n++; n >= 48 {
			return fs.SkipAll
		}
		return nil
	})
	sort.Slice(pages, func(i, j int) bool { return pages[i].href < pages[j].href })
	return pages
}

// skillCards turns the repository's HTML pages into front-page tiles: the
// "-light" build of a page shares its dark twin's tile, everything else
// gets its own.
func skillCards(pages []skillPage) []skillCard {
	all := map[string]bool{}
	for _, p := range pages {
		all[p.href] = true
	}
	lightOf := map[string]string{}
	for _, p := range pages {
		if strings.HasSuffix(p.href, "-light.html") {
			if dark := strings.TrimSuffix(p.href, "-light.html") + ".html"; all[dark] {
				lightOf[dark] = p.href
			}
		}
	}
	var cards []skillCard
	used := map[string]bool{}
	for _, p := range pages {
		if used[p.href] {
			continue
		}
		if strings.HasSuffix(p.href, "-light.html") {
			if dark := strings.TrimSuffix(p.href, "-light.html") + ".html"; all[dark] {
				continue // the dark twin's tile carries this build
			}
		}
		used[p.href] = true
		card := skillCard{title: skillCardTitle(p.href)}
		if light, ok := lightOf[p.href]; ok {
			card.pages = append(card.pages, skillPage{"dark", p.href}, skillPage{"light", light})
			used[light] = true
		} else {
			label := "open"
			if strings.HasSuffix(p.href, "-light.html") {
				label = "light"
			}
			card.pages = append(card.pages, skillPage{label, p.href})
		}
		cards = append(cards, card)
	}
	return cards
}

// skillCardTitle prettifies a page's file stem into a tile title
// ("dial-indicator" → "Dial Indicator").
func skillCardTitle(href string) string {
	base := href[strings.LastIndex(href, "/")+1:]
	base = strings.TrimSuffix(base, ".html")
	base = strings.TrimSuffix(base, "-light")
	if base == "" {
		base = "page"
	}
	return prettifyName(base)
}

// collectSkillDocs names the documentation a skill repository ships:
// SKILL.md and README at the root, then the .md guides inside a
// references/ or docs/ folder (capped at 20).
func collectSkillDocs(dir string) []skillPage {
	var docs []skillPage
	seen := map[string]bool{}
	add := func(rel string) {
		if !seen[rel] {
			seen[rel] = true
			docs = append(docs, skillPage{label: rel, href: rel})
		}
	}
	for _, name := range []string{"SKILL.md", "skill.md", "README.md", "readme.md"} {
		if fileExists(filepath.Join(dir, name)) {
			add(name)
		}
	}
	for _, sub := range []string{"references", "docs"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if len(docs) >= 20 {
				return docs
			}
			if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
				continue
			}
			add(sub + "/" + e.Name())
		}
	}
	return docs
}

// skillHref turns a repository-relative path into a relative URL.
func skillHref(rel string) string {
	segs := strings.Split(strings.ReplaceAll(rel, string(filepath.Separator), "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// prettifyName turns a file stem into a display title
// ("dial-indicator" → "Dial Indicator").
func prettifyName(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	if len(words) == 0 {
		return s
	}
	for i, w := range words {
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}
