//go:build !darwin

// Non-macOS stubs for the per-app privilege launch path. The privilege FLAG
// is portable (privileges.go); enforcement is darwin-only today — privileged
// apps elsewhere launch through the ordinary exec path, and elevation there
// is the app author's job (cmd/args → gsudo/sudo) until the runas/pkexec
// mappings land.
package main

import "fmt"

func resetTccGrants(id string) {}
func openFullDiskAccessPane()  {}

func (s *Server) launchTccApp(a *App, env []string, args []string) (int, chan struct{}, error) {
	return 0, nil, fmt.Errorf("privileged launch is macOS-only — this app runs unchanged without it")
}
