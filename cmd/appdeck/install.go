// Install-from-archive: third-party apps arrive as a zip, tar.gz (plain tar,
// tar.bz2/xz/zst and rar work too, as does 7z — the format a lot of projects
// ship their portable Windows builds in) of the app folder — manifest plus
// assets/binary — and are extracted into the deck's app container, where
// manifest discovery picks them up. zip/tar/7z are read in-process; rar goes
// through an external extractor (below). The format is sniffed from magic
// bytes, not the file name. Extraction is hardening-critical: archive paths
// are untrusted, so every entry is cleaned and confined to the destination
// before a single byte is written (zip-slip guard), links and device nodes
// are skipped, and the file count and expanded size are capped.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bodgit/sevenzip"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

const (
	installMaxArchiveBytes = 512 << 20 // upload cap
	installMaxFiles        = 20000     // archive bomb guard
	installMaxTotal        = 1 << 30   // expanded-bytes cap
	installMaxManifest     = 1 << 20   // app.json read cap during the scan
)

// ---- normalized archive streaming -----------------------------------------

// archiveHeader describes one entry; name is slash-separated, mode carries
// the type bits (dir, symlink, …) exactly as the extractor needs them.
type archiveHeader struct {
	name string
	size int64
	mode os.FileMode
}

// archiveStream is a sequential entry reader. Next returns io.EOF at the
// end; Read reads the current entry's data.
type archiveStream interface {
	Next() (*archiveHeader, error)
	Read(p []byte) (int, error)
	Close() error
}

// detectArchiveFormat sniffs the container from its magic bytes. Developer
// machines lie about file names and Content-Types; the bytes cannot.
func detectArchiveFormat(body []byte) string {
	switch {
	case len(body) >= 4 && bytes.HasPrefix(body, []byte("PK\x03\x04")):
		return "zip"
	case len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b:
		return "tar.gz"
	case len(body) >= 262 && string(body[257:262]) == "ustar":
		return "tar"
	case len(body) >= 6 && bytes.HasPrefix(body, []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}):
		return "tar.xz"
	case len(body) >= 4 && bytes.HasPrefix(body, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return "tar.zst"
	case len(body) >= 3 && bytes.HasPrefix(body, []byte("BZh")):
		return "tar.bz2"
	case len(body) >= 6 && bytes.HasPrefix(body, []byte("7z\xBC\xAF\x27\x1C")):
		return "7z"
	case isRARBody(body):
		return "rar"
	default:
		return ""
	}
}

// isRARBody matches both RAR signatures: v4 "Rar!\x1a\x07\x00" and
// v5 "Rar!\x1a\x07\x01\x00".
func isRARBody(body []byte) bool {
	return (len(body) >= 7 && bytes.HasPrefix(body, []byte("Rar!\x1a\x07\x00"))) ||
		(len(body) >= 8 && bytes.HasPrefix(body, []byte("Rar!\x1a\x07\x01\x00")))
}

// archiveOpener returns a factory for fresh streams over the archive. The
// installer walks it twice (scan, then extract), so a tar.gz decompresses
// twice — cheap next to the memory bound this keeps.
func archiveOpener(body []byte) (func() (archiveStream, error), error) {
	switch format := detectArchiveFormat(body); format {
	case "zip":
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return nil, fmt.Errorf("not a readable zip: %v", err)
		}
		return func() (archiveStream, error) { return &zipStream{r: zr}, nil }, nil
	case "tar", "tar.gz", "tar.bz2", "tar.xz", "tar.zst":
		return func() (archiveStream, error) { return newTarStream(body, format) }, nil
	case "7z":
		zr, err := sevenzip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return nil, fmt.Errorf("not a readable 7z: %v", err)
		}
		return func() (archiveStream, error) { return &sevenzipStream{r: zr}, nil }, nil
	default:
		return nil, fmt.Errorf("unsupported archive — send a .zip, .tar.gz, .tar, .7z or .rar with %s at its root (see AUTHORING.md)", manifestFileName)
	}
}

