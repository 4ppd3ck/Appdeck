//go:build !windows

package main

// popupWindowArgs has no work-area source on this platform; Chromium's
// default --app placement applies.
func popupWindowArgs() []string { return nil }
