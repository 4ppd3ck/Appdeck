// Platform compatibility: a repository — or an app's own startup output —
// can declare the OS it requires ("only runs on Windows", "Requires
// Windows 10"), and a launcher on any other OS would otherwise install a
// card that can never start. This file collects those declarations from a
// clone's release assets, README, dependency manifests and packaging files
// and refuses the import when they exclude the running OS; the same bind
// phrases in a failed start's output make the caller uninstall the app
// instead of leaving a card that can only repeat the failure. Signal
// collectors take the OS as a parameter so tests exercise any platform.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// platformSignal is one platform declaration found in a cloned repository:
// the OS it names and the evidence line for the refusal message. A partial
// signal is a packaging hint (Windows-only installer scripts, a .bat with
// no .sh counterpart) — never enough to block on its own, only to confirm
// what a strong signal names. bind marks a phrase that ties the project TO
// an OS, as opposed to one that just excludes another ("not supported on
// macOS" names darwin without binding the project anywhere).
type platformSignal struct {
	os      string
	why     string
	partial bool
	bind    bool
}

// platformConflict is an import refusal: the repository declares platform
// support that excludes the running OS. Evidence carries the per-signal
// lines the import dock logs.
type platformConflict struct {
	os       string // the OS the first strong signal names (informational)
	message  string // the whole refusal, evidence included
	evidence []string
}

// osDisplayName renders an OS kind the way the refusal messages name it
// ("darwin" → "macOS"); unknown values pass through unchanged.
func osDisplayName(osKind string) string {
	switch osKind {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	}
	return osKind
}

// platformBindClaims are the exclusivity phrases that bind a project to one
// OS, matched case-insensitively in README text and startup output alike.
// Longest-first so the most specific claim wins. The "requires …" form is
// weaker — support matrices say "requires Windows 10" — and the README
// scan guards it with readmeNamesOtherOS, while the failure classifier
// skips it entirely.
var platformBindClaims = []struct{ phrase, os string }{
	{"only runs on Windows", "windows"},
	{"only runs on macOS", "darwin"},
	{"only runs on Linux", "linux"},
	{"only works on Windows", "windows"},
	{"only works on macOS", "darwin"},
	{"only works on Linux", "linux"},
	{"only supports Windows", "windows"},
	{"only supports macOS", "darwin"},
	{"only supports Linux", "linux"},
	{"only available on Windows", "windows"},
	{"only available on macOS", "darwin"},
	{"only available on Linux", "linux"},
	{"Windows only", "windows"},
	{"macOS only", "darwin"},
	{"Linux only", "linux"},
	{"Windows-only", "windows"},
	{"macOS-only", "darwin"},
	{"Linux-only", "linux"},
	{"exclusive to Windows", "windows"},
	{"exclusive to macOS", "darwin"},
	{"exclusive to Linux", "linux"},
	{"requires Windows", "windows"},
	{"requires macOS", "darwin"},
	{"requires Linux", "linux"},
}

// platformExcludeClaims name the OS a project refuses ("not supported on
// macOS") — the inverse statement, and one that only fires against the OS
// this launcher runs.
var platformExcludeClaims = []struct{ phrase, os string }{
	{"not supported on Windows", "windows"},
	{"not supported on macOS", "darwin"},
	{"not supported on Linux", "linux"},
	{"not available on Windows", "windows"},
	{"not available on macOS", "darwin"},
	{"not available on Linux", "linux"},
	{"does not run on Windows", "windows"},
	{"does not run on macOS", "darwin"},
	{"does not run on Linux", "linux"},
	{"does not support Windows", "windows"},
	{"does not support macOS", "darwin"},
	{"does not support Linux", "linux"},
	{"Windows is not supported", "windows"},
	{"macOS is not supported", "darwin"},
	{"Linux is not supported", "linux"},
}

// detectPlatformConflict gathers the platform declarations a cloned
// repository makes about itself — release assets, README, dependency
// manifests, packaging files — and reports a conflict when a strong signal
// excludes the running OS. Weak (packaging) signals never block alone; they
// only corroborate. nil means no conflict.
func detectPlatformConflict(dir string, rel *ghReleaseInfo, goos string) *platformConflict {
	var strong, weak []string
	var bindOS []string
	firstOS := ""
	add := func(sig platformSignal) {
		if sig.os != "" && firstOS == "" {
			firstOS = sig.os
		}
		if sig.bind {
			bindOS = append(bindOS, sig.os)
		}
		if sig.partial {
			weak = append(weak, sig.why)
		} else {
			strong = append(strong, sig.why)
		}
	}
	if sig, ok := releasePlatformSignal(rel, goos); ok {
		add(sig)
	}
	for _, sig := range readmePlatformSignals(dir, goos) {
		add(sig)
	}
	if sig, ok := pythonDepPlatformSignal(dir, goos); ok {
		add(sig)
	}
	if len(strong) == 0 {
		return nil
	}
	for _, sig := range structuralPlatformSignals(dir) {
		add(sig)
	}
	var header string
	distinct := map[string]bool{}
	for _, kind := range bindOS {
		distinct[kind] = true
	}
	if len(distinct) == 1 {
		header = fmt.Sprintf("the repository declares %s-only support", osDisplayName(bindOS[0]))
	} else {
		header = fmt.Sprintf("the repository declares no %s support", osDisplayName(goos))
	}
	message := header + " — " + strings.Join(strong, "; ") + "."
	if len(weak) > 0 {
		message += " Also found: " + strings.Join(weak, "; ") + "."
	}
	message += " AppDeck did not import it."
	return &platformConflict{os: firstOS, message: message, evidence: strong}
}