// newTarStream layers the right decompressor under a tar reader. tar.xz and
// tar.zst pull in the pure-Go codecs; the caller gets a fresh stack each
// time because the installer walks archives twice (scan, then extract).
func newTarStream(body []byte, format string) (archiveStream, error) {
	br := bytes.NewReader(body)
	var r io.Reader = br
	var closer io.Closer
	switch format {
	case "tar.gz":
		g, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("not a readable gzip: %v", err)
		}
		r, closer = g, g
	case "tar.bz2":
		r = bzip2.NewReader(br)
	case "tar.xz":
		x, err := xz.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("not a readable xz: %v", err)
		}
		r = x
	case "tar.zst":
		z, err := zstd.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("not a readable zstd: %v", err)
		}
		r = z
		// zstd.Decoder.Close returns nothing; wrap it for the closer slot.
		closer = closerFunc(z.Close)
	}
	return &tarStream{tr: tar.NewReader(r), closer: closer}, nil
}

// closerFunc adapts a no-result shutdown call to io.Closer.
type closerFunc func()

func (f closerFunc) Close() error { f(); return nil }

type zipStream struct {
	r   *zip.Reader
	i   int
	cur io.ReadCloser
	hdr *archiveHeader
}

func (z *zipStream) Next() (*archiveHeader, error) {
	if z.cur != nil {
		z.cur.Close()
		z.cur = nil
	}
	if z.i >= len(z.r.File) {
		return nil, io.EOF
	}
	f := z.r.File[z.i]
	z.i++
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open %s: %v", f.Name, err)
	}
	z.cur = rc
	// Normalize backslash entry names before anything reads them: PowerShell's
	// Compress-Archive — the zip writer a Windows machine has built in —
	// writes "dir\sub\file" segments and directory entries with a trailing
	// backslash. Left as-is, a directory entry reads as a zero-byte file, is
	// extracted over the would-be folder's path, and every deeper entry's
	// MkdirAll then fails ("The system cannot find the path specified").
	name := strings.ReplaceAll(f.Name, "\\", "/")
	mode := f.Mode()
	if strings.HasSuffix(name, "/") || f.FileInfo().IsDir() {
		mode |= os.ModeDir
	}
	z.hdr = &archiveHeader{
		name: name,
		size: int64(f.UncompressedSize64),
		mode: mode,
	}
	return z.hdr, nil
}

func (z *zipStream) Read(p []byte) (int, error) { return z.cur.Read(p) }

func (z *zipStream) Close() error {
	if z.cur != nil {
		z.cur.Close()
	}
	return nil
}

// sevenzipStream adapts bodgit/sevenzip to archiveStream. 7z archives carry
// no unix permission bits, so every regular file extracts 0o755 — release
// archives hold runnables, and data files gaining the exec bit is harmless.
type sevenzipStream struct {
	r   *sevenzip.Reader
	i   int
	cur io.ReadCloser
}

func (z *sevenzipStream) Next() (*archiveHeader, error) {
	if z.cur != nil {
		z.cur.Close()
		z.cur = nil
	}
	if z.i >= len(z.r.File) {
		return nil, io.EOF
	}
	f := z.r.File[z.i]
	z.i++
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open %s: %v", f.Name, err)
	}
	z.cur = rc
	mode := os.FileMode(0o755)
	if strings.HasSuffix(f.Name, "/") || f.Attributes&0x10 != 0 {
		mode = os.ModeDir | 0o755
	}
	return &archiveHeader{
		name: strings.ReplaceAll(f.Name, "\\", "/"),
		size: int64(f.UncompressedSize),
		mode: mode,
	}, nil
}

func (z *sevenzipStream) Read(p []byte) (int, error) { return z.cur.Read(p) }

func (z *sevenzipStream) Close() error {
	if z.cur != nil {
		z.cur.Close()
	}
	return nil
}

type tarStream struct {
	tr     *tar.Reader
	closer io.Closer
	hdr    *archiveHeader
}

