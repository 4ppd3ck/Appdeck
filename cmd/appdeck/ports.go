// Automatic port assignment (ports.json).
//
// Web apps may declare port 0 ("auto-assign") in their manifest. The
// registry hands each such app a port from a reserved range and remembers
// the assignment across restarts, so an app keeps the same URL day to day
// without the author pinning one. Ports already claimed by configured apps,
// the launcher's own port, and ports with a live listener are skipped.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

const (
	// autoPortLo/Hi is the auto-assignment range. Deliberately below every
	// platform's ephemeral port range (Linux defaults to 32768–60999, macOS
	// and Windows to 49152–65535), so an auto-assigned listener can never
	// collide with an outgoing connection's source port.
	autoPortLo = 24500
	autoPortHi = 25499
)

type portRegistry struct {
	mu   sync.Mutex
	path string
	data struct {
		Assignments map[string]int `json:"assignments"`
	}
}

func newPortRegistry(path string) *portRegistry {
	r := &portRegistry{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &r.data)
	}
	if r.data.Assignments == nil {
		r.data.Assignments = map[string]int{}
	}
	return r
}

// ensureAssigned gives every portless web app a stable port. taken holds the
// ports that are already spoken for (other apps' configured ports, the
// launcher's own port); a live listener check runs too, so a stale registry
// entry pointing at a busy port is re-assigned instead of fought over.
// Desktop apps (portless by design) are left alone.
func (r *portRegistry) ensureAssigned(a *App, id string, taken map[int]bool) {
	if a.Port != 0 || a.Desktop {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if p, ok := r.data.Assignments[id]; ok && !taken[p] {
		a.Port = p
		return
	}
	for p := autoPortLo; p <= autoPortHi; p++ {
		if taken[p] || portOpen(p, defaultProbeTimeout) {
			continue
		}
		a.Port = p
		r.data.Assignments[id] = p
		r.save()
		return
	}
	// Range exhausted: leave 0; start will fail with a clear port error.
}

// prune drops assignments for apps that no longer exist, so removing an app
// eventually frees its port for reuse.
func (r *portRegistry) prune(liveIDs map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for id := range r.data.Assignments {
		if !liveIDs[id] {
			delete(r.data.Assignments, id)
			changed = true
		}
	}
	if changed {
		r.save()
	}
}

func (r *portRegistry) save() {
	data, err := json.MarshalIndent(r.data, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "launcher: ports.json: %v\n", err)
		return
	}
	if err := os.WriteFile(r.path, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "launcher: ports.json: %v\n", err)
	}
}
