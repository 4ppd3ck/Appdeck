// Import size limits: the three ceilings every import path enforces —
// the body cap (uploads and downloads are buffered whole in RAM, so it
// bounds the working set), the expanded-bundle cap (extraction writes to
// disk entry by entry, so it bounds disk use and stops zip bombs) and the
// single-file HTML cap. Defaults are conservative; the deck tools modal
// overrides them per install via launcher.json, with 0 meaning no cap.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
)

// limitRange is one limit's allowed span, in bytes. 0 (no cap) is always
// allowed and expressed outside the range.
type limitRange struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

const (
	defaultImportBody = int64(512) << 20 // 512 MB
	defaultExpand     = int64(2) << 30   // 2 GiB
	defaultImportHTML = int64(20) << 20  // 20 MB
)

// int64 everywhere: windows/386 ships (release.sh), and limits this large
// must not overflow its 32-bit int.
var (
	bodyRange   = limitRange{Min: 1 << 20, Max: 64 << 30} // 1 MB–64 GB
	expandRange = limitRange{Min: 1 << 20, Max: 1 << 40}  // 1 MB–1 TB
	htmlRange   = limitRange{Min: 64 << 10, Max: 1 << 30} // 64 KB–1 GB

	limitImportBody atomic.Int64 // bytes; 0 = uncapped
	limitExpand     atomic.Int64
	limitImportHTML atomic.Int64
)

func init() {
	limitImportBody.Store(defaultImportBody)
	limitExpand.Store(defaultExpand)
	limitImportHTML.Store(defaultImportHTML)
}

// importMaxBytes caps a whole import body (upload or download) held in RAM.
func importMaxBytes() int64 { return limitImportBody.Load() }

// expandMaxBytes caps a bundle's extracted total on disk.
func expandMaxBytes() int64 { return limitExpand.Load() }

// importMaxHTML caps a single-file HTML app body.
func importMaxHTML() int64 { return limitImportHTML.Load() }

// limitedBody wraps a whole-body read under limit bytes; limit 0 reads
// without a ceiling (the unlimited setting).
func limitedBody(r io.Reader, limit int64) io.Reader {
	if limit <= 0 {
		return r
	}
	return io.LimitReader(r, limit+1)
}

// underLimit reports whether n is strictly under limit (callers treating
// the limit as exclusive); limit 0 accepts everything.
func underLimit(n, limit int64) bool { return limit <= 0 || n < limit }

// withinLimit is underLimit's inclusive form; limit 0 accepts everything.
func withinLimit(n, limit int64) bool { return limit <= 0 || n <= limit }

// resolveLimits publishes the stored doc's limits (nil fields keep their
// defaults) into the hot caches every import reads. Called at boot and
// after each validated save.
func resolveLimits(body, expand, html *int64) {
	limitImportBody.Store(orDefaultLimit(body, defaultImportBody))
	limitExpand.Store(orDefaultLimit(expand, defaultExpand))
	limitImportHTML.Store(orDefaultLimit(html, defaultImportHTML))
}

// sanitizeLimitsDoc guards the hand-edited-file path: a limits key holding
// a non-number (a typo like "banana") unmarshals into *int64 as a zeroed
// pointer — which would read as 0, the "no cap" setting, instead of
// falling back to the default. Type-check the three keys; anything not a
// JSON number (or a document that isn't JSON at all) drops to absent.
func sanitizeLimitsDoc(data []byte, out *launcherSettings) {
	var probe map[string]any
	if json.Unmarshal(data, &probe) != nil {
		out.ImportBodyBytes, out.ImportExpandBytes, out.ImportHTMLBytes = nil, nil, nil
		return
	}
	for _, c := range []struct {
		key string
		dst **int64
	}{
		{"importBodyBytes", &out.ImportBodyBytes},
		{"importExpandBytes", &out.ImportExpandBytes},
		{"importHTMLBytes", &out.ImportHTMLBytes},
	} {
		if v, present := probe[c.key]; present {
			if _, isNum := v.(float64); !isNum {
				*c.dst = nil
			}
		}
	}
}

func orDefaultLimit(v *int64, def int64) int64 {
	if v == nil {
		return def
	}
	return *v
}

// validateLimits checks the triple a settings patch resolved to: each
// value is 0 (no cap) or inside its range, and a bounded expand must be
// at least the body cap — even stored (uncompressed) archives expand to
// the body's size, so a smaller expand would reject legitimate imports.
func validateLimits(body, expand, html int64) error {
	for _, l := range []struct {
		name string
		v    int64
		span limitRange
	}{
		{"import body", body, bodyRange},
		{"expanded bundle", expand, expandRange},
		{"html app", html, htmlRange},
	} {
		if l.v != 0 && (l.v < l.span.Min || l.v > l.span.Max) {
			return fmt.Errorf("%s limit must be 0 (no cap) or %d–%d bytes", l.name, l.span.Min, l.span.Max)
		}
	}
	if body > 0 && expand > 0 && expand < body {
		return fmt.Errorf("the expanded bundle limit (%d bytes) cannot be below the import body limit (%d bytes)", expand, body)
	}
	return nil
}

// serveLimits answers GET /api/limits: the enforced values, the defaults,
// and the byte ranges the UI validates against. Read-only — writes go
// through POST /api/launcher-settings like every other setting.
func (s *Server) serveLimits(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"current": map[string]int64{
			"importBody": importMaxBytes(),
			"expand":     expandMaxBytes(),
			"importHTML": importMaxHTML(),
		},
		"defaults": map[string]int64{
			"importBody": defaultImportBody,
			"expand":     defaultExpand,
			"importHTML": defaultImportHTML,
		},
		"ranges": map[string]limitRange{
			"importBody": bodyRange,
			"expand":     expandRange,
			"importHTML": htmlRange,
		},
	})
}