func (t *tarStream) Next() (*archiveHeader, error) {
	h, err := t.tr.Next()
	if err != nil {
		return nil, err // io.EOF passthrough
	}
	mode := h.FileInfo().Mode()
	switch h.Typeflag {
	case tar.TypeSymlink, tar.TypeLink:
		// Links are never extracted; marking them as symlinks makes the
		// extractor skip them the same way it skips zip symlinks.
		mode = os.ModeSymlink | 0o755
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		mode = os.ModeNamedPipe | 0o755
	}
	t.hdr = &archiveHeader{
		name: strings.ReplaceAll(h.Name, "\\", "/"),
		size: h.Size,
		mode: mode,
	}
	return t.hdr, nil
}

func (t *tarStream) Read(p []byte) (int, error) { return t.tr.Read(p) }

func (t *tarStream) Close() error {
	if t.closer != nil {
		return t.closer.Close()
	}
	return nil
}

// ---- the installer ---------------------------------------------------------

// installArchiveBytes extracts an app archive into the deck's app container
// and returns the validated app. The archive must carry app.json at its
// root (optionally one wrapping folder, which is stripped). The destination
// folder name comes from the manifest id; an existing install is updated
// after its running process is stopped.
func (s *Server) installArchiveBytes(body []byte, job *jobLog) (*App, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("empty upload — send the archive file's raw bytes")
	}
	if len(body) > installMaxArchiveBytes {
		return nil, fmt.Errorf("archive too large (%d bytes, limit %d)", len(body), installMaxArchiveBytes)
	}
	if detectArchiveFormat(body) == "rar" {
		return s.installRarArchive(body, job)
	}
	opener, err := archiveOpener(body)
	if err != nil {
		return nil, err
	}

	// Scan pass: collect headers (bomb guards run here, on the declared
	// sizes) and capture any root-level app.json content. Nothing is written.
	entries, manifests, err := scanArchive(opener)
	if err != nil {
		return nil, err
	}
	job.logf("archive scanned — %d entr(ies), %d app.json manifest(s)", len(entries), len(manifests))

	prefix, _ := singleRootFolderNames(headerNames(entries))
	var manifestData []byte
	for orig, data := range manifests {
		if stripArchivePrefix(orig, prefix) == manifestFileName {
			manifestData = data
			break
		}
	}
	if manifestData == nil {
		return nil, fmt.Errorf("no %s at the archive root — see AUTHORING.md for the app contract", manifestFileName)
	}
	job.logf("app.json found — validating manifest…")
	probe := &App{}
	if err := json.Unmarshal(manifestData, probe); err != nil {
		return nil, fmt.Errorf("%s: %v", manifestFileName, err)
	}
	if strings.TrimSpace(probe.ID) == "" {
		return nil, fmt.Errorf("%s has no id — cannot name the install folder", manifestFileName)
	}
	id := strings.TrimSpace(probe.ID)
	folder := slugify(probe.ID)

	dest, err := s.appsContainerDir()
	if err != nil {
		return nil, err
	}
	job.logf("installing %s into %s…", id, filepath.Base(dest))
	dest = filepath.Join(dest, folder)
	job.logf("extracting %d entr(ies) into %s…", len(entries), dest)
	if _, err := os.Stat(dest); err == nil {
		// Already installed → update in place: the archive extracts into a
		// staging folder and swaps in only after the staged copy validates.
		job.logf("app already installed — updating in place…")
		if err := s.updateInstalledApp(dest, probe.ID, func(stage string) error {
			job.logf("extracting into the staging folder %s…", stage)
			return extractArchiveTree(opener, prefix, stage, entries)
		}); err != nil {
			return nil, err
		}
	} else {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return nil, err
		}
		if err := extractArchiveTree(opener, prefix, dest, entries); err != nil {
			os.RemoveAll(dest) // never leave a half-installed app behind
			job.logf("cleanup: removed the partial install at %s", dest)
			return nil, err
		}
		if err := markEmbeddedOverride(dest, id); err != nil {
			os.RemoveAll(dest)
			job.logf("cleanup: removed the partial install at %s", dest)
			return nil, err
		}
	}
	markRebuildOnImport(dest)
	app, err := loadAppManifest(dest)
	if err != nil {
		os.RemoveAll(dest)
		job.logf("cleanup: removed the partial install at %s", dest)
		return nil, fmt.Errorf("installed folder carries an invalid manifest: %v", err)
	}
	app.Source = "manifest"
	return app, nil
}

