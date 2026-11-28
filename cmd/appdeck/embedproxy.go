// Same-origin embed for the deck's in-window app tabs.
//
// The dashboard opens apps in iframes on the dashboard's own origin, while
// the app listens on its own port — a cross-origin frame. WebKit blank-
// frames several perfectly working apps in that position: servers that send
// X-Frame-Options (GymMane's Dart server), servers that require COEP, and
// apps whose boot path touches storage or service workers that WebKit denies
// third-party frames. Serving the app under /embed/<id>/ — the dashboard's
// origin — removes the whole class: the frame is same-origin, and the two
// header families that would still refuse embedding are relaxed in transit.
//
// The route is a transport shim, not a security boundary: it forwards to the
// app's loopback port and never elevates access (a LAN peer may embed only
// apps that are themselves Wi-Fi shared). Apps keep their absolute paths and
// their own origin when opened outside the deck — this only changes how the
// deck's tab reads them.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const embedPrefix = "/embed/"

// embedBodyCap bounds the HTML buffering the base-href rewrite needs. A
// Flutter index.html is a few KB; 16 MB covers any sane document while
// keeping the proxy memory-bounded on adversarial responses.
const embedBodyCap = 16 << 20

// embedBaseTag matches a <base href="…"> pointing at an absolute path —
// Flutter web builds ship `<base href="/">`, which would resolve every
// relative asset against the dashboard root instead of the embed prefix.
var embedBaseTag = regexp.MustCompile(`(?i)<base\s+href=["'](/[^"']*)["']\s*/?>`)

// serveEmbed handles /embed/<id>/… by proxying to the app's own port with
// the path mapped onto the app's URLPath.
func (s *Server) serveEmbed(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, embedPrefix)
	id, sub, _ := strings.Cut(rest, "/")
	app := s.appByID(id)
	if app == nil || app.Desktop || app.Port == 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "no embeddable app " + id})
		return
	}
	// The deck's LAN posture applies: this machine may embed anything it can
	// open; a Wi-Fi peer only apps it could open directly anyway.
	if !fromLocalMachine(r) && !app.Network {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "this app is not shared on the Wi-Fi"})
		return
	}
	// The browser normalizes /embed/<id> (no trailing slash) to the bare
	// path — send it back so relative URLs inside the app resolve.
	if sub == "" && !strings.HasSuffix(r.URL.Path, "/") {
		http.Redirect(w, r, embedPrefix+id+"/", http.StatusFound)
		return
	}

	prefix := embedPrefix + id
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", app.Port)}
	appBase := strings.TrimSuffix(app.URLPath, "/") // "" for the default "/"
	rp := newEmbedReverseProxy(target, appBase, prefix, true)
	rp.ServeHTTP(w, r)
}

// embedFallback rescues root-absolute requests the launcher's own routes
// would 404. The shim serves app documents under /embed/<id>/, but an app
// whose page references assets and APIs absolutely — "/web/style.css",
// fetch("/api/…"), EventSource("/api/stream") — fires those at the
// dashboard's bare paths, where they would miss. When the Referer is this
// deck naming an embed prefix, re-run the request through that app's shim
// so the tab contract ("apps keep their absolute paths") covers runtime
// calls too. route()'s default (would-be-404) branch calls this; it
// reports whether it answered. Same gates as serveEmbed, and never for
// /embed/ paths themselves — the shim's own routes cannot fall back into
// it, so there is no loop.
func (s *Server) embedFallback(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, embedPrefix) {
		return false
	}
	ref, err := url.Parse(r.Header.Get("Referer"))
	if err != nil || ref.Host != r.Host || !strings.HasPrefix(ref.Path, embedPrefix) {
		return false
	}
	id, _, _ := strings.Cut(strings.TrimPrefix(ref.Path, embedPrefix), "/")
	app := s.appByID(id)
	if app == nil || app.Desktop || app.Port == 0 {
		return false
	}
	// The deck's LAN posture, identical to serveEmbed: this machine may
	// embed anything it can open; a Wi-Fi peer only apps it could open
	// directly anyway.
	if !fromLocalMachine(r) && !app.Network {
		return false
	}
	prefix := embedPrefix + id
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", app.Port)}
	appBase := strings.TrimSuffix(app.URLPath, "/")
	newEmbedReverseProxy(target, appBase, prefix, false).ServeHTTP(w, r)
	return true
}

