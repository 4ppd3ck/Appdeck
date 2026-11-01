//go:build darwin

// The macOS expose backend: a symlink in /Applications pointed at the app's
// .app bundle inside the deck container — LaunchServices follows the link,
// so Spotlight, Finder, Launchpad and the "Open with" menu see a normal
// application while the deck keeps owning the real folder. The bundle is a
// vendor one from the import when it exists; a bare binary is wrapped first
// with ensureTccBundle — the elevation flow's own generator, so expose and
// elevation share one bundle (converging on <id>.app) and the stamp check
// means neither ever rewrites what the other left consistent. While a link
// is active, Start launches through it: Launch Services runs a vendor bundle
// as itself, and the generated wrapper's shim sources the start environment
// and keeps the app log flowing. A link is only ever created or removed when
// it is really ours — an existing real /Applications/<name>.app (the user's
// own install) is never touched, and cleanup follows links by where they
// POINT (into the app folder), not by name, so bundle renames survive.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// exposeDir is where links land. LAUNCHER_EXPOSE_DIR overrides it (tests,
// unusual setups).
func exposeDir() string {
	if v := strings.TrimSpace(os.Getenv("LAUNCHER_EXPOSE_DIR")); v != "" {
		return v
	}
	return "/Applications"
}

// exposeLinkRef is one of the app's /Applications links: where it lives and
// what it points at.
type exposeLinkRef struct {
	link   string
	target string
}

// exposeAppLinks finds the symlinks in exposeDir that point into this app's
// folder — the app's links, wherever they were created from. When mustExist,
// dangling links (an update replaced the bundle) are skipped; the caller
// decides whether dangling ones are state to clean up.
func (s *Server) exposeAppLinks(a *App, mustExist bool) []exposeLinkRef {
	entries, err := os.ReadDir(exposeDir())
	if err != nil {
		return nil
	}
	appDir := a.Dir
	if abs, err := filepath.Abs(appDir); err == nil {
		appDir = abs
	}
	prefix := appDir + string(os.PathSeparator)
	var refs []exposeLinkRef
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		link := filepath.Join(exposeDir(), e.Name())
		target, err := os.Readlink(link)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(filepath.Clean(target), prefix) {
			continue
		}
		if mustExist {
			if _, err := os.Stat(target); err != nil {
				continue
			}
		}
		refs = append(refs, exposeLinkRef{link: link, target: filepath.Clean(target)})
	}
	return refs
}

// exposeDarwinBundle resolves the bundle to expose: the one the manifest's
// binary launches from first (so the exposed app is exactly the one the card
// starts), then the generated wrapper, then any vendor bundle in the folder.
// a.Binary's shape varies by discovery stage — folder-relative in a fresh
// manifest load, Root-relative ("Apps/<id>/…") once discovery rewrote it —
// so both bases are tried before the folder scan answers.
func (s *Server) exposeDarwinBundle(a *App) (string, bool) {
	if a.Dir == "" {
		return "", false
	}
	if i := strings.Index(filepath.ToSlash(a.Binary), ".app/"); i > 0 {
		rel := filepath.FromSlash(a.Binary[:i+len(".app")])
		for _, base := range []string{a.Dir, s.cfg.Root} {
			if base == "" {
				continue
			}
			bundle := filepath.Join(base, rel)
			if info, err := os.Stat(bundle); err == nil && info.IsDir() {
				return bundle, true
			}
		}
	}
	if wrapper := filepath.Join(a.Dir, slugify(a.ID)+".app"); fileExists(wrapper) {
		return wrapper, true
	}
	entries, err := os.ReadDir(a.Dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			return filepath.Join(a.Dir, e.Name()), true
		}
	}
	return "", false
}

// exposeResolvableBinary finds the program to wrap — the binary beside
// app.json (either path shape), the data-dir copy on cross-mode decks, or
// the in-bundle copy an earlier elevation moved there. Plain stats only:
// generation must never kick off a from-source build (the guideTccGrant
// rule) — a never-built app is told to start once instead.
func (s *Server) exposeResolvableBinary(a *App) (string, error) {
	if a.Binary == "" {
		return "", fmt.Errorf("the app has no binary — nothing to wrap")
	}
	var candidates []string
	for _, base := range []string{s.cfg.Root, a.Dir, s.cfg.DataDir} {
		if base == "" {
			continue
		}
		candidates = append(candidates, filepath.Join(base, filepath.FromSlash(a.Binary)))
	}
	if bin := bundleBinaryPath(a.Dir, a.ID); bin != "" {
		candidates = append(candidates, bin)
	}
	for _, c := range candidates {
		if fileExists(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("the app's binary is missing on disk — start it once (it builds then) and expose again")
}

// exposeBuildBundle wraps a bare binary into its .app — the elevation flow's
// generator with a placeholder env file, exactly like guideTccGrant stages a
// never-started app's bundle. Elevation later reuses the result untouched:
// the stamp check rebuilds only what drifted.
func (s *Server) exposeBuildBundle(a *App) (string, error) {
	binary, err := s.exposeResolvableBinary(a)
	if err != nil {
		return "", err
	}
	appData := s.appDataDir(a)
	if appData == "" {
		return "", fmt.Errorf("no app data dir for the wrapper's environment file")
	}
	envFile := filepath.Join(appData, "launch.env")
	if _, err := os.Stat(envFile); err != nil {
		_ = os.WriteFile(envFile, []byte("# placeholder until the app's first start rewrites it\n"), 0o644)
	}
	bundle, _, err := s.ensureTccBundle(a, envFile, binary)
	if err != nil {
		return "", err
	}
	return bundle, nil
}

// exposeOn makes sure a bundle exists (wrapping a bare binary when there is
// none) and links it into exposeDir.
func (s *Server) exposeOn(a *App) (string, error) {
	bundle, ok := s.exposeDarwinBundle(a)
	if !ok {
		built, err := s.exposeBuildBundle(a)
		if err != nil {
			return "", err
		}
		bundle = built
	}
	dir := exposeDir()
	link := filepath.Join(dir, filepath.Base(bundle))
	if info, err := os.Lstat(link); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("%s already exists and is not a link — remove it first if this link should replace it", link)
		}
		if cur, err := os.Readlink(link); err == nil && sameLink(cur, bundle) {
			// Already linked — still sweep dangling links from a bundle an
			// update renamed away.
			s.exposeSweepDangling(a)
			return filepath.Base(bundle) + " is already linked into " + dir, nil
		}
		return "", fmt.Errorf("%s is a link to somewhere else — remove it first if this link should replace it", link)
	}
	s.exposeSweepDangling(a)
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return "", err
	}
	if err := os.Symlink(bundle, link); err != nil {
		return "", err
	}
	return filepath.Base(bundle) + " linked into " + dir + " — Spotlight and Finder see it now", nil
}

