//go:build !darwin || !cgo

// runNativeQuit has no AppKit to bridge on non-cgo builds; the variable is
// declared here so main.go compiles unchanged everywhere.
package main

var runNativeQuit func()
