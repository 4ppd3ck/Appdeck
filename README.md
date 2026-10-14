# AppDeck — Go port

A Go (stdlib-only) launcher — **AppDeck** — that
starts/stops/opens every local web app from one dashboard — and, in its release
form, a **single executable** that carries every app inside it, opens its own
application window, and quits cleanly when that window closes.

| Python source | Go source |
|---|---|
| `../server.py` (581 lines) | `main.go` (+ `sys_windows.go` / `sys_unix.go` for process/syscall bits, `window.go` for window mode, `embed.go` for single-file mode) |

The HTTP surface:

```
GET  /                  dashboard (index.html)
GET  /api/apps          status of every app
POST /api/start   {id}  start one app
                        {id, port} start it on a custom port (1024–65535) —
                        Open/lanUrl/Stop follow the custom port until the app
                        is stopped; a clash with another app's port is named
                        in the error ("port 8145 is already used by …")
POST /api/stop    {id}  stop one app (also stops instances started elsewhere)
POST /api/all/start     start everything that is stopped
POST /api/all/stop      stop everything that is running
GET  /api/logs?id=…     tail of an app's log
GET  /api/icon?id=…     app icon from the app's own folder (icon.png/.svg)
GET  /api/settings?id=… per-app settings document (stored in the data dir)
POST /api/settings      {id, settings} store a per-app settings document (loopback only)
POST /api/install?name=…  legacy alias: install an app archive (zip / tar.gz / tar — raw
POST /api/import?name=…    type-aware app import (archive / .exe / .html / folder / URL — raw
                        bytes, app.json at the root; loopback only); the app
                        is discovered at once
POST /api/network      {id, enabled} flip an app's Wi-Fi sharing — the dashboard's
                        drag between the two sections; loopback only; the choice
                        is saved per app (network.json) and a running app restarts
                        so the new bind host applies at once
POST /api/deck/export   download the whole deck (Apps/, apps.json, network.json)
                        as one zip (loopback only)
POST /api/deck/import   {zip body} restore a deck export — apps merge (existing
                        installs are never overwritten), Wi-Fi overrides combine
                        (loopback only)
GET  /api/health        launcher self-report (LAN URL, build mode, data dir,
                        registry warnings)
POST /api/heartbeat     dashboard keepalive (?bye=1 = page closed; window mode)
POST /api/shutdown      quit the launcher + stop its apps (loopback callers only)
GET  /favicon.ico       204
```

## Authoring apps for AppDeck (third-party developers)

An app is any folder with an **`app.json`** manifest plus its entrypoint —
see **[AUTHORING.md](AUTHORING.md)** for the full contract (fields, injected
environment, security rules, distribution). `appdeck new "My App"` scaffolds
a working app (`--kind=go|html`) into the deck's `Apps/` container; the zip
install on the dashboard takes an archive with `app.json` at its root. Apps
come from three sources, merged by the launcher: the central `apps.json`
registry (wins for its own folders), per-folder `app.json` manifests, and
heuristic discovery of plain `index.html`/Go-module folders. A broken
third-party manifest never takes the deck down — the app is skipped and a
named warning shows in the dashboard.

## Two build modes

**Folder mode** (plain `go build` / `sh build.sh`): the development layout.
Apps live in the `Apps/` group folder next to `Launcher/` (root-level
sibling folders also still work); `apps.json` configures them;
a missing binary is auto-built with `go build`; folders with an `index.html`
or a Go module that appear beside `Launcher/` are discovered automatically.
The dev `appdeck.exe`/`appdeck-console.exe` built by `build.sh` use this
mode. Web apps whose manifest sets `"port": 0` (or discovered apps without
a port) get a stable auto-assigned port from the reserved range
24500–25499, remembered in `ports.json` — below every platform's ephemeral
port range, so assignments never collide with outgoing connections.

**Single-file mode** (`-tags embed`, what `release.sh` builds): every app
binary and web asset is packed inside the executable.

- Startup unpacks binaries (always) and each disk-asset app's files (only if
  missing) into a per-user data dir: `%LOCALAPPDATA%\AppDeck`
  (`~/Library/Application Support/AppDeck` on macOS, `~/.local/share/AppDeck`
  on Linux, `LAUNCHER_DATA_DIR` overrides). Nothing is written next to the exe.
  A pre-rename `ProjectLauncher` data dir is migrated (renamed) automatically
  on first start, so existing installs keep their unpacked apps and user data.
