//go:build darwin && cgo

// Bridge for the AppKit quit paths. //export needs this file to be cgo-
// processed, so it imports "C" with an (intentionally) definitions-free
// preamble; the Cocoa/WebKit glue lives in native_darwin.go.
package main

import "C"

// cgo anchor: keeps the import live for the export below.
var _ = C.int(0)

// runNativeQuit is installed by main before the native window opens; it runs
// the launcher's beginShutdown (stop started apps, then exit). Declared here
// for the darwin+cgo build; native_noquit.go declares it for everything else
// (the build tags of the two files are mutually exclusive).
var runNativeQuit func()

// appdeckRequestQuit is called from applicationShouldTerminate: on the main
// thread. It blocks until beginShutdown has stopped the apps and os.Exit
// ends the process — quit cannot outrun the app cleanup.
//
//export appdeckRequestQuit
func appdeckRequestQuit() {
	if runNativeQuit != nil {
		runNativeQuit()
	}
}
