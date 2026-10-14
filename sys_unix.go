//go:build !windows

// POSIX process helpers for the launcher (mirrors the non-Windows branches of
// server.py: lsof/ps probes, process-group kills with SIGTERM → SIGKILL, and
// start_new_session=True via Setpgid).

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processSysProcAttr mirrors Python start_new_session=True: the app gets its
// own process group so a clean group kill is possible.
func processSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// hideWindow is a Windows-only concern; POSIX has no console-creation flash.
func hideWindow(_ *exec.Cmd) {}

// showErrorMessage surfaces fatal startup errors where the user can actually
// see them. On macOS the launcher is usually double-clicked (no console), so
// a stderr line vanishes unseen and a failed start looks like "nothing
// happened" — show a real dialog instead. The message rides in argv, so no
// quoting can break the AppleScript. Linux keeps stderr-only (no guaranteed
// dialog helper).
func showErrorMessage(title, msg string) {
	if runtime.GOOS != "darwin" {
		return
	}
	script := `on run argv
display dialog (item 1 of argv) with title (item 2 of argv) buttons {"OK"} default button 1 with icon caution
end run`
	cmd := exec.Command("osascript", "-e", script, msg, title)
	_ = cmd.Start() // non-blocking: the caller exits right after; osascript outlives it
}

// shellCommand mirrors Python _shell_command on POSIX.
func shellCommand(cmd string) []string {
	return []string{"/bin/sh", "-c", cmd}
}

// normalizeAppCommand is Windows-only in the Python version.
func normalizeAppCommand(cmd string) string { return cmd }

// shellSafePath is Windows-only; POSIX callers quote the path in the shell
// command string instead.
func shellSafePath(p string) string { return p }

// pidAlive mirrors Python pid_alive on POSIX: kill(pid, 0) succeeds or
// fails with ESRCH (dead) / EPERM (alive but not ours). pid 0 would signal
// the caller's own process group — which always "succeeds" — so a stateless
// desktop app entry (PID 0) must read as dead, not as every-app-running.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// listenerPIDs mirrors Python listener_pids on POSIX: lsof plus a
// double-check that each PID is ours and really listening.
func listenerPIDs(port int) []int {
	out, err := runCommand(5*time.Second, "lsof", "-nP", fmt.Sprintf("-tiTCP:%d", port), "-sTCP:LISTEN")
	if err != nil {
		return nil
	}
	var pids []int
	for _, field := range strings.Fields(out) {
		pid, convErr := strconv.Atoi(field)
		if convErr != nil {
			continue
		}
		if !ownsAndListens(pid, port) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// ownsAndListens is Python _owns_and_listens: a mis-parsed lsof filter must
// never lead to killing an unrelated process, so verify ownership and listen
// state before acting.
func ownsAndListens(pid, port int) bool {
	uid := os.Getuid()
	out, err := runCommand(5*time.Second, "ps", "-o", "uid=", "-p", strconv.Itoa(pid))
	if err != nil {
		return false
	}
	if uid >= 0 && strings.TrimSpace(out) != strconv.Itoa(uid) {
		return false
	}
	check, err := runCommand(5*time.Second, "lsof", "-nP", "-a", "-p", strconv.Itoa(pid),
		fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN")
	if err != nil {
		return false
	}
	return strings.Contains(check, "LISTEN")
}

// procCmdline mirrors Python proc_cmdline on POSIX: basename of argv[0] plus
// the remaining arguments, truncated to ~58 characters.
func procCmdline(pid int) string {
	out, err := runCommand(5*time.Second, "ps", "-o", "command=", "-p", strconv.Itoa(pid))
	if err != nil {
		return ""
	}
	label := strings.TrimSpace(out)
	if label == "" {
		return ""
	}
	parts := strings.Fields(label)
	// show "Python server.py 8080", not the whole framework path
	label = strings.Join(append([]string{filepath.Base(parts[0])}, parts[1:]...), " ")
	if runes := []rune(label); len(runes) > 58 {
		head := string(runes[:58])
		if idx := strings.LastIndex(head, " "); idx >= 0 {
			head = head[:idx]
		}
		label = head + " …"
	}
	return label
}

// killTree is Python _kill_group on POSIX: SIGTERM the whole process group.
func killTree(pid int) {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return
	}
	_ = syscall.Kill(pgid, syscall.SIGTERM)
}

// killTreeForce is the escalation: SIGKILL the whole process group.
func killTreeForce(pid int) {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)
}

// killProc SIGTERMs a single listener PID; the error mirrors the Python
// except (ProcessLookupError, PermissionError) → not recorded as killed.
func killProc(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

// killProcForce is the listener escalation: SIGKILL.
func killProcForce(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }
