# Go app template

This folder is the Go starter created by `appdeck new "My App"`. It contains
a manifest and a small Go web server that follows AppDeck's runtime
environment.

## Run it

Start AppDeck from the `Appdeck/` folder with:

```sh
go run .
```

The new app appears in the dashboard. Select **Start**, then **Open**.
AppDeck builds the Go project the first time. Go 1.27 or newer is required.

To build the app manually from this template folder:

```sh
go build -o my-app .
./my-app
```

On Windows, build with `go build -o my-app.exe .` and run
`.\my-app.exe`.

## AppDeck settings

The manifest's `port: 0` lets AppDeck assign a stable local port. The
launcher passes `APPDECK_HOST`, `APPDECK_PORT`, and `PORT` to the process.
Write any persistent app files under `APPDECK_DATA`. Keep network sharing
disabled unless users deliberately need other devices to reach the app.

See [AppDeck's authoring guide](../../AUTHORING.md) for the complete contract.
