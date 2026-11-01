//go:build windows

package main

import (
	"errors"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

const loginItemValueName = "AppDeck"

func setLoginItem(exePath string, enabled bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !enabled {
		err := k.DeleteValue(loginItemValueName)
		if err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := filepath.Abs(exePath)
	if err != nil {
		return err
	}
	// Quoted: the Run value is a command line, and paths with spaces
	// ("C:\Program Files\…") must not split into program + arguments.
	return k.SetStringValue(loginItemValueName, `"`+exe+`"`)
}
