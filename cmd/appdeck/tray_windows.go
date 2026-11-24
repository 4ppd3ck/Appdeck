//go:build windows

// Menu bar companion on Windows: a notification-area icon (Shell_NotifyIconW)
// with a popup menu, plus a WM_CLOSE subclass on the WebView2 window so the
// red button hides it instead of quitting. Syscall-only — no cgo, matching
// the WebView2 window itself. Everything user-visible builds from the shared
// tray model (tray.go): a fresh menu each right-click, actions off-thread.
package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procCallWindowProcW     = user32.NewProc("CallWindowProcW")
	procSetWindowLongPtrW   = user32.NewProc("SetWindowLongPtrW")
	procSetWindowLongW      = user32.NewProc("SetWindowLongW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procLoadIconW           = user32.NewProc("LoadIconW")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procSetMenuDefaultItem  = user32.NewProc("SetMenuDefaultItem")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
	procShell_NotifyIconW   = shell32.NewProc("Shell_NotifyIconW")
)

const (
	// window messages
	wmClose       = 0x0010
	wmDestroy     = 0x0002
	wmNull        = 0x0000
	wmContextMenu = 0x007B
	wmLButtonUp   = 0x0204
	wmLButtonDbl  = 0x0203
	wmRButtonUp   = 0x0205
	wmUser        = 0x400

	// tray
	nimAdd  = 0
	nimDel  = 2
	nifMsg  = 0x1
	nifIcon = 0x2
	nifTip  = 0x4

	// menu flags / tracking
	mfString    = 0x0
	mfGrayed    = 0x1
	mfChecked   = 0x8
	mfSeparator = 0x800
	tpmRightBtn = 0x2
	tpmReturn   = 0x100

	// show commands
	swHide    = 0
	swRestore = 9

	// fixed menu ids (apps get 100+, quick-action toggles land in acts)
	menuDashboard   = 1
	menuQuit        = 501
	menuRunningOnly = 502
	menuSearch      = 503

	mfPopup = 0x10 // AppendMenuW: the item's "id" is a submenu handle

	gwlWndProc = ^uintptr(3) // GWLP_WNDPROC (-4) as two's complement
)

// Fixed ids are outside the dynamic range.
const menuAppBase = 100

type point struct {
	X, Y int32
}

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  uintptr
	LpszClassName uintptr
	HIconSm       uintptr
}

// notifyIconDataW mirrors winuser.h's NOTIFYICONDATAW with explicit padding
// so the 64-bit layout matches C exactly — the C struct's tail is unused
// (NIF_MESSAGE|NIF_ICON|NIF_TIP only) but cbSize must still be right.
type notifyIconDataW struct {
	CbSize           uint32
	_                uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	_                uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	_                uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	_                uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

var (
	trayMainHwnd  uintptr // the WebView2 window — shown by "Open Dashboard"
	trayMsgHwnd   uintptr // the hidden tray message window
	trayOldProc   uintptr // the WebView2 window's original wndproc
	trayIconAdded bool

	// Callbacks are created once: syscall.NewCallback allocates a slot.
	trayMsgProcCb  = syscall.NewCallback(trayMsgWndProc)
	trayMainProcCb = syscall.NewCallback(trayMainWndProc)
)

func init() {
	// Linux/Windows tray parity: apps open in a chromeless app window (the
	// same path the dashboard's Open buttons take without a native popup),
	// and the dashboard resurfaces through the main window.
	trayOpenURL = openAppWindow
	trayShowDashboard = trayWinShowDashboard
}

// utf16Ptr is syscall.UTF16PtrFromString with our own strings — a NUL can
// never occur in labels the tray renders, so a failure falls back to the
// app name. Convert to uintptr(unsafe.Pointer(...)) AT the syscall call:
// only that expression keeps the buffer alive across the call.
func utf16Ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		p, _ = syscall.UTF16PtrFromString("AppDeck")
	}
	return p
}

func trayWinShowDashboard() {
	if trayMainHwnd == 0 {
		return
	}
	procShowWindow.Call(trayMainHwnd, swRestore)
	procSetForegroundWindow.Call(trayMainHwnd)
}

