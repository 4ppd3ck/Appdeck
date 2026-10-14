//go:build windows

// Native launcher window: a real Win32 window hosting WebView2 (the same
// engine Edge uses, preinstalled on Windows 10/11). No browser UI, own
// taskbar button, and closing the window returns Run() — which the launcher
// treats as "quit the application".
package main

import (
	"os"
	"path/filepath"

	webview2 "github.com/jchv/go-webview2"
)

// runNativeWindow opens url in the launcher's own window and blocks until the
// window is closed. Returns false when WebView2 is unavailable (no runtime),
// so the caller can fall back to an Edge/Chrome app window or the browser.
func runNativeWindow(url, dataPath string) (opened bool) {
	defer func() {
		// A missing WebView2 runtime surfaces as a panic or nil from the
		// loader; either way we fall back instead of crashing.
		if r := recover(); r != nil {
			opened = false
		}
	}()

	// Keep the WebView2 profile data with the rest of the launcher data so
	// nothing appears next to the exe.
	if dataPath != "" {
		profile := filepath.Join(dataPath, "webview")
		_ = os.Setenv("WEBVIEW2_USER_DATA_FOLDER", profile)
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  dataPathOrEmpty(dataPath),
		WindowOptions: webview2.WindowOptions{
			Title:  "AppDeck",
			Width:  1180,
			Height: 820,
			Center: true,
			IconId: 1, // first icon resource embedded via rsrc_windows_*.syso
		},
	})
	if w == nil {
		return false
	}
	w.SetTitle("AppDeck")
	w.Navigate(url)
	w.Run() // blocks until the window is closed or Terminate is called
	w.Destroy()
	return true
}

func dataPathOrEmpty(dataPath string) string {
	if dataPath == "" {
		return "" // dev builds: WebView2 uses its default location
	}
	return filepath.Join(dataPath, "webview")
}
