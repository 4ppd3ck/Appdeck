// Pack stylesheet safety.
//
// Theme and background packs are imported content — a zip the user downloaded
// or a git repository URL — and their CSS is inlined into the dashboard inside
// a <style> element. Two risks are neutralised at injection time, so packs
// already on disk are covered too (no migration):
//
//   - Markup breakout: the HTML tokenizer ends that element at the first
//     "</style", whatever the surrounding CSS means, so a pack carrying that
//     sequence turns a stylesheet into markup (and a <script> or onerror
//     handler into code running on the deck's own origin, where every
//     loopback-only API is in reach). CSS-escape the slash instead: "<\/style"
//     is an ordinary CSS escape for "/" (so the declaration still parses) and
//     is no longer an end tag for the tokenizer. Only "</style" can end the
//     element, so that is the only sequence rewritten.
//   - Network pulls: @import rules and url() references to http(s) addresses
//     (protocol-relative too) become dead values, so an imported pack cannot
//     make the dashboard page fetch anything from the network — the same
//     loopback-by-default posture the apps are held to. Local references are
//     untouched: data: urls, and relative urls that resolve to the pack's own
//     image files.
package main

import "regexp"

var (
	styleEndTag  = regexp.MustCompile(`(?i)</style`)
	cssImportOne = regexp.MustCompile(`(?i)@import[^;]*;`)
	cssRemoteURL = regexp.MustCompile(`(?i)url\(\s*(['"]?)\s*(?:https?:)?//[^)]*['"]?\s*\)`)
)

// sanitizePackCSS neutralises every "</style", @import rule and remote url()
// in a pack stylesheet.
func sanitizePackCSS(css string) string {
	if css == "" {
		return ""
	}
	css = cssImportOne.ReplaceAllString(css, "")
	css = cssRemoteURL.ReplaceAllString(css, "url(about:invalid)")
	return styleEndTag.ReplaceAllString(css, `<\/style`)
}
