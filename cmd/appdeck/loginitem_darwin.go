//go:build darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const loginItemLabel = "com.appdeck.launcher"

func loginItemPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", loginItemLabel+".plist"), nil
}

// xmlEscape keeps a path with & < > " ' from breaking the plist.
var xmlEscaper = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;",
)

func setLoginItem(exePath string, enabled bool) error {
	path, err := loginItemPath()
	if err != nil {
		return err
	}
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
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
</dict>
</plist>
`, loginItemLabel, xmlEscaper.Replace(exe))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(plist), 0o644)
}
