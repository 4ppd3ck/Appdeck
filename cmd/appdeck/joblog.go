// Live progress logs for imports and installs. The dashboard starts an
// import with ?jobid=<uuid>, the handler logs its milestones into that job
// while it works, and the dashboard polls GET /api/import/progress?id=… to
// render them as they happen — the HTTP import request itself only returns
// at the end, so the response stays the authority for success/failure.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// jobLog is one running (or just-finished) import's progress lines. Safe
// for concurrent use; nil-receiver methods are no-ops so call sites can
// pass a nil job when no dashboard is watching.
type jobLog struct {
	mu      sync.Mutex
	lines   []string
	done    bool
	ok      bool
	errText string
	// ctx is canceled by cancelJob — the long-running steps of an import
	// (git clone, download) watch it, so a stuck import can be aborted from
	// the dock instead of waiting out the full timeout.
	ctx        context.Context
	cancelFunc context.CancelFunc
	canceled   bool
	// lastStep is the time of the newest log line — the watchdog's clock.
	lastStep time.Time
	// stall is the no-progress limit: when no log line arrives for this
	// long the watchdog cancels the import automatically (0 = off). Imports
	// log their way through every step, and downloads/clone progress keep
	// the clock alive, so a silent job is a stuck one.
	stall time.Duration
	// onDone fires exactly once when the job finishes, with a copy of the
	// lines — the launcher hooks it to persist the import log to disk (the
	// live dock reads the in-memory lines, which are capped and pruned).
	onDone func(lines []string, ok bool, errText string)
}

// errImportCanceled is the failure an import reports when the dock's cancel
// landed before it completed.
var errImportCanceled = errors.New("import canceled")

// jobDefaultStall is the default no-progress timeout for an import (the
// import dialog's selector overrides it per import; 0 disables it).
const jobDefaultStall = 5 * time.Minute

func newJobLog() *jobLog {
	ctx, cancel := context.WithCancel(context.Background())
	return &jobLog{ctx: ctx, cancelFunc: cancel, lastStep: time.Now(), stall: jobDefaultStall}
}

// Context returns the job's cancellation context (Background on a nil job).
func (j *jobLog) Context() context.Context {
	if j == nil {
		return context.Background()
	}
	return j.ctx
}

// Canceled reports whether cancelJob has fired (false on a nil job).
func (j *jobLog) Canceled() bool {
	if j == nil {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.canceled
}

// cancelJob aborts a running import: cancels its context so git clone and
// downloads fail fast, and flags every following step to bail out. Reports
// whether this call actually canceled something (a finished or already
// canceled job does not).
func (j *jobLog) cancelJob() bool {
	if j == nil {
		return false
	}
	j.mu.Lock()
	if j.done || j.canceled {
		j.mu.Unlock()
		return false
	}
	j.canceled = true
	cancel := j.cancelFunc
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	j.logf("cancel requested — aborting the import…")
	return true
}

const jobMaxLines = 400

// logf appends one timestamped line (a no-op on a nil job) and advances the
// watchdog's no-progress clock.
func (j *jobLog) logf(format string, args ...any) {
	if j == nil {
		return
	}
	line := time.Now().Format("15:04:05") + "  " + fmt.Sprintf(format, args...)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lines = append(j.lines, line)
	j.lastStep = time.Now()
	if len(j.lines) > jobMaxLines {
		j.lines = j.lines[len(j.lines)-jobMaxLines:]
	}
}

// setStall overrides the no-progress timeout (0 disables the watchdog).
func (j *jobLog) setStall(d time.Duration) {
	if j == nil {
		return
	}
	j.mu.Lock()
	j.stall = d
	j.mu.Unlock()
}

// startWatchdog runs the no-progress watchdog until the job finishes: an
// import that logs nothing for j.stall is canceled automatically (a stuck
// clone, a dead connection). Progress lines — including download and clone
// streaming — keep the clock alive, so only genuinely silent jobs trip it.
// The check interval adapts to the limit (fast enough to matter for the
// small stalls tests use, capped at 5 s in production).
func (j *jobLog) startWatchdog() {
	if j == nil {
		return
	}
	go func() {
		for {
			j.mu.Lock()
			done, canceled, stall, idle := j.done, j.canceled, j.stall, time.Since(j.lastStep)
			j.mu.Unlock()
			if done || canceled {
				return
			}
			if stall > 0 && idle > stall {
				j.logf("WARNING: no progress for %s — canceling the import automatically "+
					"(raise the timeout in the import dialog for slow sources)", stall.Truncate(time.Second))
				j.cancelJob()
				return
			}
			tick := 5 * time.Second
			if d := stall / 5; d > 0 && d < tick {
				tick = d
			}
			if tick < 200*time.Millisecond {
				tick = 200 * time.Millisecond
			}
			time.Sleep(tick)
		}
	}()
}

// finish marks the job done; errText empty means success. Success notes are
// LOG LINES (logf + finish("")), never finish's argument — finish's argument
// is the failure reason, and the dock renders whatever a non-empty one as
// the job's error.
func (j *jobLog) finish(errText string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	j.done, j.ok, j.errText = true, errText == "", errText
	lines := append([]string(nil), j.lines...)
	hook := j.onDone
	j.mu.Unlock()
	// Release the context resources of a job that finished on its own.
	if j.cancelFunc != nil {
		j.cancelFunc()
	}
	if hook != nil {
		hook(lines, j.ok, j.errText)
	}
}

// succeed logs the success note as a regular line and marks the job ok —
// the one true success tail (finish("") is what makes the dock green).
func (j *jobLog) succeed(note string) {
	j.logf("%s", note)
	j.finish("")
}

// snapshot returns the current lines and completion state.
func (j *jobLog) snapshot() (lines []string, done, ok bool, errText string) {
	if j == nil {
		return nil, true, true, ""
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]string, len(j.lines))
	copy(out, j.lines)
	return out, j.done, j.ok, j.errText
}

