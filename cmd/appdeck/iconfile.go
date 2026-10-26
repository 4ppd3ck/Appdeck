// Per-app icon file lookup. An app folder may carry its icon under a
// conventional name (icon.*) or named after the app's id or name
// (<slug>.*) — so a release zip's own icon.pdfcraft.svg works untouched.
// Formats: png, svg, ico, icns. icns files are decoded on the fly: modern
// .icns entries carry PNG payloads, which a tiny parser extracts (no
// platform conversion needed — this works on every OS/arch).
package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
)

// iconExts are the accepted icon file extensions in priority order.
var iconExts = []struct {
	ext   string
	ctype string
}{
	{".png", "image/png"},
	{".svg", "image/svg+xml"},
	{".ico", "image/x-icon"},
	{".icns", "image/png"}, // decoded to its embedded PNG, so the type is png
}

// iconBaseNames lists the base names tried for an app, in priority order:
// the conventional icon.*, then the app id, then the app name (both
// slugified, compared case-insensitively).
func iconBaseNames(a *App) []string {
	bases := []string{"icon"}
	if id := slugify(a.ID); id != "" {
		bases = append(bases, id)
	}
	if name := slugify(a.Name); name != "" && name != slugify(a.ID) {
		bases = append(bases, name)
	}
	return bases
}

// lookupIconDir finds an icon file in dir by the app's accepted names.
// dirContents, when non-nil, is a pre-built lowercase-name index (callers
// that check several apps can build it once); when nil the directory is
// read here.
func lookupIconDir(dir string, a *App, dirContents map[string]string) (data []byte, ctype string, ok bool) {
	if dir == "" {
		return nil, "", false
	}
	index := dirContents
	if index == nil {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, "", false
		}
		index = map[string]string{}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			index[strings.ToLower(e.Name())] = e.Name()
		}
	}
	for _, base := range iconBaseNames(a) {
		for _, ie := range iconExts {
			real, hit := index[base+ie.ext]
			if !hit {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, real))
			if err != nil {
				continue
			}
			if ie.ext == ".icns" {
				png, ok := icnsToPNG(data)
				if !ok {
					continue // undecodable icns — try the next candidate
				}
				return png, "image/png", true
			}
			return data, ie.ctype, true
		}
	}
	return nil, "", false
}

// icnsToPNG extracts the largest embedded PNG from an Apple .icns icon
// family file. Modern icns types (ic07–ic14, icp4…) store whole PNG or
// JPEG2000 images per size; the PNG-carrying types are extracted as-is and
// the largest wins. Legacy raw-bitmap types (is32/il32/…) are skipped —
// every icns shipped this decade carries at least one PNG entry.
func icnsToPNG(data []byte) ([]byte, bool) {
	if len(data) < 8 || string(data[0:4]) != "icns" {
		return nil, false
	}
	total := binary.BigEndian.Uint32(data[4:8])
	if total == 0 || total > uint32(len(data)) {
		total = uint32(len(data))
	}
	best := []byte{}
	for off := 8; off+8 <= int(total); {
		typ := string(data[off : off+4])
		size := int(binary.BigEndian.Uint32(data[off+4 : off+8]))
		if size < 8 || off+size > int(total) {
			break
		}
		payload := data[off+8 : off+size]
		if icnsTypeIsPNG(typ) && len(payload) > len(best) && bytes.HasPrefix(payload, []byte{0x89, 'P', 'N', 'G'}) {
			best = payload
		}
		off += size
	}
	if len(best) == 0 {
		return nil, false
	}
	return best, true
}

// icnsTypeIsPNG reports whether an ICNS OSType's payload is a PNG (or
// JPEG2000, which macOS itself wraps; those are skipped by the magic check
// in icnsToPNG). 16-point "mini" sizes list the 2x PNG variants too.
func icnsTypeIsPNG(typ string) bool {
	switch typ {
	case "icp4", "icp5", "ic07", "ic08", "ic09", "ic10", "ic11", "ic12", "ic13", "ic14":
		return true
	}
	return false
}
