// Importers for third-party release formats AppDeck-authored archives don't
// use. Release pages ship per-OS shapes: macOS dmgs, Linux deb/rpm/flatpak
// packages and AppImages, Windows installers. This file adds what can be
// imported safely — macOS dmgs (hdiutil, darwin only), Debian packages
// (linux only), and bare archives without app.json (a single program is
// wrapped as a desktop app) — and turns the rest into actionable refusals
// that name the asset to grab instead.
package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// looksLikeDmg reports whether an upload is an Apple UDIF disk image: by
// the .dmg name, or the "koly" trailer magic every dmg carries in its last
// 512 bytes.
func looksLikeDmg(name string, body []byte) bool {
	if strings.EqualFold(filepath.Ext(name), ".dmg") {
		return true
	}
	return len(body) >= 512 && string(body[len(body)-512:len(body)-508]) == "koly"
}

// isDebBody reports whether an upload is a Debian package: the ar magic
// plus the debian-binary member every .deb starts with.
func isDebBody(body []byte) bool {
	return len(body) > 68 && string(body[0:8]) == "!<arch>\n" &&
		bytes.Contains(body[:512], []byte("debian-binary"))
}

// refuseExts maps file extensions to the import refusal naming the asset to
// grab instead — shapes AppDeck cannot import safely, stated per format so
// the error is the next step, not a dead end.
var refuseExts = map[string]string{
	".zsync":   "is a delta-sync metadata file — download the same asset WITHOUT .zsync (the .AppImage or .tar.gz) and import that",
	".flatpak": "is a flatpak bundle and needs the flatpak runtime — import the .AppImage or .tar.gz build instead",
	".rpm":     "is an RPM package — import the .AppImage, .deb, or .tar.gz build instead (rpm extraction is not supported)",
	".pkg":     "is a macOS installer package — use the .dmg build instead",
	".snap":    "is a snap package and needs snapd — import the .AppImage or .tar.gz build instead",
}

// importExtRefusal returns the refusal message for a name the importer
// cannot handle, or "" when the name carries no refused extension.
func importExtRefusal(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if msg, ok := refuseExts[ext]; ok {
		return filepath.Base(name) + " " + msg
	}
	return ""
}

// dmgStage mounts a macOS disk image with hdiutil, imports the .app bundle
// whole (signature, resources, frameworks — the binary is never lifted out
// of its sealed context), stages a wrapped desktop app whose manifest points
// at Contents/MacOS inside the bundle, and returns the app with its finalize
// swap. Shared by the direct-import path and the release-asset channel.
// darwin only.
func (s *Server) dmgStage(body []byte, qs map[string]string, job *jobLog) (*App, string, func() error, error) {
	tmp, err := os.MkdirTemp("", "appdeck-dmg-")
	if err != nil {
		return nil, "", nil, err
	}
	defer os.RemoveAll(tmp)
	src := filepath.Join(tmp, "image.dmg")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		return nil, "", nil, err
	}
	mnt := filepath.Join(tmp, "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return nil, "", nil, err
	}
	job.logf("image staged at %s (%d KB)", src, len(body)/1024)
	attach := exec.Command("hdiutil", "attach", "-nobrowse", "-readonly", "-mountpoint", mnt, src)
	hideWindow(attach)
	job.logf("mounting read-only with hdiutil at %s…", mnt)
	if out, err := attach.CombinedOutput(); err != nil {
		return nil, "", nil, fmt.Errorf("hdiutil attach failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	defer func() {
		_ = exec.Command("hdiutil", "detach", "-force", mnt).Run()
		job.logf("cleanup: image detached, staging dir %s removed", tmp)
	}()

	// The dmg's payload: an .app bundle at the top level (single-volume
	// releases; a mounted volume with several .apps is refused as ambiguous).
	entries, err := os.ReadDir(mnt)
	if err != nil {
		return nil, "", nil, fmt.Errorf("reading the mounted dmg: %v", err)
	}
	var appDir, bundleName string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			if appDir != "" {
				return nil, "", nil, fmt.Errorf("the dmg holds several .app bundles — mount it by hand and import the one you want")
			}
			appDir, bundleName = filepath.Join(mnt, e.Name()), e.Name()
		}
	}
	if appDir == "" {
		return nil, "", nil, fmt.Errorf("no .app bundle at the dmg's top level — this image does not carry a GUI app")
	}

	binName, _, err := appBundleBinary(appDir)
	if err != nil {
		return nil, "", nil, err
	}
	appname := strings.TrimSpace(qs["appname"])
	if appname == "" {
		appname = strings.TrimSuffix(bundleName, ".app")
	}
	dest, finalize, err := s.stageImportDest(appname)
	if err != nil {
		return nil, "", nil, err
	}
	// The bundle is imported whole, signature and all: a developer's seal
	// covers the entire .app (Info.plist, Resources, Frameworks), so lifting
	// the bare executable out — what earlier builds did — left a binary
	// macOS kills at launch (AMFI, SIGKILL, empty log). The manifest points
	// into the bundle at Contents/MacOS/<bin>, which stays inside the app
	// folder and validates like any nested binary.
	job.logf("copying the %s bundle (vendor signature preserved)…", bundleName)
	bundleDest := filepath.Join(dest, bundleName)
	files, copied, err := copyBundleTree(appDir, bundleDest)
	if err != nil {
		os.RemoveAll(dest)
		job.logf("cleanup: removed the partial install at %s", dest)
		return nil, "", nil, err
	}
	job.logf("copied %d files (%.1f MB) into %s", files, float64(copied)/(1024*1024), bundleDest)
	sig := importSignatureState(bundleDest, true, job)
	if icon := appBundleIcon(appDir); len(icon) > 0 {
		_ = os.WriteFile(filepath.Join(dest, "icon.png"), icon, 0o644)
	}
	app, err := writeWrapManifest(dest, filepath.ToSlash(filepath.Join(bundleName, "Contents", "MacOS", binName)), appname, qs, job, sig)
	if err != nil {
		os.RemoveAll(dest)
		return nil, "", nil, err
	}
	return app, dest, finalize, nil
}

