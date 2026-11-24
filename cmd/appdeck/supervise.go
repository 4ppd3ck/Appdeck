// App process supervision: start, stop, build, environment, keepalive.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// conflictingAppName names another app that is currently holding port ("" when
// none can be identified). Used so a same-port clash reports the culprit
// instead of a bare bind error, and so a binary app whose port is taken by a
// *known other app* is not misreported as its own "already running".
func (s *Server) conflictingAppName(port int, excludeID string) string {
	st := loadState(s.cfg.StateFile)
	for _, other := range s.appsList() {
		if other.ID == excludeID || other.Desktop {
			continue
		}
		// Effective port: a custom-port override wins over the configured one.
		eport := other.Port
		if e := st[other.ID].PortOverride; e != 0 {
			eport = e
		}
		if eport != port {
			continue
		}
		if other.ServedBy == "launcher" {
			s.mu.Lock()
			_, ours := s.hostedListeners[other.ID]
			s.mu.Unlock()
			if ours {
				return other.Name
			}
			continue
		}
		if pid := st[other.ID].PID; pid != 0 && pidAlive(pid) {
			for _, p := range listenerPIDs(port) {
				if p == pid {
					return other.Name
				}
			}
		}
	}
	return ""
}

// startApp runs one start with the in-flight guard (Python start_app).
func (s *Server) startApp(a *App) *actionResp {
	s.mu.Lock()
	if _, busy := s.inflight[a.ID]; busy {
		s.mu.Unlock()
		return &actionResp{OK: false, Busy: true, Error: busyMessage}
	}
	s.inflight[a.ID] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, a.ID)
		s.mu.Unlock()
	}()
	resp := s.startAppLocked(a)
	if resp.OK {
		// A usage run is a launch the launcher performed — an "already
		// running" answer is someone else's instance, not a run.
		if !resp.AlreadyRunning {
			s.recordAppStart(a.ID)
		}
		s.startKeepAlive(a)
	}
	return resp
}

func (s *Server) startAppLocked(a *App) *actionResp {
	if a.ServedBy == "launcher" {
		return s.startHostedStatic(a)
	}
	// Permission gate: a card that declares permissions and has no standing
	// grant does not start — the dashboard answers with the dialog's verdict
	// (permissions.go). Hosted static pages need no process, so they skip.
	if needed := s.pendingPermissions(a); len(needed) > 0 {
		return &actionResp{OK: false, Pending: &permPrompt{
			ID: a.ID, Name: a.Name, Needed: needed, Declared: a.Permissions,
		}}
	}
	if a.Desktop {
		return s.startDesktopApp(a)
	}
	if portOpen(a.Port, defaultProbeTimeout) {
		// Someone is on our port: our own instance started outside the
		// launcher reports as already running, but a *known other app* on the
		// same port is a clash worth naming.
		if culprit := s.conflictingAppName(a.Port, a.ID); culprit != "" {
			return &actionResp{OK: false, Error: fmt.Sprintf(
				"port %d is already used by %s — start with a custom port", a.Port, culprit)}
		}
		return &actionResp{OK: true, AlreadyRunning: true}
	}
	if err := os.MkdirAll(s.cfg.LogDir, 0o755); err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	logFile, err := os.OpenFile(s.logPath(a), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "\n===== started %s =====\n", time.Now().Format("2006-01-02 15:04:05"))

	token := newToken()
	cmd, err := s.appCommand(a)
	if err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	cmd.Dir = a.Dir
	cmd.Env = s.childEnv(a, token)
	// OS sandbox (sandbox.go): rewrite argv under seatbelt/bubblewrap when
	// the manifest asks for one. Privileged launches skip it — the two
	// wrappers are exclusive, and elevation is the older contract.
	if !privilegedLaunch(a) {
		if err := s.wrapSandbox(a, cmd); err != nil {
			return &actionResp{OK: false, Error: err.Error()}
		}
	}
	// Privileged app: launch through the OS's own elevation mapping (TCC
	// identity bundle / UAC script / pkexec-sudo) so grants and the admin
	// token land on the app alone, never the launcher. The tail below is
	// unchanged: every platform's wrapper writes to the same log files.
	var exited chan struct{}
	pid := 0
	wasPrivileged := privilegedLaunch(a)
	if wasPrivileged {
		pid, exited, err = s.launchPrivileged(a, cmd.Env, cmd.Args[1:])
		if err != nil {
			logFile.WriteString("appdeck: privileged launch error: " + err.Error() + "\n")
			return &actionResp{OK: false, Error: err.Error()}
		}
	} else {
		cmd.Stdout = logFile
		cmd.Stderr = logFile // Python: stderr=subprocess.STDOUT
		cmd.SysProcAttr = processSysProcAttr()
		if err := cmd.Start(); err != nil {
			return &actionResp{OK: false, Error: err.Error()}
		}
		pid = cmd.Process.Pid
		s.sandboxPostStart(a, pid) // windows Job Object attach (sandbox.go)
	}

	s.mu.Lock()
	st := loadState(s.cfg.StateFile)
	st[a.ID] = stateEntry{PID: pid, StartedAt: float64(time.Now().UnixNano()) / 1e9, Token: token, Privileged: wasPrivileged}
	saveState(s.cfg.StateFile, st)
	s.mu.Unlock()

	if exited == nil {
		// Wait for the app's web UI to answer HTTP (not just TCP open) within
		// the configured budget — deck tools "app start timeout", per-app
		// manifest startWait overrides it.
		exited = make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(exited)
		}()
	}
	return waitForAppReady(a, pid, startWaitBudget(a), exited,
		func(port int, timeout time.Duration) bool { return portOpen(port, timeout) },
		httpReady)
}

// startWaitBudget is the effective startup wait for one app: the manifest's
// startWait (seconds) when the author declared one, else the deck tools
// "app start timeout" (launcher.json startWaitSeconds, default 30 s).
func startWaitBudget(a *App) time.Duration {
	if a.StartWait > 0 {
		return time.Duration(a.StartWait) * time.Second
	}
	return time.Duration(startWaitSecondsCache.Load()) * time.Second
}

