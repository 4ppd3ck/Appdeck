// lifecycle.go — per-card lifecycle policies: Always On (boot with the
// launcher + crash watchdog), On-Demand (stop after the last client hangs
// up), Scheduled (run inside a daily time window). The manifest declares
// defaults; the operator can override per card (policies.json) from the
// dashboard's ⏱ dialog. One enforcement loop ticks every 30 s — starts and
// stops it performs are noted in the per-app log, never silent.
//
// Client presence (On-Demand): the loop samples the TCP table once per tick
// (gopsutil, pure Go everywhere) and calls an app busy while any ESTABLISHED
// connection touches its port. An open browser tab keeps connections alive;
// a closed one goes idle and the app stops after its idle budget.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/net"
)

// lifecycleLoopInterval is the enforcement cadence. Idle budgets are minutes;
// a 30 s tick keeps stop latencies predictable without probing aggressively.
const lifecycleLoopInterval = 30 * time.Second

// lifecycleIdleDefault is the On-Demand stop budget when neither the
// manifest nor the operator names one — the roadmap's "5 minutes after
// closing the tab".
const lifecycleIdleDefault = 5 * time.Minute

// lifecycleStartGrace keeps a freshly started app safe from its own quiet
// first seconds (a page that has not connected yet).
const lifecycleStartGrace = 90 * time.Second

// validLifecycles are the policy modes. "" (and "manual") is the default:
// the operator starts and stops the card by hand.
var validLifecycles = map[string]bool{
	"": true, "manual": true, "always": true, "onDemand": true, "scheduled": true,
}

// policyOverride is the operator's per-card override (policies.json in the
// launcher data dir). Zero fields inherit the manifest.
type policyOverride struct {
	Mode          string `json:"mode,omitempty"`
	StopAfterIdle int    `json:"stopAfterIdle,omitempty"` // minutes, On-Demand
	Schedule      string `json:"schedule,omitempty"`
}

// appPolicy is the effective policy for one card: manifest defaults with the
// operator override layered on top.
type appPolicy struct {
	Mode          string `json:"mode"`                    // "", "always", "onDemand", "scheduled"
	StopAfterIdle int    `json:"stopAfterIdle,omitempty"` // minutes
	Schedule      string `json:"schedule,omitempty"`
	Source        string `json:"source,omitempty"` // "override" when the operator set any field
}

// policyFor resolves one card's effective policy. Every override field
// present replaces its manifest counterpart; any override at all marks the
// policy as operator-sourced.
func (s *Server) policyFor(a *App) appPolicy {
	p := appPolicy{Mode: a.Lifecycle, StopAfterIdle: a.StopAfterIdle, Schedule: a.Schedule}
	s.mu.Lock()
	ov, has := s.policyOverrides[a.ID]
	s.mu.Unlock()
	if !has {
		return p
	}
	if ov.Mode != "" {
		p.Mode = ov.Mode
	}
	if ov.StopAfterIdle != 0 {
		p.StopAfterIdle = ov.StopAfterIdle
	}
	if ov.Schedule != "" {
		p.Schedule = ov.Schedule
	}
	p.Source = "override"
	return p
}

// idleBudget returns the On-Demand stop budget (>= 1 minute).
func (p appPolicy) idleBudget() time.Duration {
	if p.StopAfterIdle > 0 {
		return time.Duration(p.StopAfterIdle) * time.Minute
	}
	return lifecycleIdleDefault
}

// ---- schedule -----------------------------------------------------------------

// schedule is a parsed daily time window with optional weekdays:
// "09:00-17:00", "Mon-Fri 08:30-18:00", "Sat,Sun 10:00-22:00". A window that
// crosses midnight (to < from) contains both sides.
type schedule struct {
	days [7]bool // time.Weekday-indexed; all true when no weekdays given
	from int     // minutes since midnight
	to   int
}

var scheduleDayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// parseSchedule validates one schedule string. Errors are named so a bad
// manifest fails at load with the reason, not at first tick.
func parseSchedule(s string) (schedule, error) {
	var sc schedule
	for i := range sc.days {
		sc.days[i] = true
	}
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) > 2 || len(fields) == 0 {
		return sc, fmt.Errorf("schedule %q must be \"HH:MM-HH:MM\" or \"Mon-Fri HH:MM-HH:MM\"", s)
	}
	times := fields[len(fields)-1]
	if len(fields) == 2 {
		named := map[time.Weekday]bool{}
		for _, part := range strings.Split(fields[0], ",") {
			part = strings.TrimSpace(strings.ToLower(part))
			if part == "" {
				return sc, fmt.Errorf("schedule %q has an empty weekday (use Mon Tue Wed Thu Fri Sat Sun)", s)
			}
			lo, hi, isRange := strings.Cut(part, "-")
			d1, ok := scheduleDayNames[lo]
			if !ok {
				return sc, fmt.Errorf("schedule %q: unknown weekday %q (use Mon Tue Wed Thu Fri Sat Sun)", s, lo)
			}
			if isRange {
				d2, ok2 := scheduleDayNames[hi]
				if !ok2 {
					return sc, fmt.Errorf("schedule %q: unknown weekday %q", s, hi)
				}
				for d := d1; ; d = (d + 1) % 7 {
					named[d] = true
					if d == d2 {
						break
					}
				}
			} else {
				named[d1] = true
			}
		}
		for d := range sc.days {
			sc.days[d] = named[time.Weekday(d)]
		}
	}
	lo, hi, ok := strings.Cut(times, "-")
	if !ok {
		return sc, fmt.Errorf("schedule %q needs a from-to range like 09:00-17:00", s)
	}
	if sc.from, ok = parseClock(lo); !ok {
		return sc, fmt.Errorf("schedule %q: bad start time %q (HH:MM)", s, lo)
	}
	if sc.to, ok = parseClock(hi); !ok {
		return sc, fmt.Errorf("schedule %q: bad end time %q (HH:MM)", s, hi)
	}
	return sc, nil
}