// releasePlatformSignal reads the release the import already fetched: when
// its assets name OSes and none of them is goos, the project ships binaries
// this launcher cannot run. Assets that name no OS (source tarballs) and
// metadata sidecars are ignored, so a source-only release never fires.
func releasePlatformSignal(rel *ghReleaseInfo, goos string) (platformSignal, bool) {
	if rel == nil {
		return platformSignal{}, false
	}
	var others []string // the OSes the assets declare, first-seen order
	covered := false
	var names []string // asset names backing the declaration
	extra := 0
	for _, a := range rel.Assets {
		name := filepath.Base(strings.ReplaceAll(a.Name, "\\", "/"))
		lower := strings.ToLower(name)
		if ext := filepath.Ext(lower); ext != "" && releaseRejectExts[ext] {
			continue // checksums and release notes carry no OS statement
		}
		kind := assetOSKind(lower)
		if kind == "" {
			continue
		}
		if kind == goos {
			covered = true
			break
		}
		if !slices.Contains(others, kind) {
			others = append(others, kind)
		}
		if len(names) < 3 {
			names = append(names, name)
		} else {
			extra++
		}
	}
	if covered || len(others) == 0 {
		return platformSignal{}, false
	}
	declared := make([]string, len(others))
	for i, kind := range others {
		declared[i] = osDisplayName(kind)
	}
	name := "release " + rel.Tag
	if rel.Tag == "" {
		name = "the release"
	}
	if extra > 0 {
		names = append(names, fmt.Sprintf("and %d more", extra))
	}
	return platformSignal{
		os:   others[0],
		bind: true,
		why: fmt.Sprintf("%s ships %s builds but none for %s (%s)", name,
			strings.Join(declared, " and "), osDisplayName(goos), strings.Join(names, ", ")),
	}, true
}

// readmePlatformSignals reads the repository's README and reports the claims
// in it that conflict with goos: a bind claim to another OS, or an exclude
// claim against this one. Ordinary OS mentions — support matrices, CI
// badges — never fire; only exclusivity phrasing does, and the weaker
// "requires …" phrasing is suppressed when the README names any other
// desktop OS at all (a matrix like "Windows: requires 10; macOS: requires
// 12" does not bind the project to Windows).
func readmePlatformSignals(dir, goos string) []platformSignal {
	text := lowercasedReadme(dir)
	if text == "" {
		return nil
	}
	var out []platformSignal
	for _, c := range platformBindClaims {
		phrase := strings.ToLower(c.phrase)
		if c.os == goos || !strings.Contains(text, phrase) {
			continue
		}
		if strings.HasPrefix(phrase, "requires ") && readmeNamesOtherOS(text, c.os) {
			continue
		}
		out = append(out, platformSignal{os: c.os, why: fmt.Sprintf("its README says %q", c.phrase), bind: true})
		break
	}
	for _, c := range platformExcludeClaims {
		if c.os != goos || !strings.Contains(text, strings.ToLower(c.phrase)) {
			continue
		}
		out = append(out, platformSignal{os: c.os, why: fmt.Sprintf("its README says %q", c.phrase)})
		break
	}
	return out
}

// lowercasedReadme is the repository's README, lowercased for the phrase
// checks; 32 KB is far past any platform statement that matters and bounds
// the scan.
func lowercasedReadme(dir string) string {
	for _, name := range []string{"README.md", "README", "readme.md", "readme.txt"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			if len(data) > 32*1024 {
				data = data[:32*1024]
			}
			return strings.ToLower(string(data))
		}
	}
	return ""
}

// readmeNamesOtherOS reports whether the README text names any desktop OS
// besides the claimed one — the shape of a support matrix rather than a
// single-platform project.
func readmeNamesOtherOS(text, claimed string) bool {
	tokens := map[string][]string{
		"windows": {"windows", "win32", "win64"},
		"darwin":  {"macos", "osx", "os x", "mac os", "apple silicon"},
		"linux":   {"linux", "ubuntu", "debian", "fedora", "appimage"},
	}
	for other, marks := range tokens {
		if other == claimed {
			continue
		}
		for _, m := range marks {
			if strings.Contains(text, m) {
				return true
			}
		}
	}
	return false
}

