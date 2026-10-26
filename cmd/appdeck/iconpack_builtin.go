// Built-in icon packs. The iconpack/ folder ships inside the executable and
// is seeded into <data>/iconpacks/ on boot — each pack only when none of
// its icon files exist yet, so operator edits of a seeded pack survive
// updates (delete the folder to get the built-in back on next boot).
package main

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:iconpack
var builtinIconPackFS embed.FS

// seedBuiltinIconPacks writes the embedded packs into the data directory.
func seedBuiltinIconPacks(dataDir string) {
	entries, err := fs.ReadDir(builtinIconPackFS, "iconpack")
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dest := filepath.Join(dataDir, "iconpacks", e.Name())
		if dirExists(dest) {
			continue // operator-owned or already seeded — hands off
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			continue
		}
		_ = fs.WalkDir(builtinIconPackFS, "iconpack/"+e.Name(), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel("iconpack/"+e.Name(), filepath.ToSlash(p))
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}
			if d.IsDir() {
				return os.MkdirAll(filepath.Join(dest, rel), 0o755)
			}
			data, err := builtinIconPackFS.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dest, rel), data, 0o644)
		})
	}
}