// copyBundleTree copies a mounted .app bundle into the deck verbatim:
// every regular file with its permission bits, and symlinks recreated with
// their original targets — the developer's seal covers the bundle's exact
// shape (symlink targets included), so anything less would break the
// signature macOS checks at launch. Junk filtering deliberately does not
// apply inside the bundle: a "junk" file the seal covers must stay for the
// seal to hold. Size-capped like any import. Returns the file count and
// bytes copied for the import log.
func copyBundleTree(src, dst string) (int, int64, error) {
	return copyTreeLimited(src, dst, expandMaxBytes())
}

// copyTreeLimited is copyBundleTree's engine: a bounded recursive copy that
// preserves permissions and symlinks and refuses everything exotic (devices,
// sockets), since no healthy .app carries them.
func copyTreeLimited(src, dst string, limit int64) (int, int64, error) {
	info, err := os.Lstat(src)
	if err != nil {
		return 0, 0, err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return 0, 0, err
		}
		return 1, 0, os.Symlink(target, dst)
	case info.IsDir():
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return 0, 0, err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return 0, 0, err
		}
		files, bytes := 0, int64(0)
		for _, e := range entries {
			f, b, err := copyTreeLimited(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), limit-bytes)
			files += f
			bytes += b
			if err != nil {
				return files, bytes, err
			}
		}
		return files, bytes, nil
	case info.Mode().IsRegular():
		if info.Size() > limit {
			return 0, 0, fmt.Errorf("%s is larger than the import limit (%d bytes)", filepath.Base(src), limit)
		}
		in, err := os.Open(src)
		if err != nil {
			return 0, 0, err
		}
		defer in.Close()
		perm := info.Mode().Perm()
		if perm&0o400 == 0 {
			perm |= 0o400 // a bundle file without read bits cannot be copied back
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
		if err != nil {
			return 0, 0, err
		}
		n, err := io.Copy(out, in) // capped: the file's size was checked above
		cerr := out.Close()
		if err != nil {
			return 0, n, err
		}
		if cerr != nil {
			return 0, n, cerr
		}
		return 1, n, nil
	default:
		return 0, 0, fmt.Errorf("%s: not a regular file or symlink — refusing", src)
	}
}

// importSignatureState verifies what an import just wrote and repairs it
// when the seal broke. macOS kills a badly-signed program at launch with an
// empty log (AMFI SIGKILL — the pdfcraft/wordcraft imports' silent deaths),
// so import is the moment to check. Returns the state recorded on the
// manifest for the dashboard badge:
//
//	"vendor"  seal intact — the developer's signature survived the import
//	"adhoc"   seal broken, re-signed ad hoc locally — runs, but macOS no
//	          longer attributes the app to its developer
//	"invalid" could not verify nor re-sign — macOS may kill it on launch
//	""        nothing to check (non-darwin decks; no codesign on the host)
//
// bundle marks a .app bundle (deep re-sign: nested frameworks and helpers
// re-seal before the outer bundle does).
func importSignatureState(path string, bundle bool, job *jobLog) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	verifyErr := verifyCodeSignature(path, job)
	if verifyErr == nil {
		job.logf("code signature verified — the developer's signature survived the import")
		return "vendor"
	}
	job.logf("code signature check failed (%v)", verifyErr)
	job.logf("re-signing ad hoc so macOS will launch it…")
	if err := adHocResign(path, bundle, job); err != nil {
		job.logf("WARNING: ad-hoc re-sign failed (%v) — macOS may kill this app on launch; re-import the vendor's original .dmg", err)
		return "invalid"
	}
	if err := verifyCodeSignature(path, job); err != nil {
		job.logf("WARNING: signature still invalid after re-signing (%v) — macOS may kill this app on launch", err)
		return "invalid"
	}
	job.logf("re-signed ad hoc — the app runs, but macOS no longer sees the developer's signature")
	return "adhoc"
}

