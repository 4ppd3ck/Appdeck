// Show: the desktop-app equivalent of Open. A desktop app has no URL to
// visit — when it is running, "🖥 Show" brings its window to the front
// instead. Per-OS, best effort: macOS asks System Events to front the
// process by unix pid (first use pops a one-time Automation permission),
// Windows uses WScript.AppActivate, Linux drives wmctrl when present.
package main

import (
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
)

func (s *Server) serveShow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	readBodyInto(r, &req)
	app := s.appByID(req.ID)
	if app == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
		return
	}
	st := loadState(s.cfg.StateFile)
	pid := st[app.ID].PID
	if pid == 0 || !pidAlive(pid) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not running"})
		return
	}
	if err := focusProcess(pid); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": "window focused"})
}

// focusProcess brings the app's window to the front, per OS.
func focusProcess(pid int) error {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`tell application "System Events" to set frontmost of (first process whose unix id is %d) to true`, pid)
		cmd := exec.Command("osascript", "-e", script)
		hideWindow(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = err.Error()
			}
			if strings.Contains(msg, "not allowed") || strings.Contains(msg, "-25211") || strings.Contains(msg, "-1743") {
				return fmt.Errorf("macOS needs permission: System Settings ▸ Privacy & Security ▸ Automation — allow AppDeck ▸ System Events")
			}
			return fmt.Errorf("could not focus the window: %s", lastLines(msg, 3))
		}
		return nil
	case "windows":
		script := fmt.Sprintf(`(New-Object -ComObject WScript.Shell).AppActivate(%d)`, pid)
		cmd := exec.Command("powershell", "-STA", "-NoProfile", "-NonInteractive", "-Command", script)
		hideWindow(cmd)
		if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(strings.ToLower(string(out)), "true") {
			return fmt.Errorf("could not focus the window")
		}
		return nil
	}
	// Linux and the BSDs: wmctrl lists windows with their pids; activate by
	// window id. Best effort — no window manager cooperation, no focus.
	out, err := exec.Command("wmctrl", "-lp").Output()
	if err != nil {
		return fmt.Errorf("wmctrl not available — cannot focus the window")
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[2] == fmt.Sprint(pid) {
			if err := exec.Command("wmctrl", "-i", "-a", fields[0]).Run(); err == nil {
				return nil
			}
		}
	}
	return fmt.Errorf("no window found for the process")
}
