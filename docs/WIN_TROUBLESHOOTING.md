# TROUBLESHOOTING — AppDeck on Windows

Symptom → cause → what was done. Full fix details in [FIXES.md](FIXES.md); E2E evidence in
[report.md](report.md). All items below are **fixed in the current build**
(`Appdeck/release/appdeck-allinone-windows-amd64.zip` — replace any older copy of the exe,
e.g. one on the Desktop, with the new build; apps and data survive the swap).

## Apps won't start / error on start

### "cannot run on Windows: it is a macOS executable (Mach-O)" — investment, mythology, descale
Stale macOS prebuilt binaries rode the payload as "assets" and unpacked beside the real
`.exe`; the resolver used to prefer the plain (extensionless) name.
**Fixed:** stale prebuilts removed from the sources, the build now drops foreign-platform
binaries from packed assets, and on Windows the resolver prefers `X.exe` and deletes a stale
Mach-O/ELF sibling on first start. Older data dirs self-heal on the first start after
updating the launcher.

### Descale "unable to start"
Same Mach-O root cause as above (its elevated start then failed with "%1 is not a valid
Win32 application" on the Mach-O). **Fixed** — Descale starts; note it launches elevated
(UAC) by design, because cleanup/uninstall operations need admin.

### `promptpress`: dashboard card opened to 404
The packer treated its tokenizer `//go:embed` as "UI embedded" and skipped its disk `web/`
folder. **Fixed** — the embed heuristic now reads the actual patterns; `web/` packs.

## "Run as administrator" / elevated apps

### Sentinel (or any app) "unable to run as admin"
Two stacked causes, both fixed: (1) the launcher's own PATH lacked PowerShell's directory, so
the UAC wrapper (`powershell -Command Start-Process -Verb RunAs …`) never ran; (2) elevated
starts go through `Start-Process`, which cannot execute an extensionless binary — the
resolver now hands it `X.exe`.

### "could not start … as administrator — check the app log" (but the app is running)
The outer UAC wrapper exited too early (its `-Wait` had been misplaced into the inner
PowerShell's arguments), so the launcher read the launch as failed while the elevated script
was still starting the app — and left it untracked. **Fixed:** the elevated start now
returns ok with the real PID once the prompt is approved; the launcher tracks the process
and Stop reaches it directly.

### "Unable to stop" an elevated app (Sentinel / Descale stuck on running)
Stopping an elevated app needs admin: **Stop now raises the UAC prompt by itself — approve
it and the app dies** (also for instances started by a previous launcher run; the response
says "approve the elevation prompt… then Stop again" if you declined). Two caveats:
- If you decline the prompt, the app keeps running by design — Stop again and approve.
- An app started by a *pre-fix* launcher may sit on an old, no-longer-tracked port; if Stop
  reports the app stopped but the process lingers, one elevated
  `taskkill /PID <pid> /T /F` finishes it (one-time cleanup for pre-fix installs).

### An elevated app keeps running after "Stop all"
Same integrity-level rule. Approve the UAC prompt that Stop raises, or leave it running.

## App-specific

### AppScopeMonitor dashboard empty / "powershell.exe not found" in collectors
The launcher passed its own (starved) PATH to the app. **Fixed** — the launcher now
guarantees the Windows system directories on every child's PATH and self-heals its own at
boot, no matter how it was launched. All collectors show `active` (android-adb stays
"unavailable" until Android platform-tools are installed).

### SpaceLens: "Picker failed: folder picker failed: exec: \"powershell\": executable file not found in %PATH%"
Same root cause as above. **Fixed** — the folder picker dialog opens normally now.

### Descale: the AppDeck list is empty / uninstall does nothing
Three Windows gaps, all fixed: the list never included AppDeck apps, uninstalls were routed
to the registry-uninstaller path only, and the AppDeck data dir was spelled the macOS way.
Now the Uninstall tab shows every deck app, and uninstalling an AppDeck app delegates to the
launcher (stopping it, removing the folder, keeping the same rules as the dashboard's own
uninstall). Requires the launcher to be running (or Descale falls back to trashing the
folder itself).

### Sentinel: firewall panel empty, yellow "unavailable" status
Windows PowerShell 5.1 writes firewall enums as numbers (`enabled: 1`), which failed the
JSON parse of the whole firewall dump. **Fixed** — the check is green with profiles and
per-app rules listed.

### AppScopeMonitor: no country flags next to connections
Windows has no emoji flag glyphs (regional-indicator pairs render as plain letters), and the
UI used emoji flags. **Fixed** — the dashboard now renders bundled SVG flags
(flag-icons, MIT, served locally from the app; no external requests). The flag column shows
a real flag badge per resolved country.

## Ports 9100–9118 instead of 24500–25499 / no ports.json
Embedded builds used to bake static 91xx ports into the registry. **Fixed** — portless apps
are assigned from the reserved range 24500–25499 at start and remembered in
`%LocalAppData%\AppDeck\ports.json`.

## Install / uninstall / import

### Uninstalled static app reappears after restart
The app's boot-created folder survived uninstall and discovery resurrected it. **Fixed** —
uninstall removes the folder and discovery honors `uninstalled.json`. Re-importing the same
app still works (overrides are never filtered).

### Import of a PowerShell `Compress-Archive` zip failed ("The system cannot find the path specified")
That writer emits backslash entry names; directory entries read as zero-byte files.
**Fixed** — the importer normalizes entry names. Older launchers: zip with another tool.

### .msi import is refused ("…is a Windows Installer package (MSI)…")
By design: no safe silent+directory install exists for msiexec, so MSIs are refused while
NSIS/Inno installers support `&silent=1`. The error message now says exactly that (it used
to be a generic "unsupported file"). Import the app's portable `.exe` build instead.

### One running app made boot say "unpacking apps failed" and left later apps stale
A running (elevated) app locks its exe on Windows; extraction aborted there. **Fixed** —
per-app extraction is resilient; the locked app retries on the next boot.

## Launcher general

### Where are the logs?
`%LocalAppData%\AppDeck\logs\` — `appdeck.log` (launcher), `imports.log` (import dock job
logs, written when the import carries a job id — always true from the dashboard),
`<app-id>.log` (per-app stdout/stderr).

### Launcher seems silent on first run
The all-in-one exe is a windowsgui build: no console. Boot output goes to `appdeck.log`.

### Dashboard reachable from the LAN
The dashboard binds `0.0.0.0:9090` by default (`HOST` env to change). Windows Firewall asks
once per exe; the rules live under the exe name in Windows Firewall. `LAUNCHER_DATA_DIR`
redirects the data dir (useful for testing); `PORT` overrides the port.