// installDmg imports a macOS disk image (dashboard path): wraps the dmg's
// dmgStage core with the HTTP responses. darwin only — elsewhere the
// refusal names the platform.
func (s *Server) installDmg(w http.ResponseWriter, body []byte, name string, qs map[string]string, job *jobLog) {
	if runtime.GOOS != "darwin" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": filepath.Base(name) + " is a macOS disk image — it only opens on macOS. Download the build for " +
			runtime.GOOS + " (portable zip, .AppImage, .deb, …) and import that."})
		return
	}
	app, dest, finalize, err := s.dmgStage(body, qs, job)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.finalizeImportDest(dest, app.ID, finalize, w, job); err != nil {
		return
	}
	s.importDone(w, app, job)
}

// appBundleBinary picks a .app bundle's program: the largest regular file
// in Contents/MacOS (helpers and code signatures live beside it, but the
// program is the big one). Only the magic bytes are read — the bundle copy
// brings the bytes over.
func appBundleBinary(appDir string) (string, []byte, error) {
	binDir := filepath.Join(appDir, "Contents", "MacOS")
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return "", nil, fmt.Errorf("%s has no Contents/MacOS — not an .app bundle", filepath.Base(appDir))
	}
	best, bestSize := "", int64(0)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() <= bestSize {
			continue
		}
		best, bestSize = e.Name(), info.Size()
	}
	if best == "" {
		return "", nil, fmt.Errorf("%s carries no program in Contents/MacOS", filepath.Base(appDir))
	}
	f, err := os.Open(filepath.Join(binDir, best))
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return "", nil, err
	}
	if !looksExecutable(magic) {
		return "", nil, fmt.Errorf("%s in %s is not a runnable program", best, filepath.Base(appDir))
	}
	return best, magic, nil
}

// appBundleIcon converts a .app bundle's largest .icns to PNG bytes for the
// deck's icon (""-safe: no Resources or no icns → nil, the emoji stays).
func appBundleIcon(appDir string) []byte {
	res := filepath.Join(appDir, "Contents", "Resources")
	entries, err := os.ReadDir(res)
	if err != nil {
		return nil
	}
	best, bestSize := []byte(nil), 0
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".icns") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(res, e.Name()))
		if err != nil || len(data) <= bestSize {
			continue
		}
		if png, ok := icnsToPNG(data); ok && len(png) > 0 {
			best, bestSize = png, len(data)
		}
	}
	return best
}

// installDeb imports a Debian package on Linux: parse the ar container,
// unpack data.tar.<comp> (every compression the archive layer knows), and
// wrap the largest executable — the package's program — as a desktop app.
func (s *Server) installDeb(w http.ResponseWriter, body []byte, name string, qs map[string]string, job *jobLog) {
	if runtime.GOOS != "linux" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": filepath.Base(name) + " is a Debian package — it installs on Linux only. Download the build for " +
			runtime.GOOS + " and import that."})
		return
	}
	job.logf("unpacking the deb (ar container)…")
	data, err := debDataTar(body)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	format := "tar"
	switch {
	case bytes.HasPrefix(data, []byte{0x1F, 0x8B}):
		format = "tar.gz"
	case bytes.HasPrefix(data, []byte("BZh")):
		format = "tar.bz2"
	case bytes.HasPrefix(data, []byte{0xFD, '7', 'z', 'X', 'Z', 0x00}):
		format = "tar.xz"
	case len(data) > 4 && binary.LittleEndian.Uint32(data[0:4]) == 0xFD2FB528:
		format = "tar.zst"
	}
	stream, err := newTarStream(data, format)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the deb's data.tar is unreadable: " + err.Error()})
		return
	}
	binName, binData, icon, err := tarProgram(stream)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	appname := strings.TrimSpace(qs["appname"])
	if appname == "" {
		appname = cleanBinaryAppName(binName)
	}
	dest, finalize, err := s.importDest(appname, w)
	if err != nil {
		return
	}
	job.logf("installing %s from the package…", binName)
	if err := os.WriteFile(filepath.Join(dest, binName), binData, 0o755); err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(icon) > 0 {
		_ = os.WriteFile(filepath.Join(dest, "icon.png"), icon, 0o644)
	}
	app, err := writeWrapManifest(dest, binName, appname, qs, job, "")
	if err != nil {
		os.RemoveAll(dest)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.finalizeImportDest(dest, app.ID, finalize, w, job); err != nil {
		return
	}
	s.importDone(w, app, job)
}