- The 7 plain HTML apps (Cheatsheet, Tarot, Exercise, Astro, MLOPS, Kundli
  Explorer, House Design) are hosted **inside** the launcher process — no
  child process at all; Start/Stop simply opens/closes their port.
- The 7 Go apps run from the unpacked binaries, exactly as before.
- The packaged app registry is generated from a folder scan at build time
  (see "Adding or removing apps"); heuristic folder discovery is
  disabled — the launcher scans only the data directory's `Apps/` folder,
  where installs land. An app opts out of packaging with `"pack": false` in
  its `apps.json` entry (its folder keeps working in folder mode).
- To fully reset a client install: delete the data directory.
  Note: app asset files on disk are only written when missing, so user data
  (Investment's `data.js`, LawBuddy's db) survives launcher updates; delete
  the data dir to refresh app assets after an update.

## The window (release builds)

Release builds (`-tags gui`, Windows adds `-ldflags "-H windowsgui"`) have no
console at all; output goes to `logs/appdeck.log` inside the data dir, and
fatal startup errors (port busy, unpack failure) pop a message box.

**macOS** gets the same application-window experience in **every** build
(`go run .`, dev binary, or release): the dashboard opens as a chromeless
`--app` window of the first Chromium-family browser found (Edge, Chrome,
Chromium, Brave) instead of a tab in your default browser, and closing the
last dashboard window stops the apps AppDeck started and quits — same as on
Windows. `--no-window` (or `NO_OPEN=1`) restores the old browser-tab flow,
`--browser` skips straight to the app window.

The packaged `AppDeck.app` is an **agent app** (`LSUIElement` in its
Info.plist): it never appears in the Dock or the Cmd+Tab switcher. Its visible
face is the dashboard window it opens — without the flag the controller
process would bounce in the Dock once and then sit there windowless, which
reads like the icon "moved" to the browser that owns the dashboard window.

Window order on launch:

1. **Native window** (Windows, default): a real Win32 window hosting WebView2
   (`github.com/jchv/go-webview2`, pure Go — no cgo, runtime preinstalled on
   Windows 10/11). Its close button quits the launcher and stops its apps —
   there is no console process behind it to keep alive.
2. **App window** (macOS always; Windows via `--browser` or when WebView2 is
   unavailable): a chromeless Edge/Chrome/Brave `--app=` window (falls back
   to the default browser tab). In this mode the dashboard heartbeats every
   4 s and closing the last dashboard (or 8 min of silence after a crash)
   stops the launcher and its apps, so closing the window still closes the
   application.
3. `--no-window` / `NO_OPEN=1`: no window; server keeps running (service use).

Dashboards opened on other devices (Wi-Fi link in the header) count as
activity and keep the launcher alive; **Quit** in the dashboard does an
explicit `POST /api/shutdown` (loopback-only — phones can stop apps but not
kill the launcher).

## Opening apps: window or tab

The header's **🪟 Window / 🗂 Browser tab** button chooses where apps open
when you click **Open ↗** (remembered per device in browser storage):

- **Window** (default): each app opens in its own window — a second window of
  the launcher's WebView2 when running natively, a popup window in browsers.
- **Browser tab**: the app opens as a tab in your default browser. From the
  native launcher window (which has no tabs) the launcher hands the URL to
  the default browser itself via `POST /api/open` (loopback-only); from a
  phone or a regular browser it's a plain new tab.

**Closing on stop:** the dashboard keeps a reference to every window/tab it
opened, and **Stop**, **Stop All** and **Quit** close them along with the
app's server. Launcher-hosted HTML apps additionally carry an injected
watchdog (a tiny script in every served page): when their server goes away
the open tab/window closes itself — so even app windows opened before a
dashboard reload get cleaned up. Two honest limits, imposed by browsers:
tabs the launcher sent to your **default browser** (tab mode from the native
window) live in another browser process and can't be closed remotely — they
show a connection error once the app stops; and windows opened by a page
other than the dashboard (typed by hand) are intentionally left alone.