// scanArchive walks the archive once: entry count and expanded size are
// capped on the declared sizes, and every file named app.json (small —
// manifests are bounded) is captured for the manifest lookup. Symlinks and
// other special entries are remembered but never read.
func scanArchive(opener func() (archiveStream, error)) ([]archiveHeader, map[string][]byte, error) {
	stream, err := opener()
	if err != nil {
		return nil, nil, err
	}
	defer stream.Close()

	var entries []archiveHeader
	manifests := map[string][]byte{}
	var total int64
	for {
		h, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading archive: %v", err)
		}
		if isJunkEntry(h.name) {
			continue
		}
		if len(entries) >= installMaxFiles {
			return nil, nil, fmt.Errorf("archive holds more than %d entries — refusing", installMaxFiles)
		}
		if !h.mode.IsDir() && h.mode&os.ModeType == 0 { // regular files only
			total += h.size
			if total > installMaxTotal {
				return nil, nil, fmt.Errorf("archive expands past %d bytes — refusing", installMaxTotal)
			}
			// Capture app.json contents for the manifest lookup below; the
			// entry itself still lands in entries so extraction writes it.
			if filepath.Base(h.name) == manifestFileName && len(manifests) < 16 {
				data, err := io.ReadAll(io.LimitReader(stream, installMaxManifest+1))
				if err != nil {
					return nil, nil, fmt.Errorf("read %s: %v", h.name, err)
				}
				if int64(len(data)) <= installMaxManifest {
					manifests[h.name] = data
				}
			}
		}
		entries = append(entries, *h)
	}
	return entries, manifests, nil
}

// maxArchiveWrapping bounds how many single-entry archive shells
// installUpload peels (zip → tar.gz → tar.gz is the deepest sane shape).
const maxArchiveWrapping = 3

// singleInnerArchive reports whether an archive wraps exactly one regular
// (non-junk) entry whose content is itself a readable archive, and returns
// that inner archive's bytes — mpv's macOS release zips carry a lone
// mpv.tar.gz entry that is the payload to install, not the zip. Anything
// else (several payload entries, an unreadable stream, a non-archive
// single entry) returns ok=false and the caller proceeds with the outer
// bytes as before.
func singleInnerArchive(body []byte) (inner []byte, name string, ok bool) {
	switch detectArchiveFormat(body) {
	case "zip", "tar", "tar.gz", "tar.bz2", "tar.xz", "tar.zst", "7z":
	default:
		return nil, "", false // rar goes through the external extractor
	}
	opener, err := archiveOpener(body)
	if err != nil {
		return nil, "", false
	}
	stream, err := opener()
	if err != nil {
		return nil, "", false
	}
	defer stream.Close()
	for {
		h, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", false
		}
		if h.mode.IsDir() || h.mode&os.ModeType != 0 || isJunkEntry(h.name) {
			continue
		}
		if name != "" || h.size > installMaxArchiveBytes {
			return nil, "", false // more than one payload entry
		}
		data, err := io.ReadAll(io.LimitReader(stream, h.size+1))
		if err != nil || int64(len(data)) != h.size {
			return nil, "", false
		}
		inner, name = data, h.name
	}
	if name == "" || detectArchiveFormat(inner) == "" {
		return nil, "", false // no entry, or the single entry is not an archive
	}
	return inner, name, true
}