// debDataTar extracts the data.tar member from a .deb's ar container.
// ar layout: 8-byte global magic, then per member a 60-byte ASCII header
// (name 16, mtime 12, uid 6, gid 6, mode 8, size 10, "`\n" 2) and the data,
// padded to an even offset.
func debDataTar(body []byte) ([]byte, error) {
	if !bytes.HasPrefix(body, []byte("!<arch>\n")) {
		return nil, fmt.Errorf("not an ar archive (deb files start with !<arch>)")
	}
	for off := 8; off+60 <= len(body); {
		name := strings.TrimRight(string(body[off:off+16]), " ")
		size, err := strconv.Atoi(strings.TrimSpace(string(body[off+48 : off+58])))
		if err != nil || size < 0 || off+60+size > len(body) {
			return nil, fmt.Errorf("corrupt ar member header in the deb")
		}
		if strings.HasPrefix(name, "data.tar") {
			return body[off+60 : off+60+size], nil
		}
		off += 60 + size
		if size%2 == 1 {
			off++ // members are 2-byte aligned
		}
	}
	return nil, fmt.Errorf("no data.tar member in the deb — not a Debian package")
}

// tarProgram walks a package's tar stream and returns the program to wrap:
// the largest executable regular file (the binary out of usr/bin and
// friends — libraries, docs and licenses are not executable or far smaller),
// plus the largest PNG under share/icons for the deck's icon.
func tarProgram(stream archiveStream) (binName string, binData, icon []byte, err error) {
	var binBuf, iconBuf bytes.Buffer
	var binSize, iconSize int64
	for {
		h, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, nil, fmt.Errorf("reading the package's tar: %v", err)
		}
		if h == nil || h.mode.IsDir() {
			continue
		}
		name := strings.TrimPrefix(filepath.ToSlash(h.name), "./")
		switch {
		case h.mode.Perm()&0o111 != 0 && executablePath(name):
			if h.size > binSize && underLimit(h.size, importMaxBytes()) {
				var buf bytes.Buffer
				n, _ := io.CopyN(&buf, stream, h.size)
				if n == h.size && n > binSize {
					binBuf, binSize, binName = buf, n, filepath.Base(name)
				}
			}
		case strings.Contains(name, "/share/icons/") && strings.EqualFold(filepath.Ext(name), ".png"):
			if h.size > iconSize && h.size < 4<<20 {
				var buf bytes.Buffer
				n, _ := io.CopyN(&buf, stream, h.size)
				if n == h.size && n > iconSize {
					iconBuf, iconSize = buf, n
				}
			}
		default:
			continue
		}
	}
	if binName == "" {
		return "", nil, nil, fmt.Errorf("no executable program found in the package")
	}
	return binName, binBuf.Bytes(), iconBuf.Bytes(), nil
}

// executablePath reports whether a package path points at a program (bin/,
// sbin/, games/ directories) rather than a resource that happens to carry
// the executable bit.
func executablePath(name string) bool {
	dir := filepath.ToSlash(filepath.Dir(name))
	for _, seg := range []string{"bin", "sbin", "games"} {
		if dir == seg || strings.HasSuffix(dir, "/"+seg) {
			return true
		}
	}
	return false
}

