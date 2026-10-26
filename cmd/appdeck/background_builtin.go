// Built-in background packs. The background/ folder ships inside the
// executable and is seeded into <data>/backgrounds/ on boot — each pack
// only when its background.css is missing, so operator edits of a seeded
// pack survive updates (delete the folder to get the built-in back).
package main

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:background
var builtinBackgroundFS embed.FS

// seedBuiltinBackgrounds writes the embedded packs into the data directory.
func seedBuiltinBackgrounds(dataDir string) {
	entries, err := fs.ReadDir(builtinBackgroundFS, "background")
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dest := filepath.Join(dataDir, "backgrounds", e.Name())
		if fileExists(filepath.Join(dest, "background.css")) {
			continue // operator-owned or already seeded — hands off
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			continue
		}
		err = fs.WalkDir(builtinBackgroundFS, "background/"+e.Name(), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel("background/"+e.Name(), filepath.ToSlash(p))
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
			data, err := builtinBackgroundFS.ReadFile(p)
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
