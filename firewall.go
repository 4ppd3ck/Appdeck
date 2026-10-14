// One-click fix for the Windows Firewall "Allow access?" prompts.
//
// Every exe that listens on the LAN (0.0.0.0) triggers Windows Defender
// Firewall's Allow/Cancel dialog the first time it binds — once per binary.
// The launcher itself is one prompt, but each network-capable app binary
// would add its own. This endpoint generates a PowerShell script that adds
// inbound allow-rules for the launcher and all network apps, and runs it
// through a single UAC elevation (Start-Process -Verb RunAs). After
// approving that one prompt, no app asks again.
//
// There is no way to suppress those prompts from a non-elevated process —
// by design on Windows — so one deliberate elevation is the minimum.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type firewallRule struct {
	name string
	exe  string
}

// firewallFix builds the rule list and launches the elevation. The response
// is written directly to the dashboard.
func (s *Server) firewallFix() map[string]any {
	out := map[string]any{"ok": false}
	if runtime.GOOS != "windows" {
		out["error"] = "firewall prompts only happen on Windows"
		return out
	}

	var rules []firewallRule
	if exe, err := os.Executable(); err == nil {
		// The launcher exe also hosts the in-process HTML apps, so one rule
		// covers the dashboard and all of them.
		rules = append(rules, firewallRule{"AppDeck: launcher", exe})
	}
	for _, a := range s.apps {
		// Desktop apps don't listen on the network — no prompt, no rule.
		if a.Desktop || !a.Network || a.ServedBy == "launcher" || a.Binary == "" {
			continue
		}
		bin, err := resolveBinary(s.cfg.Root, a.Binary)
		if err != nil {
			continue // not shipped/installed; it can't prompt either
		}
		rules = append(rules, firewallRule{"AppDeck: " + a.Name, bin})
	}
	if len(rules) == 0 {
		out["error"] = "no network apps found"
		return out
	}

	scriptPath := filepath.Join(os.TempDir(), "project-launcher-firewall.ps1")
	script := firewallScript(rules)
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		out["error"] = err.Error()
		return out
	}

	// Outer PowerShell (hidden) starts an ELEVATED PowerShell running the
	// script; -Verb RunAs is what triggers the single UAC prompt.
	inner := fmt.Sprintf(
		"Start-Process powershell -Verb RunAs -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-WindowStyle','Hidden','-File','%s'",
		strings.ReplaceAll(scriptPath, "'", "''"))
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", inner)
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		out["error"] = err.Error()
		return out
	}
	go func() { _ = cmd.Wait() }() // reap the outer process; elevation runs on its own

	out["ok"] = true
	out["rules"] = len(rules)
	out["note"] = "approve the Windows admin prompt once — apps will stop asking for network permission"
	return out
}

// firewallScript renders the elevated PowerShell: skip rules that already
// exist, add the rest, then exit (window closes itself).
func firewallScript(rules []firewallRule) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'SilentlyContinue'\n")
	b.WriteString("$rules = @(\n")
	for i, r := range rules {
		sep := ","
		if i == len(rules)-1 {
			sep = ""
		}
		fmt.Fprintf(&b, "  @{ n = '%s'; p = '%s' }%s\n",
			strings.ReplaceAll(r.name, "'", "''"),
			strings.ReplaceAll(r.exe, "'", "''"), sep)
	}
	b.WriteString(")\n")
	b.WriteString(`foreach ($r in $rules) {
  if (-not (Get-NetFirewallRule -DisplayName $r.n)) {
    New-NetFirewallRule -DisplayName $r.n -Direction Inbound -Action Allow -Program $r.p | Out-Null
  }
}
`)
	return b.String()
}
