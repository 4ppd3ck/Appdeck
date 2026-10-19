// Rust app support — the Cargo counterpart of the Go build flow.
//
// A Rust app is discovered from its Cargo.toml the way Go apps are from
// go.mod, and when no prebuilt platform binary exists resolveBinary builds
// one with cargo on first Start. Cargo has no -o flag, so the build writes
// to target/release inside the app folder and the artifact is copied to the
// platform-suffixed path the resolver expects (<bin>-<goos>-<goarch>).
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// buildRustBinary builds a Rust app on first use when its platform binary is
// absent. The package to build is picked by which workspace member produces
// a binary with the expected name — package name, explicit [[bin]], or
// src/bin/*.rs — the same "exactly one entrypoint" rule the Go builder
// applies to cmd/.
func buildRustBinary(output string, logw io.Writer) (string, error) {
	moduleDir := filepath.Dir(output)
	if !fileExists(filepath.Join(moduleDir, "Cargo.toml")) {
		return "", fmt.Errorf("no Cargo.toml in %s", moduleDir)
	}
	bin := expectedCargoBin(output)
	pkg, err := cargoPackageForBin(moduleDir, bin)
	if err != nil {
		return "", err
	}
	cargo, err := cargoToolPath()
	if err != nil {
		return "", fmt.Errorf("cargo toolchain not found — install Rust (https://rustup.rs) or put the built binary next to app.json")
	}
	cmd := exec.Command(cargo, "build", "--release", "-p", pkg)
	hideWindow(cmd)
	cmd.Dir = moduleDir
	// Stream the build live into the app's log (📄 Logs): a first cargo
	// build of a workspace can run many silent minutes.
	var buf bytes.Buffer
	if logw != nil {
		fmt.Fprintf(logw, "appdeck: cargo build --release -p %s (in %s)\n", pkg, moduleDir)
		cmd.Stdout = io.MultiWriter(&buf, logw)
		cmd.Stderr = io.MultiWriter(&buf, logw)
	} else {
		cmd.Stdout = &buf
		cmd.Stderr = &buf
	}
	if err := cmd.Run(); err != nil {
		out := buf.Bytes()
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		// Cargo output runs long; the tail carries the actual error.
		if lines := strings.Split(message, "\n"); len(lines) > 40 {
			message = "... " + strings.Join(lines[len(lines)-40:], "\n")
		}
		return "", fmt.Errorf("cargo build -p %s in %s: %s", pkg, moduleDir, message)
	}
	artifact, err := cargoArtifactPath(moduleDir, bin)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(artifact)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(output, data, 0o755); err != nil {
		return "", err
	}
	if err := ensureExecutable(output); err != nil {
		return "", fmt.Errorf("make built binary executable: %v", err)
	}
	// The cargo build cache (target/) is several GB per workspace and pure
	// weight afterwards: the runnable binary now lives beside app.json, and
	// a rebuild would only be needed if that binary is deleted — at which
	// point it starts fresh anyway.
	if err := os.RemoveAll(filepath.Join(moduleDir, "target")); err == nil {
		if logw != nil {
			fmt.Fprintf(logw, "appdeck: build finished → %s; removed the cargo build cache (target/) to save space\n", filepath.Base(output))
		}
	} else if logw != nil {
		fmt.Fprintf(logw, "appdeck: build finished → %s (note: could not remove target/: %v)\n", filepath.Base(output), err)
	}
	return output, nil
}

// expectedCargoBin derives the binary name a build must produce from the
// resolver's platform-suffixed output path ("pdfcraft-darwin-arm64" →
// "pdfcraft").
func expectedCargoBin(output string) string {
	base := filepath.Base(output)
	base = strings.TrimSuffix(base, ".exe")
	return strings.TrimSuffix(base, "-"+runtime.GOOS+"-"+runtime.GOARCH)
}

// cargoPkg is one [package] of a Cargo.toml: its name and the binaries it
// produces (explicit [[bin]] names, else the implicit src/main.rs name plus
// src/bin/*.rs entries; empty for a library-only package).
type cargoPkg struct {
	name string
	bins []string
}

