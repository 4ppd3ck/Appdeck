// permissions.go — declarative permission manifests. An app.json may list
// what the card needs — "network" (outbound internet), "lan" (reaching other
// machines on the LAN), "fs-read" / "fs-write" (files outside its own
// APPDECK_DATA), "exec" (spawning other programs). Before a card's first
// start the dashboard asks the operator to allow them — mobile-app style —
// and the verdict is remembered: once (this launch only) or always
// (permissions.json in the launcher data dir). Nothing is enforced yet by
// itself; the OS sandbox (sandbox.go) reads the same declarations to
// configure its profiles, so a denied card simply does not launch.
//
// The prompt only ever fires for apps that declare permissions. Legacy
// manifests and hand-written local apps stay untouched — the gate exists so
// unvetted GitHub imports announce themselves before they run.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// validPermissions are the permission names the launcher knows. Unknown
// names in a manifest are dropped, not fatal — the forward-compat rule the
// privilege list already follows (a newer launcher's vocabulary must still
// load here; the grant just does nothing).
var validPermissions = map[string]bool{
	"network":  true,
	"lan":      true,
	"fs-read":  true,
	"fs-write": true,
	"exec":     true,
}

// permissionLabels render the dialog's checklist in operator language.
var permissionLabels = map[string]string{
	"network":  "Talk to the internet (outbound requests)",
	"lan":      "Reach other machines on this network",
	"fs-read":  "Read files outside its own data folder",
	"fs-write": "Write files outside its own data folder",
	"exec":     "Run other programs / shell commands",
}

// permGrant is one app's remembered verdict ("always" grants only; "once"
// grants live in memory and die with the launcher run).
type permGrant struct {
	Allowed   []string `json:"allowed"`
	DecidedAt float64  `json:"decidedAt"`
}

// permissionsPath is <data>/permissions.json.
func (s *Server) permissionsPath() string {
	return filepath.Join(s.cfg.DataDir, "permissions.json")
}

// loadPermGrants reads the persisted "always" grants (absent = empty).
func loadPermGrants(path string) map[string]permGrant {
	out := map[string]permGrant{}
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) {
		return out
	}
	var doc struct {
		Apps map[string]permGrant `json:"apps"`
	}
	if json.Unmarshal(data, &doc) == nil && doc.Apps != nil {
		out = doc.Apps
	}
	return out
}

// savePermGrants writes the grant file atomically.
func (s *Server) savePermGrants() error {
	s.mu.Lock()
	doc := struct {
		Apps map[string]permGrant `json:"apps"`
	}{Apps: s.permGrants}
	s.mu.Unlock()
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	path := s.permissionsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// pendingPermissions returns the declared permissions the app does not yet
// hold a grant for, sorted for stable dialogs. An empty result means the
// start may proceed (nothing declared, everything granted, or prompts
// disabled at the deck level).
func (s *Server) pendingPermissions(a *App) []string {
	if len(a.Permissions) == 0 {
		return nil
	}
	ls := s.loadLauncherSettings()
	if ls.SkipPermissionPrompts {
		return nil // heavy-user opt-out: the operator answers for every card
	}
	s.mu.Lock()
	granted := s.permGrants[a.ID]
	once := s.permOnce[a.ID]
	s.mu.Unlock()
	have := map[string]bool{}
	for _, p := range granted.Allowed {
		have[p] = true
	}
	for _, p := range once {
		have[p] = true
	}
	var needed []string
	for _, p := range a.Permissions {
		if !have[p] {
			needed = append(needed, p)
		}
	}
	sort.Strings(needed)
	return needed
}

// grantPermissions records a verdict: mode "always" persists, "once" lasts
// for this launcher run. The allowed list replaces the stored one.
func (s *Server) grantPermissions(id string, allowed []string, mode string) error {
	clean := make([]string, 0, len(allowed))
	for _, p := range allowed {
		if validPermissions[p] {
			clean = append(clean, p)
		}
	}
	if mode != "always" {
		s.mu.Lock()
		s.permOnce[id] = clean
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	s.permGrants[id] = permGrant{Allowed: clean, DecidedAt: float64(time.Now().UnixNano()) / 1e9}
	s.mu.Unlock()
	return s.savePermGrants()
}

// servePermissionsGet answers GET /api/permissions: every app's declared
// permissions plus its standing grant (for the card badges and dialogs).
func (s *Server) servePermissionsGet(w http.ResponseWriter) {
	s.mu.Lock()
	apps := s.apps
	grants := make(map[string]permGrant, len(s.permGrants))
	for id, g := range s.permGrants {
		grants[id] = g
	}
	once := make(map[string][]string, len(s.permOnce))
	for id, p := range s.permOnce {
		once[id] = p
	}
	s.mu.Unlock()
	type appPerms struct {
		Declared []string   `json:"declared"`
		Granted  []string   `json:"granted"`
		Once     []string   `json:"once,omitempty"`
		Grant    *permGrant `json:"grant,omitempty"`
	}
	out := map[string]appPerms{}
	for _, a := range apps {
		if len(a.Permissions) == 0 {
			continue
		}
		ap := appPerms{Declared: a.Permissions}
		if g, ok := grants[a.ID]; ok {
			ap.Grant = &g
			ap.Granted = g.Allowed
		}
		if p, ok := once[a.ID]; ok {
			ap.Once = p
			ap.Granted = append(ap.Granted, p...)
		}
		out[a.ID] = ap
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"apps": out, "labels": permissionLabels})
}

// servePermissionsSet answers POST /api/permissions {id, allow, mode} — the
// dialog's verdict. "once" is kept in memory; "always" lands in
// permissions.json.
func (s *Server) servePermissionsSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID    string   `json:"id"`
		Allow []string `json:"allow"`
		Mode  string   `json:"mode"`
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
	if body.Mode != "once" && body.Mode != "always" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": `mode must be "once" or "always"`})
		return
	}
	for _, p := range body.Allow {
		if !validPermissions[p] {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unknown permission %q", p)})
			return
		}
	}
	if err := s.grantPermissions(body.ID, body.Allow, body.Mode); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "allowed": body.Allow})
}

// permPrompt is the start-gate payload: a start of a card with unanswered
// permissions refuses with Pending set, and the dashboard opens the dialog.
type permPrompt struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Needed   []string `json:"needed"`
	Declared []string `json:"declared"`
}

// grantEnvValue renders APPDECK_PERMISSIONS for the child (stable order).
func grantEnvValue(perms []string) string {
	cp := append([]string(nil), perms...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}
