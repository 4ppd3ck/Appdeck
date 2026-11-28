//go:build windows

// Popup app window placement. The --app popups run in a dedicated profile
// that persists its last window bounds; on RDP sessions whose resolution
// changed since the last open, Chromium restores those stale bounds and the
// popup lands over the taskbar (the search box) or partially offscreen.
// Pinning every open to the current work area overrides the restore.

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

var procSystemParametersInfoW = syscall.NewLazyDLL("user32.dll").NewProc("SystemParametersInfoW")

const spiGetWorkArea = 0x0030

type workRect struct{ Left, Top, Right, Bottom int32 }

// popupWindowArgs returns --window-position/--window-size args that center a
// popup app window in the current work area (screen minus taskbar), capped at
// the native window's 1180x820 default. nil when the work area cannot be read
// — Chromium's default placement applies.
func popupWindowArgs() []string {
	var rc workRect
	if r, _, _ := procSystemParametersInfoW.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&rc)), 0); r == 0 {
		return nil
	}
	waW, waH := int(rc.Right-rc.Left), int(rc.Bottom-rc.Top)
	if waW < 200 || waH < 200 {
		return nil
	}
	w, h := 1180, 820
	if w > waW*4/5 {
		w = waW * 4 / 5
	}
	if h > waH*4/5 {
		h = waH * 4 / 5
	}
	return []string{
		fmt.Sprintf("--window-position=%d,%d", int(rc.Left)+(waW-w)/2, int(rc.Top)+(waH-h)/2),
		fmt.Sprintf("--window-size=%d,%d", w, h),
	}
}
