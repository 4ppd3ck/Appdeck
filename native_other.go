//go:build !windows && (!darwin || !cgo)

// Native windows are a Windows feature (WebView2); on macOS they exist only
// in cgo builds (WKWebView, native_darwin.go). Everywhere else — and on any
// non-cgo darwin build — runNativeWindow always reports failure so the caller
// falls back to the Edge/Chrome app window or the default browser.
package main

// runNativeWindow always reports failure off Windows / without cgo so the
// caller falls back.
func runNativeWindow(_, _ string) bool { return false }
