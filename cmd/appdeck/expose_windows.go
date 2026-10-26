//go:build windows

// The Windows expose backend: a Start-menu shortcut (.lnk) under
// Programs\AppDeck, written through WScript.Shell — Win-key search indexes
// the Start menu, so the app is findable and launchable like any installed
// program while the deck keeps owning the folder.
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
func exposeArtifactPath(a *App) string {
	name := slugify(a.Name)
	if name == "" {
		name = a.ID
	}
	return filepath.Join(exposeDir(), name+".lnk")
}

func exposeLink(a *App, target string) (string, error) {
	dir := exposeDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	lnk := exposeArtifactPath(a)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		shortcutScript(lnk, target, a.Dir))
	hideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("start-menu shortcut failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return filepath.Base(lnk) + " pinned to the Start menu — Windows search finds it there", nil
}

func exposeUnlink(a *App, target string) (string, error) {
	lnk := exposeArtifactPath(a)
	if err := os.Remove(lnk); err != nil {
		if os.IsNotExist(err) {
			return filepath.Base(lnk) + " was not pinned", nil // already gone — idempotent
		}
		return "", err
	}
	return filepath.Base(lnk) + " removed from the Start menu", nil
}

func exposeLinked(a *App, target string) bool {
	_, err := os.Stat(exposeArtifactPath(a))
	return err == nil
}