// httpReady reports whether the app's HTTP handler is actually serving —
// a TCP port can accept connections well before the handler runs (DB
// setup, first-request init), which used to open a blank page. Any HTTP
// response counts as serving, even 401/404/500: bytes on the wire mean a
// handler is there.
func httpReady(a *App) bool {
	path := a.Health
	if path == "" {
		path = a.URLPath
	}
	if path == "" {
		path = "/"
	}
	client := http.Client{Timeout: 600 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", a.Port, path))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// waitForAppReady is the startup wait loop: probe TCP, then HTTP once the
// port is open, until the budget expires or the process exits. The probe
// funcs are parameters so tests can fake both. Returns nil when the app is
// ready; tcpSeen distinguishes the two expiry notes.
func waitForAppReady(a *App, pid int, budget time.Duration, exited <-chan struct{},
	portProbe func(int, time.Duration) bool, httpProbe func(*App) bool,
) *actionResp {
	deadline := time.Now().Add(budget)
	tcpSeen := false
	for {
		if portProbe(a.Port, startProbeTimeout) {
			tcpSeen = true
			if httpProbe(a) {
				return &actionResp{OK: true, PID: &pid}
			}
		}
		select {
		case <-exited:
			return &actionResp{OK: false, Error: "process exited during startup", PID: &pid}
		default:
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(startWaitSleep)
	}
	secs := int(budget.Seconds())
	if tcpSeen {
		return &actionResp{OK: true, PID: &pid, Slow: true, Note: fmt.Sprintf(
			"port answered, but the web UI did not respond within %d s — still starting in the background", secs)}
	}
	return &actionResp{OK: true, PID: &pid, Slow: true, Note: fmt.Sprintf(
		"started, but the port did not answer within %d s — still starting in the background", secs)}
}

// appCommand builds the exec.Cmd an app entry describes: a "binary"
// (auto-built when its platform binary is missing) with {port}/{host}-
// substituted args, else a shell "cmd" string. Shared by the web and desktop
// start paths.
func (s *Server) appCommand(a *App) (*exec.Cmd, error) {
	// An app living on an unplugged drive must fail with the reason, not
	// with a phantom "building from source".
	if msg := s.appStorageProblem(a); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	host := bindHostFor(a)
	if a.Binary != "" {
		binary, err := s.resolveAppBinary(a)
		if err != nil {
			return nil, err
		}
		if why := appBinaryProblem(binary); why != "" {
			return nil, fmt.Errorf("%s cannot run on Windows: %s", binary, why)
		}
		args := make([]string, 0, len(a.Args))
		for _, arg := range a.Args {
			args = append(args, substitutePlaceholders(arg, a.Port, host))
		}
		return exec.Command(binary, args...), nil
	}
	cmdStr := normalizeAppCommand(substitutePlaceholders(a.Cmd, a.Port, host))
	parts := shellCommand(cmdStr)
	return exec.Command(parts[0], parts[1:]...), nil
}

// bindHostFor is the host an app should bind: loopback unless the app opts
// into Wi-Fi sharing (network: true). The loopback default is a security
// decision — an app that binds 0.0.0.0 is reachable from every device on
// the network, so sharing is always an explicit author choice.
func bindHostFor(a *App) string {
	if a.Network {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// substitutePlaceholders expands the app-contract placeholders in args, cmd
// strings and env values: {port} → the app's port, {host} → its bind host.
func substitutePlaceholders(s string, port int, host string) string {
	s = strings.ReplaceAll(s, "{port}", strconv.Itoa(port))
	return strings.ReplaceAll(s, "{host}", host)
}

// startDesktopApp launches a portless desktop app (a GUI/CLI binary with
// "desktop": true). There is no port to wait for, so success is the process
// still being alive shortly after spawn — long enough to catch an instant
// crash (missing binary, bad build); after that the app lives on its own and
// status polls track it through the state entry's PID.
func (s *Server) startDesktopApp(a *App) *actionResp {
	if pid := loadState(s.cfg.StateFile)[a.ID].PID; pid != 0 && pidAlive(pid) {
		return &actionResp{OK: true, AlreadyRunning: true}
	}
	if err := os.MkdirAll(s.cfg.LogDir, 0o755); err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	logFile, err := os.OpenFile(s.logPath(a), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "\n===== started %s =====\n", time.Now().Format("2006-01-02 15:04:05"))

	// A desktop app that dies instantly usually printed why (no GUI in this
	// build, bad build, …). Capture its output alongside the log file so
	// the failure can carry the reason back to the dashboard.
	token := newToken()
	cmd, err := s.appCommand(a)
	if err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	cmd.Dir = a.Dir
	cmd.Env = s.childEnv(a, token)
	// Sandbox wrap only for the plain-spawn path below — bundle launches
	// (Launch Services) and privileged launches are their own paths and
	// never pass through the rewritten argv.
	if !privilegedLaunch(a) && s.launchableBundle(a) == "" {
		if err := s.wrapSandbox(a, cmd); err != nil {
			return &actionResp{OK: false, Error: err.Error()}
		}
	}
	if privilegedLaunch(a) {
		// Privileged: launch through the OS's own elevation mapping (TCC
		// identity bundle / UAC script / pkexec-sudo) so grants and the
		// admin token land on its identity. The wrapper writes to the same
		// log, so an instant crash is diagnosable from the Logs button.
		pid, exited, err := s.launchPrivileged(a, cmd.Env, cmd.Args[1:])
		if err != nil {
			logFile.WriteString("appdeck: privileged launch error: " + err.Error() + "\n")
			return &actionResp{OK: false, Error: err.Error()}
		}
		s.mu.Lock()
		st := loadState(s.cfg.StateFile)
		st[a.ID] = stateEntry{PID: pid, StartedAt: float64(time.Now().UnixNano()) / 1e9, Token: token, Privileged: true}
		saveState(s.cfg.StateFile, st)
		s.mu.Unlock()
		select {
		case <-exited:
			return &actionResp{OK: false, Error: "process exited during startup — check the app log", PID: &pid}
		case <-time.After(time.Second):
		}
		return &actionResp{OK: true, PID: &pid, Note: privilegedLaunchNote(a.Privacy)}
	}
	if bundle := s.launchableBundle(a); bundle != "" {
		// The app owns a .app bundle (a vendor bundle from the import, or
		// the elevation wrapper): launch it through Launch Services, exactly
		// like a double-click. Executing the in-bundle binary directly is
		// not equivalent — a CLI-first player like mpv finds no file and no
		// terminal and answers with its usage text, and the app never gets
		// its bundle context (Dock identity, GUI mode). Elevation keeps
		// precedence: the branch above wins. A generated wrapper runs
		// through its shim, which sources this start's environment and
		// writes to the same log, so the launch stays diagnosable.
		pid, exited, err := s.launchAppBundle(a, cmd.Env, bundle, cmd.Args[1:])
		if err != nil {
			logFile.WriteString("appdeck: bundle launch error: " + err.Error() + "\n")
			return &actionResp{OK: false, Error: err.Error()}
		}
		s.mu.Lock()
		st := loadState(s.cfg.StateFile)
		st[a.ID] = stateEntry{PID: pid, StartedAt: float64(time.Now().UnixNano()) / 1e9, Token: token}
		saveState(s.cfg.StateFile, st)
		s.mu.Unlock()
		select {
		case <-exited:
			return &actionResp{OK: false, Error: "process exited during startup — check the app log", PID: &pid}
		case <-time.After(time.Second):
		}
		return &actionResp{OK: true, PID: &pid}
	}
	var outBuf bytes.Buffer
	cmd.Stdout = io.MultiWriter(logFile, &outBuf)
	cmd.Stderr = io.MultiWriter(logFile, &outBuf)
	cmd.SysProcAttr = processSysProcAttr()
	if err := cmd.Start(); err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	pid := cmd.Process.Pid
	s.sandboxPostStart(a, pid) // windows Job Object attach (sandbox.go)

	s.mu.Lock()
	st := loadState(s.cfg.StateFile)
	st[a.ID] = stateEntry{PID: pid, StartedAt: float64(time.Now().UnixNano()) / 1e9, Token: token}
	saveState(s.cfg.StateFile, st)
	s.mu.Unlock()

	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	select {
	case <-exited:
		msg := "process exited during startup"
		if out := strings.TrimSpace(outBuf.String()); out != "" {
			if usageTextExit(out) {
				msg += ": the program answered with its usage text — it was started " +
					"without the arguments (or the file) it needs. Set them in the " +
					"app's app.json \"args\" or re-import with Arguments filled in. " +
					"Last output: " + lastN(out, 300)
			} else {
				msg += ": " + lastN(out, 300) // the last lines carry the actual error
			}
		}
		return &actionResp{OK: false, Error: msg, PID: &pid}
	case <-time.After(time.Second):
	}
	return &actionResp{OK: true, PID: &pid}
}

// usageTextExit reports whether a child that died at startup answered with
// its usage/help block instead of an error. A CLI-first player (mpv) does
// exactly this when the card starts it with no arguments — no file to play,
// so it prints usage and exits, and the card's real failure is the missing
// arguments, not a crash. "usage:" alone is not enough (a program can log
// it incidentally); a listed --flag alongside it makes the block certain.
func usageTextExit(out string) bool {
	if len(out) > 4096 {
		out = out[len(out)-4096:]
	}
	lo := strings.ToLower(out)
	return strings.Contains(lo, "usage:") && strings.Contains(lo, "--")
}

// removeIfIncompatible uninstalls an app whose start failed because the app
// itself declared another OS ("StellarHub only runs on Windows."): its card
// would only ever repeat that failure. The evidence is the failure message
// plus the tail of the app log's last run, and the classifier fires only on
// the app naming an OS this launcher does not run — port clashes, missing
// toolchains and timeouts never remove anything. The data folder stays,
// matching the uninstall route's default. Returns "" when the app stays;
// otherwise the note for the response. The caller reloads the app list
// (uninstallOneApp does not) and snapshots the deck.
func (s *Server) removeIfIncompatible(a *App, res *actionResp) string {
	if res == nil || res.OK || res.Busy || !strings.HasPrefix(res.Error, "process exited during startup") {
		return ""
	}
	os, ok := platformIncompatibleFailure(res.Error + "\n" + s.lastRunOutput(a))
	if !ok {
		return ""
	}
	if a.Dir == "" || !dirExists(a.Dir) {
		return "" // nothing on disk to remove (hosted app, or already gone)
	}
	if _, err := s.uninstallOneApp(a, false); err != nil {
		return "" // the app stays; the start failure reports as usual
	}
	return fmt.Sprintf("%s uninstalled — its startup output says it only runs on %s, so the card could never start", a.ID, osDisplayName(os))
}

// lastRunOutput is the tail of the app log's most recent run. The log
// appends across starts; everything before the last "===== started" marker
// belongs to an older attempt and must not speak for this one.
func (s *Server) lastRunOutput(a *App) string {
	f, err := os.Open(s.logPath(a))
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 64 << 10
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	offset := int64(0)
	if st.Size() > int64(tail) {
		offset = st.Size() - int64(tail)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, tail))
	if err != nil {
		return ""
	}
	log := string(data)
	if i := strings.LastIndex(log, "===== started"); i >= 0 {
		log = log[i:]
	}
	return log
}

// startHostedStatic hosts an HTML app inside the launcher process: no child
// process, the port is served from the embedded assets (single-file build)
// or straight from the app's disk folder (dev build).
func (s *Server) startHostedStatic(a *App) *actionResp {
	if ln, ours := s.hostedListeners[a.ID]; ours && ln != nil {
		return &actionResp{OK: true, AlreadyRunning: true}
	}
	if portOpen(a.Port, defaultProbeTimeout) {
		// Not our listener — either a foreign process or (the classic footgun)
		// another app configured on the same port. Name it if we can.
		msg := fmt.Sprintf("port %d is already in use", a.Port)
		if culprit := s.conflictingAppName(a.Port, a.ID); culprit != "" {
			msg += fmt.Sprintf(" by %s", culprit)
		}
		msg += " — start with a custom port"
		return &actionResp{OK: false, Error: msg}
	}
	handler, err := s.staticHandlerFor(a)
	if err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	if err := os.MkdirAll(s.cfg.LogDir, 0o755); err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	if logFile, err := os.OpenFile(s.logPath(a), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644); err == nil {
		fmt.Fprintf(logFile, "\n===== started %s (hosted inside launcher.exe) =====\n",
			time.Now().Format("2006-01-02 15:04:05"))
		logFile.Close()
	}
	host := "127.0.0.1"
	if a.Network {
		host = "0.0.0.0"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(a.Port)))
	if err != nil {
		return &actionResp{OK: false, Error: err.Error()}
	}
	pid := os.Getpid()
	s.mu.Lock()
	s.hostedListeners[a.ID] = ln
	st := loadState(s.cfg.StateFile)
	st[a.ID] = stateEntry{PID: pid, StartedAt: float64(time.Now().UnixNano()) / 1e9}
	saveState(s.cfg.StateFile, st)
	s.mu.Unlock()
	go func() {
		srv := &http.Server{Handler: guardHosts(hostedHandler(s, handler, a)), ReadHeaderTimeout: 10 * time.Second}
		_ = srv.Serve(ln) // returns when the app is stopped or the launcher exits
	}()
	return &actionResp{OK: true, PID: &pid}
}

// stopHostedStatic closes the in-process listener of a hosted HTML app.
func (s *Server) stopHostedStatic(a *App) *actionResp {
	s.mu.Lock()
	ln := s.hostedListeners[a.ID]
	delete(s.hostedListeners, a.ID)
	st := loadState(s.cfg.StateFile)
	delete(st, a.ID)
	saveState(s.cfg.StateFile, st)
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	for i := 0; i < 10; i++ {
		if !portOpen(a.Port, startProbeTimeout) {
			still := false
			killed := []int{}
			return &actionResp{OK: true, Killed: &killed, StillRunning: &still}
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Port still held after our listener closed → someone else's process; use
	// the ordinary external-kill path so Stop stays trustworthy.
	killed := []int{}
	for _, p := range listenerPIDs(a.Port) {
		if killProc(p) == nil {
			killed = append(killed, p)
		}
	}
	for i := 0; i < stopWaitRounds; i++ {
		if !portOpen(a.Port, startProbeTimeout) {
			break
		}
		time.Sleep(stopWaitSleep)
	}
	still := portOpen(a.Port, startProbeTimeout)
	return &actionResp{OK: !still, Killed: &killed, StillRunning: &still}
}

// resolveAppBinary resolves a.Binary with build progress streamed to the
// app's own log file — first starts of source apps (cargo/go builds) are
// long and silent otherwise, and the 📄 Logs view is where the operator
// looks.
func (s *Server) resolveAppBinary(a *App) (string, error) {
	binary, err := resolveBinary(s.cfg.Root, a.Binary)
	if err == nil {
		return s.rebuildIfImported(a, binary), nil
	}
	// The registry/discovery may express the binary Root-relative while the
	// app folder lives in the data directory (cross-mode decks); retry
	// there before giving up.
	if s.cfg.DataDir != "" && s.cfg.DataDir != s.cfg.Root {
		if binary2, err2 := resolveBinary(s.cfg.DataDir, a.Binary); err2 == nil {
			return s.rebuildIfImported(a, binary2), nil
		}
	}
	// The binary's home may be inside the app's .app bundle: an elevated
	// macOS launch moves it there (its one copy), and an app switched back
	// to inherited simply runs it from the bundle contents.
	if bundleBin := bundleBinaryPath(a.Dir, a.ID); bundleBin != "" && fileExists(bundleBin) {
		if err := ensureExecutable(bundleBin); err == nil {
			return s.rebuildIfImported(a, bundleBin), nil
		}
	}
	// Builds stream into the log only when one is actually attempted —
	// a missing binary for a manifest without sources stays a clean error.
	if a.Dir != "" {
		if logw, lerr := os.OpenFile(s.logPath(a), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644); lerr == nil {
			fmt.Fprintf(logw, "appdeck: no prebuilt binary found — building from source on first start (this can take minutes; live output below)\n")
			binary3, berr := resolveBinaryLogged(s.cfg.Root, a.Binary, logw)
			// Same cross-mode fallback as the prebuilt resolve above: a
			// bundled launcher's imported sources live in the data
			// directory, and Root-relative builds cannot see them.
			if berr != nil && s.cfg.DataDir != "" && s.cfg.DataDir != s.cfg.Root {
				binary3, berr = resolveBinaryLogged(s.cfg.DataDir, a.Binary, logw)
			}
			logw.Close()
			if berr == nil {
				// Built from the very sources an import just landed — the
				// rebuild marker is satisfied by construction; leaving it
				// would rebuild the fresh binary once more on the next start.
				os.Remove(filepath.Join(a.Dir, rebuildMarker))
				// The binary is generated: the build inputs are dead weight
				// now (the release build never ships them).
				s.pruneImportedSources(a)
			}
			return binary3, berr
		}
	}
	return "", err
}

func resolveBinary(root, configured string) (string, error) {
	return resolveBinaryLogged(root, configured, nil)
}

func resolveBinaryLogged(root, configured string, logw io.Writer) (string, error) {
	// The app folder is the single home in every build mode: embedded
	// builds unpack binaries into <data>/<RawDir> (= apps/<id>/, the same
	// place imports extract to), and resolution tries the configured path,
	// its platform-suffixed form, its plain .exe form (how embedded builds
	// unpack it on Windows), then the legacy flat bin/ cache from
	// older installs. Matching the folder-mode rules keeps one mental model:
	// "the binary lives beside app.json".
	path := configured
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path, _ = filepath.Abs(path)
	platformPath := platformBinaryPath(path)
	// The binary's one name is the plain configured one. A legacy
	// platform-suffixed binary (<name>-<os>-<arch>) on disk — older builds and
	// imports spelled it that way — is renamed to the plain name on first
	// resolve, so an app folder carries exactly one binary. A rename that
	// On Windows the canonical plain name carries .exe (embedded builds
	// unpack it there, go builds write it there). The extensionless spelling
	// is a legacy artifact: elevated starts go through Start-Process, which
	// cannot execute an extensionless file, and an extensionless Mach-O/ELF
	// that rode an asset copy would shadow the real binary entirely.
	exePath := filepath.Join(filepath.Dir(path), embeddedBinaryName(filepath.Base(configured)))
	windowsCanonical := runtime.GOOS == "windows" && exePath != path
	if windowsCanonical {
		if fileExists(platformPath) && !fileExists(exePath) {
			if err := os.Rename(platformPath, exePath); err == nil && logw != nil {
				fmt.Fprintf(logw, "appdeck: renamed %s → %s (the platform suffix is retired)\n",
					filepath.Base(platformPath), filepath.Base(exePath))
			}
		}
		if fileExists(platformPath) && fileExists(exePath) {
			if err := os.Remove(platformPath); err == nil && logw != nil {
				fmt.Fprintf(logw, "appdeck: removed %s (%s is the one that runs)\n",
					filepath.Base(platformPath), filepath.Base(exePath))
			}
		}
		// A stale foreign-platform binary (a Mach-O/ELF that rode an asset
		// copy) must never shadow the real .exe — drop it when the real
		// binary exists. An extensionless PE or script stays where it is:
		// resolution below simply prefers the .exe spelling.
		if fileExists(path) && fileExists(exePath) && foreignExecutable(path) {
			if err := os.Remove(path); err == nil && logw != nil {
				fmt.Fprintf(logw, "appdeck: removed %s (stale foreign-platform binary beside %s)\n",
					filepath.Base(path), filepath.Base(exePath))
			}
		}
	} else {
		// The binary's one name is the plain configured one. A legacy
		// platform-suffixed binary (<name>-<os>-<arch>) on disk — older
		// builds and imports spelled it that way — is renamed to the plain
		// name on first resolve, so an app folder carries exactly one
		// binary. A rename that cannot land (a running app's exe on Windows)
		// keeps resolving through the candidates below; the next resolve
		// retries.
		if fileExists(platformPath) && !fileExists(path) {
			if err := os.Rename(platformPath, path); err == nil && logw != nil {
				fmt.Fprintf(logw, "appdeck: renamed %s → %s (the platform suffix is retired)\n",
					filepath.Base(platformPath), filepath.Base(path))
			}
		}
		// Both present: the plain name is canonical (imports, extraction and
		// fresh builds all write it), so the suffixed sibling is dead weight
		// — drop it. Best effort: a running app's exe on Windows cannot be
		// removed; the next resolve retries.
		if fileExists(platformPath) && fileExists(path) {
			if err := os.Remove(platformPath); err == nil && logw != nil {
				fmt.Fprintf(logw, "appdeck: removed %s (the plain %s is the one that runs)\n",
					filepath.Base(platformPath), filepath.Base(path))
			}
		}
	}
	// The plain configured path first: imported exes keep whatever name they
	// were imported with (it may already carry a platform suffix), while
	// repo-built apps write the plain name. On Windows the .exe spelling
	// wins — see windowsCanonical above.
	var candidates []string
	if windowsCanonical {
		candidates = []string{exePath, platformPath, path}
	} else {
		candidates = []string{path, platformPath}
		if exePath != path {
			candidates = append(candidates, exePath)
		}
	}
	if embeddedBuild {
		// Legacy cache from older all-in-one installs: <data>/bin/<base>.
		candidates = append(candidates,
			filepath.Join(root, "bin", embeddedBinaryName(filepath.Base(configured))))
	}
	for _, candidate := range candidates {
		if fileExists(candidate) {
			if err := ensureExecutable(candidate); err != nil {
				return "", fmt.Errorf("make binary executable: %s: %v", candidate, err)
			}
			return candidate, nil
		}
	}
	// No prebuilt binary — build from source, the way the folder declares
	// itself: a Go module builds with go, a Cargo workspace with cargo.
	// Fresh builds take the binary's plain configured name (descale, not
	// descale-darwin-arm64); older suffixed binaries on disk keep resolving
	// through the candidates above.
	output := buildOutputPath(path)
	moduleDir := filepath.Dir(output)
	var buildErr error
	switch {
	case fileExists(filepath.Join(moduleDir, "go.mod")):
		_, buildErr = buildGoBinaryLogged(output, logw)
	case fileExists(filepath.Join(moduleDir, "Cargo.toml")):
		_, buildErr = buildRustBinary(output, logw)
	default:
		return "", fmt.Errorf("app binary not found: %s (looked in %s; the app folder has no go.mod or Cargo.toml — build the app and put the binary beside app.json)",
			configured, strings.Join(candidates, ", "))
	}
	if buildErr != nil {
		return "", fmt.Errorf("app binary not found: %s (looked in %s; automatic build failed: %v)",
			configured, strings.Join(candidates, ", "), buildErr)
	}
	return output, nil
}

// rebuildIfImported honors the .appdeck-rebuild marker an import leaves in a
// buildable app folder. Imports copy the source folder as-is — including any
// prebuilt binary it carries — and resolveBinary would run that binary
// forever: the build path only triggers when a binary is missing outright,
// never when the sources around it are newer. So a re-imported app ran its
// previous version. The marker says "sources just landed": rebuild from them
// before running. Failure (often a missing toolchain) keeps the marker and
// runs the existing binary — Start never breaks, the next Start retries.
func (s *Server) rebuildIfImported(a *App, binary string) string {
	if a.Binary == "" || a.Dir == "" {
		return binary
	}
	marker := filepath.Join(a.Dir, rebuildMarker)
	if _, err := os.Stat(marker); err != nil {
		return binary
	}
	// The rebuild writes beside app.json — the binary's canonical home —
	// wherever the running binary was found (an elevated macOS app's binary
	// lives inside its .app bundle; the fresh build lands in the folder and
	// the next privileged start moves it in).
	output := buildOutputPath(filepath.Join(a.Dir, filepath.Base(a.Binary)))
	logw, lerr := os.OpenFile(s.logPath(a), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if lerr != nil {
		logw = nil
	} else {
		defer logw.Close()
		fmt.Fprintf(logw, "appdeck: sources re-imported — rebuilding before start (the binary the folder carried may be stale)\n")
	}
	var berr error
	// go build -o refuses to overwrite a non-object file, and the stale
	// binary sits exactly at the output path — move it aside first. If the
	// build fails it goes back (it is the fallback that keeps Start working
	// until the next retry); on success it is deleted with the marker.
	fallback := ""
	if fileExists(output) {
		fallback = output + ".stale"
		if err := os.Rename(output, fallback); err != nil {
			fallback = ""
		}
	}
	switch {
	case fileExists(filepath.Join(a.Dir, "go.mod")):
		_, berr = buildGoBinaryLogged(output, logw)
	case fileExists(filepath.Join(a.Dir, "Cargo.toml")):
		_, berr = buildRustBinary(output, logw)
	default:
		if fallback != "" {
			_ = os.Rename(fallback, output)
		}
		os.Remove(marker) // folder is no longer buildable — drop the stale marker
		return binary
	}
	if berr != nil {
		if fallback != "" {
			_ = os.Rename(fallback, output)
		}
		if logw != nil {
			fmt.Fprintf(logw, "appdeck: rebuild failed — running the existing binary; the rebuild retries on the next start (%v)\n", berr)
		}
		return binary
	}
	if fallback != "" {
		_ = os.Remove(fallback)
	}
	os.Remove(marker)
	// The rebuild succeeded — the build inputs are dead weight now (the
	// release build never ships them).
	s.pruneImportedSources(a)
	if logw != nil {
		fmt.Fprintf(logw, "appdeck: rebuild finished → %s\n", filepath.Base(output))
	}
	return output
}

// pruneImportedSources strips an app's build inputs (see pruneBuildInputs)
// when its folder lives in the installed container — imported copies only:
// a dev-tree app's folder IS its live source tree and is never touched.
// Best-effort by design: a failed prune must never fail a start; it simply
// retries at the next successful build or boot sweep.
func (s *Server) pruneImportedSources(a *App) {
	if a == nil || a.Dir == "" {
		return
	}
	container, err := s.appsContainerDir()
	if err != nil {
		return
	}
	dirAbs, err := filepath.Abs(a.Dir)
	if err != nil {
		return
	}
	cAbs, err := filepath.Abs(container)
	if err != nil {
		return
	}
	if dirAbs != cAbs && !strings.HasPrefix(dirAbs, cAbs+string(filepath.Separator)) {
		return
	}
	if err := pruneBuildInputs(a.Dir); err != nil {
		fmt.Fprintf(os.Stderr, "launcher: could not prune build sources in %s: %v\n", a.Dir, err)
	}
}

// buildOutputPath names a fresh build after its configured binary — plain
// <name>, with the .exe Windows requires to execute. The legacy
// <name>-<os>-<arch> spelling stays readable through the candidates list.
func buildOutputPath(path string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(path, ".exe") {
		return path + ".exe"
	}
	return path
}

func platformBinaryPath(path string) string {
	suffixed := path + "-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		suffixed += ".exe" // Windows needs the extension to execute reliably
	}
	return suffixed
}

// foreignExecutable reports whether the file is a Mach-O or ELF binary —
// a foreign-platform executable that can never run on this machine. Such
// files usually arrive as stale prebuilts riding an asset copy, and on
// Windows they brick the app when the resolver picks them over the real
// .exe.
func foreignExecutable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	for _, m := range machoMagics {
		if bytes.Equal(head, m) {
			return true
		}
	}
	return bytes.Equal(head, []byte{0x7F, 'E', 'L', 'F'})
}

// buildGoBinary builds an app on first use when its platform binary is absent.
// Most apps have main.go at the module root; Monitor uses the conventional
// cmd/monitor entrypoint, which is discovered automatically.
func buildGoBinary(output string) (string, error) {
	return buildGoBinaryLogged(output, nil)
}

func buildGoBinaryLogged(output string, logw io.Writer) (string, error) {
	moduleDir := filepath.Dir(output)
	if !fileExists(filepath.Join(moduleDir, "go.mod")) {
		return "", fmt.Errorf("no go.mod in %s", moduleDir)
	}

	buildTarget := "."
	if !hasMainPackage(moduleDir) {
		entries, err := os.ReadDir(filepath.Join(moduleDir, "cmd"))
		if err != nil {
			return "", fmt.Errorf("no Go entrypoint in %s", moduleDir)
		}
		var entrypoints []string
		for _, entry := range entries {
			if entry.IsDir() && hasMainPackage(filepath.Join(moduleDir, "cmd", entry.Name())) {
				entrypoints = append(entrypoints, "./cmd/"+entry.Name())
			}
		}
		if len(entrypoints) == 0 {
			return "", fmt.Errorf("no Go entrypoint in %s", moduleDir)
		}
		// Multi-cmd modules (a main server beside converters/tools): the
		// entrypoint whose name matches the app's configured binary builds
		// it; a single entrypoint needs no name. Anything else is ambiguous.
		buildTarget = ""
		want := filepath.Base(output)
		for _, ep := range entrypoints {
			if strings.TrimPrefix(ep, "./cmd/") == want {
				buildTarget = ep
				break
			}
		}
		if buildTarget == "" {
			if len(entrypoints) == 1 {
				buildTarget = entrypoints[0]
			} else {
				return "", fmt.Errorf("module %s has several cmd entrypoints and none matches the binary %q", moduleDir, want)
			}
		}
	}

	goTool, err := goToolPath()
	if err != nil {
		return "", fmt.Errorf("go toolchain not found — install Go (https://go.dev/dl/) or put the built binary next to app.json")
	}
	cmd := exec.Command(goTool, "build", "-o", output, buildTarget)
	hideWindow(cmd)
	cmd.Dir = moduleDir
	var buf bytes.Buffer
	if logw != nil {
		fmt.Fprintf(logw, "appdeck: go build -o %s (in %s)\n", filepath.Base(output), moduleDir)
		cmd.Stdout = io.MultiWriter(&buf, logw)
		cmd.Stderr = io.MultiWriter(&buf, logw)
	} else {
		cmd.Stdout = &buf
		cmd.Stderr = &buf
	}
	if err := cmd.Run(); err != nil {
		outputText := buf.Bytes()
		message := strings.TrimSpace(string(outputText))
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("go build in %s: %s", moduleDir, message)
	}
	if !fileExists(output) {
		return "", fmt.Errorf("go build completed without creating %s", output)
	}
	if err := ensureExecutable(output); err != nil {
		return "", fmt.Errorf("make built binary executable: %s: %v", output, err)
	}
	return output, nil
}

// goToolPath locates the go toolchain through the shared lookup — a
// user-registered path (🧰 tab) wins; then PATH; then the well-known
// locations (double-clicked .app launches get a minimal PATH
// (/usr/bin:/bin:/usr/sbin:/sbin) that excludes every usual Go install, so
// PATH alone is not enough).
func goToolPath() (string, error) {
	if p := findToolPath("go", "go"); p != "" {
		return p, nil
	}
	return "", exec.ErrNotFound
}

func hasMainPackage(dir string) bool {
	for _, path := range goSourceFiles(dir) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), "package main") {
			return true
		}
	}
	return false
}

func ensureExecutable(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o111 == 0 {
		return os.Chmod(path, info.Mode().Perm()|0o755)
	}
	return nil
}

// childEnv builds the child environment — the app contract every app can
// rely on (see AUTHORING.md): the inherited environment, then
//
//	PORT, APPDECK_PORT   the port to serve on ({port} in args does the same)
//	APPDECK_HOST         the host to bind (loopback unless network: true)
//	APPDECK_APP_ID       the stable app id
//	APPDECK_DATA         the app's own writable data directory (created)
//	APPDECK_TOKEN        per-start secret; when set, requests must carry it
//	APPDECK_LAUNCHER_URL the dashboard URL (for apps that link back)
//
// then the app's own env entries with {port}/{host} substituted.
func (s *Server) childEnv(a *App, token string) []string {
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		if i := strings.Index(kv, "="); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	port := strconv.Itoa(a.Port)
	host := bindHostFor(a)
	env["PORT"] = port
	env["APPDECK_PORT"] = port
	env["APPDECK_HOST"] = host
	env["APPDECK_APP_ID"] = a.ID
	// Declared permissions, for cards that want to know what they asked for
	// (permissions.go) — and for the sandbox profiles built from them.
	if len(a.Permissions) > 0 {
		env["APPDECK_PERMISSIONS"] = grantEnvValue(a.Permissions)
	}
	// The event bus address (eventbus.go): cards publish and subscribe
	// through the launcher instead of finding each other's ports.
	env["APPDECK_BUS_URL"] = fmt.Sprintf("http://127.0.0.1:%d", s.cfg.Port)
	// A GUI launch inherits Launch Services' minimal PATH
	// (/usr/bin:/bin:/usr/sbin:/sbin); shell-cmd apps ("node server.js") and
	// builds' own subprocesses resolve their tools through it. Append the
	// well-known tool dirs the launcher already probes — additions only, so
	// nothing the operator put on PATH is ever shadowed.
	env["PATH"] = extendPath(env["PATH"], append(toolBinDirs(), systemBinDirs()...))
	// Best python3 on the machine (APPDECK_PYTHON): PATH extension alone
	// leaves the CLT 3.9 stub ahead of every newer install, and venv cmd
	// conventions (${APPDECK_PYTHON:-python3}) build on the newest.
	if py := appPythonPath(); py != "" {
		env["APPDECK_PYTHON"] = py
	}
	if a.Network {
		env["APPDECK_NETWORK"] = "1" // Wi-Fi sharing is on; Host checks may loosen
	}
	if token != "" {
		env["APPDECK_TOKEN"] = token
	}
	// The app-facing data home (a 💾 Storage data/app placement may link it
	// to another drive; the launcher's own files stay beside it, local).
	if home := s.appDataHome(a); home != "" {
		env["APPDECK_DATA"] = home
	}
	if dataDir := s.appDataDir(a); dataDir != "" {
		env["APPDECK_DATA"] = dataDir
	}
	env["APPDECK_LAUNCHER_URL"] = fmt.Sprintf("http://127.0.0.1:%d", s.cfg.Port)
	for k, v := range a.Env {
		env[k] = substitutePlaceholders(v, a.Port, host)
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// appDataDir returns the app's own writable directory under the launcher's
// per-user data dir (created on start): <data>/appdata/<app-id>. Apps must
// never write next to their binary — on macOS bundles and restricted
// Windows installs that fails, and updates would wipe user data.
func (s *Server) appDataDir(a *App) string {
	if s.cfg.DataDir == "" {
		return ""
	}
	dir := filepath.Join(s.cfg.DataDir, "appdata", a.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return dir
}

// newToken generates a fresh 256-bit hex token for one app start.
func newToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano()) // never expected; better than empty
	}
	return hex.EncodeToString(buf)
}

// stopApp runs one stop with the in-flight guard (Python stop_app).
func (s *Server) stopApp(a *App) *actionResp {
	s.stopKeepAlive(a.ID)
	// An app started on a custom port must be probed/killed on that port;
	// the override lives in the state entry until the stop removes it.
	if !a.Desktop {
		if e := loadState(s.cfg.StateFile)[a.ID].PortOverride; e != 0 {
			c := *a
			c.Port = e
			a = &c
		}
	}
	s.mu.Lock()
	if _, busy := s.inflight[a.ID]; busy {
		s.mu.Unlock()
		return &actionResp{OK: false, Busy: true, Error: busyMessage}
	}
	s.inflight[a.ID] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, a.ID)
		s.mu.Unlock()
	}()
	return s.stopAppLocked(a)
}