// extractArchiveTree writes every regular file of the archive into dest,
// confining each path to dest. Directory entries, symlinks and special
// entries are skipped; the exec bit survives (unix).
func extractArchiveTree(opener func() (archiveStream, error), prefix, dest string, entries []archiveHeader) error {
	stream, err := opener()
	if err != nil {
		return err
	}
	defer stream.Close()

	// Regular-file names after the wrapper strip; everything else skips.
	wanted := map[string]archiveHeader{}
	for _, e := range entries {
		name := stripArchivePrefix(e.name, prefix)
		if name == "" || e.mode.IsDir() || e.mode&os.ModeType != 0 {
			continue
		}
		wanted[name] = e
	}

	var total int64
	for {
		h, err := stream.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading archive: %v", err)
		}
		name := stripArchivePrefix(h.name, prefix)
		entry, ok := wanted[name]
		if !ok || entry.size != h.size {
			continue // dir, link, or an entry the scan rejected
		}
		rel, err := manifestRelPath(name) // the traversal guard
		if err != nil {
			return fmt.Errorf("unsafe archive entry %q: %v", name, err)
		}
		if total+h.size > installMaxTotal {
			return fmt.Errorf("archive expands past %d bytes — refusing", installMaxTotal)
		}
		data, err := io.ReadAll(io.LimitReader(stream, h.size+1))
		if err != nil {
			return fmt.Errorf("read %s: %v", name, err)
		}
		if int64(len(data)) != h.size {
			return fmt.Errorf("%s: got %d bytes, header declared %d", name, len(data), h.size)
		}
		total += int64(len(data))
		mode := os.FileMode(0o644)
		if h.mode&0o111 != 0 {
			mode = 0o755 // keep executables executable (unix)
		}
		dst := filepath.Join(dest, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, mode); err != nil {
			return err
		}
		stampMotW(dst, data) // Windows: mark downloaded executables for SmartScreen
	}
}

// singleRootFolderNames reports the archive's single wrapping folder: ok
// only when at least one entry exists and every entry lives under the same
// first path segment.
func singleRootFolderNames(names []string) (string, bool) {
	root := ""
	for _, name := range names {
		first, _, hasSub := strings.Cut(name, "/")
		if first == "" || first == ".." || strings.HasPrefix(first, ".") {
			return "", false
		}
		if !hasSub && name != first+"/" {
			return "", false // a file at the archive root → no wrapping folder
		}
		if root == "" {
			root = first
		} else if root != first {
			return "", false
		}
	}
	if root == "" {
		return "", false
	}
	return root, true
}

func stripArchivePrefix(name, prefix string) string {
	if prefix == "" {
		return name
	}
	rest, ok := strings.CutPrefix(name, prefix+"/")
	if !ok {
		return ""
	}
	return rest
}

// isJunkEntry filters what OS tooling and dev workflows sprinkle into app
// sources: macOS Finder's "__MACOSX" metadata folder and .DS_Store /
// Thumbs.db files, plus the VCS and tooling state a repository carries
// (.git, .github, .gitignore, .cargo, editor settings) — runtime junk for an
// installed app, and pure weight in an import. Folder copies and archive
// extraction share this filter; every path segment is checked, so a wrapping
// folder inside an archive cannot hide them.
func isJunkEntry(name string) bool {
	first, _, _ := strings.Cut(name, "/")
	if first == "__MACOSX" {
		return true
	}
	junkDirs := map[string]bool{
		".git": true, ".github": true, ".gitlab": true, ".circleci": true,
		".cargo": true, ".vscode": true, ".idea": true,
		".Trashes": true, ".Spotlight-V100": true, ".fseventsd": true,
		// data_raw is the convention for build-time-only converter input
		// (release.sh has always excluded it from packaged assets); a fresh
		// import must not drag hundreds of MB of it into the container.
		"data_raw": true,
	}
	junkFiles := map[string]bool{
		".DS_Store": true, "Thumbs.db": true, "desktop.ini": true,
		".gitignore": true, ".gitattributes": true, ".gitmodules": true,
		".gitkeep": true,
	}
	for _, seg := range strings.Split(filepath.ToSlash(name), "/") {
		if junkDirs[seg] {
			return true
		}
	}
	return junkFiles[filepath.Base(name)]
}

func headerNames(entries []archiveHeader) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.name)
	}
	return names
}

// ---- RAR, via an external extractor ----------------------------------------
//
// Go's stdlib cannot read RAR (the format is proprietary) and this launcher
// is stdlib-only, so RAR imports shell out to the first capable extractor
// found on the machine — bsdtar (the tar.exe shipped with Windows 10+ and
// macOS), 7-Zip or unrar. The extractor is never trusted: it unpacks into a
// dot-named staging folder inside the app container (ignored by discovery,
// same volume as the destination), and only what survives a guarded walk —
// regular files only, file-count and expanded-size caps, the junk filter —
// is copied on through the same validation every other import passes.

