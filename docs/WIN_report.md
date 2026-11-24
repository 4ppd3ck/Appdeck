# AppDeck all-in-one Windows E2E verification — 2026-10-24

**Overall verdict: NOT SHIP-READY on Windows.** The core machinery works (build, first boot,
unpacking, start/stop, import, boot-refresh, update-in-place), but **3 of 42 shipped apps are
broken on Windows**, **uninstalling a hosted-static app doesn't stick**, **Windows-native zips
fail to import**, and the **test suite has 9/18 unguarded unix-only failures**. Details and
repros below. Fixes tracked in [FIXES.md](FIXES.md); operator-facing symptom guide in
[TROUBLESHOOTING.md](TROUBLESHOOTING.md).

Environment: Windows 10/11 x64, Git for Windows (sh), go1.27.1 windows/amd64. The tree is not a
git repository; no git commands were run. Files edited only where a check failed.

## Pass/fail matrix

| # | Check | Result | Evidence |
|---|-------|--------|----------|
| 0a | `gofmt -l` / `go vet` / `go build` | **PASS** | all clean; go1.27.1 windows/amd64 |
| 0b | `go test ./...` | **FAIL** | 9 failures in `cmd/appdeck` — all unix-only test fixtures (see F1) |
| 0c | `go test -tags embed -count=1` | **FAIL** | 18 failures — same class (POSIX modes, Mach-O fixtures, symlinks) |
| 0d | Pack-set audit | **PASS** | 42 folders classify (18 Go, 24 static); **no `pack: false` anywhere**; DroidScopeMonitor/SpaceLens not in apps.json but discovered via their manifests (`droidview`, `spacelens`) |
| 1a | `release.cmd --all-in-one windows/amd64` | **PASS** | exit 0; `genembedded: 42 apps (24 static, 18 go)`; zero `note:` lines |
| 1b | Stale-artifact sweep | **PASS** | old `AppDeck-darwin-arm64.zip`+SHA256SUMS swept; release/ has exactly zip+SHA256SUMS |
| 1c | Checksum + single exe | **PASS** | `9b921492…7f4fe` zip == SHA256SUMS (CertUtil); single entry, exe **278,907,392 bytes** |
| 1d | Packed set matches expectation | **PASS** | 42/42 in build log, no pack:false leak, no drops |
| 2a | Fresh boot, data dir created | **PASS** | `%LocalAppData%\AppDeck` created; `/api/health` → `mode: single-file`, 42 configured |
| 2b | Apps unpack `app.json` + binary, no `bin/` | **PASS** | e.g. `Apps/descale/{app.json, descale.exe, css/, js/}`; no legacy `bin/` |
| 2c | `ports.json` in 24500–25499 at boot | **FAIL** | not created at boot; embedded apps carry static ports **9100–9118** from genembedded (see F6) |
| 3a | Every embedded app starts | **FAIL** | 40/42 OK; `investment` + `mythology` refuse (Mach-O, F2); `chronostamp` (desktop) OK via PID |
| 3b | HTTP 200 + real page per app | **FAIL** | 39/40 serve real pages; `promptpress` 404 (F3) |
| 3c | Deep checks | **PASS** | Descale UI + `/api/info\|journal\|guardrails\|traces\|startup` 200; AppScopeMonitor dashboard + SSE streaming live events; IPolice `/api/status` JSON (7618 networks); Sentinel `/api/status` 200 |
| 3d | Second start fast, binary present | **PASS** | descale restart 0.8 s; `.exe` in unpacked folder |
| 3e | Stop all, no orphans | **PASS** | `/api/all/stop` → 0 running; `tasklist` clean |
| 4a | Uninstall embedded app stays gone | **PARTIAL** | Go app `debtcrunch`: folder deleted, stays gone after restart ✓; hosted-static `joss`: folder survives, **resurrects after restart** ✗ (F4); reimport of joss via zip worked |
| 4b | Boot-refresh of web assets | **PASS** | marker in MoonlitArcana rebuilt → unpacked copy refreshed on boot, served content updated (marker reverted after; sha256 matches original) |
| 4c | Third-party zip import | **PARTIAL** | python-made zip: lands in `Apps/`, discovered, serves on 24501, `logs/imports.log` job log "ok" ✓; **Compress-Archive zip of a Go app fails** with mkdir error (F5); imports.log only written when request carries `jobid` |
| 4d | `--only descale,sentinel` | **PASS** | `genembedded: 2 apps`; exe (run with `LAUNCHER_DATA_DIR` redirect) lists exactly `[descale, sentinel]`; descale starts, UI 200 |
| 5 | Update-in-place + data preservation | **PASS** | plain launcher (folder mode) created `appdata/{descale,spacelens}`, `ports.json {spacelens:24500}`; all-in-one over same dir → 42 unique ids, no dups, appdata + ports.json intact, descale starts, no port collisions |
| 6a | Per-app logs, no panics/bind-fails/stack traces | **PASS** | 45 log files; grep for `panic:`/`goroutine N`/`missing`/bind errors: zero hits |
| 6b | Nothing written into source tree | **PASS** | tree diff vs baseline: only `embedded/.placeholder` (release.sh scaffold, expected) + marker file mtime (content byte-identical) |
| 7 | Privileged flows (UAC grant, Trash journal) | **SKIP** | interactive flows not run headless |

