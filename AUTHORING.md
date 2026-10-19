# Build an app for AppDeck

This guide is for people adding their own app to AppDeck. To run the apps
already in this workspace, start with the [AppDeck README](README.md).

## Quick start

1. Install Go 1.27 or newer if you are building the launcher or a Go app.
2. From the `Appdeck/` folder, create a starter project:

   ```sh
   go run . new "My App"
   ```

   To create a static HTML app instead:

   ```sh
   go run . new "My Page" --kind=html
   ```

3. Start AppDeck with `go run .` from the same folder.
4. Find the new card in the dashboard and select **Start**.

An AppDeck app is a folder with an `app.json` manifest and a supported
entrypoint. Put the folder under the launcher's `Apps/` directory. The
launcher can also import an app folder or archive from its dashboard.

## Choose an app type

| Type | Minimum files | How AppDeck runs it |
|---|---|---|
| Static page | `app.json`, `index.html` | Serves the files inside AppDeck; no build or child process |
| Go web or command app | `app.json`, `go.mod`, Go entrypoint | Builds with Go on first start unless a matching binary is present |
| Rust app | `app.json`, `Cargo.toml`, `src/main.rs` | Builds with Cargo when the matching Rust toolchain is available |
| Python app | `app.json`, Python entrypoint and dependency file | Runs the declared command; generated imports create a venv in app data |
| Prebuilt app | `app.json` and platform binary | Runs the binary matching the current OS and CPU architecture |

For the exact folder layouts and import behavior, see [GUIDE.md](GUIDE.md).

## Manifest basics

Create an `app.json` next to the app entrypoint. A small web-app example:

```json
{
  "apiVersion": 1,
  "id": "my-app",
  "name": "My App",
  "desc": "A short explanation",
  "port": 0,
  "network": false,
  "binary": "my-app",
  "args": ["-port", "{port}", "-host", "{host}"]
}
```

The `id` is a stable lowercase key (`^[a-z0-9][a-z0-9-]{0,62}$`); do not
change it after distributing the app. `name` is shown on the dashboard.
`port: 0` asks AppDeck to choose and remember a free port. Set `network:
false` unless you intentionally want other devices on the local network to
reach the app.

Use exactly one entrypoint field:

- `binary`: an executable inside the app folder; `args` are optional.
- `cmd`: a command line for apps that need a shell or interpreter.
- `servedBy: "launcher"`: a static HTML app served by AppDeck.

Set `desktop: true` for a GUI or command-line program that does not serve a
web page. Other optional fields include `icon`, `category`, `urlPath`,
`env`, `requires`, `keepAlive`, `hidden`, and `version`. Invalid manifests
are skipped with a dashboard warning rather than stopping AppDeck.

## Runtime environment

AppDeck passes these values to every child process:

| Variable | Purpose |
|---|---|
| `PORT` and `APPDECK_PORT` | Assigned app port |
| `APPDECK_HOST` | Bind address selected by the app's network setting |
| `APPDECK_NETWORK` | `1` when network sharing is enabled |
| `APPDECK_APP_ID` | Stable manifest id |
| `APPDECK_DATA` | App-specific writable data directory |
| `APPDECK_TOKEN` | Fresh per-start token for protected API requests |
| `APPDECK_LAUNCHER_URL` | URL of the AppDeck dashboard |

Store databases, settings, and user-generated files in `APPDECK_DATA`, not
beside the executable. That folder survives app updates. A Go web app should
listen on `APPDECK_HOST` and `PORT`, not on a hard-coded public interface.

## Security checklist

- Bind to loopback by default. Enable Wi-Fi sharing only when needed.
- When not shared, validate the request `Host` header to reduce DNS-rebinding
  risks.
- Protect state-changing API routes with `X-AppDeck-Token` when using
  `APPDECK_TOKEN`.
- Never put credentials in a manifest, screenshot, log, or tracked file.
- Do not execute uploaded files during import. The launcher validates and
  installs them; the user starts the app separately.

The Go and HTML templates are the reference examples for AppDeck's local
network and API conventions.

## Import, update, and remove

The dashboard's **Import** action accepts supported folders, archives,
single HTML files, portable binaries, and repository URLs. Archives need
`app.json` at their root (a single outer directory is accepted). The importer
checks paths and rejects unsupported or malformed input.

Keep an app's id stable when updating it. AppDeck replaces an imported app
with the same id after stopping it if needed. It does not remove the app's
`APPDECK_DATA` directory during an update. When removing an app, decide
separately whether its saved data should also be removed.

Apps included in an all-in-one release are controlled by the release build's
packaging configuration. Apps copied into the normal `Apps/` folder continue
to work in development mode.
