// Prebuilt release assets: when a GitHub repository import could instead run
// a binary the project already publishes, it should — no clone, no toolchain,
// no build. This file finds and ranks a release's assets for the current
// OS/arch, and installs the best candidate through the same wrap path a
// portable .exe upload takes (writeWrapManifest), or — for archives without
// an app.json — by extracting and wrapping the runnable inside.
//
// Discovery is layered around the GitHub API's anonymous rate limit: the API
// first (precise, but ~60 req/hr per IP), then — when the quota is spent —
// the same redirect + expanded_assets scrape shields.io uses, which costs no
// quota. Both failing falls back to the ordinary clone import with a hint
// that names the releases page, so the manual path (download the asset, drop
// it into the import dialog) is always one click away.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// releaseAsset is one downloadable file of a GitHub release.
type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// ghReleaseInfo is the slice of a release the importer cares about.
type ghReleaseInfo struct {
	Tag    string
	Assets []releaseAsset
}

var (
	errGitHubRateLimited = errors.New("github api rate limit hit")
	errGitHubNotFound    = errors.New("no releases found")
)

// fetchBestGitHubRelease returns the release to source a prebuilt from, or
// (nil, nil) when the repository genuinely has none. Rate limits and network
// errors fall through to the quota-free HTML scrape before giving up.
func fetchBestGitHubRelease(owner, repo string) (*ghReleaseInfo, error) {
	info, err := fetchGitHubReleaseAPI(owner, repo)
	if err == nil {
		return info, nil
	}
	if errors.Is(err, errGitHubNotFound) {
		return nil, nil
	}
	if scraped, serr := fetchGitHubReleaseHTML(owner, repo); serr == nil && scraped != nil {
		return scraped, nil
	}
	return nil, err
}

// fetchGitHubReleaseAPI reads the latest release, with a fallback to the
// listing (releases/latest 404s for repositories whose newest release is a
// prerelease).
func fetchGitHubReleaseAPI(owner, repo string) (*ghReleaseInfo, error) {
	info, err := ghAPIRelease(owner, repo, "/releases/latest")
	if errors.Is(err, errGitHubNotFound) {
		if listed, lerr := ghAPIRelease(owner, repo, "/releases?per_page=5"); lerr == nil {
			return listed, nil
		}
	}
	return info, err
}

func ghAPIRelease(owner, repo, apiPath string) (*ghReleaseInfo, error) {
	req, err := http.NewRequest(http.MethodGet,
		"https://api.github.com/repos/"+owner+"/"+repo+apiPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "AppDeck")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound:
		return nil, errGitHubNotFound
	case resp.StatusCode == http.StatusForbidden:
		// Anonymous quota: X-RateLimit-Remaining hits 0. A private repo also
		// 403s on some API surfaces — either way the HTML scrape or the clone
		// path is the next move, so both map onward distinctly.
		if remaining, perr := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining")); perr == nil && remaining == 0 {
			return nil, errGitHubRateLimited
		}
		return nil, errGitHubNotFound
	default:
		return nil, fmt.Errorf("github api: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		TagName string         `json:"tag_name"`
		Assets  []releaseAsset `json:"assets"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("github api reply: %v", err)
	}
	info := &ghReleaseInfo{Tag: payload.TagName}
	for _, a := range payload.Assets {
		if strings.TrimSpace(a.URL) != "" && strings.TrimSpace(a.Name) != "" {
			info.Assets = append(info.Assets, a)
		}
	}
	if len(info.Assets) == 0 {
		return nil, errGitHubNotFound
	}
	return info, nil
}

// fetchGitHubReleaseHTML scrapes what the releases page itself loads: the
// /releases/latest redirect carries the tag, and the expanded_assets fragment
// lists the download links. Costs no API quota; any surprise (GitHub changes
// the markup, the redirect, …) just errors out and the caller moves on.
func fetchGitHubReleaseHTML(owner, repo string) (*ghReleaseInfo, error) {
	noRedirect := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedirect.Get("https://github.com/" + owner + "/" + repo + "/releases/latest")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return nil, fmt.Errorf("releases/latest: HTTP %d", resp.StatusCode)
	}
	loc, err := resp.Location()
	if err != nil || loc == nil {
		return nil, errors.New("releases/latest sent no location")
	}
	tag := path.Base(strings.TrimSuffix(loc.Path, "/"))
	if tag == "" || tag == "releases" {
		return nil, nil // repository has no releases at all
	}
	page, err := (&http.Client{Timeout: 30 * time.Second}).Get(
		"https://github.com/" + owner + "/" + repo + "/releases/expanded_assets/" + tag)
	if err != nil {
		return nil, err
	}
	defer page.Body.Close()
	if page.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("expanded_assets: HTTP %d", page.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(page.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return parseExpandedAssets(body, owner, repo, tag)
}

// assetHrefRe matches the download anchors inside an expanded_assets
// fragment (plain hrefs, no query strings on GitHub release downloads).
var assetHrefRe = regexp.MustCompile(`href="(/[^"/]+/[^"/]+/releases/download/[^"]+)"`)

