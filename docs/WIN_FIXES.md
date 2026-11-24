# FIXES — AppDeck Windows work log

Companion to [report.md](report.md) (E2E findings) and
[TROUBLESHOOTING.md](TROUBLESHOOTING.md) (symptom guide). One entry per fix:
problem, root cause, change, verification. Gate for every milestone:
`gofmt -l cmd/appdeck tools/genembedded && go vet ./... && go build ./... && go test ./...`
plus `go test -tags embed -count=1 ./cmd/appdeck/` — **all green after this round**
(default suite 12.7 s, embed suite 13.2 s, zero failures).

| ID | Area | Status |
|----|------|--------|
| F1 | Windows-clean test suite | **FIXED** — 9 default / 18 embed failures → 0 / 0 |
| F2 | Mach-O prebuilts packed / resolved wrong | **FIXED** (sources cleaned + build-time prune + runtime resolver) |
| F3 | promptpress UI not packed | **FIXED** |
| F4 | static-app uninstall resurrects | **FIXED** |
| F5 | Compress-Archive zip import fails | **FIXED** |
| F6 | embedded ports outside reserved range | **FIXED** |
| N1 | launcher env starves children of PATH | **FIXED** (root cause of SpaceLens picker + monitor collectors) |
| N2 | AppScopeMonitor dashboard empty | **FIXED** (by N1) |
| N3 | Descale won't start | **FIXED** (stale Mach-O + resolver) |
| N4 | Sentinel run-as-admin | **FIXED** (two layers: N1 + .exe resolution); elevated app *stopping* stays a UAC flow by design |
| N5 | pdfcraft / wordcraft MSI imports | **REFUSED BY DESIGN** — specific error added; no MSI silent-install support |
| N6 | locked exe aborts the whole boot unpack | **FIXED** (found during verification) |
| N7 | elevated apps can't be stopped after launcher restart | **FIXED** (UAC fallback now covers orphaned listeners too) |
| N8 | elevated start reported "could not start … check the app log" while the app actually ran | **FIXED** (outer `-Wait` was misplaced into the inner PowerShell's arguments) |
| D1 | Descale's AppDeck list empty on Windows | **FIXED** (listApps never appended it; plus the macOS-shaped data-dir path) |
| D2 | Uninstall not working in Descale for AppDeck apps | **FIXED** (Windows route sent everything to the registry-uninstaller path) |
| D3 | Sentinel firewall panel empty, yellow status | **FIXED** (PowerShell 5.1 serializes NetSecurity enums as numbers; parser now tolerant) |
| M1 | AppScopeMonitor missing country flags | **FIXED** (Windows has no emoji flag glyphs — UI now renders bundled SVG flags) |

## N1 — launcher environment starves every child of the system PATH

**Symptoms:** SpaceLens folder picker → `exec: "powershell": executable file not found in
%PATH%`; AppScopeMonitor collectors report `powershell.exe not found` and the dashboard shows
no data; the launcher's own UAC helpers (run-as-admin, firewall fix) exec `powershell` by
name and fail the same way.

**Root cause:** the launcher passes its own process environment to every app
(`childEnv`, main.go) and uses it for its own PowerShell invocations. When the launcher
process itself is started from a context whose `PATH` lacks
`C:\Windows\System32\WindowsPowerShell\v1.0\` (scheduler job, stripped shortcut, stripped
shell), that starvation cascades into every child. Proven by A/B test: the same monitor app
under a healthy-env launcher collected 216 live connections; under the starved launcher, 0.

**Change:**
- new `sysdirs_windows.go` / `sysdirs_other.go`: `systemBinDirs()` returns the Windows system
  dirs (System32, Wbem, WindowsPowerShell\v1.0, %SystemRoot%) — nil off Windows;
- `childEnv` appends them to every child's PATH (additions-only via `extendPath`, nothing the
  operator configured is shadowed);
- `main()` self-heals the launcher's own PATH at boot (`os.Setenv`) so its elevated helpers
  exec PowerShell regardless of how it was launched.

**Verified:** launcher relaunched with `PATH=C:\Windows\System32` (deliberately starved):
monitor collectors all `active` (privacy/files/network/devices/logins/appfocus/battery;
only android-adb "unavailable" — no adb installed), 340 live connections; SpaceLens
`POST /api/pick` now opens the real FolderBrowserDialog (the call blocks waiting for the
dialog instead of failing with exec-not-found).

## N2 — AppScopeMonitor dashboard empty

Root cause and fix: N1 (its Windows collectors shell out to PowerShell). Verified above.

## N3 — Descale won't start

**Root cause:** two stacked problems. (1) The source folders carried stale darwin prebuilts
(`descale`, `monitor`, `sentinel`, `invest-server`, `mythology` — all Mach-O magic
`cffaedfe`), which `copy_assets` packed as "assets" and extraction unpacked beside the real
`.exe`; the data dir also kept legacy extensionless binaries from older installs.
(2) `resolveBinaryLogged` preferred the plain extensionless name over `X.exe`, so the Mach-O
won and the start was refused ("cannot run on Windows: it is a macOS executable"). The same
Mach-O broke the elevated start ("…is not a valid Win32 application").

**Change:**
- stale prebuilts deleted from the five source folders (safe per AGENTS.md — apps rebuild on
  first start);
- `release.sh`: new `drop_foreign_binaries` sweeps extensionless Mach-O/ELF magic files out of
  the staged assets (extensionless files only, so data files are untouched);
- `resolveBinaryLogged`: on Windows the canonical name is `X.exe` — candidates are ordered
  `X.exe` → `X-<os>-<arch>.exe` → `X`; the platform-suffix migration now renames onto
  `X.exe`; an extensionless Mach-O/ELF sibling is removed when the real `.exe` exists
  (logged to the app's log).

**Verified:** descale starts (running, `/api/info` answers); investment + mythology start;
the stale Mach-O in the user's data dir was removed on first start. The rebuilt zip is
11.4 MB lighter (dead Mach-O weight gone).

## N4 — Sentinel run-as-admin

**Root cause (two layers):** (1) the outer hidden PowerShell (`priv_windows.go`) is exec'd by
name — broken by N1 in starved environments; (2) the elevated script runs the binary via
`Start-Process -FilePath`, which cannot execute an extensionless file — the resolver could
hand it the legacy extensionless PE. The elevated Sentinel instance observed on the machine
(PID 3800) proves the UAC flow itself works once the outer step resolves.

**Change:** N1 fix (outer step) + the Windows `.exe`-first resolution (inner step gets a real
`X.exe` path). Note: *stopping* an elevated app re-raises UAC by design — an un-elevated
launcher cannot kill an elevated process (Windows integrity levels); that is documented
behavior, not a bug.

**Verified interactively by the user's own elevated run; the two layers verified as above.
Left elevated descale/sentinel running on the machine — stop them from the dashboard (UAC) or
an elevated `taskkill`.**

## N5 — pdfcraft / wordcraft MSI imports

Both `https://github.com/storytold/pdfcraft/releases/download/v0.13.0/pdfcraft-0.13.0-windows-x64.msi`
and `https://github.com/storytold/wordcraft/releases/download/v0.10.0/wordcraft-0.10.0-windows-x64.msi`
download fine (71 MB / 81 MB from GitHub) and are **refused by the importer — by design**:
MSI packages have no safe silent+directory install into an app folder (msiexec has no
NSIS-style `/D=`), and the code only implements silent runs for NSIS and Inno. Previously the
refusal was the generic "unsupported file" (an MSI body carries the OLE compound magic, not
an MZ header, so it never reached the installer refusal); now `importBytes` routes MSIs to a
specific message: *"…is a Windows Installer package (MSI) — no safe silent install into the
app folder exists for it — import the app's portable .exe build instead"*, also persisted to
`logs/imports.log`. If portable `.exe` builds exist on those release pages, import those.

## F1 — test suite now Windows-clean

All 9 default-tag + 9 embed-only failures fixed at the test level (production behavior was
already correct in each case):
- POSIX permission-bit asserts skipped/guarded on Windows: `storage_test.go`
  (exec-bit copy, chmod-0555 dir), `tls_test.go` (key.pem 0600),
  `appstorage_test.go` (symlink-based storage-move tests);
- update fixtures now ship the host's own executable format via `nativeExeBytes()`
  (MZ + PE\0\0 + AMD64 on Windows, Mach-O on darwin): `update_test.go` (4 tests),
  `update_embed_test.go`;
- binary/tool fixtures use the platform spelling (`.exe` on Windows):
  `deckrebuild_test.go` (suffixed-sibling migration), `prereqs_test.go`
  (nginx/git tool lookup), `rebuild_test.go` (stale-binary + fresh-build paths),
  `update_test.go` (cargo tool registration, installedToolBinary path join);
- **one production fix fell out of the test work:** `resolveToolPath` demanded the unix exec
  bit, which Windows never reports — every directory-registered tool (bin/sbin/cmd walks)
  resolved to "" on Windows. Now any regular file with the tool's name resolves there.

## F3 — genembedded: embed heuristic

`goAppHasDiskAssets` treated *any* `//go:embed` as "self-contained", so PromptPress (which
embeds only a tokenizer file) shipped without its disk-served `web/` UI → 404. The directive
is now parsed: the app counts as self-contained only when a pattern references a web asset
root (`web/static/templates/css/js/ui/assets/dist`) or an `.html` page. SpaceLens
(`//go:embed index.html css js`) stays binary-only; PromptPress now packs `web/`.
**Verified:** promptpress serves 200 with its real UI.

## F4 — uninstall persistence for hosted-static apps

Uninstalling a hosted app (no registry folder) left its boot-created data-dir slug folder
(`app.json` + assets) behind, and folder discovery resurrected it every boot.
**Change:** `serveUninstall` removes `Apps/<slug(id)>` for hosted embedded apps, and
`loadApps` filters discovered manifest apps whose id is in `uninstalled.json` (overrides with
the `.appdeck-override` marker are untouched, so re-imports still work).
**Verified:** joss uninstall → folder deleted, gone after full restart; re-import restores it.

## F5 — zip entries with backslashes

PowerShell's `Compress-Archive` writes `dir\sub\file` entry names and directory entries with
a trailing backslash. `zipStream.Next` normalized the name but checked the dir bit on the
raw name, so a directory entry became a zero-byte *file* (`…/cmd`), and every deeper entry's
`MkdirAll` then failed ("The system cannot find the path specified").
**Change:** normalize first, then detect directories. **Verified:** a Compress-Archive zip of
DebtCrunch (leading dir entry + nested cmd/) and of Joss both import and serve.

## F6 — ports: reserved range + ports.json

genembedded invented static ports from 9100 for every port-0 app in the embedded registry,
so the runtime assigner (24500–25499, persisted in `ports.json`) never ran. genembedded now
leaves port 0 for portless web apps; the launcher assigns and remembers.
**Verified:** fresh boot → `ports.json` filled with 24500–24518 for the 19 auto-assigned
apps; no collisions with the fixed-port apps.

## N6 — locked exe aborted the whole unpack

Found while verifying against the live data dir: one running (elevated) app kept its
`X.exe` locked, and `extractApps` aborted there — every later app stayed stale and the boot
banner showed `unpacking apps failed`. Per-app extraction is now resilient: a locked/unreadable
app logs `app %q not refreshed this boot` and the rest unpack. Binaries refresh every boot,
so the next restart catches up.

## N7 — elevated apps un-stoppable once the launcher restarts

**Symptom:** Stop on Sentinel/Descale (both UAC-elevated) reported `stillRunning: true`
forever after the launcher that started them had exited; `taskkill` from the launcher's
standard token fails with access denied.

**Root cause:** `stopAppLocked` raised the elevated-taskkill fallback only for the
state-tracked PID *and* only when the state recorded `Privileged`. An elevated instance
whose state entry is gone (launcher restart, shutdown, stop-cycle delete) was reached only
by the port-listener kill path — which never elevates — so the kill silently failed and the
response carried no hint.

**Change:** after the normal + force kill passes, if the port is still held, the surviving
listeners get the same `stopPrivilegedElevated` UAC taskkill the privileged-start path uses;
the Stop response now explains the UAC prompt (including the declined case) whenever an
elevated fallback was attempted, not only when the state flagged it.

**Verified:** Stop on sentinel (orphan elevated listener) and descale → UAC prompt → both
stopped (`ok: true`, ports freed, processes gone). One historical sentinel orphan sat on its
*stale* old port (9113, from a pre-fix registry) which no current state tracks — cleaned
with a one-time elevated `taskkill /PID 3800 /T /F`; current builds track their elevated
starts in state, so Stop reaches them directly.

## N8 — elevated start reports failure while the app runs

**Symptom:** Start on Descale (elevated) answered `could not start Descale as administrator —
check the app log` — yet the app was running, untracked by the launcher (and therefore
un-stoppable by anything but the N7 orphan path).

**Root cause:** in `launchPrivileged` the outer `Start-Process powershell -Verb RunAs` lost
its own `-Wait`: the `'-Wait'` string sat inside `-ArgumentList …,'-Wait'`, i.e. it was an
argument of the *inner* PowerShell (harmlessly ignored after `-File`). The outer process —
whose exit the poll loop watches — therefore exited the moment UAC was approved and the
inner was spawned, the `exited` channel closed, and the launcher read "outer ended, no PID"
before the elevated script had written the pidfile. The start was reported as failed while
the script went on to launch the app.

**Change:** `-Wait` moved to the outer parameter list
(`Start-Process powershell -Verb RunAs -Wait -ArgumentList …`). The outer now stays alive
until the elevated script — and the app — exit, exactly what the design comment promised;
the pidfile is always seen first, and the returned channel again closes exactly when the
app does (Stop uses that to wait for an elevated app's death).

**Verified:** fresh elevated start → `ok: true, running: true, pid: 12280` (matches
tasklist and `appdata/descale/admin-launch.pid`), app serves on its port, state tracked.
Stop on a tracked elevated app raises UAC as designed; orphaned elevated apps are covered
by N7.

## D1/D2 — Descale: AppDeck list empty, uninstall not working

**Root cause (three stacked Windows gaps in `cmd/descale`):**
1. `listApps()` appended `listAppDeckApps()` on macOS only — the Windows (and Linux)
   branches returned registry/desktop entries alone, so the Uninstall tab's AppDeck
   sub-list was always empty.
2. `handleAppUninstall` on Windows routed *every* request to `uninstallWindows`
   (registry uninstaller lookup) — an AppDeck app was never in that list, so even a
   manually-sent request failed with "application is not in the scanned install list".
3. `appDeckAppsDir()` returned the macOS spelling
   (`~/Library/Application Support/AppDeck/Apps`) on every OS, so the appdeck entries
   that did appear (via the live-launcher list) carried a nonexistent path, breaking
   previews, the launcher-down fallback and path-matched lookups.

**Change:** Windows/Linux list branches append the AppDeck sub-list; appdeck-kind entries
on Windows fall through to the cross-platform flow (delegate to the launcher, or trash +
journal when it is down — the macOS behavior); `appDeckAppsDir()` honors
`LAUNCHER_DATA_DIR`, then `%LOCALAPPDATA%\AppDeck\Apps` on Windows.

**Verified live:** the list shows all 43 deck apps (123 registry + 43 appdeck entries);
importing a throwaway app and calling Descale's uninstall (preview → confirm) delegates to
the launcher — "app folder removed by AppDeck" — the folder and the deck entry are gone.

## D3 — Sentinel firewall panel empty, yellow status

**Symptom:** the firewall check reported `unavailable` with
`firewall response: json: cannot unmarshal number into Go struct field winFwDump.profiles.0.enabled of type bool`.

**Root cause:** Windows PowerShell 5.1's `ConvertTo-Json` serializes .NET enums as their
integer values. `Get-NetFirewallProfile`'s `Enabled` (GpoBoolean), `DefaultInbound/OutboundAction`,
`Direction`, `Action` and `Profile` (flags) are all NetSecurity enums — the whole dump failed
to unmarshal on the first number.

**Change:** the PowerShell script casts every enum value (`[bool]$_.Enabled`,
`[string]$_.Direction`, …) and the Go side gained tolerant field types
(`winFwEnabled`, `winFwAct`, `winFwDir`, `winFwProf`) that accept both the string and the
numeric form, mapping the enum integers (1 Inbound/Allow/Domain, 2 Outbound/Block/Private,
4 Public, bit-flags for Profile) back to names. Regression tests pin both shapes
(`TestParseWindowsFirewallJSONEnumEnabled`).

**Verified live:** the firewall check reports `status: ok` with profiles and rules populated.

## M1 — AppScopeMonitor country flags missing

**Root cause:** the UI rendered flags as emoji regional-indicator pairs
(`ccToFlag`, flag-emoji style). Windows ships no glyphs for those pairs — Segoe UI Emoji
renders the two letters instead — so every flag spot showed a bare letter pair
("CN CN China") and the flags were effectively missing. The backend geo pipeline was fine
(`geo_cc`/`geo_country` populated for all external connections).

**Change:**
- vendored the **flag-icons** SVG sprite set (MIT; 270 ISO 3166-1 flags, ~2 MB, license
  file included) into `web/flags/` — flags now render as inline `<img class="flag">`
  badges, fully offline (no runtime third-party requests — this is a privacy dashboard);
- `serveStatic` served only `filepath.Base(name)`, which flattened sub-folder assets —
  it now serves root-relative paths under `web/` traversal-proof, so `/web/flags/<cc>.svg`
  works (`.svg` was already in the content-type map);
- `flagImgHTML(cc)` replaces the emoji at every table-cell site (connections, destinations,
  blocks); the two `<option>` dropdowns use plain "CC — Name" labels (HTML is not rendered
  inside `<option>`, and the emoji pair degrades to letters on Windows);
- two pre-existing Windows test gaps fixed on the way: the geo-cache permission assert
  (0666 on Windows) and block-list tests interpolating raw backslash paths into JSON
  (invalid escape sequences) now marshal the request properly.

**Verified live:** `GET /web/flags/cn.svg` returns `200 image/svg+xml`, traversal attempts
(`/web/../server.go`) still 404, the served `app.js` carries the new renderer, and the
running instance shows flag badges for every resolved country.

## Verification environment

Fixed build: `Appdeck/release/appdeck-allinone-windows-amd64.zip` (SHA256SUMS alongside).
Full per-app results: [report.md](report.md) — **42/42 apps start, 41/42 web apps serve
HTTP 200 with real content (chronostamp is a desktop app, no port), 0 orphans** (aside from
the two elevated UAC apps above).
