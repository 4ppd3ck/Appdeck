//go:build linux

// Menu bar companion on Linux: a freedesktop StatusNotifierItem (SNI) on
// the session bus with a com.canonical.dbusmenu menu — the tray protocol
// spoken by KDE, XFCE, Cinnamon, MATE and GNOME (with the AppIndicator
// extension). The launcher owns no window on Linux, so the tray is a
// standalone bus object and "Open Dashboard" opens an app window like the
// dashboard's own Open buttons. Pure Go over godbus/dbus.
//
// If no session bus or no StatusNotifierWatcher answers, the tray reports
// failure and the launcher keeps its old lifecycle untouched.
package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	sniPath        = "/StatusNotifierItem"
	sniIface       = "org.kde.StatusNotifierItem"
	menuPath       = sniPath + "/Menu"
	menuIface      = "com.canonical.dbusmenu"
	sniWatcher     = "org.kde.StatusNotifierWatcher"
	sniWatcherPath = "/StatusNotifierWatcher"
	trayBusName    = "org.appdeck.Launcher"
)

func init() {
	// No owned window on Linux: both the dashboard and app pages open in a
	// chromeless app window, the same path the dashboard's Open buttons use.
	trayOpenURL = openAppWindow
	trayShowDashboard = func() {
		if trayDashboardURL != "" {
			openAppWindow(trayDashboardURL)
		}
	}
	trayInstallLinux = trayInstallLinuxDBus
}

// sniIcon is one a(iiay) pixmap entry: width, height, ARGB32 bytes.
type sniIcon struct {
	W    int32
	H    int32
	Data []byte
}

// menuLayout is the com.canonical.dbusmenu layout node (ia{sv}av): children
// ride as variants, each wrapping another menuLayout.
type menuLayout struct {
	ID       int32
	Props    map[string]dbus.Variant
	Children []dbus.Variant
}

// menuItemProps is one a(ia{sv}) entry for GetGroupProperties.
type menuItemProps struct {
	ID    int32
	Props map[string]dbus.Variant
}

// linuxTray serves the org.kde.StatusNotifierItem object (properties only —
// the interface has no methods) and owns the menu model.
type linuxTray struct {
	mu     sync.Mutex
	ids    map[int32]func()
	rev    uint32
	layout menuLayout
}

// rebuild renders the cached tray snapshot into the menu layout. Called on
// every AboutToShow — menu opens are as fresh as the 5 s snapshot.
func (t *linuxTray) rebuild() {
	snap := traySnapshotCopy()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ids = map[int32]func(){}
	children := []dbus.Variant{}
	next := int32(10)

	add := func(id int32, label string, fn func(), enabled bool) {
		if fn != nil {
			t.ids[id] = fn
		}
		children = append(children, dbus.MakeVariant(menuLayout{
			ID: id,
			Props: map[string]dbus.Variant{
				"label":   dbus.MakeVariant(label),
				"enabled": dbus.MakeVariant(enabled),
			},
		}))
	}
	addSep := func() {
		children = append(children, dbus.MakeVariant(menuLayout{
			ID:    next,
			Props: map[string]dbus.Variant{"type": dbus.MakeVariant("separator")},
		}))
		next++
	}

	t.ids[2] = func() { trayShowDashboard() }
	add(2, "Open Dashboard", t.ids[2], true)
	next = 3
	// Search: dbusmenu has no text input, so the item opens the dashboard's
	// ⌘K quick switcher (#palette auto-opens it on load).
	add(next, "Search apps…", func() { traySearchApps() }, true)
	next++

	// Apps ▸ bounded submenu: running apps (click = start-when-stopped, then
	// open), capped so a 50-app deck cannot fill the screen; the ◐ filter
	// flips it to the whole deck. dbusmenu renders the filter item as a
	// checkmark via toggle-type/toggle-state.
	addSep()
	filterID := next
	next++
	t.ids[filterID] = func() {
		trayRunningOnly.Store(!trayRunningOnly.Load())
		go trayRefresh()
	}
	filterState := int32(0)
	if snap.RunningOnly {
		filterState = 1
	}
	appsKids := []dbus.Variant{dbus.MakeVariant(menuLayout{
		ID: filterID,
		Props: map[string]dbus.Variant{
			"label":        dbus.MakeVariant("Show Running Only"),
			"enabled":      dbus.MakeVariant(true),
			"toggle-type":  dbus.MakeVariant("checkmark"),
			"toggle-state": dbus.MakeVariant(filterState),
		},
	})}
	pool := trayAppsPool(snap)
	const maxRows = 10
	for i, ta := range pool {
		if i >= maxRows {
			id := next
			next++
			more := len(pool) - maxRows
			t.ids[id] = func() { traySearchApps() }
			appsKids = append(appsKids, dbus.MakeVariant(menuLayout{
				ID: id,
				Props: map[string]dbus.Variant{
					"label":   dbus.MakeVariant(fmt.Sprintf("+%d more…", more)),
					"enabled": dbus.MakeVariant(true),
				},
			}))
			break
		}
		id := next
		next++
		ta := ta
		t.ids[id] = func() { go trayLaunchApp(ta.ID, ta.URL, ta.Running) }
		appsKids = append(appsKids, dbus.MakeVariant(menuLayout{
			ID: id,
			Props: map[string]dbus.Variant{
				"label":   dbus.MakeVariant(trayRowLabel(ta)),
				"enabled": dbus.MakeVariant(true),
			},
		}))
	}
	if len(pool) == 0 {
		id := next
		next++
		appsKids = append(appsKids, dbus.MakeVariant(menuLayout{
			ID: id,
			Props: map[string]dbus.Variant{
				"label":   dbus.MakeVariant("No apps running"),
				"enabled": dbus.MakeVariant(false),
			},
		}))
	}
	appsID := next
	next++
	children = append(children, dbus.MakeVariant(menuLayout{
		ID: appsID,
		Props: map[string]dbus.Variant{
			"label":            dbus.MakeVariant("Apps"),
			"enabled":          dbus.MakeVariant(true),
			"children-display": dbus.MakeVariant("submenu"),
		},
		Children: appsKids,
	}))

	// Quick actions from the apps' manifests (trayToggle): one item per
	// running toggle app; a failed probe renders as a disabled row instead
	// of a toggle that lies.
	if len(snap.Toggles) > 0 {
		addSep()
		for _, tg := range snap.Toggles {
			id := next
			next++
			if tg.Err != "" {
				add(id, tg.Label+" — "+tg.Err, nil, false)
				continue
			}
			verb, pause := "Pause", true
			if tg.Paused {
				verb, pause = "Resume", false
			}
			tg := tg
			add(id, verb+" "+tg.Label, func() { go trayRunToggle(tg.ID, pause) }, true)
		}
	}
	addSep()
	add(201, "Quit AppDeck", func() { trayQuit() }, true)

	t.layout = menuLayout{
		ID: 0,
		Props: map[string]dbus.Variant{
			"children-display": dbus.MakeVariant("submenu"),
		},
		Children: children,
	}
	t.rev++
}

