// Install-from-archive: third-party apps arrive as a zip or tar.gz (plain
// tar works too) of the app folder — manifest plus assets/binary — and are
// extracted into the deck's app container, where manifest discovery picks
// them up. The format is sniffed from magic bytes, not the file name.
// Extraction is hardening-critical: archive paths are untrusted, so every
// entry is cleaned and confined to the destination before a single byte is
// written (zip-slip guard), links and device nodes are skipped, and the
// file count and expanded size are capped.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	default:
		return ""
	}
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
		return nil, fmt.Errorf("unsupported archive — send a .zip, .tar.gz or .tar with %s at its root (see AUTHORING.md)", manifestFileName)
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
