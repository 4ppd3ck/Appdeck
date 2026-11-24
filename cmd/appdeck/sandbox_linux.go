//go:build linux

// sandbox_linux.go — the bubblewrap side of sandbox.go: rewrite the command
// to run under bwrap with the OS read-only and the app's data writable.
// A missing bwrap is a named failure, never a silent unsandboxed start.
package main

import (
	"fmt"
	"os/exec"
)

func wrapSandboxPlatform(a *App, cmd *exec.Cmd, appDir, dataDir, profileDir string) error {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return fmt.Errorf(`sandbox requested but bubblewrap (bwrap) is not installed — install bwrap, or set "sandbox": false in the app manifest`)
	}
	// bwrap <profile args> <the original command>; Args[0] stays the argv[0]
	// convention (program name first).
	cmd.Args = append([]string{"bwrap"}, append(linuxBwrapArgs(a, appDir, dataDir), cmd.Args...)...)
	cmd.Path = bwrap
	return nil
}

// attachSandboxPlatform is the post-start hook; bwrap wraps at spawn, so
// there is nothing to attach afterwards.
func attachSandboxPlatform(a *App, pid int, logPath string) {}
