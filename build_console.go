//go:build !gui

// Console/development build (plain `go build` / `go run .`): prints to the
// terminal, window mode off by default. Build with -tags gui for the release
// behavior.
package main

const guiBuild = false