// rarTool adapts one external extractor: list entry names (for the
// fast-fail bomb cap) and extract the whole archive into a directory.
// rarTool is the machine's rar extractor: list names, extract the archive.
// log (set by the import flow, nil elsewhere) receives each command line
// before it runs — the import log shows exactly what executed.
type rarTool struct {
	name    string
	log     func(format string, args ...any)
	list    func(archive string) ([]string, error)
	extract func(archive, dest string) error
}

// cmdln logs one command line through the hook when one is attached.
func (t rarTool) cmdln(format string, args ...any) {
	if t.log != nil {
		t.log(format, args...)
	}
}

// runRarTool captures a command's output for error reporting and treats
// exit code 1 as a warning (7-Zip and unrar use it for non-fatal notes; a
// genuinely broken extraction fails the manifest validation right after).
func runRarTool(cmd *exec.Cmd) (string, error) {
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return text, nil
		}
	}
	return text, err
}

func rarBsdTar(bin string) rarTool {
	t := rarTool{name: bin}
	t.list = func(archive string) ([]string, error) {
		t.cmdln("$ %s -tf %s", bin, archive)
		out, err := runRarTool(exec.Command(bin, "-tf", archive))
		if err != nil {
			return nil, fmt.Errorf("%s: %v: %s", bin, err, out)
		}
		return strings.Split(out, "\n"), nil
	}
	t.extract = func(archive, dest string) error {
		t.cmdln("$ %s -x -f %s -C %s", bin, archive, dest)
		out, err := runRarTool(exec.Command(bin, "-x", "-f", archive, "-C", dest))
		if err != nil {
			return fmt.Errorf("%s: %v: %s", bin, err, out)
		}
		return nil
	}
	return t
}

func rar7z(bin string) rarTool {
	t := rarTool{name: bin}
	t.list = func(archive string) ([]string, error) {
		t.cmdln("$ %s l -slt %s", bin, archive)
		out, err := runRarTool(exec.Command(bin, "l", "-slt", archive))
		if err != nil {
			return nil, fmt.Errorf("%s: %v: %s", bin, err, out)
		}
		// -slt prints one block per entry after the first "----------"
		// separator; the "Path = " lines are the entry names.
		var names []string
		afterHeader := false
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimRight(line, "\r")
			if strings.HasPrefix(line, "----------") {
				afterHeader = true
				continue
			}
			if !afterHeader {
				continue
			}
			if p, ok := strings.CutPrefix(line, "Path = "); ok {
				if p = strings.TrimSpace(p); p != "" {
					names = append(names, p)
				}
			}
		}
		return names, nil
	}
	t.extract = func(archive, dest string) error {
		t.cmdln("$ %s x -y -o%s %s", bin, dest, archive)
		out, err := runRarTool(exec.Command(bin, "x", "-y", "-o"+dest, archive))
		if err != nil {
			return fmt.Errorf("%s: %v: %s", bin, err, out)
		}
		return nil
	}
	return t
}

func rarUnrar(bin string) rarTool {
	t := rarTool{name: bin}
	t.list = func(archive string) ([]string, error) {
		t.cmdln("$ %s lb %s", bin, archive)
		out, err := runRarTool(exec.Command(bin, "lb", archive))
		if err != nil {
			return nil, fmt.Errorf("%s: %v: %s", bin, err, out)
		}
		return strings.Split(out, "\n"), nil
	}
	t.extract = func(archive, dest string) error {
		t.cmdln("$ %s x -y %s %s", bin, archive, dest)
		out, err := runRarTool(exec.Command(bin, "x", "-y", archive, dest))
		if err != nil {
			return fmt.Errorf("%s: %v: %s", bin, err, out)
		}
		return nil
	}
	return t
}