// collectCargoPackages gathers every package of a Cargo workspace: the root
// package plus members up to two levels down (apps/<name>/, crates/<name>/,
// … — group folders hold members one level deeper).
func collectCargoPackages(moduleDir string) []cargoPkg {
	var pkgs []cargoPkg
	pkgs = append(pkgs, cargoPackagesIn(moduleDir)...)
	if entries, err := os.ReadDir(moduleDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() || skipCargoDir(e.Name()) {
				continue
			}
			sub := filepath.Join(moduleDir, e.Name())
			if fileExists(filepath.Join(sub, "Cargo.toml")) {
				pkgs = append(pkgs, cargoPackagesIn(sub)...)
				continue
			}
			// Group folder (apps/, crates/, …): members live one level deeper.
			if subs, err := os.ReadDir(sub); err == nil {
				for _, s := range subs {
					if !s.IsDir() || skipCargoDir(s.Name()) {
						continue
					}
					member := filepath.Join(sub, s.Name())
					if fileExists(filepath.Join(member, "Cargo.toml")) {
						pkgs = append(pkgs, cargoPackagesIn(member)...)
					}
				}
			}
		}
	}
	return pkgs
}

// cargoPackageForBin finds the workspace package that produces the named
// binary. Exactly one match is required; the error names what was found so
// the fix (usually an app.json with the right binary field) is obvious.
func cargoPackageForBin(moduleDir, bin string) (string, error) {
	var matches []string
	var seen []string
	for _, p := range collectCargoPackages(moduleDir) {
		seen = append(seen, fmt.Sprintf("%s (binaries: %s)", p.name, strings.Join(p.bins, ", ")))
		for _, b := range p.bins {
			if b == bin {
				matches = append(matches, p.name)
			}
		}
	}
	switch {
	case len(matches) == 1:
		return matches[0], nil
	case len(matches) == 0:
		if len(seen) == 0 {
			return "", fmt.Errorf("no package with a binary named %q in %s — add an app.json whose binary field points at a prebuilt binary", bin, moduleDir)
		}
		return "", fmt.Errorf("no Cargo package in %s produces a binary named %q (found: %s)", moduleDir, bin, strings.Join(seen, "; "))
	default:
		return "", fmt.Errorf("several Cargo packages in %s produce a binary named %q (%s) — describe the app with an app.json instead", moduleDir, bin, strings.Join(matches, ", "))
	}
}

// pickRustBinary chooses the binary a generated manifest should run: the one
// named like the app when it exists, else the workspace's only binary. With
// several unrelated binaries the choice is the author's — the error says so.
func pickRustBinary(moduleDir, prefer string) (string, error) {
	var bins []string
	for _, p := range collectCargoPackages(moduleDir) {
		bins = append(bins, p.bins...)
	}
	if len(bins) == 0 {
		return "", fmt.Errorf("the Cargo workspace has no binary packages — add an app.json with a binary field")
	}
	for _, b := range bins {
		if b == prefer {
			return b, nil
		}
	}
	if len(bins) == 1 {
		return bins[0], nil
	}
	return "", fmt.Errorf("several binaries in the workspace (%s) and none is named %q — add an app.json naming one in its binary field", strings.Join(bins, ", "), prefer)
}

// skipCargoDir names that are never workspace member folders (infrastructure,
// VCS, the build cache, vendored sources excluded from the workspace).
func skipCargoDir(name string) bool {
	return strings.HasPrefix(name, ".") ||
		strings.EqualFold(name, "target") || strings.EqualFold(name, "vendor")
}

