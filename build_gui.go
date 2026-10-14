//go:build gui

// Release/desktop build (go build -tags gui): no console on Windows
// (-ldflags "-H windowsgui"), window mode on by default, output redirected to
// logs/launcher.log, fatal startup errors surfaced in a message box.
package main

const guiBuild = true
