// Launcher self-update: POST /api/update replaces the running launcher
// executable with the one inside an uploaded zip. The zip carries either a
// bare launcher binary or an AppDeck.app bundle (the release packaging) —
// in both cases only the executable file is swapped, in place, with the
// previous version kept beside it as <exe>.old for manual rollback. The
// running process keeps the old image until AppDeck is restarted, so the
// response asks for a restart and the dashboard offers one.
package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// serveUpdate answers POST /api/update. The body is the zip (or ?url= where
// to download one, reusing the import fetch with its size cap).
func (s *Server) serveUpdate(w http.ResponseWriter, r *http.Request) {
	qs := parseQuery(r.RequestURI)
	if v := qs["url"]; v != "" {
		if dec, err := url.PathUnescape(v); err == nil {
			qs["url"] = dec
		}
	}
	var body []byte
	if rawurl := qs["url"]; rawurl != "" {
		data, _, err := fetchForImport(rawurl)
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		body = data
	} else {
		data, err := io.ReadAll(io.LimitReader(r.Body, installMaxTotal+1))
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading upload: " + err.Error()})
			return
		}
		body = data
	}
	note, err := s.installLauncherUpdate(body)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"note": note,
	})
}

// installLauncherUpdate validates the zip, picks the launcher executable
// inside it, installs any runtime files that zip carries beside the
// executable, and swaps the running one. Returns the human note for the
// response. Both release shapes are accepted: the all-in-one zip (a bare
// binary, everything embedded) and the folder-mode release (an AppDeck.app
// whose Contents/MacOS/ carries index.html, apps.json and an Apps/ tree
// beside the launcher). Updating an embedded install with a folder-mode zip
// switches it to folder mode: the live deck is materialized into the
// registry beside the executable first (materializeFolderDeck), so the
// smaller release zip works without emptying the deck.
func (s *Server) installLauncherUpdate(body []byte) (string, error) {
	if detectArchiveFormat(body) != "zip" {
		return "", fmt.Errorf("send a zip containing the new launcher (a bare binary or an AppDeck.app bundle)")
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", fmt.Errorf("not a readable zip: %v", err)
	}
	exe, pick, err := launcherExecutableFromZip(zr)
	if err != nil {
		return "", err
	}
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot locate the running launcher: %v", err)
	}
	exePath, err = filepath.Abs(exePath)
	if err != nil {
		return "", err
	}
	// A folder-mode release zip carries its runtime files beside the
	// executable inside the bundle — index.html, apps.json, an Apps/
	// folder. They land next to the installed exe so the deck keeps working
	// when the update crosses build modes (all-in-one ↔ folder-mode).
	companions, err := installRuntimeCompanions(zr, pick, exePath)
	if err != nil {
		return "", fmt.Errorf("installing the launcher's runtime files: %v", err)
	}
	switched := ""
	if embeddedBuild && zipHasFolderRuntime(body) {
		kept, err := s.materializeFolderDeck(exePath)
		if err != nil {
			return "", fmt.Errorf("keeping the installed deck across the switch to folder mode: %v", err)
		}
		switched = fmt.Sprintf("; switched this install to folder mode with %d app(s) kept — the deck registry now lives beside the launcher (apps stay in the data directory)", kept)
	}
	newPath := exePath + ".new"
	// The backup goes OUTSIDE the .app bundle when the launcher lives in
	// one: a modified file inside Contents/MacOS invalidates the bundle's
	// Gatekeeper signature (verified across subcomponents), and a stale
	// .old inside the bundle breaks every later codesign pass.
	oldPath := exePath + ".old"
	if i := strings.Index(exePath, ".app/Contents/"); i > 0 {
		// Next to the bundle, not inside it: anything in the bundle root
		// invalidates the Gatekeeper signature ("unsealed contents").
		bundle := exePath[:i+4]
		oldPath = filepath.Join(filepath.Dir(bundle), filepath.Base(exePath)+".old")
	}
	if err := os.WriteFile(newPath, exe, 0o755); err != nil {
		return "", fmt.Errorf("staging the new launcher: %v", err)
	}
	if err := os.Chmod(newPath, 0o755); err != nil {
		os.RemoveAll(newPath)
		return "", err
	}
	if runtime.GOOS == "windows" {
		// Windows locks a running executable's path: replace-by-rename fails.
		// The dance that works: rename the running exe aside (allowed while
		// running), then rename the new one into the freed name.
		_ = os.Remove(oldPath)
		if err := os.Rename(exePath, oldPath); err != nil {
			os.RemoveAll(newPath)
			return "", fmt.Errorf("backing up the running launcher: %v", err)
		}
		if err := os.Rename(newPath, exePath); err != nil {
			_ = os.Rename(oldPath, exePath) // put the running one back
			os.RemoveAll(newPath)
			return "", fmt.Errorf("swapping in the new launcher: %v", err)
		}
	} else {
		// POSIX: backup by copy (the running process needs its exe path
		// intact until the atomic replace below).
		if cur, err := os.Open(exePath); err == nil {
			_ = os.Remove(oldPath)
			bak, err := os.OpenFile(oldPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
			if err == nil {
				_, _ = io.Copy(bak, cur)
				_ = bak.Close()
			}
			_ = cur.Close()
		}
		if err := os.Rename(newPath, exePath); err != nil {
			os.RemoveAll(newPath)
			return "", fmt.Errorf("swapping in the new launcher: %v", err)
		}
	}
	if runtime.GOOS == "darwin" {
		// Best effort: the launcher is ad-hoc signed at release time, and
		// Gatekeeper checks the modified bundle on next start.
		_ = exec.Command("codesign", "--force", "-s", "-", exePath).Run()
	}
	note := "launcher updated — restart AppDeck to run the new version " +
		"(the previous binary is kept as " + filepath.Base(oldPath) + ")"
	if companions > 0 {
		note += fmt.Sprintf("; installed %d runtime file(s) beside the launcher (folder-mode release)", companions)
	}
	note += switched
	return note, nil
}