## Failures with repro

**F1 — Test suite not Windows-clean (step 0).** In `Appdeck/`: `go test ./...` → 9 failures;
`-tags embed` → 18. All are unix-only test assumptions: POSIX mode asserts
(`storage_test.go:103` wants 0755, Windows reports 666; `tls_test.go:99` wants 0600;
`storage_test.go:129` chmod-555 dir still writable), Mach-O zip fixtures rejected by the
launcher's own correct platform check (`update_test.go`: "is a macOS Mach-O binary — send the
build for this platform"), extensionless binary fixtures where the resolver correctly looks for
`.exe` (`deckrebuild_test.go:219`), and symlink asserts (`appstorage_test.go:43`). Production
code behaves correctly in every case; the tests need `runtime.GOOS` guards/platform fixtures
(the codebase already does this elsewhere).

**F2 — `investment` and `mythology` cannot start.** Repro: `POST /api/start
{"id":"investment"}` → 500: `…\Apps\investment\invest-server cannot run on Windows: it is a
macOS executable (Mach-O)`. Chain: stale darwin prebuilts sit extensionless in
`Apps/InvestmentExplorer/invest-server` and `Apps/Itihasas/mythology`; `copy_assets`
(release.sh:86) excludes `*.exe` but not bare Mach-O binaries, so they ride the payload as
"assets" and unpack beside the correct `invest-server.exe`; the binary resolver prefers the
plain name, finds the Mach-O, refuses. Quickest unblock: delete the two stale prebuilts (safe
per workspace AGENTS.md); real fixes: exclude foreign-platform binaries in `copy_assets` and
make the resolver prefer a runnable platform binary over the plain name.

**F3 — `promptpress` serves 404.** Repro: start it, `GET http://127.0.0.1:8190/` → 404
(`/api/status` works). Its UI lives on disk at `web/index.html`, but it embeds one tokenizer
file (`//go:embed tokens/…` in `cmd/promptpress/tokens.go`), and `goAppHasDiskAssets`
(genembedded) treats *any* `//go:embed` as "self-contained" → `web/` never packed. The
heuristic needs to be per-directive or to pack `web/` when it exists.

**F4 — Uninstalling a hosted-static embedded app doesn't stick.** Repro: uninstall `joss` →
response ok, but `Apps/joss` (created at first boot, with `app.json`) survives because the
registry entry for hosted apps has no `Dir` to delete; the app reappears immediately and
**still reappears after a full restart** — `uninstalled.json` lists it, but
`discoverManifestApps` (manifest.go:237) doesn't consult that list and re-discovers the folder.
Go-app uninstall (`debtcrunch`) works correctly — folder deleted, stays gone.

**F5 — Compress-Archive zips fail to import.** Repro: `Compress-Archive` a Go app folder (one
with a leading directory entry, e.g. DebtCrunch) and POST it to `/api/import` →
`{"error":"mkdir …Apps\debtcrunch\cmd: The system cannot find the path specified."}`, nothing
installed, no half-folder left. joss.zip (flat, no dir entries) from the same tool imports
fine. PowerShell's writer emits backslash-separated entry names — the importer must normalize
them. (A flat, third-party python-zipped app imports and runs perfectly.)

