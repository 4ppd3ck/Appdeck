// webapp — standalone file server for an AppDeck hosted web app.
//
// Built into each hosted app's folder (webserver.exe) so the app runs with
// or without the deck: double-click and it serves the folder it lives in on
// 127.0.0.1:<port> and opens the browser. Port comes from -port, then
// app.json in the folder, then 8080.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func main() {
	port := flag.String("port", "", "port to listen on (default: app.json port, else 8080)")
	noOpen := flag.Bool("no-open", false, "do not open the browser")
	flag.Parse()

	p := *port
	if p == "" {
		p = portFromAppJSON()
	}
	if p == "" {
		p = "8080"
	}
	addr := net.JoinHostPort("127.0.0.1", p)
	url := "http://" + addr + "/"

	if !*noOpen {
		open(url) // if the server is already running this still opens a tab
	}
	fmt.Println("Serving", url, " — Ctrl+C to stop")
	if err := http.ListenAndServe(addr, http.FileServer(http.Dir("."))); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// portFromAppJSON reads the app.json sitting next to the executable (or in
// the working directory) and returns its port, if any.
func portFromAppJSON() string {
	for _, base := range []string{".", dirOfExecutable()} {
		b, err := os.ReadFile(filepath.Join(base, "app.json"))
		if err != nil {
			continue
		}
		var cfg struct {
			Port int `json:"port"`
		}
		if json.Unmarshal(b, &cfg) == nil && cfg.Port > 0 {
			return fmt.Sprint(cfg.Port)
		}
	}
	return ""
}

func dirOfExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func open(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
