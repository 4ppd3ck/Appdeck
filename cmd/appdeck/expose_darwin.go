//go:build darwin

// The macOS expose backend: a symlink in /Applications pointed at the app's
// bundle inside the deck container. LaunchServices follows the link, so
// Spotlight, Finder, Launchpad and the "Open with" menu see a normal
// application while the deck keeps owning the real folder. A link is only
// ever created or removed when it is really ours — an existing real
// /Applications/<name>.app (the user's own install) is never touched.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// exposeDir is where links land. LAUNCHER_EXPOSE_DIR overrides it (tests,
// unusual setups).
func exposeDir() string {
	if v := strings.TrimSpace(os.Getenv("LAUNCHER_EXPOSE_DIR")); v != "" {
		return v
	}
	return "/Applications"
}

// sameLink compares a stored link target with the expected one.
func sameLink(cur, target string) bool {
	return filepath.Clean(cur) == filepath.Clean(target)
}

func exposeLink(a *App, target string) (string, error) {
	dir := exposeDir()
	link := filepath.Join(dir, filepath.Base(target))
	if info, err := os.Lstat(link); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("%s already exists and is not a link — remove it first if this link should replace it", link)
		}
		if cur, err := os.Readlink(link); err == nil && sameLink(cur, target) {
			return filepath.Base(target) + " is already linked into " + dir, nil
		}
		return "", fmt.Errorf("%s is a link to somewhere else — remove it first if this link should replace it", link)
	}
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return "", err
	}
	if err := os.Symlink(target, link); err != nil {
		return "", err
	}
	return filepath.Base(target) + " linked into " + dir + " — Spotlight and Finder see it now", nil
}

func exposeUnlink(a *App, target string) (string, error) {
	link := filepath.Join(exposeDir(), filepath.Base(target))
	info, err := os.Lstat(link)
	if err != nil {
		return filepath.Base(target) + " was not linked", nil // already gone — idempotent
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("%s is not a link — refusing to delete it", link)
	}
	if cur, err := os.Readlink(link); err != nil || !sameLink(cur, target) {
		return "", fmt.Errorf("%s points somewhere else — refusing to remove it", link)
	}
	if err := os.Remove(link); err != nil {
		return "", err
	}
	return filepath.Base(target) + " removed from " + exposeDir(), nil
}

func exposeLinked(a *App, target string) bool {
	cur, err := os.Readlink(filepath.Join(exposeDir(), filepath.Base(target)))
	return err == nil && sameLink(cur, target)
}
