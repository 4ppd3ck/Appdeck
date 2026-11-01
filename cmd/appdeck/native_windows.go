//go:build windows

// Native launcher window: a real Win32 window hosting WebView2 (the same
// engine Edge uses, preinstalled on Windows 10/11). No browser UI, own
// taskbar button, and closing the window returns Run() — which the launcher
// treats as "quit the application".
package main

import (
	"os"
	"path/filepath"
	"syscall"

	webview2 "github.com/jchv/go-webview2"
)

// Window transparency (user32): the whole window is blended toward the
// desktop with WS_EX_LAYERED + SetLayeredWindowAttributes(LWA_ALPHA) — the
// same "see-through window" the macOS side gets from NSWindow.alphaValue.
var (
	user32GetWindowLongW            = syscall.NewLazyDLL("user32.dll").NewProc("GetWindowLongW")
	user32SetWindowLongW            = syscall.NewLazyDLL("user32.dll").NewProc("SetWindowLongW")
	user32SetLayeredWindowAttribute = syscall.NewLazyDLL("user32.dll").NewProc("SetLayeredWindowAttributes")
)

const (
	// GWL_EXSTYLE(-20) as a uintptr: a negative constant cannot convert to
	// uintptr at compile time, so spell it as its two's-complement form.
	gwlExStyle   = ^uintptr(19)
	wsExLayered  = 0x00080000
	lwaAlphaFlag = 0x2
)

// setWinTransparency applies the knob (0..60 %) to hwnd. pct 0 leaves the
// layered style off — a normal opaque window with zero compositing overhead.
func setWinTransparency(hwnd uintptr, pct int) {
	if hwnd == 0 {
		return
	}
	if pct < 0 {
		pct = 0
	}
	if pct > maxWindowTransparency {
		pct = maxWindowTransparency
	}
	if pct > 0 {
		style, _, _ := user32GetWindowLongW.Call(hwnd, gwlExStyle)
		user32SetWindowLongW.Call(hwnd, gwlExStyle, style|uintptr(wsExLayered))
	}
	alpha := uintptr(uint32(255 * (100 - pct) / 100))
	user32SetLayeredWindowAttribute.Call(hwnd, 0, alpha, uintptr(lwaAlphaFlag))
}

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
	// Transparency bridge: the 🖼 dialog's slider calls
	// window.appdeckSetTransparency(pct) (a webview2 binding), and the
	// persisted knob is applied at creation. Dispatch hops to the UI
	// thread, where the window calls belong.
	hwnd := uintptr(w.Window())
	_ = w.Bind("appdeckSetTransparency", func(pct float64) {
		p := int(pct + 0.5)
		w.Dispatch(func() { setWinTransparency(hwnd, p) })
	})
	w.Dispatch(func() { setWinTransparency(hwnd, initialWindowTransparency) })
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

// nativeConfigureDownloads is a no-op on Windows for now: WebView2 has its
// own download UI, and the deck does not intercept it.
func nativeConfigureDownloads(string, bool) {}
