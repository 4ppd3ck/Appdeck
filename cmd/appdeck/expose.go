// Exposing an installed app to the host OS: a small artifact outside the
// deck container that makes the OS's own search see it — a symlink in
// /Applications on macOS (Spotlight, Finder, Launchpad), a Start-menu
// shortcut on Windows, a freedesktop .desktop entry on Linux. The deck keeps
// owning the real folder: updates swap the container contents in place and
// the artifact keeps pointing at the same path, uninstall removes it.
//
// On macOS the exposed thing is a .app bundle — a vendor bundle from the
// import when there is one, otherwise the bare binary is wrapped first (the
// exact generator the elevation flow builds, so expose and elevation share
// one bundle and never override each other: both converge on <id>.app, and
// the stamp check rebuilds only what drifted). While a link is active, Start
// on the card launches through that bundle — Launch Services for a vendor
// bundle (the standalone experience), the wrapper's shim for a generated one
// (this start's environment sourced, output in the app's log). The per-OS
// backends live in expose_<goos>.go; this file holds what every OS shares —
// the API endpoint, icon plumbing, and the pure content builders.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
)

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
	var note string
	var err error
	if body.Off {
		note, err = s.exposeOff(a)
	} else {
		note, err = s.exposeOn(a)
	}
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "exposed": !body.Off, "note": note})
}

// exposeRemoveForApp drops the app's OS artifact, best effort — the
// uninstall path calls it while the folder still exists so cleanup sees the
// layout it made the artifact from.
func (s *Server) exposeRemoveForApp(a *App) {
	_, _ = s.exposeOff(a)
}

// exposeBinaryPath is the app's program as an absolute path — the artifact's
// target on Windows and Linux. a.Binary's shape varies by discovery stage
// (folder-relative in a fresh manifest load, Root-relative once discovery
// rewrote it), so both bases are tried.
func (s *Server) exposeBinaryPath(a *App) (string, error) {
	if a.Dir == "" || a.Binary == "" {
		return "", fmt.Errorf("the app has no binary — nothing to expose")
	}
	for _, base := range []string{s.cfg.Root, a.Dir} {
		if base == "" {
			continue
		}
		bin := filepath.Join(base, filepath.FromSlash(a.Binary))
		if fileExists(bin) {
			return bin, nil
		}
	}
	return "", fmt.Errorf("the app's binary is missing on disk")
}

// exposeIconPath renders the app's icon.png as a .ico in its data dir —
// Windows shortcuts cannot use PNG artwork, and a wrapped Go binary's
// embedded icon is the generic one. "" when there is no PNG or the
// conversion failed (the shortcut then falls back to the exe's own icon).
func (s *Server) exposeIconPath(a *App) string {
	png := filepath.Join(a.Dir, "icon.png")
	if !fileExists(png) {
		return ""
	}
	dir := s.appDataDir(a)
	if dir == "" {
		return ""
	}
	ico := filepath.Join(dir, "icon.ico")
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", icoScript(png, ico))
	hideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil || !fileExists(ico) {
		_ = out
		return ""
	}
	return ico
}

// psQuote escapes one path for a single-quoted PowerShell string.
func psQuote(p string) string {
	return strings.ReplaceAll(p, "'", "''")
}

// icoScript renders the PowerShell that converts a PNG to a .ico (the one
// System.Drawing dance every Windows box can do). Pure — testable anywhere.
func icoScript(pngPath, icoPath string) string {
	return fmt.Sprintf(`
Add-Type -AssemblyName System.Drawing
$bmp = [System.Drawing.Image]::FromFile('%s')
$icon = [System.Drawing.Icon]::FromHandle($bmp.GetHicon())
$stream = [System.IO.File]::Create('%s')
$icon.Save($stream)
$stream.Close()
$bmp.Dispose()`, psQuote(pngPath), psQuote(icoPath))
}

// sameLink compares a stored link target with the expected one.
func sameLink(cur, target string) bool {
	return filepath.Clean(cur) == filepath.Clean(target)
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
// same tool the exe-icon extraction uses). An empty iconPath falls back to
// the program's own embedded icon.
func shortcutScript(lnkPath, targetPath, workDir, iconPath string) string {
	icon := iconPath
	if icon == "" {
		icon = targetPath
	}
	return fmt.Sprintf(`
$ws = New-Object -ComObject WScript.Shell
$s = $ws.CreateShortcut('%s')
$s.TargetPath = '%s'
$s.WorkingDirectory = '%s'
$s.IconLocation = '%s,0'
$s.Save()`, psQuote(lnkPath), psQuote(targetPath), psQuote(workDir), psQuote(icon))
}