// installBareArchive handles third-party release archives that carry no
// app.json (portable zips, cli zips, plain tarballs): exactly one program
// — a binary anywhere in the archive (root level, a wrapping folder, or a
// .app bundle's MacOS folder) — is wrapped as a desktop app, with app.json
// created for it. Returns false when the archive does not fit the shape
// (the caller then shows the original no-app.json error).
func (s *Server) installBareArchive(w http.ResponseWriter, body []byte, qs map[string]string, job *jobLog) bool {
	job.logf("no app.json in this archive — a manifest will be generated for the wrapped program…")
	var binName string
	var binData []byte
	var icon []byte
	var appname string
	// A pick that lives inside a .app bundle: the whole bundle is imported
	// (below, once the destination exists) — the seal macOS checks covers
	// Info.plist/Resources/Frameworks too, and the lifted binary alone gets
	// killed at launch. bundleRoot is the bundle's slash path in the archive,
	// bundleZr the reader its entries are extracted from.
	var bundleRoot string
	var bundleZr *zip.Reader
	// extractBundle copies the picked bundle whole into the install folder:
	// zip archives extract via extractZipAppBundle, tar-family archives via
	// the closure tarAppBundle returns (the two shapes need different
	// readers, the same guards).
	var extractBundle func(dest string) (int, int64, error)
	switch detectArchiveFormat(body) {
	case "zip":
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			job.logf("could not reopen the zip for wrapping: %v", err)
			return false
		}
		data, pick, err := zipProgramFromArchive(zr)
		if err != nil {
			if c := zipComposeFile(zr); c != "" {
				msg := fmt.Sprintf("the archive carries %s — a Docker Compose project is imported "+
					"from its repository: use ?url=<repo> and a compose app is generated "+
					"(needs the docker prerequisite)", c)
				job.finish(msg)
				s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
				return true
			}
			if zipRootFile(zr, "package.json") != nil {
				s.installNodeZip(w, body, zr, qs, job) // writes its own response
				return true
			}
			job.logf("wrap abandoned: %v", err)
			return false // ambiguity and no-program cases get the original error
		}
		nameSlash := filepath.ToSlash(pick.Name)
		if i := strings.Index(nameSlash, ".app/Contents/"); i > 0 {
			root := nameSlash[:i+len(".app")] // "<wrapper…>/X.app"
			bundle := filepath.Base(root)
			bundleRoot, bundleZr = root, zr
			job.logf("program lives inside %s — the whole bundle is imported, signature and all", bundle)
			binName = filepath.ToSlash(filepath.Join(bundle, "Contents", "MacOS", filepath.Base(nameSlash)))
			appname = strings.TrimSuffix(bundle, ".app")
		} else {
			binName, binData = filepath.Base(nameSlash), data
		}
		// A .app bundle pick: its Resources/*.icns becomes the icon.
		if i := strings.Index(nameSlash, ".app/"); i > 0 {
			root := nameSlash[:i+len(".app")]
			prefix := root + "/Contents/Resources/"
			bestSize := 0
			for _, f := range zr.File {
				fn := filepath.ToSlash(f.Name)
				if !strings.HasPrefix(fn, prefix) || !strings.EqualFold(filepath.Ext(fn), ".icns") {
					continue
				}
				if int(f.UncompressedSize64) <= bestSize {
					continue
				}
				if icns, err := readZipEntryRaw(f, importMaxBytes()); err == nil {
					if png, ok := icnsToPNG(icns); ok && len(png) > 0 {
						icon, bestSize = png, int(f.UncompressedSize64)
					}
				}
			}
			if appname == "" {
				appname = strings.TrimSuffix(filepath.Base(root), ".app")
			}
		}
	default:
		// tar.gz/tar.xz/tar.zst/7z: a program inside an .app bundle is
		// imported whole — the bundle's seal covers Info.plist, Resources
		// and Frameworks, and shipped MacOS/lib dylibs can dwarf the program
		// itself (mpv's macOS builds), so largest-executable-anywhere would
		// pick the wrong file. Without a bundle the largest bin/sbin/games
		// executable is wrapped, as before.
		if root, binPath, ic, extract, ok := tarAppBundle(body); ok {
			bundleRoot, binName, icon, extractBundle = root, binPath, ic, extract
			appname = strings.TrimSuffix(filepath.Base(root), ".app")
			job.logf("program lives inside %s — the whole bundle is imported, signature and all", filepath.Base(root))
		} else {
			opener, err := archiveOpener(body)
			if err != nil {
				return false
			}
			stream, err := opener()
			if err != nil {
				return false
			}
			defer stream.Close()
			name, data, ic, err := tarProgram(stream)
			if err != nil {
				return false
			}
			binName, binData, icon = name, data, ic
		}
	}
	if appname == "" {
		appname = strings.TrimSpace(qs["appname"])
	}
	if appname == "" {
		appname = cleanBinaryAppName(strings.TrimSuffix(binName, filepath.Ext(binName)))
	}
	dest, finalize, err := s.importDest(appname, w)
	if err != nil {
		return true // importDest wrote the error response
	}
	sig := ""
	if bundleRoot != "" {
		bundle := filepath.Base(bundleRoot)
		job.logf("extracting %s into %s…", bundle, dest)
		var files int
		var total int64
		if extractBundle != nil {
			files, total, err = extractBundle(dest)
		} else {
			files, total, err = extractZipAppBundle(bundleZr, bundleRoot, dest)
		}
		if err != nil {
			os.RemoveAll(dest)
			job.logf("cleanup: removed the partial install at %s", dest)
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return true
		}
		job.logf("extracted %d files (%.1f MB) from the bundle", files, float64(total)/(1024*1024))
		sig = importSignatureState(filepath.Join(dest, bundle), true, job)
	} else {
		job.logf("installing %s (%d KB) into %s…", binName, len(binData)/1024, dest)
		if err := os.WriteFile(filepath.Join(dest, binName), binData, 0o755); err != nil {
			os.RemoveAll(dest)
			job.logf("cleanup: removed the partial install at %s", dest)
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return true
		}
		if isMachOBody(binData) { // PE/ELF carries no macOS seal to check
			sig = importSignatureState(filepath.Join(dest, binName), false, job)
		}
	}
	if len(icon) > 0 {
		_ = os.WriteFile(filepath.Join(dest, "icon.png"), icon, 0o644)
	}
	app, err := writeWrapManifest(dest, binName, appname, qs, job, sig)
	if err != nil {
		os.RemoveAll(dest)
		job.logf("cleanup: removed the partial install at %s", dest)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return true
	}
	if err := s.finalizeImportDest(dest, app.ID, finalize, w, job); err != nil {
		return true
	}
	s.importDone(w, app, job)
	return true
}

