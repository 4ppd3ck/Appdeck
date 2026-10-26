package main

// Window transparency — the 🖼 dialog's see-through knob (background.conf
// "transparency", 0..60). Native windows blend the whole window toward the
// desktop (NSWindow.alphaValue on macOS, WS_EX_LAYERED + LWA_ALPHA on
// Windows); browser and --app windows have no page-reachable window alpha,
// so the dashboard disables the knob there. The value reaches the window
// twice: at creation (setInitialWindowTransparency, read by the native
// window right after its window object exists) and live from the slider
// through each platform's JS bridge (window.webkit.messageHandlers.
// appdeckTransparency on macOS, window.appdeckSetTransparency on Windows).

// maxWindowTransparency caps the knob: past this, text on the dashboard
// stops being comfortably readable through the blend.
const maxWindowTransparency = 60

// initialWindowTransparency is set from the persisted conf on the main
// goroutine just before runNativeWindow and read once by the native window
// on the same thread — a plain int needs no lock.
var initialWindowTransparency int

func setInitialWindowTransparency(pct int) {
	if pct < 0 {
		pct = 0
	}
	if pct > maxWindowTransparency {
		pct = maxWindowTransparency
	}
	initialWindowTransparency = pct
}
