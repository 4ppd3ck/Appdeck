// Built-in theme packs. The theme/ folder ships inside the executable and is
// seeded into <data>/themes/ on boot — each pack only when its theme.css is
// missing, so operator edits of a seeded pack survive updates (delete the
// folder to get the built-in back on next boot).
package main

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:theme
var builtinThemeFS embed.FS

// seedBuiltinThemes writes the embedded packs into the data directory.
func seedBuiltinThemes(dataDir string) {
	entries, err := fs.ReadDir(builtinThemeFS, "theme")
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dest := filepath.Join(dataDir, "themes", e.Name())
		if fileExists(filepath.Join(dest, "theme.css")) {
			continue // operator-owned or already seeded — hands off
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			continue
		}
		err := fs.WalkDir(builtinThemeFS, "theme/"+e.Name(), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel("theme/"+e.Name(), filepath.ToSlash(path))
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}
			if isJunkEntry(filepath.ToSlash(rel)) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			target := filepath.Join(dest, rel)
			if d.IsDir() {
				return os.MkdirAll(target, 0o755)
			}
			data, err := builtinThemeFS.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, 0o644)
		})
		if err != nil {
			os.RemoveAll(dest) // a half-seeded pack is worse than none
		}
	}
}
