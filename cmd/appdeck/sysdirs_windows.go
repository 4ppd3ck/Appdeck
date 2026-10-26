//go:build windows

package main

import (
	"os"
	"path/filepath"
)

// systemBinDirs lists the Windows system directories every process may
// rely on: powershell, netstat, taskkill, msiexec and friends live here.
// A launcher started from a context with a starved PATH (scheduler job,
// custom shortcut, stripped shell) would otherwise pass that starvation to
// every app process — breaking apps that shell out to PowerShell — and to
// its own elevated helpers, which exec powershell by name. Appended
// additions-only, so nothing the operator configured is ever shadowed.
func systemBinDirs() []string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	sys := filepath.Join(root, "System32")
	return []string{
		sys,
		filepath.Join(sys, "Wbem"),
		filepath.Join(sys, "WindowsPowerShell", "v1.0"),
		root,
	}
}
