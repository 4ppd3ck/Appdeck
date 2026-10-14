# __APPNAME__

An AppDeck app scaffolded by `appdeck new`. stdlib-only Go — no dependencies.

## Run it

Start the AppDeck launcher; the app appears on the dashboard and builds
itself on first start. To build by hand:

```sh
go build -o __APPID__-$(go env GOOS)-$(go env GOARCH) .
```

## The contract (see Launcher/AUTHORING.md)

- `app.json` declares identity, the entrypoint and `port: 0` (the launcher
  assigns a stable port).
- The launcher passes `-port {port} -host {host}` and the same values as
  `APPDECK_PORT` / `APPDECK_HOST` environment variables.
- `APPDECK_DATA` is this app's writable directory; `APPDECK_TOKEN`
  authenticates API clients via the `X-AppDeck-Token` header.
- Loopback-only unless `"network": true` in `app.json`.