func (s *Server) stopAppLocked(a *App) *actionResp {
	if a.ServedBy == "launcher" {
		return s.stopHostedStatic(a)
	}
	st := loadState(s.cfg.StateFile)
	pid := st[a.ID].PID
	wasPrivileged := st[a.ID].Privileged
	killed := []int{}
	var elevatedErr error

	// 1) the instance this launcher started (whole process group)
	if pid != 0 && pidAlive(pid) {
		killTree(pid)
		killed = append(killed, pid)
		for i := 0; i < stopWaitRounds; i++ {
			if !pidAlive(pid) {
				break
			}
			time.Sleep(stopWaitSleep)
		}
		if pidAlive(pid) {
			killTreeForce(pid)
		}
		// A privileged start survives both kills (root/admin owns it, the
		// launcher's standard-user token cannot signal it): re-raise the
		// OS prompt and let an elevated taskkill/kill finish the job.
		if pidAlive(pid) && wasPrivileged {
			if err := stopPrivilegedElevated(pid); err == nil {
				for i := 0; i < stopWaitRounds; i++ {
					if !pidAlive(pid) {
						break
					}
					time.Sleep(stopWaitSleep)
				}
			}
		}
	}

	// 2) anything else of ours still listening on the port (external start);
	// desktop apps have no port, so there is nothing external to find
	elevatedTried := false
	if !a.Desktop && portOpen(a.Port, defaultProbeTimeout) {
		for _, p := range listenerPIDs(a.Port) {
			if killProc(p) == nil {
				killed = append(killed, p)
			}
		}
		for i := 0; i < stopWaitRounds; i++ {
			if !portOpen(a.Port, startProbeTimeout) {
				break
			}
			time.Sleep(stopWaitSleep)
		}
		if portOpen(a.Port, startProbeTimeout) {
			for _, p := range listenerPIDs(a.Port) {
				killProcForce(p)
			}
			for i := 0; i < stopWaitRounds; i++ {
				if !portOpen(a.Port, startProbeTimeout) {
					break
				}
				time.Sleep(stopWaitSleep)
			}
		}
		// An elevated instance survives every standard-token kill (Windows
		// integrity levels). That is not limited to the state-tracked PID
		// above: a UAC start by a previous launcher run — its state entry
		// long gone — lives on as an orphaned listener. Re-raise the OS
		// prompt for it too, the same elevated taskkill the privileged
		// start path uses.
		if portOpen(a.Port, startProbeTimeout) {
			for _, p := range listenerPIDs(a.Port) {
				elevatedTried = true
				if err := stopPrivilegedElevated(p); err != nil {
					elevatedErr = err
				}
			}
			for i := 0; i < stopWaitRounds; i++ {
				if !portOpen(a.Port, startProbeTimeout) {
					break
				}
				time.Sleep(stopWaitSleep)
			}
		}
	}

	s.mu.Lock()
	st = loadState(s.cfg.StateFile)
	if entry := st[a.ID]; entry.StartedAt > 0 {
		s.recordAppStop(a.ID, entry.StartedAt) // fold this run into the usage totals
	}
	delete(st, a.ID)
	saveState(s.cfg.StateFile, st)
	s.mu.Unlock()

	// Desktop apps have no port to probe: their liveness is the PID itself.
	still := !a.Desktop && portOpen(a.Port, startProbeTimeout)
	if a.Desktop && pid != 0 && pidAlive(pid) {
		still = true
	}
	resp := &actionResp{OK: !still, Killed: &killed, StillRunning: &still}
	if still && wasPrivileged {
		resp.Error = "still running — approve the elevation prompt (UAC on Windows, polkit/sudo on Linux) when Stop asks, then Stop again"
	} else if still && elevatedTried {
		if elevatedErr != nil && uacDeclined(elevatedErr.Error()) {
			resp.Error = "elevation declined — the app keeps running; Stop again and approve the prompt to stop it"
		} else {
			resp.Error = "still running — approve the elevation prompt (UAC on Windows) when Stop asks, then Stop again"
		}
	}
	return resp
}