Study Hub used to open a browser tab by itself whenever it started; it no
longer does — the launcher's Open button decides. (Run `studyapp -open` to
restore the standalone auto-open.)

## Windows Firewall prompts ("Allow access?")

Any exe that listens on the LAN (0.0.0.0) triggers Windows Defender
Firewall's Allow/Cancel dialog once per binary — Windows does not allow
skipping this from a non-elevated process. The dashboard's
**🔓 Wi-Fi permission** button (Windows only) collapses all of them into
**one** prompt: it generates a PowerShell script that adds inbound
allow-rules for the launcher and every network app and runs it through a
single UAC elevation (`POST /api/firewall`, loopback-only). Approve once and
no app asks again. Rules are named `AppDeck: <app>` — remove them in
Windows Firewall settings if you ever want the prompts back.

## Icons (all platforms)

- **Windows**: `icon.ico` (rocket on the dashboard gradient) is embedded into
  every build via the `rsrc_windows_{amd64,386,arm64}.syso` files in this
  folder — Explorer, the taskbar and the native window (title bar + taskbar)
  all use it.
- **macOS**: bare Unix executables cannot carry an icon, and Finder treats
  them as unknown documents (the "Siri is unable to open…" error when
  double-clicked from /Applications). `release.sh` therefore wraps the darwin
  binary in a proper `AppDeck.app` bundle — `Info.plist` + `AppDeck.icns` +
  ad-hoc signature — which double-clicks like any Mac app and shows the icon
  in Finder, the Dock and the window. Use the `AppDeck-darwin-<arch>.zip`
  from `release/`, not the bare binary, when installing on a Mac.
- **Linux**: ELF binaries have no icon either; `release.sh` ships an XDG
  `appdeck.desktop` entry + `appdeck.png` next to the binary
  (`AppDeck-linux-<arch>.zip`) so desktop environments list AppDeck in
  application menus with its icon.

To change the icon: edit `tools/makeicon.py`, run `python tools/makeicon.py`
(needs Pillow), then regenerate everything:

    go run github.com/akavel/rsrc@latest -arch amd64 -ico icon.ico -o rsrc_windows_amd64.syso
    go run github.com/akavel/rsrc@latest -arch 386   -ico icon.ico -o rsrc_windows_386.syso
    go run github.com/akavel/rsrc@latest -arch arm64 -ico icon.ico -o rsrc_windows_arm64.syso

    # macOS .icns + Linux PNG (on a Mac, from this folder):
    ICONSET="$(mktemp -d)/AppDeck.iconset" && mkdir -p "$ICONSET" && \
      for s in 16 32 128 256 512; do \
        sips -z $s $s icon-preview.png --out "$ICONSET/icon_${s}x${s}.png" >/dev/null; \
        sips -z $((s*2)) $((s*2)) icon-preview.png --out "$ICONSET/icon_${s}x${s}@2x.png" >/dev/null; \
      done && iconutil -c icns "$ICONSET" -o AppDeck.icns && \
      sips -z 256 256 icon-preview.png --out appdeck-icon.png >/dev/null

