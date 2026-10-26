//go:build windows

// Windows privilege mapping: a privileged app starts through one deliberate
// UAC elevation. An outer hidden PowerShell asks Windows for the admin
// token (Start-Process -Verb RunAs — the same mechanism the firewall fix
// uses) and runs a per-start script (adminLaunchScript in priv_common.go)
// that sets the child environment, starts the real binary with its output
// going to the app's log files, publishes its PID, and stays alive until
// the app exits. AppDeck itself never elevates: the launcher process keeps
// its standard-user token, and every other app keeps running as before.
//
// The trade the design accepts: an unelevated process cannot terminate an
// elevated one (Windows integrity levels), so Stop on a privileged app
// re-raises the UAC prompt and an elevated taskkill finishes the job —
// the same trust boundary the macOS TCC path draws around privacy grants.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The UAC prompt stays on screen until answered, and the PID only appears
// once it is — poll this long before giving up on the approval.
const uacApprovalWindow = 30 * time.Second

func resetTccGrants(id string) {}
func openPrivacyPane(_ string) {}

// The .app wrapper layout exists only on macOS (see tcc_darwin.go): no
// bundle path to fall back to, nothing to restore on un-elevate.
func bundleBinaryPath(appDir, appID string) string { return "" }
func restoreBundleBinary(a *App)                   {}

// guideTccGrant is the macOS grant walkthrough (see tcc_darwin.go); the
// Windows elevation needs no manual settings step — UAC asks at every start.
func (s *Server) guideTccGrant(a *App) bool { return false }

// launchPrivileged is the Windows privileged start: write this start's
// elevation script, raise the UAC prompt through the outer hidden
// PowerShell, then find the app's real PID (the script publishes it once
// the elevation is approved and Start-Process has spawned the binary).
func (s *Server) launchPrivileged(a *App, env []string, args []string) (int, chan struct{}, error) {
	bin, err := s.resolveAppBinary(a)
	if err != nil {
		return 0, nil, err
	}
	if why := windowsExeProblem(bin); why != "" {
		return 0, nil, fmt.Errorf("%s cannot run on Windows: %s", bin, why)
	}
	appData := s.appDataDir(a)
	if appData == "" {
		return 0, nil, fmt.Errorf("no writable app data directory for the elevation script")
	}
	scriptPath := filepath.Join(appData, "admin-launch.ps1")
	pidPath := filepath.Join(appData, "admin-launch.pid")
	script := adminLaunchScript(bin, a.Dir, args, env, s.logPath(a), pidPath, !a.Desktop)
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return 0, nil, fmt.Errorf("writing the elevation script: %v", err)
	}
	os.Remove(pidPath) // a previous start's PID must never be mistaken for this one

	// Outer hidden PowerShell: -Verb RunAs is what triggers the UAC prompt;
	// -Wait (an outer Start-Process parameter, NOT an argument of the inner
	// one — as an ArgumentList item it reached the inner powershell as a
	// stray script argument and the outer exited right after the UAC
	// approval, so the launcher read "outer ended, no PID" and reported a
	// failed start while the elevated script was still starting the app)
	// keeps the outer process alive until the elevated script (and the app)
	// exits, so the returned channel closes exactly when the app does.
	// The script path arrives pre-quoted because -ArgumentList items are
	// joined verbatim on Windows PowerShell 5.1.
	inner := "Start-Process powershell -Verb RunAs -Wait -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-File'," +
		psSingleQuoted(windowsArgToken(scriptPath))
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", inner)
	hideWindow(cmd)
	exited := make(chan struct{})
	var mu sync.Mutex
	var outerFail string
	go func() {
		out, err := cmd.CombinedOutput()
		if err != nil {
			mu.Lock()
			outerFail = lastN(strings.TrimSpace(string(out)), 300)
			if outerFail == "" {
				outerFail = err.Error()
			}
			mu.Unlock()
		}
		close(exited)
	}()

	deadline := time.Now().Add(uacApprovalWindow)
	for time.Now().Before(deadline) {
		if p := readAdminPid(pidPath); p > 1 {
			return p, exited, nil
		}
		select {
		case <-exited:
			mu.Lock()
			fail := outerFail
			mu.Unlock()
			if uacDeclined(fail) {
				return 0, exited, fmt.Errorf("elevation declined — the app was not started")
			}
			if fail != "" {
				return 0, exited, fmt.Errorf("could not elevate %s: %s", a.Name, fail)
			}
			// The outer process ended without a PID and without a captured
			// error: the script failed after UAC and its catch block wrote
			// the reason to the app log.
			return 0, exited, fmt.Errorf("could not start %s as administrator — check the app log", a.Name)
		default:
		}
		time.Sleep(250 * time.Millisecond)
	}
	return 0, exited, fmt.Errorf("no UAC approval within 30 s — approve the prompt, then start the app again")
}

// stopPrivilegedElevated stops a running admin app: the launcher's
// standard-user token cannot kill it, so one more UAC prompt runs an
// elevated taskkill on the app's PID tree. When the app dies, the
// per-start wrapper's WaitForExit returns and the outer -Wait process
// exits with it, so the launcher's exited channel closes too.
func stopPrivilegedElevated(pid int) error {
	inner := "Start-Process -Verb RunAs -FilePath taskkill -ArgumentList '/PID','" +
		strconv.Itoa(pid) + "','/T','/F' -Wait"
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", inner)
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if uacDeclined(string(out)) {
			return fmt.Errorf("elevation declined — the app keeps running")
		}
		return fmt.Errorf("%v: %s", err, lastN(strings.TrimSpace(string(out)), 200))
	}
	return nil
}

// readAdminPid parses the pidfile the elevated script writes once the app
// is running (empty when it does not exist yet or holds garbage).
func readAdminPid(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}