// killAllApps stops every app this launcher started (state-tracked instances).
// It backs --kill-apps-on-exit: on Ctrl+C or closing the console the panel
// takes the apps it started down with it instead of leaving them detached.
// Apps running without a state entry (started outside the launcher) survive.
func (s *Server) killAllApps() {
	st := loadState(s.cfg.StateFile)
	for _, a := range s.appsList() {
		if _, ours := st[a.ID]; !ours {
			continue
		}
		if res := s.stopApp(a); !res.OK {
			fmt.Printf("  ! could not stop %s: %s\n", a.Name, res.Error)
		}
	}
}

// tailLog returns the last `lines` lines of the app's log ([] when missing).
func (s *Server) tailLog(a *App, lines int) []string {
	data, err := os.ReadFile(s.logPath(a))
	if err != nil {
		return []string{}
	}
	// Windows privileged starts split stderr into a sibling file (the
	// elevated PowerShell cannot merge two streams into one file); show it
	// as part of the same log.
	if errData, err := os.ReadFile(s.logPath(a) + ".err"); err == nil && len(errData) > 0 {
		data = append(data, append([]byte("\n--- stderr ---\n"), errData...)...)
	}
	all := splitLines(strings.ToValidUTF8(string(data), "\uFFFD"))
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	if all == nil {
		return []string{}
	}
	return all
}

