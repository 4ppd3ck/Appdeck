//go:build freebsd

// FreeBSD has no process sandbox integration in AppDeck yet. Refuse sandboxed
// starts explicitly rather than running them without the requested isolation.
package main

import (
	"fmt"
	"os/exec"
)

func wrapSandboxPlatform(a *App, cmd *exec.Cmd, appDir, dataDir, profileDir string) error {
	return fmt.Errorf(`sandbox requested but process sandboxing is not supported on FreeBSD — set "sandbox": false in the app manifest`)
}

func attachSandboxPlatform(a *App, pid int, logPath string) {}
