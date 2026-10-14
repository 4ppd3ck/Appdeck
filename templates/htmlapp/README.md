# __APPNAME__

A static AppDeck app scaffolded by `appdeck new`. Plain HTML — the launcher
hosts it in-process, no build step, no child process.

## Run it

Start the AppDeck launcher; the app appears on the dashboard (discovered
from this folder's `app.json`). Edit `index.html`, restart the app from the
dashboard to see changes.

## The contract (see Launcher/AUTHORING.md)

- `app.json` declares identity and `servedBy: "launcher"` (in-process
  hosting); `port: 0` lets the launcher assign a stable port.
- Loopback-only unless `"network": true` in `app.json`.
- Static apps have no process of their own — for writable storage or
  background work, use the `go` template instead.