**F6 — Port contract deviation.** Embedded decks never exercise the runtime assigner
(ports.go 24500–25499) because genembedded statically assigns 9100–9118 to every port-0 app in
the embedded registry; `ports.json` is therefore never created for embedded apps. No collisions
(all unique, below the ephemeral range), but it contradicts AUTHORING.md "Ports" and the boot
expectation.

## Windows-specific quirks found

- **Firewall:** allow rules (TCP In, Private profile) were created per exe once the
  dashboard/network apps bound `0.0.0.0` — named `appdeck-allinone-windows-amd64.exe` /
  `appdeck.exe`. They now point at deleted temp paths (dead rules); deleting them needs admin,
  so they're left in place.
- **Folder-mode layout:** the plain zip ships an empty `Apps/` *inside* the launcher folder,
  but discovery scans the launcher folder's *parent* for the `Apps/` group folder — an
  extracted plain launcher discovers nothing until apps sit in a **sibling** `Apps/`.
- No SmartScreen/MotW friction (locally built exe launched programmatically).
  `LAUNCHER_DATA_DIR` and `PORT` env overrides work as advertised; the windowsgui exe logs its
  boot banner to `<data>/logs/appdeck.log`, not a console.
- `/api/shutdown` is slow-but-complete for the plain launcher (a few seconds before processes
  exit). `release.sh`'s sweep means building plain after all-in-one removes the all-in-one
  artifact — the final rebuild left `release/` with the clean all-in-one (`a9ee8f63…88f36`,
  exe 278,907,392 bytes) + matching SHA256SUMS.

## State restored

Original `%LocalAppData%\AppDeck` renamed in step 2 was moved back into place; all test data
dirs, scratch zips, and temp extractions were deleted; the boot-refresh marker was reverted
(sha256-verified identical); no processes are running; no files were added to or removed from
the source tree.

---

# Addendum — fix round & per-app verification (2026-10-24, later same day)

Every finding above was root-caused and fixed; gates are green
(`gofmt`/`go vet`/`go build`/`go test ./...` and `go test -tags embed -count=1`, zero
failures). Fix details with root causes: [FIXES.md](FIXES.md). The rebuilt
`Appdeck/release/appdeck-allinone-windows-amd64.zip` was re-verified live against the real
data dir, including a deliberate starved-PATH launcher simulation.

## Root causes (summary)

| Symptom | Root cause | Fix |
|---|---|---|
| SpaceLens picker: `exec: "powershell" not found` | launcher passes its own (starved) PATH to children; system dirs not guaranteed | `systemBinDirs()` appended to child env + launcher PATH self-heal at boot (sysdirs_windows.go) |
| AppScopeMonitor empty, collectors "powershell.exe not found" | same N1 root cause | same fix; all collectors active, 340 live connections after |
| Descale/investment/mythology won't start (Mach-O error) | stale darwin prebuilts packed as assets + resolver preferred extensionless plain name over `X.exe` | sources cleaned, release.sh `drop_foreign_binaries` prune, Windows resolver prefers `X.exe` and removes a stale foreign sibling |
| Sentinel run-as-admin failed | launcher's own `exec powershell` (N1) + `Start-Process` can't run extensionless binaries | N1 fix + `.exe`-first resolution; elevated *stop* stays a UAC flow by design |
| promptpress UI 404 | any `//go:embed` made the packer skip disk assets | genembedded parses the patterns; only web-asset-embedding apps ship binary-only |
| Static app uninstall resurrected | uninstall left the boot-created folder; discovery ignored `uninstalled.json` | uninstall removes the slug folder + discovery filters uninstalled ids |
| Compress-Archive zip import failed | backslash entry names; trailing-backslash dir entries read as zero-byte files | normalize before dir detection in `zipStream.Next` |
| Ports 9100–9118, no ports.json | genembedded invented static ports | port 0 left to the runtime assigner (24500–25499 → ports.json) |
| Boot "unpacking apps failed" (found while verifying) | one locked exe (running elevated app) aborted the whole unpack | per-app resilient extraction with warning |
| Test suite 9/18 failures | unix-only fixtures (modes, Mach-O bytes, symlinks, exec bits) | platform-aware fixtures/skips; one production fix (tool dir-walk exec bit) |
| MSI imports (pdfcraft, wordcraft) | refused by design (no safe msiexec silent+dir flags) | specific actionable refusal message added; both MSIs verified to download + refuse cleanly |