// windowsOnlyPythonPackages are pip names that only ever work on Windows —
// the win32 API bindings, WMI access, and the Windows-only curses port.
// Their presence binds a Python project to Windows.
var windowsOnlyPythonPackages = []string{
	"pywin32", "pywin32-ctypes", "pythoncom", "win32api", "win32com",
	"win32console", "win32event", "win32evtlog", "win32gui", "win32pipe",
	"win32print", "win32process", "win32service", "win32timezone", "wmi",
	"winshell", "windows-curses",
}

// win32Markers are the PEP 508 environment markers that declare a
// dependency Windows-only regardless of which package it guards.
var win32Markers = []string{
	"sys_platform == 'win32'", `sys_platform == "win32"`,
	"platform_system == 'windows'", `platform_system == "windows"`,
}

// pythonDepPlatformSignal reads the dependency manifests and reports a
// Windows binding when one names a Windows-only package or carries a win32
// environment marker. Comments are stripped first — a cross-platform
// project's "# pywin32 is optional on Windows" must not count — and the
// name match is boundary-checked so "wmi" cannot ride along inside
// "swmidget".
func pythonDepPlatformSignal(dir, goos string) (platformSignal, bool) {
	if goos == "windows" {
		return platformSignal{}, false // these packages are native there
	}
	var text string
	for _, name := range []string{"requirements.txt", "pyproject.toml"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			text += stripHashComments(string(data)) + "\n"
		}
	}
	if text == "" {
		return platformSignal{}, false
	}
	lo := strings.ToLower(text)
	for _, marker := range win32Markers {
		if strings.Contains(lo, marker) {
			return platformSignal{os: "windows", bind: true,
				why: "a dependency declares itself win32-only (a sys_platform/platform_system marker)"}, true
		}
	}
	for _, pkg := range windowsOnlyPythonPackages {
		if containsToken(lo, pkg) {
			return platformSignal{os: "windows", bind: true,
				why: fmt.Sprintf("the dependency manifests name the Windows-only package %s", pkg)}, true
		}
	}
	return platformSignal{}, false
}

// containsToken reports whether name appears in text with a non-word
// character (or an edge) on both sides.
func containsToken(text, name string) bool {
	for from := 0; ; from++ {
		j := strings.Index(text[from:], name)
		if j < 0 {
			return false
		}
		at := from + j
		before, after := at-1, at+len(name)
		if (before < 0 || !isWordByte(text[before])) &&
			(after >= len(text) || !isWordByte(text[after])) {
			return true
		}
		from = at // the post-statement steps past this occurrence
	}
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
}

// stripHashComments cuts every line at '#', the comment marker both
// requirements files and pyproject use.
func stripHashComments(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if k := strings.IndexByte(line, '#'); k >= 0 {
			lines[i] = line[:k]
		}
	}
	return strings.Join(lines, "\n")
}

// structuralPlatformSignals names the Windows-only packaging at the
// repository root: Inno Setup, NSIS and WiX scripts build Windows
// installers, and a .bat/.cmd launcher with no .sh counterpart means the
// scripted path is Windows-only. These are hints — a strong signal alone
// blocks; these only corroborate it.
func structuralPlatformSignals(dir string) []platformSignal {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []platformSignal
	bat, sh := 0, 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		switch {
		case strings.HasSuffix(name, ".iss"), strings.HasSuffix(name, ".nsi"), strings.HasSuffix(name, ".wxs"):
			out = append(out, platformSignal{os: "windows", partial: true,
				why: fmt.Sprintf("a %s installer script (%s)", strings.TrimPrefix(filepath.Ext(name), "."), e.Name())})
		case strings.HasSuffix(name, ".bat"), strings.HasSuffix(name, ".cmd"):
			bat++
		case strings.HasSuffix(name, ".sh"):
			sh++
		}
	}
	if bat > 0 && sh == 0 {
		out = append(out, platformSignal{os: "windows", partial: true,
			why: "a Windows-only launcher script (.bat/.cmd, no .sh counterpart)"})
	}
	return out
}

// platformIncompatibleFailure reads an app's own startup output for a
// platform bind claim: "StellarHub only runs on Windows." is the app
// announcing it can never work here, and the card it leaves behind would
// only repeat the failure. ok is true only when the named OS is not the
// running one — an app that names the running OS is left alone, and so is
// everything a platform check cannot pin on an OS (port clashes, missing
// toolchains, timeouts). The "requires …" form is skipped: help text can
// mention OS requirements without the app being Windows-only.
func platformIncompatibleFailure(output string) (string, bool) {
	lo := strings.ToLower(output)
	for _, c := range platformBindClaims {
		if strings.HasPrefix(strings.ToLower(c.phrase), "requires ") {
			continue
		}
		if strings.Contains(lo, strings.ToLower(c.phrase)) {
			return c.os, c.os != runtime.GOOS
		}
	}
	return "", false
}
