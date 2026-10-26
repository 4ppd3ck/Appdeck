// ⌨ CLI apps (manifest "cli": true — an MCP stdio server, a command-line
// toolkit) have no window to focus and no port to open: their natural home
// is a terminal in the app's own folder, where the tool's commands live.
// Per-OS best effort; nothing is executed in the folder — the terminal
// opens a plain interactive shell there.
package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
)

// serveTerminal answers POST /api/terminal {id}: open a terminal window in
// the app's folder. Loopback only (it opens a window on this machine).
func (s *Server) serveTerminal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	readBodyInto(r, &req)
	app := s.appByID(req.ID)
	if app == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown id"})
		return
	}
	if app.Dir == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the app has no folder on this machine"})
		return
	}
	if info, err := os.Stat(app.Dir); err != nil || !info.IsDir() {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the app folder is gone: " + app.Dir})
		return
	}
	if err := openTerminalAt(app.Dir); err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": "terminal opened in the app's folder"})
}

// openTerminalAt opens an interactive terminal cd'd into dir.
func openTerminalAt(dir string) error {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`tell application "Terminal"
	activate
	do script %s
end tell`, psSingleQuoted("cd "+shSingleQuoted(dir)+" && clear"))
		cmd := exec.Command("osascript", "-e", script)
		hideWindow(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("could not open Terminal: %s", lastLines(string(out), 2))
		}
		return nil
	case "windows":
		// start's empty title argument keeps a quoted path from being read
		// as the title; /k keeps the console open in the folder.
		cmd := exec.Command("cmd", "/d", "/c",
			fmt.Sprintf(`start "" cmd /k cd /d "%s"`, dir))
		hideWindow(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("could not open a console: %s", lastLines(string(out), 2))
		}
		return nil
	}
	// Linux and the BSDs: try the usual terminal emulators by their
	// working-directory flag, first installed one wins.
	for _, spec := range [][2]string{
		{"gnome-terminal", "--working-directory"},
		{"konsole", "--workdir"},
		{"xfce4-terminal", "--working-directory"},
		{"mate-terminal", "--working-directory"},
		{"kitty", "--directory"},
		{"alacritty", "--working-directory"},
		{"x-terminal-emulator", "--working-directory"},
	} {
		path, err := exec.LookPath(spec[0])
		if err != nil {
			continue
		}
		c := exec.Command(path, spec[1], dir)
		c.SysProcAttr = processSysProcAttr() // own group: the launcher never waits on it
		if err := c.Start(); err != nil {
			return fmt.Errorf("could not open %s: %v", spec[0], err)
		}
		go func() { _ = c.Wait() }() // reap without blocking the request
		return nil
	}
	return fmt.Errorf("no known terminal emulator found (tried gnome-terminal, konsole, xfce4-terminal, mate-terminal, kitty, alacritty)")
}

// shSingleQuoted renders a POSIX shell single-quoted literal.
func shSingleQuoted(s string) string {
	out := "'"
	for _, r := range s {
		if r == '\'' {
			out += `'\''`
		} else {
			out += string(r)
		}
	}
	return out + "'"
}