(## Adding or removing apps — just folders (then re-releasing))

The app set is **discovered automatically**. `tools/genembedded` scans the
project root at build time (every folder with a `go.mod` or an HTML entry
page, except `Launcher/`) and generates the packaged
registry + build manifest. There is no per-app list to maintain anywhere —
`apps-embedded.json` does not even exist in the repo, it is generated into
the exe on every build.

**Add an app:** create a folder under `Apps/` (the app group folder; a
root-level folder also works) —
- a plain HTML folder (any `.html` at its root) → hosted in-process, or
- a Go module (`go.mod`) → compiled and shipped as a binary; if a root `.go`
  file uses `//go:embed` it ships binary-only, otherwise its web assets
  (root `index.html`, or `templates/ static/ web/ css/ js/ data/` dirs) are
  packaged beside it. Entrypoint is the root `package main`, or the single
  `cmd/<name>/main.go`.

**Remove an app:** delete (or move away) its folder. The rebuilt exe does
not contain it at all — no binary, no assets, no dashboard card.

Then `sh release.sh` → new `appdeck-<os>-<arch>[.exe]` (plus the macOS
`AppDeck.app` and Linux desktop-entry zips). Clients just replace
their exe; stale binaries are pruned from their data dir on next start
(their `apps/<id>/` data folder is kept until the data dir is deleted).

**Customize an app** (name, icon, description, port, open path, Wi-Fi on/off,
custom CLI args): add/edit its entry in **`Launcher/apps.json`** — the one
registry worth maintaining; `"dir": "../<FolderName>"` ties it to the folder.
Folders not listed there still ship with defaults (📦 icon, next free port
from 9100, opens at `/`). An apps.json entry whose folder is gone is dropped
from the release with a printed notice and skipped with a warning in dev.

**Desktop apps** (GUI/CLI binaries with no web server): set `"desktop": true`
and leave the port at 0. The launcher starts the binary as-is (no args unless
given), reports the card running while its state-tracked process is alive,
and Stop kills that process tree — no port probing, no Open button, no Wi-Fi
badge, no firewall rule. ChronoStamp is the worked example.

Desktop apps whose GUI needs cgo (ChronoStamp on darwin/linux) come out
CLI-only from cross-compiled builds — their Gio backends link the OS window
system and cannot cross-compile. `build.sh` (host builds) and `release.sh`
(host-platform target) therefore build such apps natively with cgo, falling
back to a CLI-only build with a printed note when the C toolchain or GUI dev
headers are missing. A CLI-only desktop binary exits immediately when the
launcher starts it GUI-style; the launcher surfaces the app's own output
(e.g. "this build has no graphical interface…") in the error, the card toast
and `logs/<id>.log`, instead of a bare "process exited during startup".

## App icons — from the app's own folder

Every app can carry its icon **in its own folder**: a 256×256 `icon.png`
(or `icon.svg`) at the app's root. The launcher serves it to the dashboard
(`GET /api/icon?id=…`, ETag-cached) and the card shows it instead of the
apps.json emoji, which stays as the fallback. Adding, changing or deleting
an icon never touches `Launcher/` — drop the file in the app folder and
restart (dev) or re-release (clients).

- To regenerate the stock icons (emoji on the AppDeck gradient, sampled from
  `icon-preview.png`): `python3 tools/genicons.py` (missing only) or
  `--force` (all). Needs Pillow; emoji rendering uses the macOS color-emoji
  font.
- In single-file builds the icon ships inside the exe: static apps and
  asset-packaged apps carry it with their assets; binary-only apps get it
  staged explicitly by `release.sh` (`embedded/assets/<id>/icon.png`).

## Autostart, card colors, filter, restart

- **Autostart**: set `"autostart": true` on an `apps.json` entry and that app
  boots with the launcher (0.8 s after start, unless its port is already
  open). `--start-apps` still starts everything. The dashboard marks such
  apps with a small ⚡ auto badge.
- **Card colors**: each card's left bar, icon frame and Start button pick up
  an accent derived from the app's `icon.png` (dominant hue, background
  excluded). Set `"accent": "#aabbcc"` on the entry to override the derived
  color, or remove the icon to fall back to the theme's default accent.
  Derived colors are computed once per launch — restart after changing an
  icon.
- **Filter**: the header's filter box (or pressing `/`) narrows the grid by
  name, category and description; Esc leaves the box.
  The segmented control next to it filters by state — All / ▶ running /
  ■ stopped — and the choice is remembered per device. The segmented control
  next to it filters by state — All / ▶ running / ■ stopped — and the choice
  is remembered per device.
- **Theme**: the header's 🌓 control picks **Auto** (follow the OS light/dark
  setting, live), **Light** or **Dark**; the choice is remembered per device,
  like the open mode. Auto is the default.
- **Restart**: running cards show a ⟳ Restart button — a stop followed by a
  start, using the same API the other buttons use.

**Change ports / descriptions / icons:** edit `apps.json`, rebuild, done.

## Releasing to clients (one file)

```bash
cd Launcher
sh release.sh                    # all default targets
sh release.sh windows/amd64      # one target
```

