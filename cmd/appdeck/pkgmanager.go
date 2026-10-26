// The package-manager fallback for source-only imports: when a repository
// AppDeck cannot build (C/C++ autotools and friends) fails to import, the
// dashboard probes the OS package manager for a matching package and —
// after one explicit click — installs it and registers a ⌨ CLI app card.
// Nothing is ever installed silently, and nothing from the imported
// repository is ever executed: the software comes from the OS's own
// channel (Homebrew / winget / apt / dnf), which is the whole point.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type pkgInfo struct {
	Manager   string `json:"manager"`
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	Desc      string `json:"desc,omitempty"`
	Installed bool   `json:"installed"`
}

// pkgTool resolves this machine's package manager ("" when none).
func pkgTool() (tool, manager string) {
	switch runtime.GOOS {
	case "darwin":
		for _, c := range []string{"/opt/homebrew/bin/brew", "/usr/local/bin/brew"} {
			if fileExists(c) {
				return c, "Homebrew"
			}
		}
		if p, err := exec.LookPath("brew"); err == nil {
			return p, "Homebrew"
		}
	case "windows":
		if p, err := exec.LookPath("winget"); err == nil {
			return p, "winget"
		}
	default:
		if p, err := exec.LookPath("apt-cache"); err == nil {
			return p, "apt"
		}
		if p, err := exec.LookPath("dnf"); err == nil {
			return p, "dnf"
		}
		// Linuxbrew — Homebrew's Linux install — keeps the same flow as on
		// macOS for machines with no apt/dnf (or a brew-only setup).
		for _, c := range []string{"/home/linuxbrew/.linuxbrew/bin/brew", "/opt/homebrew/bin/brew"} {
			if fileExists(c) {
				return c, "Homebrew"
			}
		}
		if p, err := exec.LookPath("brew"); err == nil {
			return p, "Homebrew"
		}
	}
	return "", ""
}

func pkgRun(tool string, args ...string) (string, error) {
	cmd := exec.Command(tool, args...)
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// parseBrewInfo reads `brew info --json=v2` output.
func parseBrewInfo(data []byte) (pkgInfo, bool) {
	var v struct {
		Formulae []struct {
			Name     string `json:"name"`
			Desc     string `json:"desc"`
			Versions struct {
				Stable string `json:"stable"`
			} `json:"versions"`
		} `json:"formulae"`
	}
	if json.Unmarshal(data, &v) != nil || len(v.Formulae) == 0 {
		return pkgInfo{}, false
	}
	f := v.Formulae[0]
	return pkgInfo{Manager: "Homebrew", Name: f.Name, Version: f.Versions.Stable, Desc: f.Desc}, true
}

// parseAptPolicy reads `apt-cache policy <name>` output.
func parseAptPolicy(out string) (version string, installed bool, found bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Installed:"):
			installed = !strings.Contains(line, "(none)")
		case strings.HasPrefix(line, "Candidate:"):
			version = strings.TrimSpace(strings.TrimPrefix(line, "Candidate:"))
			found = version != "" && version != "(none)"
		}
	}
	return version, installed, found
}

// pkgManagerProbe looks a name up in the OS package manager (read-only).
func pkgManagerProbe(name string) (pkgInfo, bool) {
	tool, manager := pkgTool()
	if tool == "" || name == "" {
		return pkgInfo{}, false
	}
	switch manager {
	case "Homebrew":
		out, err := pkgRun(tool, "info", "--json=v2", name)
		if err != nil {
			return pkgInfo{}, false
		}
		info, ok := parseBrewInfo([]byte(out))
		if !ok {
			return pkgInfo{}, false
		}
		if _, err := pkgRun(tool, "list", "--formula", name); err == nil {
			info.Installed = true
		}
		return info, true
	case "apt":
		out, _ := pkgRun(tool, "policy", name)
		version, installed, found := parseAptPolicy(out)
		if !found {
			return pkgInfo{}, false
		}
		return pkgInfo{Manager: "apt", Name: name, Version: version, Installed: installed}, true
	case "dnf":
		out, err := pkgRun(tool, "-q", "info", name)
		if err != nil {
			return pkgInfo{}, false
		}
		version := ""
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "Version") {
				version = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
				break
			}
		}
		return pkgInfo{Manager: "dnf", Name: name, Version: version}, true
	case "winget":
		out, _ := pkgRun(tool, "search", "--exact", "--id", name, "--accept-source-agreements")
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(strings.TrimPrefix(line, strings.Repeat(" ", len(line)-len(strings.TrimLeft(line, " ")))))
			if len(f) >= 2 && strings.EqualFold(f[0], name) && !strings.HasPrefix(f[0], "Name") {
				return pkgInfo{Manager: "winget", Name: name, Version: f[1]}, true
			}
		}
		return pkgInfo{}, false
	}
	return pkgInfo{}, false
}

// pkgBinaries lists the commands a package puts on PATH (best effort).
func pkgBinaries(manager, name string) []string {
	var out string
	switch manager {
	case "Homebrew":
		if tool, _ := pkgTool(); tool != "" {
			out, _ = pkgRun(tool, "list", name)
		}
	case "apt":
		out, _ = pkgRun("dpkg", "-L", name)
	case "dnf":
		out, _ = pkgRun("rpm", "-ql", name)
	}
	var bins []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "/bin/") && !strings.HasSuffix(line, "/") {
			bins = append(bins, filepath.Base(line))
		}
	}
	return bins
}

