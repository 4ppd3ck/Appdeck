# GUIDE — app skeletons per language

The one rule: an AppDeck app is a folder with an **`app.json`** manifest next
to its entrypoint. Everything else is convention the launcher understands.
This guide shows the minimal folder shape for each language, and what happens
when you import a repository that has none of it (the git importer generates
the manifest and icon for you).

Full field reference and the runtime contract (injected env vars, security
rules): **[AUTHORING.md](AUTHORING.md)**.

---

## Static web page

```
my-page/
  app.json        { "id": "my-page", "name": "My Page", "servedBy": "launcher" }
  index.html      the page (any .html works; index.html is the entrypoint)
  …css/js…        loaded relative — the launcher serves the folder in-process
```

- No process, no port of its own, no build. The launcher hosts the folder.
- Import paths: zip the folder · drop it into `Apps/` · upload the single
  `.html` file · `?folder=` · git URL.
- **Generation for repos without app.json:** any repository with a root
  `.html` file imports as a static app.

---

## Go

```
my-app/
  app.json        { "id": "my-app", "name": "My App", "binary": "my-app" }
  go.mod
  main.go         package main — web server (net/http) or GUI/CLI
  my-app-darwin-arm64, my-app-windows-amd64.exe   (optional prebuilts)
```

- **No prebuilt needed:** on first Start the launcher runs
  `go build` in the folder (`./cmd/<name>` entrypoints are discovered).
- **Web or desktop?** decided by your manifest (`desktop: true`) — or, when
  generating from a git repo, by grepping the sources for `net/http` /
  `ListenAndServe` / `http.Server`.
- Web apps must serve on `$PORT` (`PORT`/`APPDECK_HOST` are injected).

```
workspace variant:
my-monorepo/
  app.json        { "id": "my-app", "binary": "my-app", … }
  go.mod          the workspace module
  cmd/my-app/     exactly one cmd/ entrypoint, or package main at the root
```

---

## Rust

```
my-app/
  app.json        { "id": "my-app", "name": "My App", "desktop": true,
                    "binary": "my-app" }
  Cargo.toml      [package] name = "my-app" …
  src/main.rs     the binary
```

- **No prebuilt needed:** on first Start the launcher finds the workspace
  package that produces a binary named like the app (package name, explicit
  `[[bin]]`, or `src/bin/*.rs` — searched up to two levels: `apps/`,
  `crates/`, group folders included) and runs
  `cargo build --release -p <package>`.
- The artifact is copied beside `app.json` as
  `my-app-<os>-<arch>[.exe]`, so later Starts skip the build.
- **Web or desktop?** your manifest decides. When generating from a git
  repo, a dependency on a known HTTP crate (`axum`, `actix-web`, `rocket`,
  `warp`, `tiny_http`, `hyper`, `salvo`, `poem`, `ntex`) makes it a web app;
  everything else (egui/iced/tauri GUIs, CLIs) becomes `desktop: true`.

```
workspace variant (e.g. PdfCraft):
pdfcraft/
  app.json        { "id": "pdfcraft", "desktop": true, "binary": "pdfcraft" }
  Cargo.toml      [workspace] members = ["apps/*", "crates/*"]
  apps/pdfcraft/      [package] name = "pdfcraft" + src/main.rs  ← picked
  apps/pdfcraft-cli/  [package] name = "pdfcraft-cli" (+ [[bin]])
  crates/…             library crates
```

- The `binary` field names which binary to build and run — the member that
  produces it is built. Several members producing the same name, or none,
  is an error that names what it found.

---

## Python

```
my-app/
  app.json        { "id": "my-app", "name": "My App", "port": 0,
                    "cmd": "V=\"$APPDECK_DATA/.venv\"; if [ ! -x \"$V/bin/python\" ]; then python3 -m venv \"$V\" && \"$V/bin/pip\" install -q -r requirements.txt; fi; exec \"$V/bin/python\" main.py" }
  requirements.txt
  main.py         entrypoint (main/app/server/run/bot/start.py are searched)
```

- Python apps run through a `cmd` (a shell line; POSIX `sh -c`, Windows
  `cmd` variant generated for you). The generated command creates a
  **virtual environment under `$APPDECK_DATA`** — the app's writable data
  directory, so re-imports keep the installed environment — installs
  dependencies on **first Start** (`-r requirements.txt`, or `-e .` for
  `pyproject.toml`/`setup.py` projects), then runs the entrypoint.
- Nothing is executed at import time.
- **Web or desktop?** when generating from a git repo, a dependency on
  `flask`, `fastapi`, `uvicorn`, `django`, `tornado`, `bottle`, `sanic`,
  `quart`, `starlette`, `gradio` or `streamlit` makes it a web app (it must
  listen on `$PORT`); everything else becomes `desktop: true`.
- A repository with dependency files but **none** of the known entrypoint
  names is refused with a message listing them — add an `app.json` with your
  own `cmd`, or rename your entrypoint.

---

## Prebuilt binary (any language)

```
my-app/
  app.json        { "id": "my-app", "desktop": true, "binary": "my-app" }
  my-app-darwin-arm64        ← <binary>-<goos>-<goarch>[.exe], or the plain
  my-app-windows-amd64.exe     name (checked second)
  icon.png
```

- Ship one folder with per-platform binaries; each machine Starts its own.
- A single uploaded binary wraps into this shape automatically —
  `?kind=auto` (the default) sniffs the bytes for web-server markers and
  picks the card kind; force it with `?kind=desktop` or `?kind=web&port=N`.
- No `app.json` needed for the upload path; the manifest is written for you.

---

## What the git importer generates (summary)

`POST /api/import?url=https://github.com/user/repo` clones shallowly
(`…/tree/main/Folder` installs just that folder of a monorepo) and, when the
repository has no `app.json`:

| Repo layout | Generated manifest |
|---|---|
| root `.html` file | static app (`servedBy: launcher`) |
| `go.mod` | Go app — binary `<id>-launcher`, web iff sources smell of `net/http` |
| `Cargo.toml` | Rust app — binary picked by name/uniqueness, web iff an HTTP crate is declared |
| `requirements.txt` / `pyproject.toml` / `setup.py` | Python app — self-setting-up venv `cmd`, web iff a web framework is declared |
| `package.json` | Node app — `npm install --omit=dev` + the package entrypoint (`scripts.start`, then `bin`/`main`, then `server.js`/`app.js`/`index.js`) on first start, web iff a web framework is declared, `requires: node` |
| `docker-compose.yml` / `compose.yaml` (nothing else recognized) | Docker Compose app — desktop card running `docker compose up`, `requires: docker`; `.env` is seeded from `.env.example` when missing |
| anything else | refused, listing what the launcher looked for |

A missing icon is resolved from the repo's conventional locations
(`icon.png`, `logo.png/svg`, `favicon.png`, `assets/…`, `docs/…`,
`.github/…`, `resources/…`, `res/…`) or a lettered `icon.svg` is generated.
An existing `app.json` is validated, never overwritten.

Repository baggage never lands in the installed app: `.git`, `.github`,
`.gitignore`, `.gitattributes`, `.cargo`, `.vscode`, `.idea`, `.DS_Store`,
`Thumbs.db` and friends are filtered on every import path.