// exposeSweepDangling removes the app's links whose bundle no longer exists
// (an update landed a differently-named bundle) so the fresh link is the one
// left standing.
func (s *Server) exposeSweepDangling(a *App) {
	for _, ref := range s.exposeAppLinks(a, false) {
		if _, err := os.Stat(ref.target); err != nil {
			_ = os.Remove(ref.link)
		}
	}
}

// exposeOff removes every link into the app's folder — the app may have
// several after bundle renames, and cleanup is by where they point.
func (s *Server) exposeOff(a *App) (string, error) {
	refs := s.exposeAppLinks(a, false)
	if len(refs) == 0 {
		return "was not linked", nil
	}
	for _, ref := range refs {
		if info, err := os.Lstat(ref.link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("%s is not a link — refusing to delete it", ref.link)
		}
	}
	for _, ref := range refs {
		if err := os.Remove(ref.link); err != nil {
			return "", err
		}
	}
	name := filepath.Base(refs[0].target)
	return name + " removed from " + exposeDir(), nil
}

// exposeInfo is the card button's data: an app is exposeable when it has a
// bundle, or a binary that can be wrapped into one, or a link to clean up.
func (s *Server) exposeInfo(a *App) (exposeable, exposed bool) {
	links := s.exposeAppLinks(a, false)
	exposed = len(links) > 0
	_, bundled := s.exposeDarwinBundle(a)
	generatable := false
	if !bundled {
		if _, err := s.exposeResolvableBinary(a); err == nil {
			generatable = true
		}
	}
	return bundled || generatable || exposed, exposed
}

// launchableBundle is the start path's gate: the bundle a Start should go
// through, "" for an ordinary binary launch. Two sources qualify — the
// bundle the manifest's own binary path names (a vendor bundle from the
// import: mpv.app/Contents/MacOS/mpv), and the elevation wrapper (its
// in-bundle binary present) whether or not the app is currently elevated.
// While a /Applications link is active it pins whichever bundle it points
// at (the no-override rule: a Start after expose is a Start through the
// exposed app). Anything else — a stray .app folder the manifest does not
// use — must not hijack the launch, so the folder scan expose uses is
// deliberately NOT consulted here. A link whose bundle vanished (an update
// renamed it) is skipped: the ordinary binary start serves.
func (s *Server) launchableBundle(a *App) string {
	if refs := s.exposeAppLinks(a, true); len(refs) > 0 {
		return refs[0].target
	}
	if bundle, ok := s.exposeDarwinBundle(a); ok {
		// exposeDarwinBundle falls back to the wrapper and the folder scan;
		// only the manifest-named bundle and a populated wrapper qualify
		// here (see the comment above).
		if i := strings.Index(filepath.ToSlash(a.Binary), ".app/"); i > 0 {
			return bundle
		}
		if filepath.Base(bundle) == slugify(a.ID)+".app" && fileExists(bundleBinaryPath(a.Dir, a.ID)) {
			return bundle
		}
	}
	return ""
}

// launchAppBundle starts an app through its bundle: a generated wrapper
// goes through the elevated-launch machinery (the shim sources this start's
// environment and keeps the log flowing — identical to elevation from here
// on, which is the no-override contract), a vendor bundle is handed to
// Launch Services as itself, standalone-style.
func (s *Server) launchAppBundle(a *App, env []string, bundle string, args []string) (int, chan struct{}, error) {
	if filepath.Base(bundle) == slugify(a.ID)+".app" && fileExists(bundleBinaryPath(a.Dir, a.ID)) {
		return s.launchPrivileged(a, env, args)
	}
	// A vendor bundle: Launch Services runs the app as itself. The pgrep
	// anchor is its program — the largest executable in Contents/MacOS (the
	// dmg import's rule; helper dylibs in sibling folders dwarf nothing
	// here).
	binName, _, err := appBundleBinary(bundle)
	if err != nil {
		return 0, nil, err
	}
	return openViaLaunchServices(bundle, filepath.Join(bundle, "Contents", "MacOS", binName), args)
}
