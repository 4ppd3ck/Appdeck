//go:build darwin && cgo

// cgo bridge between the menu bar companion's Objective-C layer
// (native_darwin.go) and the platform-neutral tray model (tray.go). The
// exports run on the AppKit main thread when a menu opens or an item is
// clicked; anything slow is handed to a goroutine at once.
package main

/*
#include <stdlib.h>
extern void deckOpenAppURL(const char *url);
extern void deckShowMainWindow(void);
extern void deckTraySetVisible(int on);
*/
import "C"

import (
	"encoding/json"
	"unsafe"
)

func init() {
	trayOpenURL = func(url string) {
		cs := C.CString(url)
		C.deckOpenAppURL(cs)
		C.free(unsafe.Pointer(cs))
	}
	trayShowDashboard = func() {
		C.deckShowMainWindow()
	}
	// macOS can install/show/hide the companion live — the General
	// settings trayIcon toggle calls this on save.
	trayApplyVisible = func(on bool) {
		if on {
			C.deckTraySetVisible(1)
		} else {
			C.deckTraySetVisible(0)
		}
	}
}

// appdeckTrayMenuJSON hands the Objective-C menu builder the cached
// snapshot. The caller frees the returned C string.
//
//export appdeckTrayMenuJSON
func appdeckTrayMenuJSON() *C.char {
	trayMu.Lock()
	snap := trayModel
	trayMu.Unlock()
	raw, err := json.Marshal(snap)
	if err != nil {
		return C.CString("{}")
	}
	return C.CString(string(raw))
}

// appdeckTraySearch opens the dashboard's quick switcher on demand; the tray
// menu itself stays compact and does not enumerate installed apps.
//
//export appdeckTraySearch
func appdeckTraySearch() {
	go traySearchApps()
}

// appdeckTrayAppAction is a tray app row click: start it when stopped, then
// open its page in a popup window. Takes ownership of both C strings.
//
//export appdeckTrayAppAction
func appdeckTrayAppAction(cid, curl *C.char, running C.int) {
	id := C.GoString(cid)
	url := C.GoString(curl)
	C.free(unsafe.Pointer(cid))
	C.free(unsafe.Pointer(curl))
	go trayLaunchApp(id, url, running != 0)
}

// appdeckTrayToggle is a tray quick-action click: flips the app's
// manifest-declared enforcement toggle (pause != 0 → pause, 0 → resume).
// Takes ownership of cid.
//
//export appdeckTrayToggle
func appdeckTrayToggle(cid *C.char, pause C.int) {
	id := C.GoString(cid)
	C.free(unsafe.Pointer(cid))
	go trayRunToggle(id, pause != 0)
}

// appdeckTrayRunningOnlyToggle flips the tray menu's ◐ display filter
// (show only running apps) and reports the new state, so an open menu can
// re-filter its rows immediately. The menu rebuilds from the snapshot on
// its next open; Go's refresh lands meanwhile.
//
//export appdeckTrayRunningOnlyToggle
func appdeckTrayRunningOnlyToggle() C.int {
	next := !trayRunningOnly.Load()
	trayRunningOnly.Store(next)
	go trayRefresh()
	if next {
		return 1
	}
	return 0
}
