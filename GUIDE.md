# AppDeck app folder guide

An app is a folder containing an `app.json` manifest and an entrypoint. The
launcher reads the manifest, builds supported source projects when needed,
and shows the app on its dashboard.

## Static HTML

```text
my-page/
├── app.json
├── index.html
├── styles.css
└── app.js
```

Use `"servedBy": "launcher"` in `app.json`. AppDeck serves the page itself:
there is no separate server, build step, or runtime process. Keep writable
data in browser storage or choose a server-backed app if the page needs to
write files.

## Go

```text
my-app/
├── app.json
├── go.mod
├── main.go
└── web/
```

AppDeck builds the Go module on first start when no matching prebuilt binary
is present. The web server should read `APPDECK_HOST` and `PORT` (or
`APPDECK_PORT`) and bind only to that address. A monorepo may use a
`cmd/<name>/` entrypoint.

## Rust

```text
my-app/
├── app.json
├── Cargo.toml
└── src/main.rs
```

AppDeck can build the matching workspace package when Cargo is installed and
the manifest names its binary. Use `"desktop": true` for a GUI/CLI program
with no web server.

## Python

Python projects need an entrypoint and a dependency declaration such as
`requirements.txt`, `pyproject.toml`, or `setup.py`. A manifest can use
`cmd` to create and run a virtual environment under `APPDECK_DATA`. This
keeps installed packages separate from system Python. Install Python and any
required packages before starting the app if it does not use a generated
setup command.

## Prebuilt programs

Put a binary in the app folder and set `binary` in the manifest. For more
than one platform, use names like `my-app-darwin-arm64` and
`my-app-windows-amd64.exe`. AppDeck looks for the current OS/architecture
variant before trying a plain binary name or building from source.

## Importing a repository

The dashboard's **Import** action can clone supported public repository
layouts. An existing `app.json` is validated, not replaced. Without one,
AppDeck recognizes static HTML, Go, Rust, and certain Python layouts. It
does not run imported code until the user starts the app.

For manifest fields, environment variables, and security guidance, read
[AUTHORING.md](AUTHORING.md).