## Per-app verification — all 42 apps (fixed build)

Started via the launcher API from the real data dir; HTTP checked on each app's port.

| App (id) | Start | HTTP | Port |
|---|---|---|---|
| Aacharam (aacharam) | ok | 200 · 5045 B | 8145 |
| Ankashastra (ankashastra) | ok | 200 · 32797 B | 24500 |
| AppScope Monitor (monitor) | ok | 200 · 22526 B | 8765 |
| Astro Chart Studio (astro) | ok | 200 · 3781 B | 8082 |
| Backstage (backstage) | ok | 200 · 3775 B | 24501 |
| CargoScope (cargoscope) | ok | 200 · 6068 B | 8154 |
| ChronoStamp (chronostamp) | ok | desktop app — no port | — |
| CloudDojo (cloud-dojo) | ok | 200 · 3799 B | 24502 |
| DebtCrunch (debtcrunch) | ok | 200 · 25473 B | 24503 |
| Descale (descale) | ok | 200 · 10457 B · /api/info ok | 8212 |
| DevOps Cheatsheet (cheatsheet) | ok | 200 · 68283 B | 8000 |
| DevopsDojo (devops-dojo) | ok | 200 · 4157 B | 24504 |
| DocVault (docvault) | ok | 200 · 4562 B | 8147 |
| DroidScope Monitor (droidview) | ok | 200 · 3650 B | 8766 |
| FDE Dojo (fde-dojo) | ok | 200 · 3724 B | 24505 |
| FormCoach (exercise) | ok | 200 · 4202 B | 8002 |
| Gita Upanishads (gita) | ok | 200 · 2541 B | 8146 |
| HouseFlowStudio (house) | ok | 200 · 11146 B | 8734 |
| IPolice (ipolice) | ok | 200 · 17498 B | 24506 |
| InvestmentExplorer (investment) | ok | 200 · 13457 B | 8912 |
| Itihasas (mythology) | ok | 200 · 2056 B | 8081 |
| Joss (joss) | ok | 200 · 7852 B | 24507 |
| KsanaLedger (ksanaledger) | ok | 200 · 15132 B | 8148 |
| LawBuddy (lawbuddy) | ok | 200 · 4530 B | 5050 |
| MoonlitArcana (tarot) | ok | 200 · 4214 B | 8001 |
| Panchangam (panchangam) | ok | 200 · 20232 B | 24508 |
| PocketInflation (pocketinflation) | ok | 200 · 7756 B | 24509 |
| PromptPress (promptpress) | ok | 200 · 5477 B (was 404) | 8190 |
| Purana (purana) | ok | 200 · 4724 B | 24510 |
| PyMLOps (mlops) | ok | 200 · 121856 B | 8137 |
| Recall (recall) | ok | 200 · 11382 B | 24511 |
| Redact (redact) | ok | 200 · 16431 B | 9000 |
| RepoLens (repolens) | ok | 200 · 5849 B | 24512 |
| Sentinel (sentinel) | ok | 200 · 17152 B | 24513 |
| ShrinkRay (shrinkray) | ok | 200 · 10236 B | 24514 |
| SpaceLens (spacelens) | ok | 200 · 3014 B · picker opens dialog | 24515 |
| StackScout (stackscout) | ok | 200 · 1107 B | 24516 |
| StudyHub (study) | ok | 200 · 5631 B | 8140 |
| TokenReport (tokenreport) | ok | 200 · 18884 B | 8177 |
| Vault (vault) | ok | 200 · 14303 B | 24517 |
| VedicStotrams (vedic) | ok | 200 · 2291 B | 8080 |
| WifiGuard (wifiguard) | ok | 200 · 11774 B | 24518 |

