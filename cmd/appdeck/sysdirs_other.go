//go:build !windows

package main

// systemBinDirs is a no-op off Windows (see sysdirs_windows.go): unix
// launchers inherit a usable PATH from every launch context that matters.
func systemBinDirs() []string { return nil }