// cargoPackagesIn parses <dir>/Cargo.toml far enough to find its package
// name and binaries. This is deliberately not a full TOML parser: the
// manifest shape AppDeck needs ([package] name, [[bin]] names) is
// line-oriented, same trade-off the Go discovery makes by grepping sources.
func cargoPackagesIn(dir string) []cargoPkg {
	data, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		return nil
	}
	var pkg *cargoPkg
	var explicit []string
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, "[") {
			section = t
			if t == "[package]" && pkg == nil {
				pkg = &cargoPkg{}
			}
			continue
		}
		name, ok := cargoLineName(t)
		if !ok || pkg == nil {
			continue
		}
		switch section {
		case "[package]":
			if pkg.name == "" {
				pkg.name = name
			}
		case "[[bin]]":
			explicit = append(explicit, name)
		}
	}
	if pkg == nil || pkg.name == "" {
		return nil // virtual manifest (pure [workspace]) or unreadable — no package here
	}
	pkg.bins = cargoPackageBins(dir, explicit, pkg.name)
	return []cargoPkg{*pkg}
}

// cargoLineName reports the value of a `name = "…"` line ("", false when the
// line is something else).
func cargoLineName(line string) (string, bool) {
	eq := strings.Index(line, "=")
	if eq < 0 || strings.TrimSpace(line[:eq]) != "name" {
		return "", false
	}
	rest := line[eq+1:]
	i := strings.Index(rest, `"`)
	if i < 0 {
		return "", false
	}
	rest = rest[i+1:]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// cargoPackageBins lists the binaries a package produces: explicit [[bin]]
// names win; otherwise src/main.rs (named after the package) and every
// src/bin/*.rs.
func cargoPackageBins(dir string, explicit []string, pkgName string) []string {
	if len(explicit) > 0 {
		return explicit
	}
	var bins []string
	if fileExists(filepath.Join(dir, "src", "main.rs")) {
		bins = append(bins, pkgName)
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "src", "bin")); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".rs") {
				bins = append(bins, strings.TrimSuffix(e.Name(), ".rs"))
			}
		}
	}
	return bins
}

// cargoArtifactPath locates the binary a finished cargo build produced.
func cargoArtifactPath(moduleDir, bin string) (string, error) {
	name := bin
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	targetDir := filepath.Join(moduleDir, "target")
	if env := os.Getenv("CARGO_TARGET_DIR"); env != "" {
		if filepath.IsAbs(env) {
			targetDir = env
		} else {
			targetDir = filepath.Join(moduleDir, env)
		}
	}
	artifact := filepath.Join(targetDir, "release", name)
	if fileExists(artifact) {
		return artifact, nil
	}
	return "", fmt.Errorf("cargo build finished without producing %s — check the project's Cargo configuration", artifact)
}

// cargoToolPath locates the cargo toolchain. A user-registered path (🧰 tab)
// wins; then PATH; then the common locations — double-clicked .app launches
// get a minimal PATH (/usr/bin:/bin:/usr/sbin:/sbin) that excludes every
// usual Rust install (the same list goToolPath carries, plus rustup's
// ~/.cargo/bin).
func cargoToolPath() (string, error) {
	if p := registeredToolPath("rust"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("cargo"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		"/opt/homebrew/bin/cargo", // Homebrew (Apple silicon)
		"/usr/local/bin/cargo",    // Homebrew (Intel) / manual symlink
		"/opt/local/bin/cargo",    // MacPorts
	}
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".cargo", "bin", "cargo"),     // rustup default
			filepath.Join(home, ".cargo", "bin", "cargo.exe"), // rustup on Windows
		)
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return c, nil
		}
	}
	return "", exec.ErrNotFound
}

// rustHTTPProject guesses whether a Cargo project serves a web UI: its
// manifest depends on a known HTTP server crate. Best effort, in the spirit
// of the Go discovery's net/http sniff — an app.json is the precise way to
// describe an app.
func rustHTTPProject(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		return false
	}
	t := string(data)
	for _, crate := range []string{
		"axum = ", "actix-web = ", "rocket = ", "warp = ", "tiny_http = ",
		"hyper = ", "salvo = ", "poem = ", "ntex = ",
	} {
		if strings.Contains(t, crate) {
			return true
		}
	}
	return false
}
