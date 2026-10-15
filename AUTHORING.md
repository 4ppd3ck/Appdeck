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
app is discovered, built (Go apps build on first start) and shown.

---

## Installing & importing (the dashboard's 📥 button)

One endpoint — `POST /api/import` — accepts every source and normalizes all
of them into the same outcome: a validated app folder under `Apps/`. Nothing
is executed at import time; the folder either works or is removed again.

| Source | How | Notes |
|---|---|---|
| Archive (zip/tar.gz/tar/rar) | upload, or `?url=` | must carry `app.json` at its root (one wrapping folder is stripped); a `.rar` is read with the machine's bsdtar (built into Windows/macOS), 7-Zip or unrar |
| Portable `.exe`/binary | upload with `?name=` + `?appname=` | wrapped in place — **never executed**; `?kind=desktop` (default, portless card) or `?kind=web&port=N` (it serves a page); `?args=` space-separated |
| Installer `.exe` | detected by content (NSIS/Inno/MSI/name) | **refused** by default; `&silent=1` (Windows only) attempts a silent install *confined to the app folder* — NSIS `/S /D=`, Inno `/VERYSILENT /DIR=`; anything else is refused with "import a portable build" |
| Single `.html` page | upload + `?appname=` | becomes a `servedBy: launcher` static app |
| Local folder | `?folder=/path` | folder must carry a valid `app.json`; copied (not linked) into the container |
| URL | `?url=https://…` | downloaded (512 MB cap) and routed by sniffed content |

Rules that hold for every source: the app lands in `Apps/<id>/` — there is no
install path to ask about; ids must be unique (remove the old folder first);
archives are zip-slip-guarded, symlink-free and size-capped; and an import
that fails validation never leaves a half-installed folder behind.

---


```
my-app/
  app.json          the manifest (below)
  index.html        static app (servedBy: "launcher") …
  … or …
  main.go / go.mod  Go app (built automatically on first start)
  my-app-windows-amd64.exe, my-app-darwin-arm64, …   per-arch binaries (optional)
  icon.png          dashboard card icon (optional; emoji fallback)
```

The launcher looks for a platform-suffixed binary first
(`<binary>-<goos>-<goarch>[.exe]`, e.g. `myapp-windows-amd64.exe`), then the
plain name, then — when the folder is a Go module — it builds one. Ship
prebuilt binaries for machines without Go; ship only `main.go` for
developers.

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
| `icon`        | no | Emoji fallback; a folder `icon.png`/`icon.svg` wins. |
| `desc`        | no | One-liner under the title. |
| `category`    | no | Dashboard grouping (default `Tools`). |
| `port`        | no | `0` (default) = the launcher assigns a stable port from its reserved range and remembers it. Otherwise 1024–65535. |
| `urlPath`     | no | Path opened by the dashboard (default `/`). |
| `network`     | no | **Loopback by default.** `true` shares the app over the LAN/Wi-Fi and shows its LAN URL. Leave false unless the app is meant to be reached from other devices. This is only the default — the deck's operator can flip it per app any time by dragging the card between the dashboard's two sections (saved in the launcher's data dir, not here). |
| `version`     | no | Display-only string shown as a chip on the app card (e.g. `"1.2"`). |
| `keepAlive`   | no | `true` = the launcher restarts the app's process when it dies (gives up after 5 consecutive crashes). |
| `binary`      | one of binary/cmd/servedBy | Executable, relative to the app folder. Must not reach outside it (`..` and absolute paths are rejected). |
| `args`        | no | Arguments for `binary`; `{port}` and `{host}` placeholders are substituted. |
| `cmd`         | one of | Shell command line instead of a binary (same placeholders). |
| `env`         | no | Extra environment entries for the app process. |
| `desktop`     | no | `true` for a portless GUI/CLI app: the card tracks the process, there is no Open/Stop-port machinery. Requires `binary`. |
| `servedBy`    | one of | `"launcher"` hosts a static HTML app inside the launcher process — no child process, no build. |

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
  the exec bit of scripts/binaries survives. An existing install is never
  overwritten — bump the `id` or remove the old folder first. In a
  single-file release build, an app id already packaged inside the launcher's
  own exe is **refused**: the embedded copy always wins (its binary is
  re-extracted on every boot), so the import would only create a duplicate
  folder the launcher ignores. Update those apps by replacing the launcher;
  import a variant under a different app name instead.
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
- **Uninstall** — the dashboard can remove imported (non-registry) apps:
  stop → delete the app folder → optionally delete the appdata directory.
  Apps listed in apps.json are removed by deleting their entry there.
- **Ports** — new apps.json entries should omit `"port"`: the launcher
  assigns the next free port and remembers it in ports.json. Fixed ports act
  as overrides only.
