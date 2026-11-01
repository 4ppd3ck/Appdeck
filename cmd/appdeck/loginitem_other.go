//go:build !darwin && !windows

package main

import (
	"os"
	"path/filepath"
)

func setLoginItem(exePath string, enabled bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".config", "autostart", "appdeck.desktop")
	if !enabled {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	exe, err := filepath.Abs(exePath)
	if err != nil {
		return err
	}
	entry := `[Desktop Entry]
Type=Application
Name=AppDeck
Comment=Local app launcher
Exec="` + exe + `"
Terminal=false
X-GNOME-Autostart-enabled=true
`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(entry), 0o644)
}