// parseExpandedAssets pulls the download links out of the fragment's HTML.
// The shape is stable (plain anchors), so a targeted regexp beats an HTML
// dependency; misses simply produce no assets.
func parseExpandedAssets(body []byte, owner, repo, tag string) (*ghReleaseInfo, error) {
	info := &ghReleaseInfo{Tag: tag}
	prefix := "/" + owner + "/" + repo + "/releases/download/"
	seen := map[string]bool{}
	for _, m := range assetHrefRe.FindAllSubmatch(body, -1) {
		href := string(m[1])
		if !strings.HasPrefix(href, prefix) {
			continue
		}
		name, err := url.PathUnescape(path.Base(href))
		if err != nil || name == "" || seen[name] {
			continue
		}
		seen[name] = true
		info.Assets = append(info.Assets, releaseAsset{
			Name: name,
			URL:  "https://github.com" + href,
		})
	}
	if len(info.Assets) == 0 {
		return nil, errors.New("expanded_assets: no download links")
	}
	return info, nil
}

// ---- ranking ----------------------------------------------------------------

// releaseRejectExts are file types a card can never run: installers that need
// an OS wizard (msi/dmg/pkg), OS packages (deb/rpm/snap), and the metadata
// projects attach to releases (checksums, signatures, SBOMs, release notes).
var releaseRejectExts = map[string]bool{
	".txt": true, ".md": true, ".json": true, ".sig": true, ".asc": true,
	".pem": true, ".pub": true, ".sha256": true, ".sha512": true, ".md5": true,
	".sbom": true, ".torrent": true, ".yml": true, ".yaml": true, ".xml": true,
	".html": true, ".htm": true, ".pdf": true,
	".msi": true, ".dmg": true, ".pkg": true, ".deb": true, ".rpm": true,
	".snap": true, ".flatpak": true, ".appinstaller": true, ".iso": true,
	".zsync": true, ".b3": true, ".rsp": true,
}

// releaseRejectTokens reject by word: source tarballs, installers, debug
// symbol packages, headers/devel bundles. Token match is on separators
// (_, -, ., space), so "dev" never hits "devdrive".
var releaseRejectTokens = map[string]bool{
	"source": true, "sources": true, "src": true,
	"installer": true, "installers": true, "setup": true,
	"update": true, "updater": true, "upgrade": true,
	"devel": true, "dev": true, "debug": true, "symbols": true,
	"headers": true, "header": true,
	"doc": true, "docs": true, "manual": true, "man": true,
	"license": true, "licenses": true, "checksums": true, "checksum": true,
}

// rankReleaseAssets orders a release's downloadable files by how well they
// fit this launcher: strict OS+arch matches first, arch-neutral OS matches
// next, and genuinely ambiguous files (no OS hint in the name — like
// ImageMagick's "portable-Q16-x64.7z") last, because their real platform only
// shows in the contents and gets verified at install time. Rejected outright:
// wrong-OS/wrong-arch files, installers, and metadata.
func rankReleaseAssets(assets []releaseAsset, goos, goarch string) []releaseAsset {
	type scored struct {
		asset releaseAsset
		rank  int
		boost int
	}
	var kept []scored
	for _, a := range assets {
		name := strings.ToLower(filepath.Base(strings.ReplaceAll(a.Name, "\\", "/")))
		if ext := filepath.Ext(name); ext != "" && releaseRejectExts[ext] {
			continue
		}
		if tokens := releaseNameTokens(name); anyReleaseToken(tokens, releaseRejectTokens) {
			continue
		}
		osKind := assetOSKind(name)
		if osKind != "" && osKind != goos {
			continue
		}
		arch := assetArchKind(name)
		if arch != "" && arch != goarch {
			continue
		}
		sc := scored{asset: a}
		switch {
		case osKind == "":
			sc.rank = 3 // ambiguous: verify from the bytes at install time
		case arch == "":
			sc.rank = 2 // right OS, no arch statement
		default:
			sc.rank = 0
		}
		for _, t := range releaseNameTokens(name) {
			if t == "portable" || t == "standalone" || t == "static" {
				sc.boost = 2
				break
			}
		}
		if sc.boost == 0 && (isArchiveName(name) || isBareBinaryName(name, goos)) {
			sc.boost = 1
		}
		kept = append(kept, sc)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].rank != kept[j].rank {
			return kept[i].rank < kept[j].rank
		}
		if kept[i].boost != kept[j].boost {
			return kept[i].boost > kept[j].boost
		}
		return kept[i].asset.Name < kept[j].asset.Name
	})
	out := make([]releaseAsset, 0, len(kept))
	for _, k := range kept {
		out = append(out, k.asset)
	}
	return out
}

