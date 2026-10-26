//go:build !windows && !darwin

// Linux privilege mapping: a privileged app runs under pkexec when polkit
// is installed — the desktop's graphical authorization prompt, the GUI
// equivalent of sudo — or `sudo -n` where passwordless sudo is configured.
// A GUI launcher has no terminal to type a sudo password into, so sudo is
// probed with -n and refused with directions when it would prompt.
//
// AppDeck itself never elevates: only the app's process runs as root, and
// every other app keeps running as the launching user.
//
// The trade the design accepts: an unprivileged process cannot signal a
// root one (EPERM on kill), so Stop on a privileged app re-raises the
// polkit/sudo prompt and a root kill finishes the job — the same trust
// boundary the macOS TCC path draws around privacy grants.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func resetTccGrants(id string) {}
func openPrivacyPane(_ string) {}

// The .app wrapper layout exists only on macOS (see tcc_darwin.go): no
// bundle path to fall back to, nothing to restore on un-elevate.
func bundleBinaryPath(appDir, appID string) string { return "" }
func restoreBundleBinary(a *App)                   {}

// guideTccGrant is the macOS grant walkthrough (see tcc_darwin.go); the
// Linux elevation needs no manual settings step — pkexec asks at every start.
func (s *Server) guideTccGrant(a *App) bool { return false }

// launchPrivileged is the Linux privileged start: compose the argv (env
// through env(1), cwd through a tiny shell), pick pkexec when polkit is
// installed and passwordless sudo otherwise, then exec it with the app's
// regular log as its output.
func (s *Server) launchPrivileged(a *App, env []string, args []string) (int, chan struct{}, error) {
	bin, err := s.resolveAppBinary(a)
	if err != nil {
		return 0, nil, err
	}
	argv := elevatedUnixArgs(env, bin, args, a.Dir)
	if tool, err := exec.LookPath("pkexec"); err == nil {
		return s.startElevated(tool, argv, a)
	}
	tool, err := exec.LookPath("sudo")
	if err != nil {
		return 0, nil, fmt.Errorf("neither pkexec nor sudo is available — install polkit to authorize elevated apps")
	}
	// Probe before prompting: sudo -n fails fast when a password would be
	// needed, which turns "process exited during startup" into a message
	// that says what to do about it. LC_ALL=C pins the message English for
	// the check — sudo localizes it otherwise.
	probe := exec.Command(tool, "-n", "true")
	probeEnv := []string{"LC_ALL=C", "LANG=C"}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "LC_") || strings.HasPrefix(kv, "LANG=") {
			continue
		}
		probeEnv = append(probeEnv, kv)
	}
	probe.Env = probeEnv
	if out, err := probe.CombinedOutput(); err != nil {
		if strings.Contains(strings.ToLower(string(out)), "password") {
			return 0, nil, fmt.Errorf("sudo would ask for a password and the launcher cannot prompt — install polkit (pkexec) or configure NOPASSWD for this app")
		}
		return 0, nil, fmt.Errorf("sudo -n refused: %s", lastN(strings.TrimSpace(string(out)), 200))
	}
	return s.startElevated(tool, append([]string{"-n"}, argv...), a)
}

// stopPrivilegedElevated stops a running root app: the launcher's own
// token gets EPERM, so the same elevation tool that started it (pkexec,
// else sudo -n with NOPASSWD) performs one graceful-then-forced kill of
// the app and its children — a single prompt, both phases inside the
// script. The PID rides as $1; nothing is interpolated.
func stopPrivilegedElevated(pid int) error {
	const script = `pkill -TERM -P "$1"; kill -TERM "$1" 2>/dev/null; sleep 3; pkill -KILL -P "$1" 2>/dev/null; kill -KILL "$1" 2>/dev/null; true`
	p := strconv.Itoa(pid)
	var cmd *exec.Cmd
	if _, err := exec.LookPath("pkexec"); err == nil {
		cmd = exec.Command("pkexec", "/bin/sh", "-c", script, "sh", p)
	} else if _, err := exec.LookPath("sudo"); err == nil {
		cmd = exec.Command("sudo", "-n", "/bin/sh", "-c", script, "sh", p)
	} else {
		return fmt.Errorf("neither pkexec nor sudo is available — kill PID %d as root yourself", pid)
	}
	cmd.SysProcAttr = processSysProcAttr()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, lastN(strings.TrimSpace(string(out)), 200))
	}
	return nil
}

// startElevated spawns the elevation tool and reports the wrapper PID.
// pkexec and sudo exec their target rather than lingering, so the PID
// stands for the app in status checks and the exited channel closes
// exactly when the app does.
func (s *Server) startElevated(tool string, argv []string, a *App) (int, chan struct{}, error) {
	logFile, err := os.OpenFile(s.logPath(a), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return 0, nil, err
	}
	cmd := exec.Command(tool, argv...)
	cmd.Dir = a.Dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = processSysProcAttr()
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return 0, nil, err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		logFile.Close()
		close(exited)
	}()
	return cmd.Process.Pid, exited, nil
}