// pickPrimaryBinary chooses the card's smoke-test command: the package's
// own name when it provides it, else a command the name contains (ripgrep
// → rg, imagemagick → magick), else the first command, else the name.
func pickPrimaryBinary(bins []string, name string) string {
	for _, b := range bins {
		if b == name {
			return b
		}
	}
	for _, b := range bins {
		if len(b) >= 3 && strings.Contains(name, b) {
			return b
		}
	}
	if len(bins) > 0 {
		return bins[0]
	}
	return name
}

// pkgInstallArgv composes the install command line. On Linux a system
// package install needs root: the same elevation mapping privileged starts
// use — pkexec's desktop prompt when polkit is there, else passwordless
// sudo; with neither, the bare command runs and fails honestly.
func pkgInstallArgv(tool, manager, name string) []string {
	var argv []string
	switch manager {
	case "Homebrew":
		argv = []string{tool, "install", name}
	case "apt":
		argv = []string{"apt-get", "install", "-y", name}
	case "dnf":
		argv = []string{tool, "install", "-y", name}
	case "winget":
		argv = []string{tool, "install", "--exact", "--id", name,
			"--silent", "--accept-source-agreements", "--accept-package-agreements"}
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		if _, err := exec.LookPath("pkexec"); err == nil {
			return append([]string{"pkexec"}, argv...)
		}
		if _, err := exec.LookPath("sudo"); err == nil {
			return append([]string{"sudo", "-n"}, argv...)
		}
	}
	return argv
}

// wingetCommandName derives a smoke-test command from a winget package id:
// ids are publisher-prefixed ("ImageMagick.ImageMagick") while the command
// the package installs matches the last segment.
func wingetCommandName(id string) string {
	if i := strings.LastIndex(id, "."); i >= 0 && i+1 < len(id) {
		return id[i+1:]
	}
	return id
}

// servePkgProbe answers GET /api/pkg/probe?name=… — the read-only lookup
// behind the import dialog's package offer. Loopback only.
func (s *Server) servePkgProbe(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "package lookup is only allowed from this computer"})
		return
	}
	info, found := pkgManagerProbe(r.URL.Query().Get("name"))
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "found": found, "package": info})
}

// installPackageApp is the import flow behind the consented offer
// (?install=pkg&pkgname=…): install (or reuse) the package through the OS
// package manager with the job log streaming every line, then generate the
// ⌨ CLI app card that points at it.
func (s *Server) installPackageApp(w http.ResponseWriter, qs map[string]string, job *jobLog) {
	tool, manager := pkgTool()
	name := strings.TrimSpace(qs["pkgname"])
	if tool == "" || name == "" {
		errText := "no package manager found on this machine — install the tool manually and drop its binary here"
		job.finish(errText)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": errText})
		return
	}
	job.logf("looking up %s in %s…", name, manager)
	info, found := pkgManagerProbe(name)
	if !found {
		errText := fmt.Sprintf("%s has no package named %q", manager, name)
		job.finish(errText)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": errText})
		return
	}
	if !info.Installed {
		job.logf("installing %s with %s — system-wide, from the OS's own channel (the version may differ from the imported tag)", name, manager)
		argv := pkgInstallArgv(tool, manager, name)
		cmd := exec.Command(argv[0], argv[1:]...)
		hideWindow(cmd)
		stdout, _ := cmd.StdoutPipe()
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			errText := fmt.Sprintf("starting %s: %v", manager, err)
			job.finish(errText)
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errText})
			return
		}
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			job.logf("  %s", sc.Text())
		}
		if err := cmd.Wait(); err != nil {
			errText := fmt.Sprintf("%s install failed: %v (details above — nothing was added to the deck)", manager, err)
			job.finish(errText)
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": errText})
			return
		}
	} else {
		job.logf("%s is already installed via %s — adding the card", name, manager)
	}

	// The card: a ⌨ CLI app whose folder holds just the manifest; the tool
	// itself lives wherever the package manager put it.
	display := strings.TrimSpace(qs["appname"])
	if display == "" {
		display = name
	}
	bins := pkgBinaries(manager, name)
	primary := pickPrimaryBinary(bins, name)
	if manager == "winget" && len(bins) == 0 {
		// winget lists no per-package commands: the id's last segment is
		// the closest guess at the installed command's name.
		primary = wingetCommandName(name)
	}
	id := slugify(display)
	container, err := s.appsContainerDir()
	if err != nil {
		job.finish(err.Error())
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	dir := filepath.Join(container, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		job.finish(err.Error())
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	binList := primary
	if len(bins) > 1 {
		binList = strings.Join(bins[:min(6, len(bins))], "/")
	}
	if err := writePkgAppManifest(dir, id, display, info, primary, binList); err != nil {
		_ = os.RemoveAll(dir)
		job.finish(err.Error())
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := markEmbeddedOverride(dir, id); err != nil {
		_ = os.RemoveAll(dir)
		job.finish(err.Error())
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.reloadApps(); err != nil {
		job.finish(err.Error())
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	job.finish("")
	job.logf("installed and discovered — it appears on the dashboard now")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": id, "name": display,
		"note": fmt.Sprintf("installed with %s and added as a ⌨ CLI app", manager)})
}

// writePkgAppManifest generates the CLI card for a package-manager install.
func writePkgAppManifest(dir, id, display string, info pkgInfo, primary, binList string) error {
	a := App{
		ID:       id,
		Name:     display,
		Icon:     "⌨",
		Category: "Tools",
		URLPath:  "/",
		Desc: fmt.Sprintf("Installed with %s — %s on your PATH. Run its commands from the ⌨ terminal action.",
			info.Manager, binList),
		Cmd:       primary + " --version",
		Desktop:   true,
		Cli:       true,
		Autostart: false,
	}
	if info.Version != "" {
		a.Version = info.Version
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, manifestFileName), data, 0o644)
}
