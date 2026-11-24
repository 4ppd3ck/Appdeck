// sandbox.go — OS-level process sandboxes for cards that ask for one
// (app.json "sandbox": true). The profile is derived from the card's
// declared permissions (permissions.go): a sandboxed card can always read
// the OS, its own folder and its APPDECK_DATA, and can bind its loopback
// port — everything else stays closed unless a declared permission opens
// it.
//
// Per OS, honestly:
//   - darwin: sandbox-exec with a generated seatbelt profile — deny-default
//     filesystem ($HOME stays dark: no ~/.ssh, no browser cookies), loopback
//     bind allowed, outbound network only with the "network"/"lan"
//     permissions.
//   - linux: bubblewrap (bwrap) when installed — bind-mounts the OS
//     read-only, the app folder and data dir writable, --die-with-parent.
//     Network: unshared for sandboxed desktop apps without network
//     permissions; a sandboxed web app must keep its loopback listener
//     reachable, so its namespace shares the host net (documented limit).
//   - windows: no in-process filesystem sandbox exists; the honest
//     equivalent is a Job Object (2 GB commit cap, kill-on-close) attached
//     right after start — runaway RAM dies, and the app cannot outlive the
//     launcher. Nothing pretends to be filesystem isolation.
//   - freebsd: process sandboxing is not implemented; sandboxed starts fail
//     explicitly rather than running without the requested isolation.
//
// A sandboxed start that cannot be honored fails with a named error instead
// of silently running unsandboxed.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// sandboxEnabled reports whether the app asks for a sandbox. Hosted static
// pages run inside the launcher process — there is nothing to sandbox.
func sandboxEnabled(a *App) bool {
	return a.Sandbox && a.ServedBy != "launcher"
}

// permSet is the app's declared permissions as a set.
func permSet(a *App) map[string]bool {
	out := map[string]bool{}
	for _, p := range a.Permissions {
		out[p] = true
	}
	return out
}

// sandboxProfileDir is where generated seatbelt profiles live: inside the
// launcher data dir, per app, never in the app folder (an update must not
// carry it).
func (s *Server) sandboxProfileDir() string {
	return filepath.Join(s.cfg.DataDir, "sandbox")
}

// effectiveAppDataDir mirrors childEnv's APPDECK_DATA resolution — the
// sandbox fence and the app's writable home must agree exactly.
func (s *Server) effectiveAppDataDir(a *App) string {
	if home := s.appDataHome(a); home != "" {
		return home
	}
	if d := s.appDataDir(a); d != "" {
		return d
	}
	return filepath.Join(s.cfg.DataDir, "appdata", a.ID)
}

// wrapSandbox rewrites cmd to run under the OS sandbox. Called by both
// start paths after appCommand built the plain command and before Start —
// darwin/linux rewrite argv; windows wraps nothing here (its Job Object
// attaches after start, see sandboxPostStart).
func (s *Server) wrapSandbox(a *App, cmd *exec.Cmd) error {
	if !sandboxEnabled(a) {
		return nil
	}
	return wrapSandboxPlatform(a, cmd, a.Dir, s.effectiveAppDataDir(a), s.sandboxProfileDir())
}

// sandboxPostStart runs platform attach steps that need a live PID (the
// Windows Job Object). Best effort: an attach failure is noted in the app's
// log but does not fail the start — the app is already running.
func (s *Server) sandboxPostStart(a *App, pid int) {
	if !sandboxEnabled(a) || pid <= 0 {
		return
	}
	attachSandboxPlatform(a, pid, s.logPath(a))
}

// darwinSandboxProfile renders the seatbelt profile for one app. The words
// matter: profile text is load-bearing, so tests assert its shape.
func darwinSandboxProfile(a *App, appDir, dataDir string) string {
	perms := permSet(a)
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(deny default)\n")
	// Process basics: exec the app's own folder, the data dir (Python venvs
	// live there) and the standard tool locations the launcher's PATH uses.
	b.WriteString("(allow process-fork)\n")
	fmt.Fprintf(&b, "(allow process-exec (subpath %q) (subpath %q) (subpath \"/usr/bin\") (subpath \"/bin\") (subpath \"/sbin\") (subpath \"/usr/local/bin\") (subpath \"/opt/homebrew\") (subpath \"/usr/libexec\") (subpath \"/usr/sbin\"))\n", appDir, dataDir)
	b.WriteString("(allow sysctl-read)\n")
	b.WriteString("(allow mach-lookup)\n")
	b.WriteString("(allow ipc-posix-shm)\n")
	b.WriteString("(allow ipc-posix-semaphore)\n")
	b.WriteString("(allow system-socket)\n")
	// Read the OS and the app's own world. $HOME is never granted, so keys,
	// browser cookies and documents stay dark.
	fmt.Fprintf(&b, "(allow file-read* (subpath \"/usr\") (subpath \"/bin\") (subpath \"/sbin\") (subpath \"/usr/lib\") (subpath \"/System\") (subpath \"/private/var/db/timezone\") (subpath \"/etc\") (subpath %q) (subpath %q))\n", appDir, dataDir)
	// Write only the app's own world and the system temp areas.
	fmt.Fprintf(&b, "(allow file-write* (subpath %q) (subpath %q) (subpath \"/private/tmp\") (subpath \"/tmp\") (subpath \"/private/var/folders\"))\n", appDir, dataDir)
	// Loopback always: the app serves its port and talks to the launcher.
	b.WriteString("(allow network-bind (local ip \"127.0.0.1:*\"))\n")
	b.WriteString("(allow network-bind (local ip \"[::1]:*\"))\n")
	b.WriteString("(allow network-inbound (local ip \"127.0.0.1:*\"))\n")
	b.WriteString("(allow network-outbound (remote ip \"127.0.0.1:*\"))\n")
	b.WriteString("(allow network-outbound (remote ip \"[::1]:*\"))\n")
	// Declared permissions widen the fence.
	if perms["lan"] || perms["network"] {
		b.WriteString("(allow network-bind)\n")
		b.WriteString("(allow network-inbound)\n")
	}
	if perms["network"] || perms["lan"] {
		b.WriteString("(allow network-outbound)\n")
	}
	return b.String()
}

// linuxBwrapArgs assembles the bubblewrap command tail for one app: the
// OS read-only, the app folder read-only, the data dir writable, /dev and
// /proc inside, and the network unshared only when the app is portless and
// declared nothing that needs it.
func linuxBwrapArgs(a *App, appDir, dataDir string) []string {
	perms := permSet(a)
	args := []string{
		"--die-with-parent", "--new-session",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--tmpfs", "/var/tmp", "--tmpfs", "/run",
	}
	// Read-only OS: whatever exists of the common trees.
	for _, d := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc", "/opt"} {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			args = append(args, "--ro-bind", d, d)
		}
	}
	// The app's world: folder read-only (the launcher owns updates), data
	// dir writable (its APPDECK_DATA).
	args = append(args, "--ro-bind", appDir, appDir)
	args = append(args, "--bind", dataDir, dataDir)
	// Network: a portless desktop app with no network permission runs fully
	// offline; a web app must keep its loopback listener reachable, so its
	// namespace shares the host net — the documented linux limit.
	if a.Desktop && !perms["network"] && !perms["lan"] {
		args = append(args, "--unshare-net")
	}
	// The child command follows.
	return args
}