// parseClock reads "HH:MM" into minutes-since-midnight.
func parseClock(s string) (int, bool) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || h == "" || m == "" {
		return 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// contains reports whether the schedule covers t.
func (sc schedule) contains(t time.Time) bool {
	if !sc.days[t.Weekday()] {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if sc.from <= sc.to {
		return m >= sc.from && m <= sc.to
	}
	return m >= sc.from || m <= sc.to // midnight-crossing window
}

// ---- enforcement ----------------------------------------------------------------

// connFetcher is the TCP-table seam the On-Demand idle check reads — the
// shape of gopsutil's net.Connections. NewServer installs the real one;
// tests stub it with canned tables.
type connFetcher func(kind string) ([]net.ConnectionStat, error)

// installNetFetcher wires the production TCP-table fetcher. Lives here (not
// inlined in NewServer) so main.go never needs the gopsutil net import.
func (s *Server) installNetFetcher() { s.netConns = net.Connections }

// busyPorts is one TCP-table pass's answer: the set of ports with at least
// one ESTABLISHED connection on either end. Pure function so tests can feed
// canned tables.
func busyPorts(conns []net.ConnectionStat) map[int]bool {
	out := map[int]bool{}
	for _, c := range conns {
		if !strings.EqualFold(c.Status, "ESTABLISHED") {
			continue
		}
		if c.Laddr.Port != 0 { // port 0 is never an app port
			out[int(c.Laddr.Port)] = true
		}
		if c.Raddr.Port != 0 {
			out[int(c.Raddr.Port)] = true
		}
	}
	return out
}

// lifecycleTick runs one enforcement pass: sample the TCP table once, then
// apply each app's policy.
func (s *Server) lifecycleTick() {
	s.mu.Lock()
	apps := s.apps
	s.mu.Unlock()

	// Policies decide what should run; a single TCP-table pass answers every
	// On-Demand idle question at once. s.netConns is a seam (tests stub it);
	// NewServer installs the gopsutil implementation.
	var conns []net.ConnectionStat
	for _, a := range apps {
		if s.policyFor(a).Mode == "onDemand" {
			var err error
			conns, err = s.netConns("inet")
			if err != nil {
				conns = nil // treat as "nothing is busy" — the next tick retries
			}
			break
		}
	}
	busy := busyPorts(conns)

	st := loadState(s.cfg.StateFile)
	now := time.Now()
	s.policyMu.Lock()
	defer s.policyMu.Unlock()

	for _, a := range apps {
		p := s.policyFor(a)
		effectivePort := a.Port
		if e := st[a.ID].PortOverride; e != 0 {
			effectivePort = e
		}
		running := st[a.ID].PID != 0 && pidAlive(st[a.ID].PID)
		switch p.Mode {
		case "onDemand":
			if a.Desktop || a.ServedBy == "launcher" {
				continue // nothing to watch for a portless or in-process app
			}
			if !running {
				delete(s.policyIdle, a.ID)
				continue
			}
			if busy[effectivePort] {
				s.policyIdle[a.ID] = now
				continue
			}
			last, seen := s.policyIdle[a.ID]
			if !seen {
				// First tick we notice the app: stamp now; the start-grace
				// check below covers the pre-tick quiet seconds.
				s.policyIdle[a.ID] = now
				continue
			}
			if now.Sub(last) < p.idleBudget() {
				continue
			}
			if now.Sub(time.Unix(int64(st[a.ID].StartedAt), 0)) < lifecycleStartGrace {
				continue // freshly started, no client yet — hold off
			}
			s.policyIdle[a.ID] = now // do not re-fire every tick if the stop fails
			go s.lifecycleStop(a, "on-demand idle budget elapsed (no connected client)")
		case "scheduled":
			sc, err := parseSchedule(p.Schedule)
			if err != nil {
				continue // a bad schedule never start/stops anything
			}
			if sc.contains(now) {
				if !running && !s.policyLoopStarted[a.ID] {
					// Only restart an app the loop started this window: an
					// operator's deliberate Stop inside the window stays.
					s.policyLoopStarted[a.ID] = true
					go s.lifecycleStart(a, fmt.Sprintf("schedule window opened (%s)", p.Schedule))
				}
			} else {
				delete(s.policyLoopStarted, a.ID)
				if running {
					go s.lifecycleStop(a, "schedule window closed")
				}
			}
		}
	}
}

// lifecycleLoop runs the enforcement pass forever. Started from main; tests
// construct Servers without it and drive lifecycleTick directly.
func (s *Server) lifecycleLoop() {
	ticker := time.NewTicker(lifecycleLoopInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.lifecycleTick()
	}
}

// lifecycleStart/lifecycleStop wrap the shared start/stop paths and record
// the loop's role in the app's own log — policy-driven actions are never
// silent.
func (s *Server) lifecycleStart(a *App, why string) {
	s.logLifecycleNote(a, "auto-start: "+why)
	if res := s.startApp(a); res != nil && !res.OK && !res.AlreadyRunning {
		s.policyMu.Lock()
		delete(s.policyLoopStarted, a.ID) // let the next tick try again
		s.policyMu.Unlock()
	}
}

func (s *Server) lifecycleStop(a *App, why string) {
	s.logLifecycleNote(a, "auto-stop: "+why)
	s.stopApp(a)
}

// logLifecycleNote appends one line to the app's log file (best effort).
func (s *Server) logLifecycleNote(a *App, note string) {
	if s.cfg.LogDir == "" {
		return
	}
	f, err := os.OpenFile(s.logPath(a), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "\n===== lifecycle %s =====\n%s\n", time.Now().Format("2006-01-02 15:04:05"), note)
}

// ---- persistence + API -----------------------------------------------------------

// policiesPath is <data>/policies.json; the file holds only operator
// overrides, so a missing file is the normal first-run state.
func (s *Server) policiesPath() string {
	return filepath.Join(s.cfg.DataDir, "policies.json")
}

// loadPolicyOverrides reads the operator override file (absent = empty).
func loadPolicyOverrides(path string) map[string]policyOverride {
	out := map[string]policyOverride{}
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) {
		return out
	}
	var doc struct {
		Apps map[string]policyOverride `json:"apps"`
	}
	if json.Unmarshal(data, &doc) == nil && doc.Apps != nil {
		for id, ov := range doc.Apps {
			if !validLifecycles[ov.Mode] {
				ov.Mode = "" // a newer launcher's mode falls back to inherit
			}
			out[id] = ov
		}
	}
	return out
}

// savePolicyOverrides writes the override file atomically.
func (s *Server) savePolicyOverrides() error {
	s.mu.Lock()
	doc := struct {
		Apps map[string]policyOverride `json:"apps"`
	}{Apps: s.policyOverrides}
	s.mu.Unlock()
	if doc.Apps == nil {
		doc.Apps = map[string]policyOverride{}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	path := s.policiesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// serveLifecycleGet answers GET /api/lifecycle: every app's effective policy
// plus which apps the loop started this window (the card badge's source).
func (s *Server) serveLifecycleGet(w http.ResponseWriter) {
	s.mu.Lock()
	apps := s.apps
	s.mu.Unlock()
	s.policyMu.Lock()
	loopStarted := make(map[string]bool, len(s.policyLoopStarted))
	for id := range s.policyLoopStarted {
		loopStarted[id] = true
	}
	s.policyMu.Unlock()
	out := map[string]appPolicy{}
	for _, a := range apps {
		out[a.ID] = s.policyFor(a)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"apps": out, "loopStarted": loopStarted})
}

// serveLifecycleSet answers POST /api/lifecycle {id, mode?, stopAfterIdle?,
// schedule?, clear?}: stores the operator override (empty fields inherit the
// manifest), validates the schedule, and clears the loop's memory for the
// app so the new mode applies cleanly on the next tick.
func (s *Server) serveLifecycleSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID            string `json:"id"`
		Mode          string `json:"mode"`
		StopAfterIdle int    `json:"stopAfterIdle"`
		Schedule      string `json:"schedule"`
		Clear         bool   `json:"clear"` // remove the override entirely (back to manifest)
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON body"})
		return
	}
	a := s.appByID(body.ID)
	if a == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown app " + body.ID})
		return
	}
	if !validLifecycles[body.Mode] {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("mode %q is not one of manual, always, onDemand, scheduled", body.Mode)})
		return
	}
	if body.StopAfterIdle < 0 || body.StopAfterIdle > 60*24*7 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "stopAfterIdle must be 0 (default) to 10080 minutes"})
		return
	}
	if body.Schedule != "" {
		if _, err := parseSchedule(body.Schedule); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	s.mu.Lock()
	if body.Clear || (body.Mode == "" && body.StopAfterIdle == 0 && body.Schedule == "") {
		delete(s.policyOverrides, body.ID)
	} else {
		s.policyOverrides[body.ID] = policyOverride{
			Mode: body.Mode, StopAfterIdle: body.StopAfterIdle, Schedule: body.Schedule,
		}
	}
	s.mu.Unlock()
	s.policyMu.Lock()
	delete(s.policyIdle, body.ID)
	delete(s.policyLoopStarted, body.ID)
	s.policyMu.Unlock()
	if err := s.savePolicyOverrides(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "policy": s.policyFor(a)})
}
