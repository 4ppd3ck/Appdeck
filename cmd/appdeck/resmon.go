// resmon.go — per-card resource sampling: resident RAM, CPU and thread
// counts for every running app's process tree. Pure Go through gopsutil
// (sysctl on darwin, /proc on Linux, the Windows API elsewhere) — no cgo,
// no per-platform files. One goroutine refreshes a small cache every few
// seconds; the dashboard, the tray and GET /api/resources all read the last
// snapshot — sampling never runs on a request path.
package main

import (
	"sort"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// resmonInterval is the sampling cadence. Cheap on purpose: one pass over
// the process table per tick (a few hundred sysctl/proc reads, done in
// well under a tick's budget), so menus and cards stay honest without a
// probe storm.
const resmonInterval = 3 * time.Second

// appResources is one card's sample. It travels inside /api/apps entries
// (the card chip) and on GET /api/resources (the ⚙ monitor table).
type appResources struct {
	PID        int     `json:"pid"`
	RSSMB      float64 `json:"rssMB"`             // resident set of the whole tree, MB
	CPUPercent float64 `json:"cpuPercent"`        // % of one core, tree total since the previous tick
	Threads    int     `json:"threads,omitempty"` // tree total
	Procs      int     `json:"procs,omitempty"`   // processes in the tree (shell-cmd apps run ≥ 2: the sh shim plus the program)
}

// pidSample is one PID's raw reading: RSS in bytes and cumulative CPU
// seconds (user+system since process start). createdMS guards against a
// recycled PID pretending to be the app.
type pidSample struct {
	rssBytes  uint64
	cpuTotal  float64
	threads   int32
	createdMS int64
}

// samplePID reads one PID through gopsutil. ok=false when every read failed
// (the process is gone); a partially failed read still counts — partial data
// beats nothing and the next tick fills the gap.
func samplePID(pid int) (pidSample, bool) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return pidSample{}, false
	}
	var out pidSample
	ok := false
	if mi, err := p.MemoryInfo(); err == nil && mi != nil {
		out.rssBytes = mi.RSS
		ok = true
	}
	if t, err := p.Times(); err == nil && t != nil {
		out.cpuTotal = t.User + t.System
		ok = true
	}
	if n, err := p.NumThreads(); err == nil {
		out.threads = n
	}
	if c, err := p.CreateTime(); err == nil {
		out.createdMS = c
	}
	return out, ok
}

// treeClosure returns roots plus every descendant reachable through the
// parent map (child → parent pid). A child whose parent is missing from the
// map counts as reachable only if named a root — the map is the whole table,
// so reaped intermediates simply leave their children orphaned at 1/init on
// unix (which is never an app root here) and drop out naturally.
func treeClosure(parent map[int]int, roots []int) []int {
	children := make(map[int][]int, len(parent))
	for c, p := range parent {
		children[p] = append(children[p], c)
	}
	seen := make(map[int]bool, len(roots))
	out := make([]int, 0, len(roots))
	stack := append([]int(nil), roots...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		stack = append(stack, children[id]...)
	}
	sort.Ints(out)
	return out
}

// resmonLoop refreshes the cache forever. Started from main once the server
// exists; tests construct Servers without it, so handler tests never sample.
func (s *Server) resmonLoop() {
	ticker := time.NewTicker(resmonInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.resmonTick()
	}
}

// resmonTick samples every running app's process tree into the cache.
func (s *Server) resmonTick() {
	st := loadState(s.cfg.StateFile)
	s.mu.Lock()
	apps := s.apps
	s.mu.Unlock()

	// Tracked: id → state entry for apps whose recorded PID is still alive.
	type track struct {
		pid       int
		startedAt float64
	}
	tracked := map[string]track{}
	for _, a := range apps {
		e := st[a.ID]
		if e.PID == 0 || !pidAlive(e.PID) {
			continue
		}
		tracked[a.ID] = track{pid: e.PID, startedAt: e.StartedAt}
	}
	if len(tracked) == 0 {
		s.resMu.Lock()
		s.resCache = map[string]appResources{}
		s.resCPULast = map[int]float64{}
		s.resMu.Unlock()
		return
	}

	// One pass over the process table for parent links; per-PID detail reads
	// only happen for tree members.
	procs, err := process.Processes()
	if err != nil {
		return
	}
	parent := make(map[int]int, len(procs))
	for _, p := range procs {
		if pp, err := p.Ppid(); err == nil {
			parent[int(p.Pid)] = int(pp)
		}
	}

	s.resMu.Lock()
	lastTick := s.resLast
	cpuLast := s.resCPULast
	s.resMu.Unlock()
	now := time.Now()
	elapsed := now.Sub(lastTick).Seconds()

	snap := make(map[string]appResources, len(tracked))
	cpuSeen := make(map[int]float64, len(tracked))
	for id, tr := range tracked {
		tree := treeClosure(parent, []int{tr.pid})
		var rss uint64
		var threads int32
		var cpu float64
		sampled := 0
		recycled := false
		for _, pid := range tree {
			smp, ok := samplePID(pid)
			if !ok {
				continue
			}
			// PID-reuse guard: a process created long before this app's
			// start record is someone else who now holds the number.
			if tr.startedAt > 0 && smp.createdMS > 0 && smp.createdMS < int64((tr.startedAt-90)*1000) {
				recycled = true
				break
			}
			rss += smp.rssBytes
			threads += smp.threads
			cpu += smp.cpuTotal
			sampled++
		}
		if recycled || sampled == 0 {
			continue
		}
		res := appResources{
			PID:     tr.pid,
			RSSMB:   float64(rss) / (1024 * 1024),
			Threads: int(threads),
			Procs:   sampled,
		}
		if last, have := cpuLast[tr.pid]; have && elapsed > 0.5 {
			pct := (cpu - last) / elapsed * 100
			if pct < 0 {
				pct = 0 // a dying process's counters can go backwards
			}
			res.CPUPercent = pct
		}
		cpuSeen[tr.pid] = cpu
		snap[id] = res
	}

	s.resMu.Lock()
	s.resCache = snap
	s.resCPULast = cpuSeen
	s.resLast = now
	s.resMu.Unlock()
}

// resourcesSnapshot copies the whole cache (id → sample).
func (s *Server) resourcesSnapshot() map[string]appResources {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	out := make(map[string]appResources, len(s.resCache))
	for id, r := range s.resCache {
		out[id] = r
	}
	return out
}

// appResFor returns the cached sample for one app (nil when none).
func (s *Server) appResFor(id string) *appResources {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	if r, ok := s.resCache[id]; ok {
		return &r
	}
	return nil
}

// resUpdatedAt is the last tick's unix timestamp (0 = never sampled).
func (s *Server) resUpdatedAt() float64 {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	if s.resLast.IsZero() {
		return 0
	}
	return float64(s.resLast.UnixNano()) / 1e9
}
