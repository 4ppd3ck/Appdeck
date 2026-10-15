// Install-from-archive: third-party apps arrive as a zip, tar.gz (plain tar
// and rar work too) of the app folder — manifest plus assets/binary — and
// are extracted into the deck's app container, where manifest discovery
// picks them up. zip/tar are read in-process by the stdlib; rar goes
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
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	switch detectArchiveFormat(body) {
	case "zip":
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return nil, fmt.Errorf("not a readable zip: %v", err)
		}
		return func() (archiveStream, error) { return &zipStream{r: zr}, nil }, nil
	case "tar.gz", "tar":
		gz := detectArchiveFormat(body) == "tar.gz"
		return func() (archiveStream, error) { return newTarStream(body, gz) }, nil
	default:
		return nil, fmt.Errorf("unsupported archive — send a .zip, .tar.gz, .tar or .rar with %s at its root (see AUTHORING.md)", manifestFileName)
	}
}

func newTarStream(body []byte, gz bool) (archiveStream, error) {
	var r io.Reader = bytes.NewReader(body)
	var closer io.Closer
	if gz {
		g, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("not a readable gzip: %v", err)
		}
		r, closer = g, g
	}
	return &tarStream{tr: tar.NewReader(r), closer: closer}, nil
}

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
	mode := f.Mode()
	if strings.HasSuffix(f.Name, "/") || f.FileInfo().IsDir() {
		mode |= os.ModeDir
	}
	z.hdr = &archiveHeader{
		name: strings.ReplaceAll(f.Name, "\\", "/"),
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
// folder name comes from the manifest id; an existing install is never
// overwritten — remove it first.
func (s *Server) installArchiveBytes(body []byte) (*App, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("empty upload — send the archive file's raw bytes")
	}
	if len(body) > installMaxArchiveBytes {
		return nil, fmt.Errorf("archive too large (%d bytes, limit %d)", len(body), installMaxArchiveBytes)
	}
	if detectArchiveFormat(body) == "rar" {
		return s.installRarArchive(body)
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
	probe := &App{}
	if err := json.Unmarshal(manifestData, probe); err != nil {
		return nil, fmt.Errorf("%s: %v", manifestFileName, err)
	}
	if strings.TrimSpace(probe.ID) == "" {
		return nil, fmt.Errorf("%s has no id — cannot name the install folder", manifestFileName)
	}
	id := strings.TrimSpace(probe.ID)
	if embedded := embeddedAppIDs(); embedded[id] || embedded[slugify(id)] {
		return nil, embeddedDuplicateErr(id)
	}
	folder := slugify(probe.ID)

	dest, err := s.appsContainerDir()
	if err != nil {
		return nil, err
	}
	dest = filepath.Join(dest, folder)
	if _, err := os.Stat(dest); err == nil {
		// Already installed → update in place: the archive extracts into a
		// staging folder and swaps in only after the staged copy validates.
		if err := s.updateInstalledApp(dest, probe.ID, func(stage string) error {
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
			return nil, err
		}
	}
	app, err := loadAppManifest(dest)
	if err != nil {
		os.RemoveAll(dest)
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

// isJunkEntry filters what OS tooling sprinkles into archives — macOS
// Finder's "__MACOSX" metadata folder and .DS_Store / Thumbs.db files.
// Without this, a Finder-made tar.gz carries two root folders and the
// wrapper detection (and the manifest lookup) breaks for every mac dev.
func isJunkEntry(name string) bool {
	first, _, _ := strings.Cut(name, "/")
	if first == "__MACOSX" {
		return true
	}
	switch filepath.Base(name) {
	case ".DS_Store", "Thumbs.db":
		return true
	}
	return false
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
type rarTool struct {
	list    func(archive string) ([]string, error)
	extract func(archive, dest string) error
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
	return rarTool{
		list: func(archive string) ([]string, error) {
			out, err := runRarTool(exec.Command(bin, "-tf", archive))
			if err != nil {
				return nil, fmt.Errorf("%s: %v: %s", bin, err, out)
			}
			return strings.Split(out, "\n"), nil
		},
		extract: func(archive, dest string) error {
			out, err := runRarTool(exec.Command(bin, "-x", "-f", archive, "-C", dest))
			if err != nil {
				return fmt.Errorf("%s: %v: %s", bin, err, out)
			}
			return nil
		},
	}
}

func rar7z(bin string) rarTool {
	return rarTool{
		list: func(archive string) ([]string, error) {
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
		},
		extract: func(archive, dest string) error {
			out, err := runRarTool(exec.Command(bin, "x", "-y", "-o"+dest, archive))
			if err != nil {
				return fmt.Errorf("%s: %v: %s", bin, err, out)
			}
			return nil
		},
	}
}

func rarUnrar(bin string) rarTool {
	return rarTool{
		list: func(archive string) ([]string, error) {
			out, err := runRarTool(exec.Command(bin, "lb", archive))
			if err != nil {
				return nil, fmt.Errorf("%s: %v: %s", bin, err, out)
			}
			return strings.Split(out, "\n"), nil
		},
		extract: func(archive, dest string) error {
			out, err := runRarTool(exec.Command(bin, "x", "-y", archive, dest))
			if err != nil {
				return fmt.Errorf("%s: %v: %s", bin, err, out)
			}
			return nil
		},
	}
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
func (s *Server) installRarArchive(body []byte) (*App, error) {
	tool, err := findRarExtractor()
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", "appdeck-rar-*.rar")
	if err != nil {
		return nil, err
	}
	rarPath := tmp.Name()
	defer os.Remove(rarPath)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return nil, err
	}
	tmp.Close()

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
	defer os.RemoveAll(stage)
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
	if embedded := embeddedAppIDs(); embedded[id] || embedded[slugify(id)] {
		return nil, embeddedDuplicateErr(id)
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
	}
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
// or the Apps folder inside the data directory (single-file builds).
func (s *Server) appsContainerDir() (string, error) {
	base := s.cfg.ProjectRoot
	if base == "" {
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