// materializeFolderDeck carries the embedded deck across an update to a
// folder-mode launcher. Folder mode reads its registry from apps.json beside
// the executable and never scans the data directory's app store, so the swap
// alone would empty the deck. The live app list is written out with
// data-dir-relative paths — the folder-mode loader resolves app folders and
// binaries against the data directory when they are missing beside the exe —
// and hosted apps whose assets lived only inside the old exe get those
// assets unpacked to disk first. Returns how many apps the new registry
// keeps. The zip's own (empty) apps.json never overwrites this one:
// installRuntimeCompanions skips existing registry files.
func (s *Server) materializeFolderDeck(exePath string) (int, error) {
	apps := s.appsList()
	out := struct {
		Apps []*App `json:"apps"`
	}{Apps: make([]*App, 0, len(apps))}
	unpacked := 0
	for _, a := range apps {
		if a == nil || a.ID == "" {
			continue
		}
		c := *a
		// Hosted apps whose assets live only inside the running exe have no
		// folder on disk; a folder-mode launcher has no embedded FS to serve
		// them from. Unpack the packaged assets so they keep working.
		if c.ServedBy == "launcher" && c.Dir == "" {
			appDir := filepath.Join(s.cfg.DataDir, "Apps", slugify(c.ID))
			if err := extractTree(embeddedFS, "embedded/assets/"+c.ID, appDir, false); err != nil {
				return 0, fmt.Errorf("unpack assets for %s: %v", c.ID, err)
			}
			if entries, err := os.ReadDir(appDir); err != nil || len(entries) == 0 {
				continue // nothing was packaged for this id — cannot keep it
			}
			c.RawDir = filepath.ToSlash(filepath.Join("Apps", slugify(c.ID)))
			unpacked++
		}
		out.Apps = append(out.Apps, &c)
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return 0, err
	}
	regPath := filepath.Join(filepath.Dir(exePath), "apps.json")
	if err := os.WriteFile(regPath, append(data, '\n'), 0o644); err != nil {
		return 0, fmt.Errorf("writing %s: %v", filepath.Base(regPath), err)
	}
	fmt.Fprintf(os.Stderr, "launcher: folder-mode update: %d app(s) materialized beside the launcher (%d asset folder(s) unpacked)\n",
		len(out.Apps), unpacked)
	return len(out.Apps), nil
}