// newEmbedReverseProxy builds the transport shim one embed needs: path
// mapping, isolation-header relaxation, redirect and base-href rewriting.
// stripPrefix selects the mode: the embed route serves under /embed/<id>/
// and trims that prefix off the inbound path; the bare-path fallback
// (embedFallback) passes the whole path through — both land on the app via
// joinEmbedPath.
func newEmbedReverseProxy(target *url.URL, appBase, prefix string, stripPrefix bool) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			sub := pr.In.URL.Path
			if stripPrefix {
				sub = strings.TrimPrefix(sub, prefix)
			}
			pr.Out.URL.Path = joinEmbedPath(appBase, sub)
			pr.Out.URL.RawPath = ""
			// Identity encoding only: the base-href rewrite below must read
			// the document, not gzip — loopback bandwidth makes this a non-
			// issue, and ModifyResponse runs before any client-side
			// decompression could help.
			pr.Out.Header.Del("Accept-Encoding")
		},
		ModifyResponse: func(res *http.Response) error {
			h := res.Header
			// The two header families that refuse embedding. CORP stays: all
			// embed traffic is same-origin through this route, and COEP-less
			// documents may load same-origin CORP resources fine.
			h.Del("X-Frame-Options")
			h.Del("Cross-Origin-Embedder-Policy")
			h.Del("Cross-Origin-Opener-Policy")
			// The launcher's baseline middleware already set these on the
			// ResponseWriter; the app's copies would only double the values.
			h.Del("X-Content-Type-Options")
			h.Del("Referrer-Policy")
			// Apps redirect to their own absolute paths ("Location: /") —
			// those must stay inside the embed prefix, not escape to the
			// app's real origin and bounce the tab out of the deck.
			if loc := h.Get("Location"); loc != "" {
				if out := rewriteEmbedURL(prefix, target.Host, loc); out != loc {
					h.Set("Location", out)
				}
			}
			// Document-level rewrite: point <base href> at the embed prefix
			// so the app's relative assets resolve through the shim.
			if isHTMLResponse(res) {
				return rewriteEmbedHTML(res, prefix)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "<!DOCTYPE html><html><body style=\"font:14px -apple-system,sans-serif;color:#888;background:#111;display:grid;place-items:center;height:100vh;margin:0\"><p>This app is not running — start it from the dashboard.</p></body></html>")
		},
		FlushInterval: -1, // stream immediately: SSE and streamed responses stay live
	}
	return rp
}

// joinEmbedPath maps the embed sub-path onto the app's own URLPath:
// /embed/<id>/a/b + URLPath /dash/ → /dash/a/b.
func joinEmbedPath(appBase, sub string) string {
	sub = strings.TrimPrefix(sub, "/")
	if appBase == "" {
		return "/" + sub
	}
	return appBase + "/" + sub
}

// rewriteEmbedURL keeps redirects inside the embed prefix: absolute app
// paths and self-URLs are re-rooted, foreign targets pass untouched.
func rewriteEmbedURL(prefix, selfHost, loc string) string {
	if strings.HasPrefix(loc, "/") {
		return prefix + loc
	}
	if u, err := url.Parse(loc); err == nil && u.Host == selfHost && (u.Scheme == "http" || u.Scheme == "https") {
		out := prefix + u.Path
		if u.RawQuery != "" {
			out += "?" + u.RawQuery
		}
		return out
	}
	return loc
}

func isHTMLResponse(res *http.Response) bool {
	return strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "text/html")
}

// rewriteEmbedHTML buffers an HTML document (bounded), rewrites the base tag,
// and swaps the body in with a corrected Content-Length.
func rewriteEmbedHTML(res *http.Response, prefix string) error {
	// ContentLength 0 is an empty or body-less response (HEAD, 204, 304) with
	// nothing to rewrite. Chunked/unknown length is -1 and falls through.
	if res.ContentLength > embedBodyCap || res.ContentLength == 0 {
		return nil // empty or oversized — pass through untouched
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, embedBodyCap+1))
	res.Body.Close()
	if err != nil {
		return err
	}
	if int64(len(body)) > embedBodyCap {
		out := bytes.NewReader(body)
		res.Body = io.NopCloser(out)
		return nil
	}
	if m := embedBaseTag.FindSubmatch(body); m != nil {
		// Re-root the base but keep its path: <base href="/app/"> inside
		// prefix /embed/id/ serves assets from /embed/id/app/.
		base := prefix + string(m[1])
		fixed := embedBaseTag.ReplaceAll(body, []byte(`<base href="`+base+`">`))
		body = fixed
	}
	res.Body = io.NopCloser(bytes.NewReader(body))
	res.ContentLength = int64(len(body))
	res.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}