// extractZipAppBundle writes every zip entry under the archive's bundleRoot
// ("<wrapper…>/X.app/…") into dest, preserving the bundle's inner layout
// and permission bits. Deliberately no junk filter inside the bundle: the
// developer's seal covers the exact file set, and dropping a "junk" entry
// would break it. Zip-stored symlinks cannot be recreated by this code path
// (a seal broken that way is what the ad-hoc re-sign fallback repairs).
// Returns the extracted file count and byte total for the import log.
func extractZipAppBundle(zr *zip.Reader, bundleRoot, dest string) (int, int64, error) {
	prefix := strings.TrimSuffix(bundleRoot, "/") + "/"
	bundle := filepath.Base(prefix)
	files, total := 0, int64(0)
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok || rest == "" || f.FileInfo().IsDir() {
			continue
		}
		rel, err := manifestRelPath(filepath.Join(bundle, rest))
		if err != nil {
			return files, total, fmt.Errorf("unsafe archive entry %q: %v", name, err)
		}
		// Declared-size check before the read (the tar path's rule): a zip
		// header promising past-the-limit expansion is rejected without
		// writing a byte.
		if limit := expandMaxBytes(); limit > 0 && total+int64(f.UncompressedSize64) > limit {
			return files, total, fmt.Errorf("bundle expands past the import limit (%d bytes)", limit)
		}
		data, err := readZipEntryRaw(f, expandMaxBytes())
		if err != nil {
			return files, total, err
		}
		// A short read means the entry lied about its size or hit the read
		// cap — either way the bytes would install corrupt.
		if int64(len(data)) != int64(f.UncompressedSize64) {
			return files, total, fmt.Errorf("%s: got %d bytes, header declared %d", rest, len(data), f.UncompressedSize64)
		}
		total += int64(len(data))
		mode := f.Mode().Perm()
		if mode&0o111 != 0 {
			mode = 0o755
		} else if mode&0o400 == 0 {
			mode = 0o644
		}
		// Contents/MacOS holds the program and its helpers — executable by
		// bundle convention, and zips written without unix mode bits (Windows
		// tooling) must not strip that.
		if strings.HasPrefix(rest, "Contents/MacOS/") && mode != 0o755 {
			mode = 0o755
		}
		dst := filepath.Join(dest, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return files, total, err
		}
		if err := os.WriteFile(dst, data, mode); err != nil {
			return files, total, err
		}
		files++
	}
	if info, err := os.Stat(filepath.Join(dest, bundle, "Contents", "MacOS")); err != nil || !info.IsDir() {
		return files, total, fmt.Errorf("%s carries no Contents/MacOS — not a .app bundle", bundle)
	}
	return files, total, nil
}