**Where you can run it:** any of the three desktop OSes — the script is plain
POSIX sh and every target is cross-compiled (`CGO_ENABLED=0`, pure Go), so a
Linux box can produce Windows/Mac exes and vice versa. On Windows run it from
**Git Bash**, or just double-click **`release.cmd`** (same for `build.cmd`);
both need only Go and Git for Windows on PATH. A macOS host additionally
ad-hoc signs the darwin builds.

Targets: `windows/{amd64,arm64,386}`, `linux/{amd64,arm64,arm}`,
`darwin/{amd64,arm64}`.

Output under `Launcher/release/`:

- `appdeck-<os>-<arch>[.exe]` — THE deliverable. ~80 MB, everything inside.
- `appdeck-windows-<arch>.zip` — the Windows exe zipped for distribution
  (~65% smaller); the bare exe ships next to it.
- `AppDeck-darwin-<arch>.zip` — `AppDeck.app` (icon, Info.plist, ad-hoc
  signed): the double-clickable macOS form. Unzip, drag AppDeck.app into
  /Applications. (Bare darwin binaries stay in the list for Terminal use.)
- `AppDeck-linux-<arch>.zip` — binary + `appdeck.desktop` + icon for Linux
  application menus (see the INSTALL.txt inside).
- `SHA256SUMS`.

`release.sh` runs `tools/genembedded` to scan the project root and
`Apps/`, stages
`Launcher/embedded/` (per-target binaries from each app module + assets + the
generated registry), builds with `-tags "gui embed"`, then restores the
placeholder. Misconfigurations degrade gracefully: an app whose binary never
got staged, or whose folder vanished, is skipped with a warning at startup
and never keeps the launcher from booting the rest.

For real distribution: sign the exe (`signtool /fd sha256`) to avoid
SmartScreen warnings; unsigned macOS builds need `xattr -dr
com.apple.quarantine <path>` on first run — or right-click → Open the first
time — after downloading the zip from the internet.

## Dev: build & run

```bash
cd Launcher
sh build.sh --console         # appdeck.exe (GUI dev) + appdeck-console.exe + all app binaries
sh build.sh --app promptpress # build ONE app's exe into its own folder (dev loop:
                              # change code → build → restart the app from the deck;
                              # matches id, folder name or binary name, case-insensitive)
go run .                 # folder mode, console, opens the dashboard
go run . --start-apps          # also boot every stopped app after 0.8 s
go run . --no-open             # don't open anything (or NO_OPEN=1)
go run . --no-window           # macOS too: plain browser tab, no app window
go run . --browser             # even in GUI builds, use an app window not the native one
go run . --kill-apps-on-exit   # Ctrl+C also stops the apps this panel started
PORT=8080 HOST=127.0.0.1 go run .
```

`--kill-apps-on-exit` (off by default in folder mode): on Ctrl+C or closing
the console window, the launcher stops every app *it started* (tracked in
`state.json`) before exiting; apps running without a launcher state entry are
left alone. Windows gives a console ~5 s on window close, so with many apps
running at once a force-closed window may finish only some of the stops — the
dashboard Stop buttons / `POST /api/stop` remain the reliable path.

## Python → Go mapping