// splitLines mirrors Python str.splitlines() for \n / \r\n / \r input
// (trailing newline does not produce a trailing empty line).
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// ---------------------------------------------------- per-app icon files ----

// startHealthProber refreshes the does-it-actually-answer cache every 20s.
// Running ≠ serving: a wedged process holds its pid but stops answering.
func (s *Server) startHealthProber() {
	go func() {
		for {
			st := loadState(s.cfg.StateFile)
			for _, a := range s.appsList() {
				if a.Desktop {
					continue
				}
				entry := st[a.ID]
				port := a.Port
				if entry.PortOverride != 0 {
					port = entry.PortOverride
				}
				if entry.PID == 0 || !pidAlive(entry.PID) {
					continue // stopped — the prober has no opinion
				}
				// A manifest with a health path is probed on that path: the
				// port can answer while the app is still booting, and Ready
				// should mean the app says it is ready.
				probePath := a.URLPath
				if a.Health != "" {
					probePath = a.Health
				}
				client := http.Client{Timeout: 2 * time.Second}
				resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, probePath))
				ok := err == nil
				if resp != nil {
					resp.Body.Close()
				}
				s.mu.Lock()
				s.health[a.ID] = ok
				s.mu.Unlock()
				// Contract lint: a this-computer-only app must not answer
				// on the LAN address. A refused/timeout dial means it bound
				// loopback as promised; anything else means the app ignored
				// APPDECK_HOST and the card gets a ⚠ exposed badge.
				if !a.Network {
					exposed := false
					if lan := lanIP(); lan != "" {
						exposed = hostPortOpen(lan, port, defaultProbeTimeout)
					}
					s.mu.Lock()
					s.lanExposed[a.ID] = exposed
					s.mu.Unlock()
				}
			}
			time.Sleep(20 * time.Second)
		}
	}()
}

