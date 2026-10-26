// Exposing an installed app to the host OS: a small artifact outside the
// deck container that makes the OS's own search see it — a symlink in
// /Applications on macOS (Spotlight, Finder, Launchpad), a Start-menu
// shortcut on Windows, a freedesktop .desktop entry on Linux. The deck keeps
// owning the real folder: updates swap the container contents in place and
// the artifact keeps pointing at the same path, uninstall removes it. The
// per-OS backends live in expose_<goos>.go; this file holds what every OS
// shares — target resolution, the API endpoint, and the pure content
// builders the backends render.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// exposeTarget resolves what the OS surface should point at: the .app bundle
// the manifest's binary lives in on macOS (bundle imports, mpv-style wraps —
// derived from the binary path so the exposed bundle is exactly the one the
// card launches), the program binary itself on Windows and Linux. An app
// without a real executable — launcher-hosted static pages, shell-cmd apps —
// has nothing to expose.
func exposeTarget(a *App) (string, error) {
	if a.Dir == "" {
		return "", fmt.Errorf("the app has no folder on disk")
	}
	if runtime.GOOS == "darwin" {
		if i := strings.Index(filepath.ToSlash(a.Binary), ".app/"); i > 0 {
			bundle := filepath.Join(a.Dir, filepath.FromSlash(a.Binary[:i+len(".app")]))
			if info, err := os.Stat(bundle); err == nil && info.IsDir() {
				return bundle, nil
			}
		}
		entries, err := os.ReadDir(a.Dir)
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
				return filepath.Join(a.Dir, e.Name()), nil
			}
		}
		return "", fmt.Errorf("no .app bundle in the app folder — nothing to put in /Applications")
	}
	if a.Binary == "" {
		return "", fmt.Errorf("the app has no binary — nothing to expose")
	}
	bin := filepath.Join(a.Dir, filepath.FromSlash(a.Binary))
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("the app's binary is missing on disk")
	}
	return bin, nil
}

// exposeInfo reports the card button's data: whether this app carries
// something the host OS can surface, and whether it currently is.
func exposeInfo(a *App) (exposeable, exposed bool) {
	target, err := exposeTarget(a)
	if err != nil {
		return false, false
	}
	return true, exposeLinked(a, target)
}

// exposeRemoveForApp drops the app's OS artifact, best effort — the
// uninstall path calls it while the folder still exists so the target
// resolves and only a link that is really ours gets removed.
func (s *Server) exposeRemoveForApp(a *App) {
	if target, err := exposeTarget(a); err == nil {
		_, _ = exposeUnlink(a, target)
	}
}

// serveExpose handles POST /api/expose {id, off?}: create or remove the
// app's OS artifact (idempotent in both directions).
func (s *Server) serveExpose(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID  string `json:"id"`
		Off bool   `json:"off"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || body.ID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send {id, off?}"})
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
	target, err := exposeTarget(a)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var note string
	if body.Off {
		note, err = exposeUnlink(a, target)
	} else {
		note, err = exposeLink(a, target)
	}
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "exposed": !body.Off, "note": note})
}

// desktopEntryBody renders a freedesktop .desktop entry's body (the Linux
// backend's content, kept here so it is testable on every OS).
func desktopEntryBody(name, comment, exec, dir, icon string) string {
	var b strings.Builder
	b.WriteString("[Desktop Entry]\nType=Application\n")
	b.WriteString("Name=" + name + "\n")
	if comment != "" {
		b.WriteString("Comment=" + strings.ReplaceAll(comment, "\n", " ") + "\n")
	}
	b.WriteString("Exec=" + exec + "\n")
	if dir != "" {
		b.WriteString("Path=" + dir + "\n")
	}
	if icon != "" {
		b.WriteString("Icon=" + icon + "\n")
	}
	b.WriteString("Terminal=false\nCategories=Utility;\n")
	return b.String()
}

// desktopExecQuote quotes one Exec= argument per the freedesktop entry spec:
// double quotes, with backslash escaping (single quotes are literal
// characters there, not quoting).
func desktopExecQuote(p string) string {
	for _, c := range []string{"\\", `"`, "$", "`"} {
		p = strings.ReplaceAll(p, c, "\\"+c)
	}
	return `"` + p + `"`
}

// shortcutScript renders the PowerShell that writes a Start-menu .lnk —
// WScript.Shell is the one COM object every Windows box has for this (the
// same tool the exe-icon extraction uses).
func shortcutScript(lnkPath, targetPath, workDir string) string {
	ps := func(p string) string { return strings.ReplaceAll(p, "'", "''") }
	return fmt.Sprintf(`
$ws = New-Object -ComObject WScript.Shell
$s = $ws.CreateShortcut('%s')
$s.TargetPath = '%s'
$s.WorkingDirectory = '%s'
$s.IconLocation = '%s,0'
$s.Save()`, ps(lnkPath), ps(targetPath), ps(workDir), ps(targetPath))
}