// importsLogName is the file every finished import appends its full log to,
// under the launcher's logs directory (next to per-app logs and appdeck.log).
const importsLogName = "imports.log"

// persistImportLog appends one finished import's lines to the launcher's
// imports.log. Best effort: a full disk must never fail an import that
// already succeeded.
func persistImportLog(path string, lines []string, ok bool, errText string) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	status := "ok"
	if !ok {
		status = "FAILED: " + errText
	}
	fmt.Fprintf(f, "===== import %s — %s =====\n", time.Now().Format("2006-01-02 15:04:05"), status)
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
	fmt.Fprintln(f)
}

// jobsMaxFinished caps how many finished jobs linger for late pollers.
const jobsMaxFinished = 20

func (s *Server) job(id string) *jobLog {
	if id == "" {
		return nil
	}
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if j, ok := s.jobs[id]; ok {
		return j
	}
	// Prune: drop finished jobs past the cap, oldest first.
	if finished := countFinished(s.jobs); finished >= jobsMaxFinished {
		dropOldestFinished(s.jobs, finished-jobsMaxFinished+1)
	}
	j := newJobLog()
	j.startWatchdog()
	if s.cfg.LogDir != "" {
		path := filepath.Join(s.cfg.LogDir, importsLogName)
		j.onDone = func(lines []string, ok bool, errText string) {
			persistImportLog(path, lines, ok, errText)
		}
	}
	if s.jobs == nil {
		s.jobs = map[string]*jobLog{}
	}
	s.jobs[id] = j
	return j
}

func countFinished(jobs map[string]*jobLog) int {
	n := 0
	for _, j := range jobs {
		done, _, _ := func() (bool, bool, string) { j.mu.Lock(); defer j.mu.Unlock(); return j.done, j.ok, j.errText }()
		if done {
			n++
		}
	}
	return n
}

func dropOldestFinished(jobs map[string]*jobLog, n int) {
	// Order by first line's timestamp; jobs without lines go first out.
	for id, j := range jobs {
		if n <= 0 {
			return
		}
		j.mu.Lock()
		done := j.done
		j.mu.Unlock()
		if done {
			delete(jobs, id)
			n--
		}
	}
}

// serveImportCancel answers POST /api/import/cancel {id} — the dock's
// Cancel button. Loopback is enforced by the route (imports write into the
// app container); the job handle is the same unguessable one-shot id the
// progress poll uses.
func (s *Server) serveImportCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	readBodyInto(r, &req)
	s.jobsMu.Lock()
	j := s.jobs[req.ID]
	s.jobsMu.Unlock()
	if j == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown job id"})
		return
	}
	canceled := j.cancelJob()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": canceled})
}

// importCanceled is the shared checkpoint: when the dock's cancel landed,
// finish the job with the named error and answer the import request.
// Reports whether the import should stop.
func importCanceled(w http.ResponseWriter, job *jobLog) bool {
	if !job.Canceled() {
		return false
	}
	job.finish(errImportCanceled.Error())
	writeErrJSON(w, errImportCanceled.Error())
	return true
}

// serveJobProgress answers GET /api/import/progress?id=… — the dashboard's
// poll while an import runs. Read-only and unauthenticated: job ids are
// unguessable one-shot handles, and the lines carry no secrets.
func (s *Server) serveJobProgress(w http.ResponseWriter, r *http.Request) {
	id := parseQuery(r.RequestURI)["id"]
	s.jobsMu.Lock()
	j := s.jobs[id]
	s.jobsMu.Unlock()
	lines, done, ok, errText := j.snapshot()
	out := map[string]any{"lines": lines, "done": done, "ok": ok}
	if errText != "" {
		out["error"] = errText
	}
	// A paused security prompt rides the poll: the dock renders the
	// findings with Install anyway / Cancel while the job waits.
	if p, found := s.securityPendingForJob(id); found {
		out["pendingSecurity"] = true
		out["scanId"] = p.ID
		out["findings"] = p.Findings
	}
	s.writeJSON(w, http.StatusOK, out)
}