// actionFor looks a menu id up; unknown ids are a no-op.
func (t *linuxTray) actionFor(id int32) func() {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ids[id]
}

func (t *linuxTray) currentLayout() (uint32, menuLayout) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rev, t.layout
}

func (t *linuxTray) itemProps(id int32) (map[string]dbus.Variant, bool) {
	_, root := t.currentLayout()
	if id == 0 {
		return root.Props, true
	}
	for _, v := range root.Children {
		var child menuLayout
		if dbus.Store([]interface{}{&child}, v.Value()) != nil {
			continue
		}
		if child.ID == id {
			return child.Props, true
		}
	}
	return nil, false
}

// trayPixmap renders the AppDeck rocket mark as ARGB32 pixels.
// Byte order matches the dominant readers (QImage ARGB32): B, G, R, A.
func trayPixmap() []sniIcon {
	const size = 24
	data := make([]byte, 0, size*size*4)
	inside := func(x, y float64, points [][2]float64) bool {
		in := false
		for i, j := 0, len(points)-1; i < len(points); j, i = i, i+1 {
			xi, yi := points[i][0], points[i][1]
			xj, yj := points[j][0], points[j][1]
			if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
				in = !in
			}
		}
		return in
	}
	body := [][2]float64{{12, 22}, {17, 15}, {16, 7}, {12, 3}, {8, 7}, {7, 15}}
	leftFin := [][2]float64{{8, 12}, {2, 7}, {6, 17}, {9, 15}}
	rightFin := [][2]float64{{16, 12}, {22, 7}, {18, 17}, {15, 15}}
	flame := [][2]float64{{9, 5}, {12, 1}, {15, 5}, {12, 3}}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			inMark := inside(fx, fy, body) || inside(fx, fy, leftFin) ||
				inside(fx, fy, rightFin) || inside(fx, fy, flame)
			inWindow := (fx-12)*(fx-12)+(fy-13)*(fy-13) <= 2.2*2.2
			if inMark && !inWindow {
				data = append(data, 0, 0, 0, 0xFF)
			} else {
				data = append(data, 0, 0, 0, 0)
			}
		}
	}
	return []sniIcon{{W: size, H: size, Data: data}}
}

// sniProps serves org.freedesktop.DBus.Properties for the item object.
type sniProps struct{}

var sniPropertyValues = map[string]dbus.Variant{
	"Category":   dbus.MakeVariant("ApplicationStatus"),
	"Id":         dbus.MakeVariant("appdeck"),
	"Title":      dbus.MakeVariant("AppDeck"),
	"Status":     dbus.MakeVariant("Active"),
	"IconName":   dbus.MakeVariant(""),
	"IconPixmap": dbus.MakeVariant(trayPixmap()),
	"Menu":       dbus.MakeVariant(dbus.ObjectPath(menuPath)),
}

