//go:build !embed

// Folder/development build: apps live in sibling folders next to Launcher/
// exactly as in the source tree. The embedded FS stays empty and unused.
package main

import (
	"embed"
)

var embeddedIndexHTML []byte

var embeddedFS embed.FS

const embeddedBuild = false