// findRarExtractor probes the machine for a tool that can read RAR. GNU tar
// cannot, so a bare "tar" only counts when its --version banner proves it
// is libarchive/bsdtar — and on Windows the bsdtar shipped with the OS is
// probed explicitly, because Git's GNU tar usually shadows it on PATH.
// 7-Zip's and WinRAR's installers do not touch PATH either, so their
// default install locations are probed too.
func findRarExtractor() (rarTool, error) {
	if _, err := exec.LookPath("bsdtar"); err == nil {
		return rarBsdTar("bsdtar"), nil
	}
	for _, bin := range tarCandidates() {
		if out, err := runRarTool(exec.Command(bin, "--version")); err == nil &&
			(strings.Contains(out, "bsdtar") || strings.Contains(out, "libarchive")) {
			return rarBsdTar(bin), nil
		}
	}
	for _, bin := range []string{"7zz", "7z", "7za"} {
		if _, err := exec.LookPath(bin); err == nil {
			return rar7z(bin), nil
		}
	}
	for _, bin := range sevenZipCandidates() {
		if fileExists(bin) {
			return rar7z(bin), nil
		}
	}
	if _, err := exec.LookPath("unrar"); err == nil {
		return rarUnrar("unrar"), nil
	}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		if bin := filepath.Join(pf, "WinRAR", "UnRAR.exe"); fileExists(bin) {
			return rarUnrar(bin), nil
		}
	}
	return rarTool{}, fmt.Errorf("no RAR reader on this machine — install 7-Zip (https://7-zip.org) " +
		"or unrar (zip and tar.gz imports need no extra tools), then import the .rar again")
}

// tarCandidates is the list of tar executables worth version-checking:
// whatever PATH holds plus the bsdtar shipped with Windows 10+ (Git for
// Windows puts GNU tar — which cannot read RAR — ahead of it on PATH).
func tarCandidates() []string {
	candidates := []string{"tar"}
	if sysroot := os.Getenv("SystemRoot"); sysroot != "" {
		candidates = append(candidates, filepath.Join(sysroot, "System32", "tar.exe"))
	}
	return candidates
}

// sevenZipCandidates covers 7-Zip's default install locations on Windows
// (its installer does not add itself to PATH).
func sevenZipCandidates() []string {
	var candidates []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if pf := os.Getenv(env); pf != "" {
			candidates = append(candidates, filepath.Join(pf, "7-Zip", "7z.exe"))
		}
	}
	return candidates
}

// singleRootFolderDir reports an extracted tree's single wrapping folder:
// set only when the directory holds exactly one non-junk entry, it is a
// folder, and no file sits beside it (a file at the root means no wrapper).
// The RAR twin of singleRootFolderNames, read from disk because external
// tools cannot be relied on for a clean listing.
func singleRootFolderDir(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var dirs []string
	for _, e := range entries {
		if isJunkEntry(e.Name()) {
			continue
		}
		if !e.IsDir() {
			return ""
		}
		dirs = append(dirs, e.Name())
	}
	if len(dirs) != 1 {
		return ""
	}
	return dirs[0]
}