// trayInstallWindows creates the message window and adds the icon. Runs on
// the main/UI thread (called from runNativeWindow before w.Run()): this
// thread's message pump serves the tray window's wndproc too.
func trayInstallWindows(mainHwnd uintptr) bool {
	trayMainHwnd = mainHwnd
	hInst, _, _ := procGetModuleHandleW.Call(0)

	className, _ := syscall.UTF16PtrFromString("AppDeckTrayWnd")
	wc := wndClassExW{
		LpfnWndProc:   trayMsgProcCb,
		HInstance:     hInst,
		LpszClassName: uintptr(unsafe.Pointer(className)), // className stays in scope below
	}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	hwndMessage := ^uintptr(2) // HWND_MESSAGE (-3)
	trayMsgHwnd, _, _ = procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(className)), 0, 0,
		0, 0, 0, 0, hwndMessage, 0, hInst, 0)
	if trayMsgHwnd == 0 {
		return false
	}

	// The release build carries the deck icon as resource 1 (rsrc syso);
	// dev builds fall back to the stock application icon.
	icon, _, _ := procLoadIconW.Call(hInst, 1)
	if icon == 0 {
		icon, _, _ = procLoadIconW.Call(0, 32512) // IDI_APPLICATION
	}

	var nid notifyIconDataW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = trayMsgHwnd
	nid.UID = 1
	nid.UFlags = nifMsg | nifIcon | nifTip
	nid.UCallbackMessage = wmUser + 1
	nid.HIcon = icon
	tip, _ := syscall.UTF16FromString("AppDeck")
	copy(nid.SzTip[:], tip)
	ok, _, _ := procShell_NotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	trayIconAdded = ok != 0
	return trayIconAdded
}

// trayRemoveIcon deletes the notification icon (window teardown path).
func trayRemoveIcon() {
	if !trayIconAdded || trayMsgHwnd == 0 {
		return
	}
	var nid notifyIconDataW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = trayMsgHwnd
	nid.UID = 1
	procShell_NotifyIconW.Call(nimDel, uintptr(unsafe.Pointer(&nid)))
	trayIconAdded = false
}

// subclassMainWindow intercepts WM_CLOSE on the WebView2 window: with the
// tray, the red button hides the window (quit moves to the tray menu);
// without it, the close flows to the original proc and quits as before.
func subclassMainWindow(hwnd uintptr) {
	if trayOldProc != 0 || hwnd == 0 {
		return
	}
	if err := procSetWindowLongPtrW.Find(); err == nil {
		old, _, _ := procSetWindowLongPtrW.Call(hwnd, gwlWndProc, trayMainProcCb)
		trayOldProc = old
	} else {
		// 32-bit Windows: SetWindowLongW is the pointer-sized variant there
		old, _, _ := procSetWindowLongW.Call(hwnd, gwlWndProc, trayMainProcCb)
		trayOldProc = old
	}
}

// trayMainWndProc is the WebView2 window's new wndproc. Only WM_CLOSE and
// WM_DESTROY are intercepted; everything else passes through untouched.
func trayMainWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmClose:
		if trayEnabled() {
			procShowWindow.Call(hwnd, swHide)
			return 0
		}
	case wmDestroy:
		trayRemoveIcon()
	}
	r, _, _ := procCallWindowProcW.Call(trayOldProc, hwnd, msg, wp, lp)
	return r
}

// trayMsgWndProc serves the hidden tray window: icon clicks open the menu
// (right) or resurface the dashboard (left/double).
func trayMsgWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmUser + 1:
		switch lp {
		case wmRButtonUp, wmContextMenu:
			trayWinShowMenu(hwnd)
		case wmLButtonUp, wmLButtonDbl:
			trayWinShowDashboard()
		}
		return 0
	case wmDestroy:
		trayRemoveIcon()
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

