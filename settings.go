// Per-app settings store: a tiny JSON-document-per-app persistence the
// launcher hosts so third-party apps don't each invent their own storage
// layout. Documents live under <data>/settings/<app-id>.json, next to the
// per-app data directories — one place to back up, one place to wipe.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// appSettingsDoc is the GET /api/settings body.
type appSettingsDoc struct {
	ID       string          `json:"id"`
	Settings json.RawMessage `json:"settings"`
}

func (s *Server) settingsDir() string {
	return filepath.Join(s.cfg.DataDir, "settings")
}

func (s *Server) settingsPath(id string) string {
	return filepath.Join(s.settingsDir(), id+".json")
}

// readAppSettings returns the app's stored document (nil when none was ever
// saved) and whether it exists.
func (s *Server) readAppSettings(id string) (json.RawMessage, bool, error) {
	data, err := os.ReadFile(s.settingsPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// Validate before handing out: the store only ever holds whole JSON docs.
	if !json.Valid(data) {
		return nil, false, fmt.Errorf("settings for %q are corrupt", id)
	}
	return json.RawMessage(data), true, nil
}

// writeAppSettings stores one settings document atomically (tmp + rename),
// replacing whatever was there.
func (s *Server) writeAppSettings(id string, doc json.RawMessage) error {
	if !json.Valid(doc) {
		return fmt.Errorf("settings must be a valid JSON document")
	}
	if err := os.MkdirAll(s.settingsDir(), 0o755); err != nil {
		return err
	}
	path := s.settingsPath(id)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, doc, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
