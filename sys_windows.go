//go:build windows

// Windows process helpers for the launcher (mirrors the IS_WINDOWS branches of
// server.py: tasklist/netstat/powershell probes, taskkill kills, and the
// CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW creation flags).

package main

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// createNoWindow is subprocess.CREATE_NO_WINDOW from the Python version
// (not exported by Go's syscall package).
const createNoWindow = 0x08000000

// hideWindow keeps helper commands (tasklist, netstat, powershell, the
// browser opener, go build) from flashing a console window. It matters most in
// the windowsgui build, where the launcher itself has no console and every
// console child would otherwise allocate a visible one.
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

// showErrorMessage surfaces fatal startup errors in the windowsgui build,
// where there is no console to print them to. No-op cost in console builds.
func showErrorMessage(title, msg string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	box := user32.NewProc("MessageBoxW")
	tptr, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	mptr, err := syscall.UTF16PtrFromString(msg)
	if err != nil {
		return
	}
	// HWND 0, MB_ICONERROR (0x10) — fire and forget.
	_, _, _ = box.Call(0, uintptr(unsafe.Pointer(mptr)), uintptr(unsafe.Pointer(tptr)), 0x10)
}

// processSysProcAttr keeps managed apps out of the launcher's console and in
// their own process group; closing the launcher console must not deliver
// CTRL_CLOSE_EVENT to the apps (Python _app_creationflags).
func processSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow,
	}
}

// shellCommand mirrors Python _shell_command on Windows.
func shellCommand(cmd string) []string {
	return []string{"cmd.exe", "/d", "/s", "/c", cmd}
}

// normalizeAppCommand mirrors Python _normalize_app_command on Windows.
func normalizeAppCommand(cmd string) string {
	fixed := strings.Replace(cmd, "exec ", "", 1)
	return strings.ReplaceAll(fixed, "python3", "python")
}

// shellSafePath returns an 8.3 short path when the path contains spaces, so
// the command string survives cmd.exe quote stripping (callers build
// "<path> --args" style command lines).
func shellSafePath(p string) string {
	if !strings.Contains(p, " ") {
		return p
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getShort := kernel32.NewProc("GetShortPathNameW")
	ptr, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	buf := make([]uint16, 1024)
	n, _, _ := getShort.Call(
		uintptr(unsafe.Pointer(ptr)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if n == 0 || n > uintptr(len(buf)) {
		return p
	}
	return syscall.UTF16ToString(buf[:n])
}

// pidAlive mirrors Python pid_alive on Windows: scan `tasklist /FO CSV /NH`.
// PID 0 is the System Idle Process, which tasklist happily reports as alive —
// a stateless desktop entry must read as dead (matches the POSIX guard).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := runCommand(5*time.Second, "tasklist", "/FO", "CSV", "/NH")
	if err != nil {
		return false
	}
	want := strconv.Itoa(pid)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, `"PID"`) {
			continue
		}
		csv := strings.Trim(strings.TrimSpace(line), `"`)
		parts := strings.Split(csv, `","`)
		if len(parts) >= 2 && strings.TrimSpace(parts[1]) == want {
			return true
		}
	}
	return false
}

// listenerPIDs mirrors Python listener_pids on Windows: parse
// `netstat -ano -p tcp`, dedupe while preserving order.
func listenerPIDs(port int) []int {
	out, err := runCommand(5*time.Second, "netstat", "-ano", "-p", "tcp")
	if err != nil {
		return nil
	}
	want := strconv.Itoa(port)
	seen := make(map[int]bool)
	var pids []int
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 5 || parts[0] != "TCP" {
			continue
		}
		if !strings.Contains(parts[1], ":") {
			continue
		}
		if local := parts[1][strings.LastIndex(parts[1], ":")+1:]; local != want {
			continue
		}
		if parts[3] != "LISTENING" {
			continue
		}
		pid, convErr := strconv.Atoi(parts[4])
		if convErr != nil {
			continue
		}
		if !seen[pid] {
			seen[pid] = true
			pids = append(pids, pid)
		}
	}
	return pids
}

// procCmdline mirrors Python proc_cmdline on Windows: the raw command line
// via PowerShell, or "" when unavailable (rendered as null in JSON).
func procCmdline(pid int) string {
	out, err := runCommand(5*time.Second, "powershell", "-NoProfile", "-Command",
		fmt.Sprintf(`(Get-CimInstance Win32_Process -Filter "ProcessId = %d").CommandLine`, pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// runTaskkill ignores nonzero exit codes (Python check=False) but reports
// spawn failures (Python OSError).
func runTaskkill(args ...string) error {
	cmd := exec.Command("taskkill", args...)
	hideWindow(cmd)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return err
}

// killTree is Python _kill_group on Windows (also used as the escalation).
func killTree(pid int) { _ = runTaskkill("/PID", strconv.Itoa(pid), "/T", "/F") }

// killTreeForce is the "still alive after SIGTERM" escalation; on Windows it
// is the same forced tree kill.
func killTreeForce(pid int) { _ = runTaskkill("/PID", strconv.Itoa(pid), "/T", "/F") }

// killProc kills a single listener PID (taskkill /F, no /T).
func killProc(pid int) error { return runTaskkill("/PID", strconv.Itoa(pid), "/F") }

// killProcForce is the listener escalation (same forced kill on Windows).
func killProcForce(pid int) { _ = runTaskkill("/PID", strconv.Itoa(pid), "/F") }