// trayWinBuildMenu renders the cached snapshot. Fixed ids: 1 = dashboard,
// 501 = quit, 502 = the ◐ running-only filter; dynamic rows (Apps submenu,
// quick actions) from 100 land in acts.
func trayWinBuildMenu() (hMenu uintptr, acts map[uintptr]func()) {
	snap := traySnapshotCopy()
	hMenu, _, _ = procCreatePopupMenu.Call()
	acts = map[uintptr]func(){}
	id := uintptr(menuAppBase)

	appendItem := func(label string, fn func(), flags uintptr) uintptr {
		procAppendMenuW.Call(hMenu, flags, id,
			uintptr(unsafe.Pointer(utf16Ptr(label))))
		if fn != nil {
			acts[id] = fn
		}
		id++
		return id - 1
	}
	appendSep := func() {
		procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	}

	appendItem(fmt.Sprintf("AppDeck — %d app(s) running", snap.RunningCount),
		nil, mfGrayed|mfString)
	procAppendMenuW.Call(hMenu, mfString, uintptr(menuSearch),
		uintptr(unsafe.Pointer(utf16Ptr("Search apps…"))))
	procAppendMenuW.Call(hMenu, mfString, uintptr(menuDashboard),
		uintptr(unsafe.Pointer(utf16Ptr("Open Dashboard"))))

	// Apps ▸ bounded submenu: running apps (click = start-when-stopped, then
	// open), capped so a 50-app deck cannot fill the screen; the ◐ filter
	// flips it to the whole deck (checked item at the top).
	{
		sub, _, _ := procCreatePopupMenu.Call()
		filterFlags := uintptr(mfString)
		if snap.RunningOnly {
			filterFlags |= mfChecked
		}
		procAppendMenuW.Call(sub, filterFlags, uintptr(menuRunningOnly),
			uintptr(unsafe.Pointer(utf16Ptr("Show Running Only"))))
		pool := trayAppsPool(snap)
		const maxRows = 10
		for i, ta := range pool {
			if i >= maxRows {
				more := len(pool) - maxRows
				procAppendMenuW.Call(sub, mfString, id,
					uintptr(unsafe.Pointer(utf16Ptr(fmt.Sprintf("+%d more…", more)))))
				acts[id] = func() { go traySearchApps() }
				id++
				break
			}
			ta := ta
			procAppendMenuW.Call(sub, mfString, id,
				uintptr(unsafe.Pointer(utf16Ptr(trayRowLabel(ta)))))
			acts[id] = func() { go trayLaunchApp(ta.ID, ta.URL, ta.Running) }
			id++
		}
		if len(pool) == 0 {
			procAppendMenuW.Call(sub, mfGrayed|mfString, id,
				uintptr(unsafe.Pointer(utf16Ptr("No apps running"))))
			id++
		}
		procAppendMenuW.Call(hMenu, mfString|mfPopup, sub,
			uintptr(unsafe.Pointer(utf16Ptr("Apps"))))
	}

	// Quick actions from the apps' manifests (trayToggle): one item per
	// running toggle app; a failed probe renders as a disabled row instead
	// of a toggle that lies.
	if len(snap.Toggles) > 0 {
		appendSep()
		for _, tg := range snap.Toggles {
			if tg.Err != "" {
				appendItem(tg.Label+" — "+tg.Err, nil, mfGrayed|mfString)
				continue
			}
			verb, pause := "Pause", true
			if tg.Paused {
				verb, pause = "Resume", false
			}
			tg := tg
			appendItem(verb+" "+tg.Label, func() { go trayRunToggle(tg.ID, pause) }, mfString)
		}
	}
	appendSep()
	procAppendMenuW.Call(hMenu, mfString, uintptr(menuQuit),
		uintptr(unsafe.Pointer(utf16Ptr("Quit AppDeck"))))
	procSetMenuDefaultItem.Call(hMenu, uintptr(menuDashboard), 0)
	return hMenu, acts
}

// trayWinShowMenu is the right-click handler. TrackPopupMenu with
// TPM_RETURNCMD returns the chosen id directly; the SetForegroundWindow +
// WM_NULL pair is the documented tray-menu focus quirk.
func trayWinShowMenu(hwnd uintptr) {
	hMenu, acts := trayWinBuildMenu()
	defer procDestroyMenu.Call(hMenu)

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(hwnd)
	sel, _, _ := procTrackPopupMenu.Call(hMenu, tpmRightBtn|tpmReturn,
		uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
	procPostMessageW.Call(hwnd, wmNull, 0, 0)

	switch sel {
	case menuDashboard:
		trayWinShowDashboard()
	case menuSearch:
		go traySearchApps()
	case menuRunningOnly:
		trayRunningOnly.Store(!trayRunningOnly.Load()) // next right-click rebuilds
		go trayRefresh()
	case menuQuit:
		go trayQuit()
	default:
		if fn, ok := acts[sel]; ok {
			go fn()
		}
	}
}
