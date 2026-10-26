//go:build windows

// The Windows expose backend: a Start-menu shortcut (.lnk) under
// Programs\AppDeck, written through WScript.Shell — Win-key search indexes
// the Start menu, so the app is findable and launchable like any installed
// program while the deck keeps owning the folder. The shortcut's icon is the
// app's own icon.png rendered to a .ico in its data dir (a .lnk cannot use
// PNG, and a wrapped binary's embedded icon is the generic one); without a
// PNG it falls back to the program's embedded icon.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// exposeDir is where shortcuts land. LAUNCHER_EXPOSE_DIR overrides it
// (tests, unusual setups).
func exposeDir() string {
	if v := strings.TrimSpace(os.Getenv("LAUNCHER_EXPOSE_DIR")); v != "" {
		return v
	}
	return filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "AppDeck")
}

// exposeArtifactPath is the shortcut's file — the app's slug name inside the
// deck's own Start-menu folder (search shows the file name; the slug is what
// ids and folders use anyway).
func (s *Server) exposeArtifactPath(a *App) string {
	name := slugify(a.Name)
	if name == "" {
		name = a.ID
	}
	return filepath.Join(exposeDir(), name+".lnk")
}

func (s *Server) exposeOn(a *App) (string, error) {
	target, err := s.exposeBinaryPath(a)
	if err != nil {
		return "", err
	}
	dir := exposeDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	lnk := s.exposeArtifactPath(a)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		shortcutScript(lnk, target, a.Dir, s.exposeIconPath(a)))
	hideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("start-menu shortcut failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return filepath.Base(lnk) + " pinned to the Start menu — Windows search finds it there", nil
}

func (s *Server) exposeOff(a *App) (string, error) {
	lnk := s.exposeArtifactPath(a)
	if err := os.Remove(lnk); err != nil {
		if os.IsNotExist(err) {
			return filepath.Base(lnk) + " was not pinned", nil // already gone — idempotent
		}
		return "", err
	}
	return filepath.Base(lnk) + " removed from the Start menu", nil
}

func (s *Server) exposeInfo(a *App) (exposeable, exposed bool) {
	if _, err := s.exposeBinaryPath(a); err != nil {
		return false, false
	}
	return true, fileExists(s.exposeArtifactPath(a))
}

// exposedLaunchBundle is the launch gate; only macOS launches through a
// bundle — Windows shortcuts launch the program directly.
func (s *Server) exposedLaunchBundle(a *App) string { return "" }

// launchExposedBundle never runs off macOS — the start path's gate answers
// "" there (exposedLaunchBundle), so this is unreachable and says so.
func (s *Server) launchExposedBundle(a *App, env []string, bundle string, args []string) (int, chan struct{}, error) {
	return 0, nil, fmt.Errorf("bundle launches are macOS-only")
}