// tarAppBundle finds a tar-family archive's .app-bundle program: the largest
// regular file directly inside <…>.app/Contents/MacOS — the rule the dmg
// path's appBundleBinary applies, because bundles ship helper dylibs in
// MacOS/lib (mpv's dwarf the program itself) that a largest-executable
// pick would mistake for the program. Foreign-platform programs are skipped
// like the zip path skips them; a foreign-only archive returns ok=false so
// the bin/sbin/games wrap takes its turn. The returned extract closure
// copies the winning bundle whole into the install folder (second pass, a
// fresh stream — same guards as extractZipAppBundle).
func tarAppBundle(body []byte) (bundleRoot, binPath string, icon []byte, extract func(string) (int, int64, error), ok bool) {
	opener, err := archiveOpener(body)
	if err != nil {
		return "", "", nil, nil, false
	}
	stream, err := opener()
	if err != nil {
		return "", "", nil, nil, false
	}
	defer stream.Close()

	type bundlePick struct {
		root    string
		size    int64
		binPath string
	}
	var best bundlePick
	icons := map[string][]byte{}
	var foreign string
	for {
		h, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", nil, nil, false
		}
		if h.mode.IsDir() || h.mode&os.ModeType != 0 || isJunkEntry(h.name) {
			continue
		}
		name := strings.TrimPrefix(filepath.ToSlash(h.name), "./")
		if i := strings.Index(name, ".app/Contents/MacOS/"); i >= 0 {
			leaf := name[i+len(".app/Contents/MacOS/"):]
			if leaf != "" && !strings.Contains(leaf, "/") && withinLimit(h.size, importMaxBytes()) {
				// The program's magic decides, not the mode bits (zip mode
				// bits taught this codebase that creators lie; tar lies less
				// but the content check is free here).
				head, err := io.ReadAll(io.LimitReader(stream, 512))
				if err == nil && looksExecutable(head) {
					if why := foreignBinaryReason(head); why != "" {
						if foreign == "" {
							foreign = fmt.Sprintf("%s is %s", filepath.Base(name), why)
						}
					} else if why := foreignArchReason(head); why != "" {
						if foreign == "" {
							foreign = fmt.Sprintf("%s is built for %s", filepath.Base(name), why)
						}
					} else if h.size > best.size {
						root := name[:i+len(".app")]
						best = bundlePick{root: root, size: h.size, binPath: root + "/Contents/MacOS/" + leaf}
					}
				}
				continue
			}
		}
		if i := strings.Index(name, ".app/Contents/Resources/"); i >= 0 {
			leaf := name[i+len(".app/Contents/Resources/"):]
			root := name[:i+len(".app")]
			if leaf != "" && !strings.Contains(leaf, "/") &&
				strings.EqualFold(filepath.Ext(leaf), ".icns") && h.size <= 4<<20 &&
				h.size > int64(len(icons[root])) {
				data, err := io.ReadAll(io.LimitReader(stream, h.size))
				if err == nil && int64(len(data)) == h.size {
					if png, ok := icnsToPNG(data); ok && len(png) > 0 {
						icons[root] = png
					}
				}
			}
		}
	}
	if best.root == "" {
		return "", "", nil, nil, false // no bundle (or foreign-only): tarProgram's turn
	}
	root := best.root
	extract = func(dest string) (int, int64, error) {
		return extractTarAppBundle(opener, root, dest)
	}
	return root, best.binPath, icons[root], extract, true
}

// extractTarAppBundle writes every regular tar entry under the archive's
// bundleRoot ("<…>/X.app") into dest, preserving the bundle's inner layout
// — the tar twin of extractZipAppBundle, with the same guards
// (manifestRelPath, expanded-size cap) and the same deliberate
// junk-filter exemption: the developer's seal covers the exact file set,
// and dropping a "junk" entry would break it. Symlinks are skipped like
// the zip path skips them; a seal broken that way is what the ad-hoc
// re-sign fallback repairs. Returns the extracted file count and byte
// total for the import log.
func extractTarAppBundle(opener func() (archiveStream, error), bundleRoot, dest string) (int, int64, error) {
	prefix := strings.TrimSuffix(bundleRoot, "/") + "/"
	bundle := filepath.Base(prefix)
	stream, err := opener()
	if err != nil {
		return 0, 0, err
	}
	defer stream.Close()
	files, total := 0, int64(0)
	for {
		h, err := stream.Next()
		if err == io.EOF {
			return files, total, nil
		}
		if err != nil {
			return files, total, err
		}
		name := strings.TrimPrefix(filepath.ToSlash(h.name), "./")
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok || rest == "" || h.mode.IsDir() || h.mode&os.ModeType != 0 {
			continue
		}
		rel, err := manifestRelPath(filepath.Join(bundle, rest))
		if err != nil {
			return files, total, fmt.Errorf("unsafe archive entry %q: %v", name, err)
		}
		if total+h.size > expandMaxBytes() {
			return files, total, fmt.Errorf("bundle expands past the import limit (%d bytes)", expandMaxBytes())
		}
		data, err := io.ReadAll(io.LimitReader(stream, h.size+1))
		if err != nil {
			return files, total, err
		}
		if int64(len(data)) != h.size {
			return files, total, fmt.Errorf("%s: got %d bytes, header declared %d", rest, len(data), h.size)
		}
		total += int64(len(data))
		mode := os.FileMode(0o644)
		if h.mode&0o111 != 0 {
			mode = 0o755
		}
		// Contents/MacOS holds the program and its helpers — executable by
		// bundle convention, and tars written without the exec bit must not
		// strip that.
		if strings.HasPrefix(rest, "Contents/MacOS/") {
			mode = 0o755
		}
		dst := filepath.Join(dest, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return files, total, err
		}
		if err := os.WriteFile(dst, data, mode); err != nil {
			return files, total, err
		}
		files++
	}
}

