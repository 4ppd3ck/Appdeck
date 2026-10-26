//go:build !windows && !darwin

// The freedesktop expose backend — every non-Apple, non-Microsoft deck
// (Linux and the BSDs, whose desktops follow the same XDG spec). The
// artifact is a .desktop entry in ~/.local/share/applications (per-user —
// no root needed), where GNOME Activities, KDE kickoff, XFCE and every
// other desktop's search find it. The entry is named appdeck-<id>.desktop:
// ids are unique and the prefix namespaces our entries inside the shared
// applications dir, so expose and uninstall never touch an entry that is
// not ours. The entry carries the app's icon.png when it has one —
// freedesktop entries take PNG artwork directly, no conversion needed.
package main

import (
	"fmt"
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

// exposeArtifactPath is the entry's file — the id is the stable,
// filesystem-safe name (the entry's Name= carries the display name).
func (s *Server) exposeArtifactPath(a *App) string {
	return filepath.Join(exposeDir(), "appdeck-"+a.ID+".desktop")
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
	name := a.Name
	if strings.TrimSpace(name) == "" {
		name = a.ID
	}
	icon := filepath.Join(a.Dir, "icon.png")
	if !fileExists(icon) {
		icon = ""
	}
	entry := s.exposeArtifactPath(a)
	body := desktopEntryBody(name, a.Desc, desktopExecQuote(target), a.Dir, icon)
	if err := os.WriteFile(entry, []byte(body), 0o644); err != nil {
		return "", err
	}
	// Best-effort database refresh — most desktops notice the file anyway.
	if upd, err := exec.LookPath("update-desktop-database"); err == nil {
		cmd := exec.Command(upd, dir)
		hideWindow(cmd)
		_ = cmd.Run()
	}
	return name + " added to the applications menu — the desktop search finds it there", nil
}

func (s *Server) exposeOff(a *App) (string, error) {
	entry := s.exposeArtifactPath(a)
	if err := os.Remove(entry); err != nil {
		if os.IsNotExist(err) {
			return filepath.Base(entry) + " was not in the applications menu", nil // already gone — idempotent
		}
		return "", err
	}
	return filepath.Base(entry) + " removed from the applications menu", nil
}

func (s *Server) exposeInfo(a *App) (exposeable, exposed bool) {
	if _, err := s.exposeBinaryPath(a); err != nil {
		return false, false
	}
	return true, fileExists(s.exposeArtifactPath(a))
}

// exposedLaunchBundle is the launch gate; only macOS launches through a
// bundle — freedesktop entries launch the program directly.
func (s *Server) exposedLaunchBundle(a *App) string { return "" }

// launchExposedBundle never runs off macOS — the start path's gate answers
// "" there (exposedLaunchBundle), so this is unreachable and says so.
func (s *Server) launchExposedBundle(a *App, env []string, bundle string, args []string) (int, chan struct{}, error) {
	return 0, nil, fmt.Errorf("bundle launches are macOS-only")
}