| Python (`server.py`) | Go (`main.go` et al.) |
|---|---|
| `ThreadingHTTPServer` + `BaseHTTPRequestHandler.do_GET/do_POST` | `http.Server` + `Server.ServeHTTP`/`route` (raw-path matching, JSON 404 fallback) |
| `_json()` helper (Content-Type/Length, `Cache-Control: no-store`) | `writeJSON()` — same headers, plus `Server: AppDeck/1.1` |
| `APPS = load_apps()` at import, `APP_BY_ID` | `NewServer(cfg)` → `loadApps`, duplicate-id / missing-folder errors abort startup with the same messages |
| `load_state` / `save_state` (tmp file + replace, `indent=1`) | `loadState` / `saveState` (`os.WriteFile` + `os.Rename`) |
| `lan_ip()` (PowerShell → macOS `ipconfig` → UDP 8.8.8.8 trick, 30 s cache) | `lanIP()` — identical command sequence and fallbacks |
| `port_open()` | `portOpen()` (`net.DialTimeout`) |
| `pid_alive()` — Windows `tasklist /FO CSV /NH` parse / POSIX `kill(pid,0)` | `pidAlive` in `sys_windows.go` / `sys_unix.go` |
| `listener_pids()` — Windows `netstat -ano -p tcp` parse / POSIX `lsof` + `_owns_and_listens` | `listenerPIDs` + `ownsAndListens` |
| `proc_cmdline()` — PowerShell `Get-CimInstance` / `ps -o command=` with 58-char truncation | `procCmdline` per platform |
| `subprocess.Popen(cmd.exe /d /s /c …` or `/bin/sh -c …`, `start_new_session`, `CREATE_NEW_PROCESS_GROUP|CREATE_NO_WINDOW`) | `os/exec` + `shellCommand` + `SysProcAttr` per platform (`Setpgid` on POSIX) |
| `_kill_group` / escalation (`taskkill /T /F`, `killpg SIGTERM`→`SIGKILL`) | `killTree` / `killTreeForce` / `killProc` / `killProcForce` |
| `MUTEX` + `INFLIGHT` set | `sync.Mutex` + `inflight` map (`busy` responses preserved) |
| `tail_log(errors="replace", last 150)` | `tailLog` + `splitLines` (UTF-8 replacement, last 150) |
| `PORT` / `HOST` env, default `0.0.0.0:9090` | identical (`DefaultConfig`) |
| `--start-apps`, `--no-open`, `NO_OPEN=1`, `webbrowser.open` | same flags/env, `openBrowser` (cmd/open/xdg-open) |
| banner prints, port-busy exit 1, Ctrl+C farewell | same strings in `main()` |

Response JSON field names are byte-for-byte the same (`ok`, `busy`, `alreadyRunning`, `error`, `pid`, `slow`, `note`, `killed`, `stillRunning`, `status`, `id`, `name`, `icon`, `iconFile`, `accent`, `autostart`, `desc`, `category`, `port`, `url`, `cmd`, `network`, `lanUrl`, `running`, `managed`, `external`, `proc`, `startedAt`). Pointer fields are used so a key is present exactly when the Python dict would include it (e.g. `"killed": []` stays visible, `"lanUrl": null` stays null).

## Path resolution

The Go launcher resolves its files at runtime from the executable directory instead of using a compile-time source path. LAUNCHER_ROOT=/path/to/Launcher overrides the asset root for packaged deployments.

When an app has a `binary` entry but no matching executable for the current
platform, the launcher automatically runs `go build` in that app's module.
Modules with a root `main.go` are built directly; modules using a single
`cmd/<name>/main.go` entrypoint are detected automatically. Existing binaries
are always preferred, so normal startup does not rebuild applications. Binaries
are named `<configured-name>-<GOOS>-<GOARCH>[.exe]`; unqualified binaries are never
used, preventing a binary built for another platform from being executed.

The launcher scans the `Apps/` group folder (and root-level siblings) at
startup. Any folder with an `app.json` manifest is a first-class app
(AUTHORING.md); folders containing only a `go.mod` or an HTML entry page are
still discovered heuristically when not already listed in `apps.json`. Go
modules are built using the same automatic build logic as configured apps;
HTML projects are hosted in-process by the launcher. Portless apps get a
stable auto-assigned port from the reserved 24500–25499 range (remembered in
`ports.json`); `apps.json` is
optional and acts as an override layer for custom metadata, commands, and fixed
ports; configured entries always take priority over discovery.


## Deviations from server.py

- Errors Python would have turned into a handler traceback (log-file open failure, `Popen` spawn failure) return `{"ok": false, "error": …}` with HTTP 500 instead of dropping the connection.
- `HEAD` requests get a JSON 404 (Python's BaseHTTPRequestHandler answered 501 for unsupported methods).
- Log tails are decoded as UTF-8 with replacement characters; Python used the locale default encoding with `errors="replace"`.
- An invalid `Content-Length` header or a save-state failure no longer crashes the request.
- HTTP responses carry Go's default `Date`/`Content-Type` handling; only `Server: AppDeck/1.1` is set explicitly (Python advertised its full server string).
- `{port}` and `{host}` placeholders are supported in `args`/`cmd`/`env` values (`{host}` is `127.0.0.1` unless the app opts into Wi-Fi sharing with `"network": true`).