// installRarArchive extracts a RAR through an external tool into staging
// and then follows the same rules as zip/tar: wrapper folder stripped, junk
// ignored, caps enforced, app.json at the root validated, existing installs
// updated via the staged swap.
func (s *Server) installRarArchive(body []byte, job *jobLog) (*App, error) {
	tool, err := findRarExtractor()
	if err != nil {
		return nil, err
	}
	// From here on the tool's own commands echo into the import log.
	tool.log = job.logf
	job.logf("rar extractor: %s", tool.name)
	tmp, err := os.CreateTemp("", "appdeck-rar-*.rar")
	if err != nil {
		return nil, err
	}
	rarPath := tmp.Name()
	defer func() {
		os.Remove(rarPath)
		job.logf("cleanup: removed the staged archive %s", rarPath)
	}()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return nil, err
	}
	tmp.Close()
	job.logf("archive staged at %s (%d KB)", rarPath, len(body)/1024)

	// Fast-fail bomb cap on the listing; the exact caps run on the
	// extracted tree below.
	names, err := tool.list(rarPath)
	if err != nil {
		return nil, fmt.Errorf("reading the RAR: %v", err)
	}
	if len(names) > installMaxFiles {
		return nil, fmt.Errorf("archive holds more than %d entries — refusing", installMaxFiles)
	}

	container, err := s.appsContainerDir()
	if err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(container, ".rar-stage-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		os.RemoveAll(stage)
		job.logf("cleanup: removed the extraction stage %s", stage)
	}()
	if err := tool.extract(rarPath, stage); err != nil {
		return nil, err
	}
	root := filepath.Join(stage, singleRootFolderDir(stage))

	// The guarded walk: junk dropped, symlinks/devices dropped, caps exact.
	var files int
	var total int64
	var manifestData []byte
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if isJunkEntry(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		files++
		if files > installMaxFiles {
			return fmt.Errorf("archive holds more than %d files — refusing", installMaxFiles)
		}
		total += info.Size()
		if total > installMaxTotal {
			return fmt.Errorf("archive expands past %d bytes — refusing", installMaxTotal)
		}
		if filepath.ToSlash(rel) == manifestFileName && info.Size() <= installMaxManifest {
			if data, rerr := os.ReadFile(path); rerr == nil && len(data) <= installMaxManifest {
				manifestData = data
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if manifestData == nil {
		return nil, fmt.Errorf("no %s at the archive root — see AUTHORING.md for the app contract", manifestFileName)
	}
	probe := &App{}
	if err := json.Unmarshal(manifestData, probe); err != nil {
		return nil, fmt.Errorf("%s: %v", manifestFileName, err)
	}
	id := strings.TrimSpace(probe.ID)
	if id == "" {
		return nil, fmt.Errorf("%s has no id — cannot name the install folder", manifestFileName)
	}
	dest := filepath.Join(container, slugify(id))
	if _, err := os.Stat(dest); err == nil {
		// Already installed → update in place through the staged swap.
		if err := s.updateInstalledApp(dest, id, func(target string) error {
			return copyAppFolder(root, target)
		}); err != nil {
			return nil, err
		}
	} else {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return nil, err
		}
		if err := copyAppFolder(root, dest); err != nil {
			os.RemoveAll(dest)
			return nil, err
		}
		if err := markEmbeddedOverride(dest, id); err != nil {
			os.RemoveAll(dest)
			return nil, err
		}
	}
	markRebuildOnImport(dest)
	app, err := loadAppManifest(dest)
	if err != nil {
		os.RemoveAll(dest)
		return nil, fmt.Errorf("installed folder carries an invalid manifest: %v", err)
	}
	app.Source = "manifest"
	return app, nil
}

// appsContainerDir is where installed archives land, matching what manifest
// discovery scans: the Apps group folder beside the launcher (folder mode)
// or the Apps folder inside the data directory (single-file builds). A
// bundled launcher (.app) must never install beside itself: Contents/ is
// inside the code-signature seal, and writing there breaks Gatekeeper —
// its apps land in the data directory instead.
func (s *Server) appsContainerDir() (string, error) {
	base := s.cfg.ProjectRoot
	if base == "" || insideAppBundle(base) {
		base = s.cfg.DataDir
	}
	if base == "" {
		return "", fmt.Errorf("no app container available in this build mode")
	}
	dir := filepath.Join(base, "Apps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// insideAppBundle reports whether dir sits inside a macOS .app bundle
// (…/X.app/Contents[/MacOS]) — the sealed region a launcher install must
// never write into. Same shape as the exe check in installLauncherUpdate.
func insideAppBundle(dir string) bool {
	return strings.Index(filepath.ToSlash(dir), ".app/Contents") > 0
}

// appsContainerPaths locates the data directory's Apps container: its path
// and the RawDir prefix matching its real on-disk name. On case-insensitive
// macOS filesystems a folder created as "apps" by older builds stays
// lowercase on disk, and the prefix has to match what was installed — the
// binary resolver only uses the base name, so this only has to stay
// self-consistent.
func appsContainerPaths(dataDir string) (prefix, container string) {
	name := "Apps"
	if entries, err := os.ReadDir(dataDir); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.EqualFold(e.Name(), "Apps") {
				name = e.Name()
				break
			}
		}
	}
	return name + "/", filepath.Join(dataDir, name)
}

// slugify reduces an app id to a safe folder name (same shape as
// genembedded's slug, so dev and release layouts agree).
func slugify(name string) string {
	s := strings.ToLower(strings.NewReplacer(" ", "-", "_", "-", "'", "", "\"", "").Replace(strings.TrimSpace(name)))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "-")
}
