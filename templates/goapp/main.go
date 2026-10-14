// __APPNAME__ — an AppDeck app (Go template).
//
// A working demonstration of the AppDeck app contract (AUTHORING.md):
//
//   - flags -port/-host arrive via app.json args with {port}/{host}
//     substituted; the APPDECK_PORT / APPDECK_HOST environment carries the
//     same values for apps that prefer env over flags;
//   - APPDECK_HOST is 127.0.0.1 unless the manifest opts into Wi-Fi sharing
//     (network: true) — never bind wider than you meant to;
//   - when APPDECK_NETWORK is not "1", the Host header is checked against
//     the loopback names, which is what stops DNS-rebinding requests from
//     a malicious web page riding on your localhost port;
//   - APPDECK_DATA is this app's own writable directory — user data goes
//     there, never next to the binary;
//   - APPDECK_TOKEN (when set) authenticates non-browser API clients via
//     the X-AppDeck-Token header.
//
// The launcher builds this module on first start (platform binary naming is
// handled for you) and shows it on the dashboard.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

func main() {
	envPort, _ := strconv.Atoi(firstNonEmpty(os.Getenv("APPDECK_PORT"), os.Getenv("PORT"), "0"))
	host := firstNonEmpty(os.Getenv("APPDECK_HOST"), "127.0.0.1")
	port := flag.Int("port", envPort, "port to serve on")
	flag.StringVar(&host, "host", host, "host to bind (loopback unless AppDeck says otherwise)")
	flag.Parse()

	if *port <= 0 {
		log.Fatal("no port — start me from the AppDeck dashboard")
	}
	dataDir := firstNonEmpty(os.Getenv("APPDECK_DATA"), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("data dir %s: %v", dataDir, err)
	}

	visits := &counter{path: filepath.Join(dataDir, "visits.json")}
	visits.load()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		n := visits.bump()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, page, esc("__APPNAME__"), esc("__APPID__"), host, *port, dataDir, n)
	})
	mux.HandleFunc("GET /api/count", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"visits": visits.bump()})
	})

	var handler http.Handler = mux
	if os.Getenv("APPDECK_NETWORK") != "1" {
		handler = loopbackHostOnly(*port, handler)
	}
	if token := os.Getenv("APPDECK_TOKEN"); token != "" {
		// Non-browser API clients authenticate with X-AppDeck-Token; the
		// browser pages are covered by the loopback Host check above.
		handler = tokenAPI(token, handler)
	}

	addr := host + ":" + strconv.Itoa(*port)
	log.Printf("__APPNAME__ listening on http://%s (data: %s)", addr, dataDir)
	log.Fatal(http.ListenAndServe(addr, handler))
}

// loopbackHostOnly rejects requests whose Host header is not this app's
// loopback address — the browser-compatible defense against DNS rebinding.
func loopbackHostOnly(port int, next http.Handler) http.Handler {
	allowed := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
		fmt.Sprintf("[::1]:%d", port):     true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenAPI enforces X-AppDeck-Token on /api/* paths when APPDECK_TOKEN is
// set; page loads stay header-free so browsers can open the app normally.
func tokenAPI(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Header.Get("X-AppDeck-Token") != token {
			http.Error(w, "missing or wrong X-AppDeck-Token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// counter persists its number in APPDECK_DATA — proof the folder is writable.
type counter struct {
	mu    sync.Mutex
	path  string
	count int
}

func (c *counter) load() {
	if data, err := os.ReadFile(c.path); err == nil {
		_ = json.Unmarshal(data, &c.count)
	}
}

func (c *counter) bump() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	if data, err := json.Marshal(c.count); err == nil {
		_ = os.WriteFile(c.path, data, 0o644)
	}
	return c.count
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func esc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

const page = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>__APPNAME__</title>
<style>
  body { font: 16px/1.6 system-ui, sans-serif; background: #0b0e14; color: #e6eaf2;
         display: grid; place-items: center; min-height: 100vh; margin: 0; }
  main { background: #131722; border: 1px solid #232a3b; border-radius: 14px;
         padding: 32px 40px; max-width: 560px; }
  h1 { margin: 0 0 4px; font-size: 22px; }
  code { color: #38c6f4; font-family: ui-monospace, Menlo, monospace; }
  dl { display: grid; grid-template-columns: auto 1fr; gap: 4px 18px;
       font-size: 14px; color: #8b93a7; margin-top: 18px; }
  dt { font-weight: 600; color: #e6eaf2; }
</style>
</head>
<body>
<main>
  <h1>🧩 %s</h1>
  <p>The AppDeck Go template, running. This page is served by your own
     process — edit <code>main.go</code> and restart from the dashboard.</p>
  <dl>
    <dt>App id</dt><dd><code>%s</code></dd>
    <dt>Address</dt><dd><code>http://%s:%d</code></dd>
    <dt>Data dir</dt><dd><code>%s</code></dd>
    <dt>Visits (persisted)</dt><dd><code>%d</code></dd>
  </dl>
</main>
</body>
</html>
`