**Result: 42/42 start; 41/41 web apps serve HTTP 200 with real content; ports from manifests
or auto-assigned 24500–24518 (reserved range, ports.json); AppScopeMonitor collectors all
active with live data; Descale serves its API; SpaceLens picker opens the OS dialog.**

After `Stop all`, only the two UAC-elevated apps (descale, sentinel from the interactive
admin test) remained — un-killable by a standard-token launcher by Windows design; stop them
from the dashboard (UAC) or an elevated `taskkill`.

## Remaining known limitations (by design, not bugs)

1. **MSI packages are refused** — no safe silent+directory install exists for msiexec;
   NSIS/Inno support `&silent=1`. Import portable `.exe` builds instead.
2. **Stopping elevated apps re-raises UAC** — Windows integrity levels.
3. **Windows Firewall allow-rules** from earlier test exes point at deleted temp paths
   (dead rules; deleting needs admin).
4. `logs/imports.log` is written only for imports that carry a job id (always true from the
   dashboard's dock; raw curl without `&jobid=` skips persistence).


---

# Addendum 2 — live-use fixes (2026-10-24, continued)

Issues hit during real use of the deck, each root-caused and fixed; details with evidence in
[FIXES.md](FIXES.md), operator guide in [TROUBLESHOOTING.md](TROUBLESHOOTING.md).

| Symptom | Root cause | Fix |
|---|---|---|
| Elevated apps (Sentinel/Descale) un-stoppable | the UAC taskkill fallback only ran for the launcher's own state-tracked PID; orphaned elevated listeners were killed with a standard token only | `stopAppLocked` re-raises the UAC fallback for surviving port listeners and explains the prompt in the Stop response |
| "could not start … as administrator — check the app log" while the app ran | the outer `Start-Process powershell -Verb RunAs` had lost its own `-Wait` (it sat inside the inner PowerShell's `-ArgumentList`), so the launcher read "outer ended, no PID" before the elevated script wrote the pidfile | `-Wait` moved to the outer parameter list; the start now returns ok with the real PID and the process is tracked |
| Descale's AppDeck list empty | `listApps()` appended the AppDeck sub-list on macOS only; `appDeckAppsDir()` returned the macOS data-dir spelling on Windows | Windows/Linux branches append the sub-list; the data dir resolves per OS (`%LOCALAPPDATA%\AppDeck\Apps`, `LAUNCHER_DATA_DIR` honored) |
| Descale uninstall not working for deck apps | `handleAppUninstall` on Windows routed everything to the registry-uninstaller lookup, which can never contain an AppDeck app | appdeck-kind entries fall through to the cross-platform flow (delegate to the launcher, or trash + journal when it is down) |
| Sentinel firewall panel empty, yellow | Windows PowerShell 5.1 serializes NetSecurity enums (`Enabled`, `Direction`, `Action`, `Profile`) as JSON numbers → unmarshal failed for the whole dump | script casts every enum value; the Go parser accepts both shapes and maps the integers back to names (bit-flags for Profile) |
| AppScopeMonitor: no country flags | Windows ships no emoji flag glyphs — regional-indicator pairs render as plain letters; the UI used emoji flags | flag-icons SVG set (MIT, 270 flags, ~2 MB) vendored into `web/flags/`; table cells render inline `<img>` badges; `serveStatic` extended to traversal-proof sub-folder serving |

## Per-app state after this round (Windows, live-verified)

- **Descale** — starts elevated, tracked, stoppable; Uninstall tab lists all 43 deck apps
  (123 registry + deck entries); uninstalling a deck app delegates to the launcher
  end-to-end (folder removed, deck updated).
- **Sentinel** — starts elevated, firewall check green with profiles and per-app rules.
- **AppScope Monitor** — all collectors active with live data; country flags render as SVG
  badges (`/web/flags/<cc>.svg`, traversal-guarded).
- **Launcher** — elevated starts return ok and are tracked; elevated orphans stoppable via
  UAC on Stop; all gates green (`gofmt`/`vet`/`build`/tests, default + embed).

Latest all-in-one: `Appdeck/release/appdeck-allinone-windows-amd64.zip`
(SHA256SUMS alongside; currently `ccda4f10…b8faf`). Older Desktop copies of the exe should be
replaced with this build.
