//go:build embed

// Single-file release build (go build -tags embed): every app binary and web
// asset is packed inside the executable. release.sh stages the embedded/
// directory before building; a plain `go build` (no tags) uses folder mode
// instead and never looks at it.
package main

import (
	"embed"
)

//go:embed index.html
var embeddedIndexHTML []byte

//go:embed all:embedded
var embeddedFS embed.FS

const embeddedBuild = true
