# Authoring AppDeck apps

AppDeck is a launcher, not a framework: an app is any folder that carries an
**`app.json`** manifest plus its entrypoint. Drop the folder into the deck's
`Apps/` container (or install a zip from the dashboard) and the app appears
on the dashboard — no registration, no restart of your dev loop beyond the
launcher's own.

Quick start:

```sh
appdeck new "My App"                 # Go template (full contract demo)
appdeck new "My Page" --kind=html    # static page hosted in-process
```

The scaffold lands in the deck's `Apps/` container. Start the launcher; the
app is discovered, built (Go and Rust apps build on first start, Python apps
set up a venv on first start) and shown.

**[GUIDE.md](GUIDE.md)** is the quick map of folder skeletons per language —
static HTML, Go, Rust, Python, and prebuilt binaries — and what the launcher
generates when a git-imported repository has no `app.json` of its own.

---

## Installing & importing (the dashboard's 📥 button)

One endpoint — `POST /api/import` — accepts every source and normalizes all
of them into the same outcome: a validated app folder under `Apps/`. Nothing
is executed at import time; the folder either works or is removed again.

| Source | How | Notes |
|---|---|---|
| Archive (zip/tar.gz/tar/rar) | upload, or `?url=` | must carry `app.json` at its root (one wrapping folder is stripped); an archive whose only entry is another archive is unwrapped first (mpv's macOS release zips nest a tar.gz); a `.rar` is read with the machine's bsdtar (built into Windows/macOS), 7-Zip or unrar |
| macOS `.dmg` | upload, or `?url=` | darwin only: the `.app` bundle is imported **whole** — manifest, resources, frameworks and code signature — and `binary` points at `X.app/Contents/MacOS/X`. A dmg with several `.app`s is refused as ambiguous |
| Portable `.exe`/binary | upload with `?name=` + `?appname=` | wrapped in place — **never executed**; `?kind=auto` (default) sniffs the binary for web-server markers, or force `?kind=desktop` / `?kind=web&port=N`; `?args=` space-separated |
| Installer `.exe` | detected by content (NSIS/Inno/MSI/name) | **refused** by default; `&silent=1` (Windows only) attempts a silent install *confined to the app folder* — NSIS `/S /D=`, Inno `/VERYSILENT /DIR=`; anything else is refused with "import a portable build" |
| Single `.html` page | upload + `?appname=` | becomes a `servedBy: launcher` static app |
| Local folder | `?folder=/path` | folder must carry a valid `app.json`; copied (not linked) into the container |
| Git repository | `?url=https://github.com/user/repo` | cloned shallow with the machine's git; a missing `app.json` is generated from the layout (Rust, Go, Python, Node, Docker Compose, or static) and a missing icon is taken from the repo or generated. A source zip with no executable but a root `package.json` imports the same way |
| Git monorepo folder | `?url=https://github.com/user/repo/tree/main/Folder` | clones the repo but installs only that folder as the app (same generation rules) |
| URL | `?url=https://…` | downloaded (512 MB cap) and routed by sniffed content |

Rules that hold for every source: the app lands in `Apps/<id>/` — there is no
install path to ask about; ids must be unique (remove the old folder first);
archives are zip-slip-guarded, symlink-free and size-capped; and an import
that fails validation never leaves a half-installed folder behind. Imports
that carry no `app.json` (a wrapped program, a dmg, an `.app` bundle in a
tar or a nested archive) generate one and say so in the import log.

**The import log** is live in the dashboard's import dock and persisted to
`logs/imports.log` in the launcher data directory (beside `appdeck.log` and
the per-app logs) once the import finishes — every line, the ok/FAILED
verdict, every command that ran (hdiutil mount, the codesign verify and
re-sign invocations, the rar extractor), every staging path and every
cleanup. Success renders green in the dock, failure red; `WARNING:` lines
stay amber either way. A stuck import can always be aborted with the dock's
**■ Cancel** button (git clone and downloads die at once, later steps bail
out), and a **no-progress watchdog** cancels an import that logs nothing for
the dialog's timeout (2/5/10/30 minutes or no limit; default 5) — git's own
clone counters and download milestones stream into the log, so only a
genuinely silent import trips it.

## Code signatures (macOS)

A Mach-O program's code signature seals more than the file: for a `.app`
bundle it covers `Info.plist`, `Resources/` and `Frameworks/` too. Lifting
the bare executable out of its bundle — or re-packing it through a format
that drops metadata — breaks that seal, and macOS kills the process at
launch (SIGKILL, empty log; nothing the launcher can catch). Windows and
Linux have no launch-time equivalent, so everything below is darwin-only
and a no-op on other decks.

Import therefore:

1. **Preserves bundles.** A dmg's `.app` is copied in whole, symlinks and
   permission bits included; a zip or tar/7z archive whose program sits
   inside a `.app/` folder gets the same treatment (the tar path picks the
   largest direct `Contents/MacOS` child, not the largest executable —
   bundles ship dylibs bigger than the program). The manifest's `binary`
   points into the bundle (`X.app/Contents/MacOS/X`) — a normal nested path
   in the app folder.
2. **Verifies what landed.** `codesign --verify --strict` runs on the
   imported program. A broken seal is repaired on the spot with an ad-hoc
   re-signature (`codesign --force --sign -`): the app runs, but macOS no
   longer attributes it to its developer.
3. **Records the outcome** on the manifest as `"signature": "vendor"`
   (developer signature verified), `"adhoc"` (re-signed by the import) or
   `"invalid"` (nothing verified — macOS may still kill it; re-import the
   original `.dmg`). The dashboard card shows a 🔏/✍/⚠ badge for exactly
   that state, so a macOS security prompt about the app later has context.
   Import-log lines carrying `WARNING:` render amber even in an otherwise
   green import.

---


```
my-app/
  app.json          the manifest (below)
  index.html        static app (servedBy: "launcher") …
  … or …
  main.go / go.mod  Go app (built automatically on first start)
  … or …
  Cargo.toml        Rust app (built automatically on first start: the
                    workspace package producing the binary named in
                    "binary" is built with cargo build --release)
  … or …
  requirements.txt + main.py   Python app (a self-setting-up cmd is
                    generated when a git import has no app.json: venv
                    under APPDECK_DATA, dependencies installed on first
                    start — see GUIDE.md)
  my-app-windows-amd64.exe, my-app-darwin-arm64, …   per-arch binaries (optional)
  icon.png          dashboard and macOS permission-bundle icon (optional; emoji fallback)
```

Imports never carry repository or editor baggage: `.git`, `.github`,
`.gitignore`, `.gitattributes`, `.cargo`, `.vscode`, `.idea`, `.DS_Store`,
`Thumbs.db` and friends are filtered out of every import path (folder copy,
archive extraction, git clone).

The launcher looks for a platform-suffixed binary first
(`<binary>-<goos>-<goarch>[.exe]`, e.g. `myapp-windows-amd64.exe`), then the
plain name, then — when the folder is a Go module or a Cargo workspace — it
builds one. Ship prebuilt binaries for machines without Go or Rust; ship
only sources for developers.

## app.json reference

```json
{
  "apiVersion": 1,
  "id": "my-app",
  "name": "My App",
  "icon": "🧩",
  "desc": "One line for the dashboard card",
  "category": "Tools",
  "port": 0,
  "urlPath": "/",
  "network": false,
  "binary": "my-app",
  "args": ["-port", "{port}", "-host", "{host}"],
  "env": { "MYAPP_MODE": "local" },
  "desktop": false,
  "servedBy": ""
}
```

| Field         | Required | Meaning |
|---------------|----------|---------|
| `apiVersion`  | no (default 1) | Contract version. A launcher that speaks a different version refuses the manifest with a named error instead of guessing. |
| `id`          | yes | Stable identifier: `^[a-z0-9][a-z0-9-]{0,62}$`. Used as registry key, log file name and install folder name. Never rename after release. |
| `name`        | yes | Dashboard card title. |
| `icon`        | no | Emoji fallback; a folder `icon.png`/`icon.svg` wins. PNG artwork is also used for the app's macOS permission-bundle icon. |
| `desc`        | no | One-liner under the title. |
| `category`    | no | Dashboard grouping (default `Tools`). |
| `port`        | no | `0` (default) = the launcher assigns a stable port from its reserved range and remembers it. Otherwise 1024–65535. |
| `urlPath`     | no | Path opened by the dashboard (default `/`). |
| `network`     | no | **Loopback by default.** `true` shares the app over the LAN/Wi-Fi and shows its LAN URL. Leave false unless the app is meant to be reached from other devices. This is only the default — the deck's operator can flip it per app any time by dragging the card between the dashboard's two sections (saved in the launcher's data dir, not here). |
| `version`     | no | Display-only string shown as a chip on the app card (e.g. `"1.2"`). |
| `requires`    | no | Prerequisite tool ids this app needs — any of `go`, `rust`, `git`, `python`, `node`, `docker`, `nginx`. Shown against the 🧰 Prerequisites tab so the operator can install or register them. |
| `keepAlive`   | no | `true` = the launcher restarts the app's process when it dies (gives up after 5 consecutive crashes). |
| `binary`      | one of binary/cmd/servedBy | Executable, relative to the app folder. Must not reach outside it (`..` and absolute paths are rejected). |
| `args`        | no | Arguments for `binary`; `{port}` and `{host}` placeholders are substituted. |
| `cmd`         | one of | Shell command line instead of a binary (same placeholders). Valid for `desktop: true` too — a portless app launched by command (what generated Python apps use). |
| `env`         | no | Extra environment entries for the app process. |
| `desktop`     | no | `true` for a portless GUI/CLI app: the card tracks the process, there is no Open/Stop-port machinery, and a running card gets a 🖥 Show button that focuses the window. Requires `binary` or `cmd`. When a central `apps.json` entry omits it, the folder's own `app.json` value wins — the flag is structural, not presentation. |
| `servedBy`    | one of | `"launcher"` hosts a static HTML app inside the launcher process — no child process, no build. |
| `signature`   | no | Set by the importer on macOS decks only — `"vendor"`, `"adhoc"` or `"invalid"` (see *Code signatures* above). Hand-written manifests should leave it out; unknown values are dropped. |

Invalid manifests do not break the deck: the app is skipped and the problem
shows up as a warning in the dashboard (and on `/api/health`).

## The runtime contract (what your app can rely on)

The launcher injects these environment variables into every app process:

| Variable | Meaning |
|----------|---------|
| `PORT` / `APPDECK_PORT` | The port to serve on. |
| `APPDECK_HOST` | The host to bind: `127.0.0.1` unless `network: true` (then `0.0.0.0`). |
| `APPDECK_NETWORK` | `"1"` when Wi-Fi sharing is on. |
| `APPDECK_APP_ID` | This app's stable id. |
| `APPDECK_DATA` | This app's own writable directory (`<data>/appdata/<id>`). **All user data goes here** — never next to the binary (macOS bundles and restricted Windows installs forbid it, and updates may replace app files). |
| `APPDECK_TOKEN` | A fresh per-start secret. API clients authenticate with the `X-AppDeck-Token` header. |
| `APPDECK_PYTHON` | The highest-versioned `python3` found on the machine (Homebrew, python.org, pyenv, PATH — newest first). Python apps' venv convention is `${APPDECK_PYTHON:-python3}` so a double-clicked launcher never pins a venv to the CLT's 3.9 stub. |
| `APPDECK_LAUNCHER_URL` | The dashboard URL, for apps that link back. |

### Security rules every app should follow

1. **Bind loopback by default.** The launcher hands you `APPDECK_HOST` —
   use it. A wider bind is a deliberate choice (`network: true`), not an
   accident of a framework default.
2. **Check the `Host` header** when `APPDECK_NETWORK` is not `"1"`
   (accept only `127.0.0.1:<port>`, `localhost:<port>`, `[::1]:<port>`).
   That is the browser-compatible defense against DNS-rebinding pages
   attacking your localhost port. The `go` template implements it.
3. **Require `X-AppDeck-Token` on API routes** when `APPDECK_TOKEN` is set
   (browser page loads cannot send headers — keep pages open, lock down
   `/api/*`). The `go` template implements this too.

The two templates under `templates/` are the reference
implementation of all three rules.

## Distributing an app

- **Archive it**: the dashboard's *Import app* button (or `POST /api/import`)
  takes a `.zip`, `.tar.gz`/`.tgz`, `.tar` or `.rar` whose root holds `app.json`
  (one wrapping folder is fine; Finder's `__MACOSX`/`.DS_Store` junk is
  ignored). Zip and tar are read in-process; a `.rar` is unpacked with the
  machine's bsdtar (the `tar` shipped with Windows 10+/macOS), 7-Zip or unrar
  — nothing extra to install on most machines.
  The launcher detects the format from the bytes, extracts it into the deck's
  `Apps/` container, validates the manifest and discovers it immediately —
  the exec bit of scripts/binaries survives. An existing install with the
  same id is replaced after the launcher stops it if it is running. In a
  single-file release, importing an app already packaged in the executable
  creates a disk override — but only until the next launcher boot: the
  packaged copy then replaces the import wholesale (binary, web assets and
  manifest) and the folder returns to deck management. What the release
  carries is what runs. Imports of ids the deck does not package keep their
  marker permanently and are never touched by updates; an app removed via
  uninstall stays removed.
- **Folder copy**: zipping is only transport — a plain folder with
  `app.json` in the `Apps/` container works the same.
- **Single-file decks**: a release build carries exactly the apps staged by
  `release.sh`. An app opts out of packaging with `"pack": false` in its
  central `apps.json` entry (its folder keeps working in folder mode).
  Installed third-party zips live in the data directory's `Apps/` folder and
  are discovered there even in single-file builds. In all-in-one builds the
  app folder (`<data>/Apps/<id>/`) is the single home for everything — the
  manifest, web assets and the binary; there is no separate binaries cache
  (the legacy `<data>/bin/` is retired automatically on first boot).

## Ports

Portless web apps get a stable port from the launcher's reserved range
(24500–25499 — below every platform's ephemeral range), remembered in
`ports.json`. Pin an explicit `port` only when something outside the deck
must always find the app on the same address; remember that two apps with
the same fixed port cannot run at the same time, while auto-assigned apps
never collide.

## Updates, data & uninstall (2026-09 additions)

- **`"hidden": true` in app.json** — the folder stays in `Apps/` but the deck
  skips it entirely (discovery is silent, no warning). Use for apps you keep
  around but do not offer.
- **`"version": "1.0"` in app.json** — shown on the app card; consumed by the
  app-replace flow.
- **APPDECK_DATA (runtime contract)** — every started app receives
  `APPDECK_DATA` pointing at its own per-user data directory
  (`%LOCALAPPDATA%\AppDeck\appdata\<id>` on Windows). Write databases, state
  and user files there: that directory survives app updates and is never
  touched when an app is replaced or uninstalled.
- **Uninstall** — the dashboard can remove imported apps:
  stop → delete the app folder → optionally delete the appdata directory.
  Apps listed in apps.json uninstall the same way: the launcher removes
  their entry there first (folder builds), then deletes the folder. A
  single-file build's payload cannot be edited, so the removal is recorded
  instead and the app stays restorable from ⚙ Deck tools.
- **Expose to the OS (🔗 on every card)** — makes an installed app findable
  outside the deck: a symlink in `/Applications` pointing at the app's
  `.app` bundle on macOS (Spotlight, Finder, Launchpad), a Start-menu
  shortcut under `Programs\AppDeck` on Windows (icon from the app's
  `icon.png`, rendered to a `.ico`), a `.desktop` entry in
  `~/.local/share/applications` on Linux. On macOS a bare binary is wrapped
  into its own `.app` first — the same wrapper bundle the elevation feature
  builds, so the two share one bundle and never override each other; a
  vendor bundle from the import is linked as-is. While a link is active,
  Start launches the app through that bundle (Launch Services); elevation
  keeps precedence. The deck keeps owning the real folder — updates swap
  the container contents in place and the artifact keeps pointing at the
  same path, uninstall removes it, and an existing non-deck item at the
  target path is never touched. Apps launched standalone this way run
  without the launcher's environment (ports, `APPDECK_DATA`), so it suits
  self-contained apps (players, editors, GUI tools).
- **Ports** — new apps.json entries should omit `"port"`: the launcher
  assigns the next free port and remembers it in ports.json. Fixed ports act
  as overrides only.
