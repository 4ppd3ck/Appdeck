//go:build !windows

// Mark-of-the-Web stamping is a Windows concept (Zone.Identifier alternate
// data streams); everywhere else it is a no-op.
package main

// stampMotW does nothing off Windows.
func stampMotW(string, []byte) {}
