// HTTP-layer guards for the launcher's own listener: a Host allowlist (the
// DNS-rebinding fence) and a request-body cap.
//
// Why a Host check exists at all: every "loopback only" decision in this
// package asks the TCP peer address. That is the right question for a LAN
// peer, but a browser can be pointed at 127.0.0.1 from a page on any domain
// (DNS rebinding): the peer really is loopback, while the Host header still
// names the attacker's domain. Validating Host against the names this machine
// actually owns is what turns "the peer is local" back into "the caller meant
// this launcher". The guard runs before the router, so no handler — including
// the loopback-only ones — ever sees a rebound request.
//
// Portability: only net/os/strings are used, no build tags. Interface
// enumeration, hostname lookup and IPv6 parsing behave the same on darwin,
// linux and windows; a name that cannot be resolved is simply absent from the
// allowlist, never an error.
package main

import (
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	allowlistOnce sync.Once
	allowlist     map[string]bool
)

// hostAllowlist is the set of Host values this launcher answers to: loopback
// names, the machine's hostname (and its .local mDNS form), every address
// assigned to a local interface (LAN, Tailscale/WireGuard, VPN), and any
// extra names the operator lists in LAUNCHER_ALLOWED_HOSTS — the escape hatch
// for reaching the deck through a reverse proxy or a custom domain.
//
// Computed once per process: interfaces change rarely, and a per-request
// enumeration would be wasteful. A launcher that moves networks picks up the
// new address on its next start, which is also when its certificate is minted.
func hostAllowlist() map[string]bool {
	allowlistOnce.Do(func() {
		m := map[string]bool{
			"localhost": true,
			"127.0.0.1": true,
			"::1":       true,
		}
		if host, err := os.Hostname(); err == nil {
			if h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), ".")); h != "" {
				m[h] = true
				m[h+".local"] = true
			}
			// Windows reports the machine name without a domain; a joined
			// host may also answer to its short name. Both are covered by the
			// bare hostname above.
		}
		if ifaces, err := net.Interfaces(); err == nil {
			for _, ifc := range ifaces {
				addrs, aerr := ifc.Addrs()
				if aerr != nil {
					continue
				}
				for _, a := range addrs {
					switch v := a.(type) {
					case *net.IPNet:
						if v.IP != nil {
							m[strings.ToLower(v.IP.String())] = true
						}
					case *net.IPAddr:
						if v.IP != nil {
							m[strings.ToLower(v.IP.String())] = true
						}
					}
				}
			}
		}
		for _, raw := range strings.Split(os.Getenv("LAUNCHER_ALLOWED_HOSTS"), ",") {
			if v := normalizeHost(raw); v != "" {
				m[v] = true
			}
		}
		allowlist = m
	})
	return allowlist
}

// normalizeHost lowercases a host value, drops a :port, IPv6 brackets and an
// IPv6 zone (%en0 / %25en0), and trims the trailing root dot ("macbook.").
func normalizeHost(hostport string) string {
	h := strings.TrimSpace(hostport)
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	if i := strings.IndexByte(h, '%'); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// hostAllowedName reports whether a request Host (or an Origin's host) names
// this machine. An empty Host is allowed: HTTP/1.0 clients send none, and a
// browser cannot be made to omit it, so this cannot be used to rebind.
func hostAllowedName(hostport string) bool {
	h := normalizeHost(hostport)
	if h == "" {
		return true
	}
	return hostAllowlist()[h]
}

// guardHosts rejects a request whose Host is not a name this machine owns.
// 421 Misdirected Request is the honest status: the connection reached a
// server, but not one that answers to that name.
func guardHosts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowedName(r.Host) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusMisdirectedRequest)
			_, _ = w.Write([]byte("AppDeck answers only names that belong to this computer " +
				"(DNS-rebinding guard). Reaching the deck through a domain or proxy? " +
				"Add it to LAUNCHER_ALLOWED_HOSTS.\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bodyLimitFor returns the request-body ceiling for a path. Imports, deck
// restores and launcher updates carry archives (up to 1 GiB by their own
// caps), so they get a generous ceiling; every other mutation is a small JSON
// document and gets a tight one. A Content-Length the client invented is
// never trusted.
//
// A raw upload path missing from this list is capped at 8 MiB and fails with
// "http: request body too large" — an all-in-one launcher zip is tens of MB,
// so /api/update belongs here even though the JSON form of that route only
// sends a small body.
func bodyLimitFor(path string) int64 {
	for _, p := range []string{
		"/api/import",
		"/api/install",
		"/api/deck/import",
		"/api/update",
		"/api/themes/import",
		"/api/backgrounds/import",
		"/api/iconpacks/import",
	} {
		if strings.HasPrefix(path, p) {
			return 1 << 30 // 1 GiB — the archive readers enforce their own caps
		}
	}
	return 8 << 20 // 8 MiB
}

// limitBodies caps every mutation body at the listener, so a LAN peer cannot
// make the launcher allocate unbounded memory with a huge POST. readBodyInto
// reads Content-Length bytes from this reader, which now stops at the cap.
func limitBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, bodyLimitFor(r.URL.Path))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// serverTimeouts is the shared timeout set for both listeners (plain HTTP and
// the TLS twin). ReadHeaderTimeout is the Slowloris fence; ReadTimeout is
// deliberately left at zero because it covers the whole body, and a legitimate
// 512 MB import over Wi-Fi must not be truncated. IdleTimeout reaps keep-alive
// connections the dashboard no longer uses.
func serverTimeouts() (readHeader, read, write, idle time.Duration) {
	return 10 * time.Second, 0, 0, 120 * time.Second
}