// installRuntimeCompanions writes a folder-mode zip's runtime files —
// everything sitting beside the picked executable inside Contents/MacOS/ —
// next to the installed launcher: plain files (index.html, apps.json,
// icons) overwrite; the Apps/ tree is written only where nothing exists
// yet, so an installed deck is never clobbered by an update. Returns how
// many files were written.
func installRuntimeCompanions(zr *zip.Reader, pick *zip.File, exePath string) (int, error) {
	prefix, _ := filepath.Split(filepath.ToSlash(pick.Name))
	if prefix == "" {
		return 0, nil // bare-binary zip: everything is inside the executable
	}
	destDir := filepath.Dir(exePath)
	written := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || f.Name == pick.Name {
			continue
		}
		name := filepath.ToSlash(f.Name)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(name, prefix)
		if rel == "" || isJunkEntry(rel) {
			continue
		}
		target := filepath.Join(destDir, filepath.FromSlash(rel))
		if strings.HasPrefix(rel, "Apps/") || rel == "apps.json" {
			if _, err := os.Stat(target); err == nil {
				continue // never overwrite the installed deck or its registry
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return written, err
		}
		data, err := readZipEntryRaw(f)
		if err != nil {
			return written, err
		}
		mode := f.Mode()
		if mode.IsRegular() && mode.Perm()&0o111 != 0 {
			mode = 0o755
		} else {
			mode = 0o644
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// readZipEntryRaw returns a file entry's contents without the executable
// sniff (runtime files are data, not programs).
func readZipEntryRaw(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, installMaxTotal+1))
}

// zipHasFolderRuntime reports whether an AppDeck.app zip carries the
// companion files used by folder-mode builds (index.html/apps.json beside
// the launcher) — the marker of a folder-mode release zip.
func zipHasFolderRuntime(body []byte) bool {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return false
	}
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		if strings.HasSuffix(name, "/Contents/MacOS/index.html") ||
			strings.HasSuffix(name, "/Contents/MacOS/apps.json") {
			return true
		}
	}
	return false
}

// launcherExecutableFromZip finds the launcher executable inside a zip: the
// Mach-O/PE/ELF file of an app bundle's MacOS folder, or a single bare file.
// Anything else is refused with what was actually found. Returns the bytes
// and the picked entry (the update flow extracts its neighbors).
func launcherExecutableFromZip(zr *zip.Reader) ([]byte, *zip.File, error) {
	var bare, bundle []string
	var bareFile, bundleFile *zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || isJunkEntry(f.Name) {
			continue
		}
		name := filepath.ToSlash(f.Name)
		isCandidate := false
		if i := strings.LastIndex(name, "Contents/MacOS/"); i >= 0 {
			rest := name[i+len("Contents/MacOS/"):]
			isCandidate = rest != "" && !strings.Contains(rest, "/")
		} else if !strings.Contains(name, "/") {
			isCandidate = true
		}
		if !isCandidate {
			continue
		}
		executable, err := zipEntryLooksExecutable(f)
		if err != nil {
			return nil, nil, fmt.Errorf("reading %s from zip: %w", name, err)
		}
		if !executable {
			continue
		}
		if strings.Contains(name, "Contents/MacOS/") {
			bundle = append(bundle, name)
			bundleFile = f
		} else {
			bare = append(bare, name)
			bareFile = f
		}
	}
	// An app bundle wins over stray files; exactly one bare file is the
	// fallback; anything else is ambiguous and refused.
	if len(bundle) > 1 {
		return nil, nil, fmt.Errorf("the zip holds several launcher executables (%s) — send the one launcher you want to install", strings.Join(bundle, ", "))
	}
	pick := bundleFile
	if pick == nil {
		if len(bare) == 1 {
			pick = bareFile
		} else if len(bare) > 1 {
			return nil, nil, fmt.Errorf("the zip holds several files (%s) — send a zip with just the launcher executable", strings.Join(bare, ", "))
		}
	}
	if pick == nil {
		return nil, nil, fmt.Errorf("the zip holds no launcher executable — include the launcher binary or an AppDeck.app bundle")
	}
	data, err := readZipEntryRaw(pick)
	if err != nil {
		return nil, nil, err
	}
	if !looksExecutable(data) {
		return nil, nil, fmt.Errorf("%s in the zip is not an executable", filepath.Base(pick.Name))
	}
	if why := foreignBinaryReason(data); why != "" {
		return nil, nil, fmt.Errorf("%s in the zip is a %s — this launcher runs on %s/%s; send the build for this platform",
			filepath.Base(pick.Name), why, runtime.GOOS, runtime.GOARCH)
	}
	if why := foreignArchReason(data); why != "" {
		return nil, nil, fmt.Errorf("%s in the zip is built for %s — this launcher runs on %s/%s; send the build for this platform",
			filepath.Base(pick.Name), why, runtime.GOOS, runtime.GOARCH)
	}
	return data, pick, nil
}