// startWatchFolder rescans the app container every 10s; a folder dropped in
// with a parseable app.json joins the deck without touching the launcher.
func (s *Server) startWatchFolder() {
	go func() {
		known := map[string]bool{}
		for _, a := range s.appsList() {
			known[a.ID] = true
		}
		for range time.Tick(10 * time.Second) {
			container, err := s.appsContainerDir()
			if err != nil {
				continue
			}
			entries, err := os.ReadDir(container)
			if err != nil {
				continue
			}
			fresh := false
			for _, e := range entries {
				if !e.IsDir() || known[e.Name()] {
					continue
				}
				data, err := os.ReadFile(filepath.Join(container, e.Name(), manifestFileName))
				if err != nil {
					continue
				}
				var probe struct {
					ID string `json:"id"`
				}
				if json.Unmarshal(data, &probe) != nil || strings.TrimSpace(probe.ID) == "" {
					continue // half-copied folder — wait until it settles
				}
				fresh = true
			}
			if !fresh {
				continue
			}
			if err := s.reloadApps(); err != nil {
				continue
			}
			known = map[string]bool{}
			for _, a := range s.appsList() {
				known[a.ID] = true
			}
		}
	}()
}

// startKeepAlive spawns the crash watchdog for one app: poll its state pid
// every KeepAliveDelay seconds (default 10) and start it again when the
// process dies. Gives up after KeepAliveLimit consecutive failures (default
// 5) — a crash loop must stay visible, not silent. The "always" lifecycle
// policy turns the watchdog on for any process app, manifest flag or not.
func (s *Server) startKeepAlive(a *App) {
	if a.Desktop || a.ServedBy == "launcher" {
		return
	}
	if !a.KeepAlive && s.policyFor(a).Mode != "always" {
		return
	}
	s.mu.Lock()
	if _, watching := s.keepAlive[a.ID]; watching {
		s.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	s.keepAlive[a.ID] = stop
	s.mu.Unlock()
	limit := a.KeepAliveLimit
	if limit <= 0 {
		limit = 5
	}
	delay := time.Duration(a.KeepAliveDelay) * time.Second
	if delay <= 0 {
		delay = 10 * time.Second
	}
	go func() {
		failures := 0
		for {
			select {
			case <-stop:
				return
			case <-time.After(delay):
			}
			st := loadState(s.cfg.StateFile)[a.ID]
			if st.PID != 0 && pidAlive(st.PID) {
				failures = 0
				continue
			}
			if failures >= limit {
				return
			}
			failures++
			s.startApp(a)
		}
	}()
}

// stopKeepAlive retires the crash watcher before (and only before) a
// deliberate stop — the next start spawns a fresh one.
func (s *Server) stopKeepAlive(id string) {
	s.mu.Lock()
	stop, watching := s.keepAlive[id]
	delete(s.keepAlive, id)
	s.mu.Unlock()
	if watching {
		close(stop)
	}
}

// ---- deck export / import: the whole user setup (Apps container, registry,
// Wi-Fi overrides) as one zip, so moving to a new machine is a download and
// an upload instead of hand-copying folders. Loopback only — the deck is
// this machine's operator's data.