func (p *sniProps) Get(iface, prop string) (dbus.Variant, *dbus.Error) {
	if iface == sniIface {
		if v, ok := sniPropertyValues[prop]; ok {
			return v, nil
		}
	}
	return dbus.MakeVariant(nil),
		dbus.MakeFailedError(fmt.Errorf("unknown property %s.%s", iface, prop))
}

func (p *sniProps) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface == sniIface {
		return sniPropertyValues, nil
	}
	return nil, dbus.MakeFailedError(fmt.Errorf("unknown interface %s", iface))
}

func (p *sniProps) Set(iface, prop string, v dbus.Variant) *dbus.Error {
	return nil // read-only tray properties
}

// menuProps serves the menu object's properties (Version, Status, …).
type menuProps struct{}

func (p *menuProps) Get(iface, prop string) (dbus.Variant, *dbus.Error) {
	if iface == menuIface {
		switch prop {
		case "Version":
			return dbus.MakeVariant(uint32(3)), nil
		case "Status":
			return dbus.MakeVariant("normal"), nil
		}
	}
	return dbus.MakeVariant(nil),
		dbus.MakeFailedError(fmt.Errorf("unknown property %s.%s", iface, prop))
}

func (p *menuProps) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface == menuIface {
		return map[string]dbus.Variant{
			"Version": dbus.MakeVariant(uint32(3)),
			"Status":  dbus.MakeVariant("normal"),
		}, nil
	}
	return nil, dbus.MakeFailedError(fmt.Errorf("unknown interface %s", iface))
}

func (p *menuProps) Set(iface, prop string, v dbus.Variant) *dbus.Error {
	return nil
}

// linuxMenu serves com.canonical.dbusmenu.
type linuxMenu struct {
	t *linuxTray
}

// AboutToShow always rebuilds and answers true: hosts refetch the layout,
// so every menu open reflects the latest snapshot.
func (m *linuxMenu) AboutToShow(id int32) (bool, *dbus.Error) {
	m.t.rebuild()
	return true, nil
}

func (m *linuxMenu) GetLayout(parentID int32, recursionDepth int32,
	propertyNames []string) (uint32, menuLayout, *dbus.Error) {
	rev, root := m.t.currentLayout()
	return rev, root, nil
}

func (m *linuxMenu) GetGroupProperties(ids []int32, propertyNames []string) ([]menuItemProps, *dbus.Error) {
	out := []menuItemProps{}
	for _, id := range ids {
		if props, ok := m.t.itemProps(id); ok {
			out = append(out, menuItemProps{ID: id, Props: props})
		}
	}
	return out, nil
}

func (m *linuxMenu) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	if props, ok := m.t.itemProps(id); ok {
		if v, ok := props[name]; ok {
			return v, nil
		}
	}
	return dbus.MakeVariant(nil),
		dbus.MakeFailedError(fmt.Errorf("no property %s on item %d", name, id))
}

// Event dispatches clicks. Everything runs off the bus thread — the menu
// model itself is only touched under its own lock.
func (m *linuxMenu) Event(id int32, eventID string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if eventID != "clicked" {
		return nil
	}
	if fn := m.t.actionFor(id); fn != nil {
		go fn()
	}
	return nil
}

// trayInstallLinuxDBus wires the whole SNI object graph onto the session
// bus and registers with the StatusNotifierWatcher. Bounded: a missing or
// hung watcher costs at most ~3 s and simply means "no tray".
func trayInstallLinuxDBus() bool {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	t := &linuxTray{ids: map[int32]func(){}}
	t.rebuild()
	menu := &linuxMenu{t: t}

	if err := conn.Export(t, sniPath, sniIface); err != nil {
		return false
	}
	if err := conn.Export(menu, menuPath, menuIface); err != nil {
		return false
	}
	if err := conn.Export(&sniProps{}, sniPath, "org.freedesktop.DBus.Properties"); err != nil {
		return false
	}
	if err := conn.Export(&menuProps{}, menuPath, "org.freedesktop.DBus.Properties"); err != nil {
		return false
	}
	_ = conn.BusObject().Call("org.freedesktop.DBus.RequestName", 0,
		trayBusName, uint32(4)) // DO_NOT_QUEUE; best effort — unique names work

	watcher := conn.Object(sniWatcher, sniWatcherPath)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call := watcher.CallWithContext(ctx, sniWatcher+".RegisterStatusNotifierItem",
		0, sniPath)
	if call.Err != nil {
		// A named reason beats a silent no-tray: hosts without a session
		// bus answer "ServiceUnknown", a hung watcher times out — both
		// simply mean the old lifecycle stays.
		fmt.Fprintf(os.Stderr, "  launcher: tray not installed — %v\n", call.Err)
		return false
	}
	// conn is intentionally leaked: the tray lives for the process's life.
	return true
}
