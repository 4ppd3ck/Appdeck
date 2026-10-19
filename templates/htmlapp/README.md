# Static HTML app template

This folder is the static starter created by
`appdeck new "My Page" --kind=html`. AppDeck serves the HTML, CSS, and
JavaScript directly; the app has no separate process or build step.

## Run it

Start AppDeck from the `Appdeck/` folder with:

```sh
go run .
```

The page appears in the dashboard. Select **Start**, then **Open**. Edit
`index.html` or the accompanying assets and reopen the app to see the change.

The manifest uses `servedBy: "launcher"` and `port: 0`. Keep network sharing
disabled unless the page is intentionally meant to be reachable by other
devices. A static page cannot write arbitrary files on the computer; use a
server-backed app for local file operations or background work.

See [AppDeck's authoring guide](../../AUTHORING.md) for the full manifest and
runtime contract.