func releaseNameTokens(name string) []string {
	return strings.FieldsFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

func anyReleaseToken(tokens []string, banned map[string]bool) bool {
	for _, t := range tokens {
		if banned[t] {
			return true
		}
	}
	return false
}

// assetOSKind names the platform a filename declares: by extension first
// (bytes cannot lie about .exe), then by OS word tokens.
func assetOSKind(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".exe", ".msi":
		return "windows"
	case ".appimage":
		return "linux"
	case ".dmg", ".pkg":
		return "darwin"
	}
	tokens := releaseNameTokens(name)
	for _, t := range tokens {
		switch t {
		case "win", "windows", "win32", "win64", "mingw", "mingw64", "cygwin", "msvc":
			return "windows"
		case "mac", "macos", "macosx", "osx", "darwin", "apple":
			return "darwin"
		case "linux", "ubuntu", "debian", "fedora", "arch", "centos", "opensuse", "appimage", "manylinux":
			return "linux"
		}
	}
	return ""
}

// assetArchKind names the CPU a filename declares. Compound forms are checked
// as substrings before their prefixes ("x86_64" before "x86"); an absent
// statement is "" — neutral, never a mismatch.
func assetArchKind(name string) string {
	for _, s := range []string{"x86_64", "x86-64", "amd64", "x64", "win64"} {
		if strings.Contains(name, s) {
			return "amd64"
		}
	}
	for _, s := range []string{"arm64", "aarch64", "silicon"} {
		if strings.Contains(name, s) {
			return "arm64"
		}
	}
	for _, s := range []string{"i386", "i686", "win32", "x86"} {
		if strings.Contains(name, s) {
			return "386"
		}
	}
	return ""
}

