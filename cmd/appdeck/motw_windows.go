//go:build windows

// Mark-of-the-Web stamping: files the launcher downloads carry no
// Zone.Identifier, so executables extracted from an import would escape
// SmartScreen/Defender reputation. Stamping ZoneId=3 (internet zone) on
// executables and scripts hands them to the OS's own protections.
package main

import (
	"os"
	"path/filepath"
	"strings"
)

const motwZone = "[ZoneTransfer]\r\nZoneId=3\r\n"

// motwExtensions are the file types Windows treats as code.
var motwExtensions = map[string]bool{
	".exe": true, ".dll": true, ".scr": true, ".com": true, ".pif": true,
	".bat": true, ".cmd": true, ".ps1": true, ".vbs": true, ".js": true,
	".jse": true, ".wsf": true, ".hta": true, ".msi": true, ".jar": true,
	".lnk": true,
}

// stampMotW marks one extracted file (best effort). Callers pick the files;
// on Windows this writes the Zone.Identifier alternate data stream.
func stampMotW(path string, head []byte) {
	if secMagicOf(head) == "" && !motwExtensions[strings.ToLower(filepath.Ext(path))] {
		return
	}
	_ = os.WriteFile(path+":Zone.Identifier", []byte(motwZone), 0o644)
}