// zipProgramFromArchive picks the program to wrap out of a third-party zip
// with no app.json: the largest entry whose content carries an executable
// magic (Mach-O/PE/ELF), at any depth — release zips commonly wrap their
// binary in a folder (wordcraft-macos-universal/wordcraft), and zip mode
// bits are unreliable across creators, so the content decides. A zip with
// binaries for several platforms prefers one this machine can actually
// run; picking by size mirrors the tar path (tarProgram).
func zipProgramFromArchive(zr *zip.Reader) ([]byte, *zip.File, error) {
	var candidates []*zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || isJunkEntry(f.Name) {
			continue
		}
		executable, err := zipEntryLooksExecutable(f)
		if err != nil {
			return nil, nil, fmt.Errorf("reading %s from the zip: %w", filepath.ToSlash(f.Name), err)
		}
		if executable {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("no executable program found in the archive")
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].UncompressedSize64 > candidates[j].UncompressedSize64
	})
	var foreign, oversized string
	for _, f := range candidates {
		// A program past the body cap can never import — skip it with a
		// reason instead of reading it whole (mirrors the foreign skips).
		if v := importMaxBytes(); v > 0 && int64(f.UncompressedSize64) > v {
			if oversized == "" {
				oversized = fmt.Sprintf("%s is larger than the import body limit", filepath.Base(f.Name))
			}
			continue
		}
		data, err := readZipEntryRaw(f, importMaxBytes())
		if err != nil {
			return nil, nil, err
		}
		if why := foreignBinaryReason(data); why != "" {
			if foreign == "" {
				foreign = fmt.Sprintf("%s is %s", filepath.Base(f.Name), why)
			}
			continue
		}
		if why := foreignArchReason(data); why != "" {
			if foreign == "" {
				foreign = fmt.Sprintf("%s is built for %s", filepath.Base(f.Name), why)
			}
			continue
		}
		return data, f, nil
	}
	if foreign == "" && oversized != "" {
		return nil, nil, fmt.Errorf("%s — raise the import body limit in deck tools to install it", oversized)
	}
	return nil, nil, fmt.Errorf("%s — this launcher runs on %s/%s; send the build for this platform",
		foreign, runtime.GOOS, runtime.GOARCH)
}

// zipRootFile returns the archive entry for name at the root level — the
// archive root or inside its single wrapping folder (GitHub source zips
// wrap everything in "<repo>-<tag>/").
func zipRootFile(zr *zip.Reader, name string) *zip.File {
	for _, f := range zr.File {
		n := filepath.ToSlash(f.Name)
		if n == name {
			return f
		}
		if i := strings.Index(n, "/"); i >= 0 && n[i+1:] == name && !strings.Contains(n[:i], "/") {
			return f
		}
	}
	return nil
}

// zipComposeFile returns the compose manifest at the archive root, or "".
func zipComposeFile(zr *zip.Reader) string {
	for _, c := range composeManifests {
		if zipRootFile(zr, c) != nil {
			return c
		}
	}
	return ""
}

// installNodeZip imports a zip that wraps no executable but carries a root
// package.json (a Node.js source archive, e.g. a GitHub tag zip): the tree
// is extracted with the manifest archive's guards, an app.json is generated
// (npm install + entrypoint on first start) and the folder enters the
// normal folder import. Writes its own response.
func (s *Server) installNodeZip(w http.ResponseWriter, body []byte, zr *zip.Reader, qs map[string]string, job *jobLog) {
	job.logf("no executable in the archive, but a package.json — installing as a generated Node app…")
	opener, err := archiveOpener(body)
	if err != nil {
		job.finish(err.Error())
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	entries, _, err := scanArchive(opener)
	if err != nil {
		job.finish(err.Error())
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	prefix, _ := singleRootFolderNames(headerNames(entries))
	tmp, err := os.MkdirTemp("", "appdeck-node-")
	if err != nil {
		job.finish(err.Error())
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer os.RemoveAll(tmp)
	if err := extractArchiveTree(opener, prefix, tmp, entries); err != nil {
		job.finish(err.Error())
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if wrap := singleRootFolderDir(tmp); wrap != "" {
		tmp = filepath.Join(tmp, wrap)
	}
	name := strings.TrimSpace(qs["appname"])
	if name == "" {
		if f := zipRootFile(zr, "package.json"); f != nil {
			if data, err := readZipEntryRaw(f, importMaxBytes()); err == nil {
				var p packageJSON
				if json.Unmarshal(data, &p) == nil {
					name = strings.ReplaceAll(strings.TrimLeft(p.Name, "@"), "/", "-")
				}
			}
		}
	}
	if name == "" {
		name = "node-app"
	}
	if err := prepareGeneratedApp(tmp, name); err != nil {
		job.finish(err.Error())
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.importFolder(w, tmp, job)
}