func isArchiveName(name string) bool {
	for _, ext := range []string{".zip", ".7z", ".tar", ".gz", ".bz2", ".xz", ".zst", ".rar"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// isBareBinaryName covers the run-directly files each OS ships as assets:
// .exe on Windows, .AppImage on Linux; macOS release binaries are typically
// extensionless and land here unnamed (their bytes get sniffed instead).
func isBareBinaryName(name, goos string) bool {
	switch goos {
	case "windows":
		return strings.HasSuffix(name, ".exe")
	case "linux":
		return strings.HasSuffix(name, ".appimage")
	}
	return false
}

// ---- binary sniffing --------------------------------------------------------

func isELFBody(body []byte) bool {
	return len(body) >= 4 && body[0] == 0x7f && body[1] == 'E' && body[2] == 'L' && body[3] == 'F'
}

// isAppImageBody: an AppImage is an ELF whose bytes 8–10 carry "AI" + format
// version (0x01–0x03).
func isAppImageBody(body []byte) bool {
	return isELFBody(body) && len(body) >= 11 && body[8] == 'A' && body[9] == 'I' &&
		body[10] >= 0x01 && body[10] <= 0x03
}

func isMachOBody(body []byte) bool {
	if len(body) < 4 {
		return false
	}
	magic := []byte{body[0], body[1], body[2], body[3]}
	for _, m := range [][]byte{
		{0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf}, // MH_MAGIC(64)
		{0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe}, // byte-swapped
		{0xca, 0xfe, 0xba, 0xbe}, {0xbe, 0xba, 0xfe, 0xca}, // fat binaries
	} {
		if bytes.Equal(magic, m) {
			return true
		}
	}
	return false
}

// isHostBinary reports whether the bytes are a program this OS can exec:
// PE on Windows, Mach-O on macOS, ELF (AppImages included) on Linux.
func isHostBinary(body []byte) bool {
	switch runtime.GOOS {
	case "windows":
		return len(body) >= 2 && body[0] == 'M' && body[1] == 'Z'
	case "darwin":
		return isMachOBody(body)
	case "linux":
		return isELFBody(body)
	}
	return false
}

// ---- install ----------------------------------------------------------------

// importReleaseAssets tries the ranked candidates (top three — a misranked
// name falls through to the next) and installs the first that verifies.
// Returns whether an install happened, plus a human reason when it did not.
func (s *Server) importReleaseAssets(w http.ResponseWriter, rel *ghReleaseInfo, repo, appname string, qs map[string]string) (bool, string) {
	candidates := rankReleaseAssets(rel.Assets, runtime.GOOS, runtime.GOARCH)
	if len(candidates) == 0 {
		return false, fmt.Sprintf("release %s has no prebuilt asset for %s/%s",
			rel.Tag, runtime.GOOS, runtime.GOARCH)
	}
	var lastErr error
	for i, asset := range candidates {
		if i >= 3 {
			break
		}
		body, _, err := fetchForImport(asset.URL)
		if err != nil {
			lastErr = fmt.Errorf("%s: %v", asset.Name, err)
			continue
		}
		if err := verifyReleaseChecksum(rel, asset, body); err != nil {
			lastErr = fmt.Errorf("%s: %v", asset.Name, err)
			continue
		}
		app, err := s.installReleaseAsset(asset, body, appname, qs)
		if err != nil {
			lastErr = fmt.Errorf("%s: %v", asset.Name, err)
			continue
		}
		if app.Dir != "" {
			writeReleaseProvenance(app.Dir, repo, rel, asset, body)
		}
		note := fmt.Sprintf("installed prebuilt %s from release %s — it appears on the dashboard now",
			asset.Name, rel.Tag)
		if releaseHasChecksum(rel, asset) {
			note += " (sha256 verified)"
		}
		s.importDoneNote(w, app, note)
		return true, ""
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no usable prebuilt asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return false, fmt.Sprintf("prebuilt install failed (%s)", lastErr)
}

// releaseHasChecksum reports whether the release carries a .sha256 sidecar
// for the asset (many projects publish one next to each download).
func releaseHasChecksum(rel *ghReleaseInfo, asset releaseAsset) bool {
	_, ok := releaseChecksumAsset(rel, asset)
	return ok
}

func releaseChecksumAsset(rel *ghReleaseInfo, asset releaseAsset) (releaseAsset, bool) {
	want := strings.ToLower(asset.Name) + ".sha256"
	for _, a := range rel.Assets {
		if strings.ToLower(a.Name) == want {
			return a, true
		}
	}
	return releaseAsset{}, false
}

// verifyReleaseChecksum checks the downloaded bytes against the release's
// .sha256 sidecar when one exists — no sidecar, no check (the provenance
// record still carries the hash we observed).
func verifyReleaseChecksum(rel *ghReleaseInfo, asset releaseAsset, body []byte) error {
	sidecar, ok := releaseChecksumAsset(rel, asset)
	if !ok {
		return nil
	}
	data, _, err := fetchForImport(sidecar.URL)
	if err != nil {
		return fmt.Errorf("fetching %s: %v", sidecar.Name, err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return fmt.Errorf("%s is not a sha256 sum file", sidecar.Name)
	}
	sum := sha256.Sum256(body)
	if !strings.EqualFold(fields[0], hex.EncodeToString(sum[:])) {
		return fmt.Errorf("sha256 mismatch against %s — the download is not what the release published", sidecar.Name)
	}
	return nil
}

// releaseProvenance is the audit record written into an installed app's
// folder: where these bytes came from, when, and their observed hash —
// "arbitrary binary from the internet" becomes an answerable supply chain.
type releaseProvenance struct {
	Source     string `json:"source"`
	Repo       string `json:"repo"`
	Tag        string `json:"tag"`
	Asset      string `json:"asset"`
	URL        string `json:"url"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Verified   bool   `json:"checksumVerified"`
	ImportedAt string `json:"importedAt"`
}

func writeReleaseProvenance(dir, repo string, rel *ghReleaseInfo, asset releaseAsset, body []byte) {
	sum := sha256.Sum256(body)
	p := releaseProvenance{
		Source:     "github-release",
		Repo:       repo,
		Tag:        rel.Tag,
		Asset:      asset.Name,
		URL:        asset.URL,
		SHA256:     hex.EncodeToString(sum[:]),
		Size:       int64(len(body)),
		Verified:   releaseHasChecksum(rel, asset),
		ImportedAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "provenance.json"), data, 0o644)
}

// installReleaseAsset lands one asset in the deck: a bare binary is verified
// against this OS and wrapped as-is; an archive (release archives carry no
// app.json) is extracted and its runnable for this OS becomes the app. The
// staging/swap semantics of importDest apply — an existing install is only
// replaced after the new one fully validates.
func (s *Server) installReleaseAsset(asset releaseAsset, body []byte, appname string, qs map[string]string) (*App, error) {
	name := filepath.Base(strings.ReplaceAll(asset.Name, "\\", "/"))
	// The import dialog's kind=auto is a portable-exe notion; release
	// binaries are CLI/GUI programs, so they wrap as desktop cards.
	if qs["kind"] == "auto" {
		qs["kind"] = "desktop"
	}
	dest, finalize, err := s.stageImportDest(appname)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*App, error) {
		os.RemoveAll(dest)
		return nil, err
	}

	if detectArchiveFormat(body) != "" {
		opener, err := archiveOpener(body)
		if err != nil {
			return fail(err)
		}
		entries, _, err := scanArchive(opener)
		if err != nil {
			return fail(err)
		}
		prefix, _ := singleRootFolderNames(headerNames(entries))
		if err := extractArchiveTree(opener, prefix, dest, entries); err != nil {
			return fail(err)
		}
		bin, err := findRunnableBinary(dest)
		if err != nil {
			return fail(err)
		}
		if runtime.GOOS == "windows" {
			extractExeIconPNG(filepath.Join(dest, filepath.FromSlash(bin)), filepath.Join(dest, "icon.png"))
		}
		app, err := writeWrapManifest(dest, bin, appname, qs)
		if err != nil {
			return fail(err)
		}
		return app, s.finishReleaseImport(dest, app, finalize)
	}

	if kind := installerKind(name, body); kind != "" {
		return fail(fmt.Errorf("looks like an installer (%s) — skipped", kind))
	}
	if !isHostBinary(body) {
		return fail(fmt.Errorf("not a %s/%s binary or an archive", runtime.GOOS, runtime.GOARCH))
	}
	if err := os.WriteFile(filepath.Join(dest, name), body, 0o755); err != nil {
		return fail(err)
	}
	if runtime.GOOS == "windows" {
		extractExeIconPNG(filepath.Join(dest, name), filepath.Join(dest, "icon.png"))
	}
	app, err := writeWrapManifest(dest, name, appname, qs)
	if err != nil {
		return fail(err)
	}
	return app, s.finishReleaseImport(dest, app, finalize)
}

func (s *Server) finishReleaseImport(dest string, app *App, finalize func() error) error {
	if err := markEmbeddedOverride(dest, app.ID); err != nil {
		os.RemoveAll(dest)
		return err
	}
	if finalize == nil {
		return nil
	}
	if err := finalize(); err != nil {
		os.RemoveAll(dest)
		return err
	}
	return nil
}

// findRunnableBinary picks the executable an extracted release archive should
// wrap, for the running OS: the largest candidate wins (release bundles ship
// their launcher plus helpers), shallow paths beat deep ones.
func findRunnableBinary(dir string) (string, error) {
	type candidate struct {
		rel   string
		size  int64
		depth int
	}
	var best *candidate
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		lower := strings.ToLower(rel)
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		var runnable bool
		switch runtime.GOOS {
		case "windows":
			runnable = strings.HasSuffix(lower, ".exe") && !strings.HasSuffix(lower, "uninstall.exe")
		case "darwin":
			// A .app bundle's real program lives in Contents/MacOS; wrapping
			// the inner binary keeps the card launchable.
			runnable = strings.Contains(lower, ".app/contents/macos/") || fileHasMagic(p, isMachOBody)
		case "linux":
			runnable = fileHasMagic(p, isELFBody)
		}
		if !runnable || info.Size() < 4096 {
			return nil
		}
		c := candidate{rel: rel, size: info.Size(), depth: strings.Count(rel, "/")}
		if best == nil || c.depth < best.depth || (c.depth == best.depth && c.size > best.size) {
			best = &c
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if best == nil {
		return "", fmt.Errorf("the archive holds no runnable %s program", runtime.GOOS)
	}
	return best.rel, nil
}

func fileHasMagic(path string, match func([]byte) bool) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	return match(head[:n])
}
