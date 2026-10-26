//go:build linux

// The Linux expose backend: a freedesktop .desktop entry in
// ~/.local/share/applications (per-user — no root needed), where GNOME
// Activities, KDE kickoff and every other desktop's search find it. The
// entry carries the app's icon.png when it has one.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// exposeDir is where entries land. LAUNCHER_EXPOSE_DIR overrides it (tests,
// unusual setups).
func exposeDir() string {
	if v := strings.TrimSpace(os.Getenv("LAUNCHER_EXPOSE_DIR")); v != "" {
		return v
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "share", "applications")
}

// exposeArtifactPath is the entry's file — the id is the stable, filesystem-
// safe name (the entry's Name= carries the display name).
func exposeArtifactPath(a *App) string {
	return filepath.Join(exposeDir(), "appdeck-"+a.ID+".desktop")
}

func exposeLink(a *App, target string) (string, error) {
	dir := exposeDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := exposeArtifactPath(a)
	icon := ""
	if fileExists(filepath.Join(a.Dir, "icon.png")) {
		icon = filepath.Join(a.Dir, "icon.png")
	}
	name := a.Name
	if strings.TrimSpace(name) == "" {
		name = a.ID
	}
	body := desktopEntryBody(name, a.Desc, desktopExecQuote(target), a.Dir, icon)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	// Best effort database refresh — most desktops notice the file anyway.
	if upd, err := exec.LookPath("update-desktop-database"); err == nil {
		cmd := exec.Command(upd, dir)
		hideWindow(cmd)
		_ = cmd.Run()
	}
	return name + " added to the applications menu — the desktop search finds it there", nil
}

func exposeUnlink(a *App, target string) (string, error) {
	path := exposeArtifactPath(a)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return a.ID + " was not in the applications menu", nil // already gone — idempotent
		}
		return "", err
	}
	return a.ID + " removed from the applications menu", nil
}

func exposeLinked(a *App, target string) bool {
	_, err := os.Stat(exposeArtifactPath(a))
	return err == nil
}
