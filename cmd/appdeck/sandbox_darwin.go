//go:build darwin

// sandbox_darwin.go — the seatbelt side of sandbox.go: generate the profile
// for this app, then rewrite the command to run under sandbox-exec. The
// profile file lives in the launcher data dir (never the app folder), mode
// 0600 — it is launcher-owned plumbing, not app content.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func wrapSandboxPlatform(a *App, cmd *exec.Cmd, appDir, dataDir, profileDir string) error {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		return fmt.Errorf(`sandbox requested but sandbox-exec is unavailable: %w — set "sandbox": false, or repair macOS`, err)
	}
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return fmt.Errorf("sandbox profile dir: %w", err)
	}
	profile := darwinSandboxProfile(a, appDir, dataDir)
	path := filepath.Join(profileDir, a.ID+".sb")
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		return fmt.Errorf("sandbox profile: %w", err)
	}
	// sandbox-exec -f <profile> -- <the original command>
	wrapped := append([]string{"sandbox-exec", "-f", path}, cmd.Args...)
	cmd.Path = "/usr/bin/sandbox-exec"
	cmd.Args = wrapped
	return nil
}

// attachSandboxPlatform is the post-start hook; darwin's seatbelt wraps at
// spawn, so there is nothing to attach afterwards.
func attachSandboxPlatform(a *App, pid int, logPath string) {}