// foreignBinaryReason names a loader magic that cannot run on this machine:
// a PE on macOS/Linux, an ELF on macOS/Windows, a Mach-O elsewhere — the
// class of mistake where a cross-platform release zip installs a launcher
// that can never boot on the machine that downloaded it.
func foreignBinaryReason(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	pe := bytes.HasPrefix(b, []byte("MZ"))
	elf := bytes.HasPrefix(b, []byte{0x7F, 'E', 'L', 'F'})
	var macho bool
	if !pe && !elf {
		be := binary.BigEndian.Uint32(b)
		le := binary.LittleEndian.Uint32(b)
		const (
			macho32 = 0xFEEDFACE
			macho64 = 0xFEEDFACF
			fatBe   = 0xCAFEBABE
			fatLe   = 0xBEBAFECA
		)
		macho = be == macho32 || be == macho64 || be == fatBe ||
			le == macho32 || le == macho64 || le == fatLe
	}
	other := ""
	switch {
	case pe:
		other = "a Windows PE binary"
	case elf:
		other = "a Linux ELF binary"
	case macho:
		other = "a macOS Mach-O binary"
	}
	if other != "" && ((pe && runtime.GOOS != "windows") ||
		(elf && runtime.GOOS != "linux") || (macho && runtime.GOOS != "darwin")) {
		return other
	}
	return ""
}

// foreignArchReason reads the CPU architecture out of a same-OS binary
// header and names it when this machine could never run it. Fail-open: an
// unparseable or multi-arch (fat) header returns "" — today's behavior.
// Rosetta is respected on macOS (an amd64 binary on arm64 is fine), where
// Linux and Windows have no such translation layer.
func foreignArchReason(b []byte) string {
	le16 := binary.LittleEndian.Uint16
	le32 := binary.LittleEndian.Uint32
	host := runtime.GOARCH
	if bytes.HasPrefix(b, []byte{0x7F, 'E', 'L', 'F'}) && len(b) >= 20 {
		switch le16(b[18:20]) {
		case 62: // EM_X86_64
			if host == "arm64" || host == "386" {
				return "linux/amd64"
			}
		case 183: // EM_AARCH64
			if host != "arm64" {
				return "linux/arm64"
			}
		case 3: // EM_386
			if host == "amd64" || host == "arm64" {
				return "linux/386"
			}
		}
		return ""
	}
	if bytes.HasPrefix(b, []byte("MZ")) && len(b) >= 0x40 {
		if off := int(le32(b[0x3C:0x40])); off > 0 && off+6 <= len(b) &&
			bytes.HasPrefix(b[off:off+4], []byte("PE\x00\x00")) {
			switch le16(b[off+4 : off+6]) {
			case 0x8664: // IMAGE_FILE_MACHINE_AMD64
				if host == "arm64" || host == "386" {
					return "windows/amd64"
				}
			case 0xAA64: // ARM64
				if host != "arm64" {
					return "windows/arm64"
				}
			case 0x014C: // I386
				if host == "amd64" || host == "arm64" {
					return "windows/386"
				}
			}
		}
		return ""
	}
	const (
		macho64LE = 0xFEEDFACF // little-endian MH_MAGIC_64
		macho64BE = 0xCFFAEDFE
		cpuX86_64 = 0x01000007
		cpuARM64  = 0x0100000C
		cpuABI64  = 0x01000000
		cpuI386   = 7
	)
	be32 := binary.BigEndian.Uint32
	if len(b) >= 8 && (le32(b[0:4]) == macho64LE || be32(b[0:4]) == macho64BE) {
		cputype := le32(b[4:8])
		switch cputype {
		case cpuX86_64:
			if host == "arm64" {
				return "" // Rosetta runs it
			}
			if host != "amd64" {
				return "macOS x86_64"
			}
		case cpuARM64:
			if host != "arm64" {
				return "macOS arm64"
			}
		case cpuI386:
			if host == "arm64" {
				return "macOS i386"
			}
		}
	}
	// 32-bit thin Mach-O and fat binaries: fat carries every arch, 32-bit
	// Mach-O is a legacy shape — both stay fail-open.
	return ""
}

func zipEntryLooksExecutable(f *zip.File) (bool, error) {
	rc, err := f.Open()
	if err != nil {
		return false, err
	}
	defer rc.Close()
	header, err := io.ReadAll(io.LimitReader(rc, 4))
	if err != nil {
		return false, err
	}
	return looksExecutable(header), nil
}

// looksExecutable sniffs the loader magics the launcher ships as: Mach-O
// (thin big/little-endian and fat), PE (MZ), ELF.
func looksExecutable(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	if bytes.HasPrefix(b, []byte{0x7F, 'E', 'L', 'F'}) || bytes.HasPrefix(b, []byte("MZ")) {
		return true
	}
	be := binary.BigEndian.Uint32(b)
	le := binary.LittleEndian.Uint32(b)
	const (
		macho32 = 0xFEEDFACE
		macho64 = 0xFEEDFACF
		fatBe   = 0xCAFEBABE
		fatLe   = 0xBEBAFECA
	)
	return be == macho32 || be == macho64 || be == fatBe ||
		le == macho32 || le == macho64 || le == fatLe
}
