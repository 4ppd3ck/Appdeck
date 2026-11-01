// Uninstalled-app records: the list of payload apps the operator removed.
//
// An all-in-one (or --only) launcher carries its apps inside the executable,
// so deleting an unpacked folder is not enough — the next boot would put it
// back. This file is what makes "uninstall" mean uninstall in every build:
// the id is written here, extraction skips it, and the deck drops the card.
//
// It is deliberately conservative, because an earlier exclusion list in this
// codebase shipped a bad regression — a stale file silently hid half the deck
// after an update, with no way back. Three rules keep that from repeating:
//
//   - Only a document this launcher wrote is honoured. The file carries
//     "version": 1; anything else (an older build's list, a hand-written
//     array, a foreign JSON) is ignored, so it can never hide an app.
//   - Every removal is explicit and per-app: one id, written when the
//     operator uninstalls that one app. Nothing adds ids in bulk.
//   - Every removal is visible and reversible: the deck lists what is
//     removed and restores it in one click (⚙ Deck tools → Restore removed
//     apps), and importing the app again clears its record.
//
// A removal can be *forgotten* (🧹 on the deck's removed-apps shelf): the
// entry stays — for a payload app it is the only thing keeping the app off
// the deck — but it is no longer offered as a restore. Forgotten entries are
// never silent: they are listed under ⚙ Deck tools → Forgotten apps and can
// be restored from there. An entry whose app this build no longer carries has
// nothing left to block, so forgetting it drops the record entirely.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	removedAppsFileName = "uninstalled.json"
	removedAppsVersion  = 1
)

// removedApp is one removal: the app id plus when it happened (display only).
// Forgotten marks a removal the operator no longer wants offered back — the
// record itself must stay (it is what keeps a payload app off the deck), but
// the restore shelf hides it; the Forgotten apps list still shows it.
type removedApp struct {
	ID        string `json:"id"`
	At        string `json:"at,omitempty"`
	Forgotten bool   `json:"forgotten,omitempty"`
}

type removedAppsDoc struct {
	Version int          `json:"version"`
	Apps    []removedApp `json:"apps"`
}

func removedAppsPath(dataDir string) string {
	return filepath.Join(dataDir, removedAppsFileName)
}

// readRemovedAppsDoc loads the removal list. A missing, unreadable or
// foreign document yields an empty list — never an error: a corrupt record
// must not keep the launcher from starting.
func readRemovedAppsDoc(dataDir string) removedAppsDoc {
	out := removedAppsDoc{Version: removedAppsVersion}
	if dataDir == "" {
		return out
	}
	data, err := os.ReadFile(removedAppsPath(dataDir))
	if err != nil {
		return out
	}
	var doc removedAppsDoc
	if json.Unmarshal(data, &doc) != nil || doc.Version != removedAppsVersion {
		return out
	}
	seen := map[string]bool{}
	for _, a := range doc.Apps {
		id := strings.TrimSpace(a.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out.Apps = append(out.Apps, removedApp{ID: id, At: a.At, Forgotten: a.Forgotten})
	}
	return out
}

// readRemovedApps is the membership set behind the skip checks.
func readRemovedApps(dataDir string) map[string]bool {
	out := map[string]bool{}
	for _, a := range readRemovedAppsDoc(dataDir).Apps {
		out[a.ID] = true
	}
	return out
}

// writeRemovedAppsDoc replaces the record atomically. An empty list removes
// the file: "nothing is removed" is the absence of a record, not a stub.
func writeRemovedAppsDoc(dataDir string, apps []removedApp) error {
	if dataDir == "" {
		return nil
	}
	path := removedAppsPath(dataDir)
	if len(apps) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(removedAppsDoc{Version: removedAppsVersion, Apps: apps}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// markAppRemoved records one uninstall. Idempotent on the id, but a fresh
// uninstall of an app whose record was forgotten makes it visible again: the
// operator uninstalled that app just now, so the shelf offers it back.
func markAppRemoved(dataDir, id string) error {
	id = strings.TrimSpace(id)
	if dataDir == "" || id == "" {
		return nil
	}
	doc := readRemovedAppsDoc(dataDir)
	for i, a := range doc.Apps {
		if a.ID == id {
			if !a.Forgotten {
				return nil
			}
			doc.Apps[i].Forgotten = false
			return writeRemovedAppsDoc(dataDir, doc.Apps)
		}
	}
	doc.Apps = append(doc.Apps, removedApp{ID: id, At: time.Now().UTC().Format(time.RFC3339)})
	return writeRemovedAppsDoc(dataDir, doc.Apps)
}

// forgetRemovedApps stops offering removals back. For an app still carried by
// this build's payload the entry must stay (it is what keeps the app off the
// deck), so it is flagged Forgotten and the shelf hides it; the Forgotten
// apps list still shows it. An entry whose app this build does not carry has
// nothing left to block — a stale record from an older build — so it is
// dropped outright. Every id is named explicitly by the operator; unknown ids
// are reported, not written.
func forgetRemovedApps(dataDir string, ids []string, carried func(string) bool) (forgotten, dropped, unknown []string, err error) {
	if dataDir == "" || len(ids) == 0 {
		return nil, nil, ids, nil
	}
	doc := readRemovedAppsDoc(dataDir)
	if len(doc.Apps) == 0 {
		return nil, nil, ids, nil
	}
	changed := false
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		found := false
		for i, a := range doc.Apps {
			if a.ID != id {
				continue
			}
			found = true
			if carried != nil && carried(id) {
				if !doc.Apps[i].Forgotten {
					doc.Apps[i].Forgotten = true
					changed = true
				}
				forgotten = append(forgotten, id)
			} else {
				doc.Apps = append(doc.Apps[:i], doc.Apps[i+1:]...)
				changed = true
				dropped = append(dropped, id)
			}
			break
		}
		if !found {
			unknown = append(unknown, id)
		}
	}
	if changed {
		err = writeRemovedAppsDoc(dataDir, doc.Apps)
	}
	return forgotten, dropped, unknown, err
}

// restoreRemovedApps clears records and returns the ids it cleared. An empty
// ids slice restores everything (the deck's "restore all").
func restoreRemovedApps(dataDir string, ids []string) ([]string, error) {
	if dataDir == "" {
		return nil, nil
	}
	doc := readRemovedAppsDoc(dataDir)
	if len(doc.Apps) == 0 {
		return nil, nil
	}
	want := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}
	restoreAll := len(want) == 0
	var kept, restored []string
	for _, a := range doc.Apps {
		if restoreAll || want[a.ID] {
			restored = append(restored, a.ID)
			continue
		}
		kept = append(kept, a.ID)
	}
	if len(restored) == 0 {
		return nil, nil
	}
	keptApps := make([]removedApp, 0, len(kept))
	for _, a := range doc.Apps {
		for _, id := range kept {
			if a.ID == id {
				keptApps = append(keptApps, a)
				break
			}
		}
	}
	return restored, writeRemovedAppsDoc(dataDir, keptApps)
}

// serveRemovedApps answers GET /api/apps/removed: the payload apps this
// machine has uninstalled, with display names, so the deck can offer them
// back. Entries forgotten from the shelf come back in a separate list (still
// restorable, just not offered by default). Loopback only — it is an
// inventory of this computer, and the app list itself is what the Wi-Fi may
// see or not.
func (s *Server) serveRemovedApps(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "removed apps are only listed for this computer"})
		return
	}
	doc := readRemovedAppsDoc(s.cfg.DataDir)
	registry := embeddedRegistryApps()
	name := func(id string) string {
		if reg := registry[id]; reg != nil && reg.Name != "" {
			return reg.Name
		}
		return id
	}
	out := make([]map[string]string, 0, len(doc.Apps))
	forgottenList := make([]map[string]string, 0)
	for _, a := range doc.Apps {
		entry := map[string]string{"id": a.ID, "name": name(a.ID)}
		if a.Forgotten {
			forgottenList = append(forgottenList, entry)
			continue
		}
		out = append(out, entry)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"apps": out, "count": len(out), "restorable": embeddedBuild,
		"forgotten": forgottenList, "forgottenCount": len(forgottenList),
	})
}

// serveForgetApps answers POST /api/apps/forget {ids:[…]}: stop offering the
// named removals back. Unlike restore there is no all — forgetting in bulk is
// how the old silent-deck regression would come back through a wide-open
// front door; one id per explicit operator action, like the record itself.
func (s *Server) serveForgetApps(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "forgetting removed apps is only allowed from this computer"})
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send {ids:[…]}"})
		return
	}
	if len(body.IDs) == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name the apps to forget — there is no forget-all"})
		return
	}
	registry := embeddedRegistryApps()
	forgotten, dropped, unknown, err := forgetRemovedApps(s.cfg.DataDir, body.IDs, func(id string) bool {
		return embeddedBuild && registry[id] != nil
	})
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var note string
	if len(forgotten) > 0 {
		note = fmt.Sprintf("%d app(s) forgotten — still uninstalled, no longer offered here (listed under Forgotten apps)", len(forgotten))
	}
	if len(dropped) > 0 {
		if note != "" {
			note += "; "
		}
		note += fmt.Sprintf("%d stale record(s) dropped — not carried by this launcher", len(dropped))
	}
	if note == "" {
		note = "nothing to forget"
	}
	if len(unknown) > 0 {
		note += " — not on the removed list: " + strings.Join(unknown, ", ")
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "note": note, "forgotten": forgotten, "dropped": dropped, "unknown": unknown,
	})
}

// serveRestoreApps answers POST /api/apps/restore {ids:[…]} or {all:true}:
// the record is cleared and the payload copy is unpacked again, so the card
// comes back without restarting the launcher.
func (s *Server) serveRestoreApps(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "restoring apps is only allowed from this computer"})
		return
	}
	var body struct {
		IDs []string `json:"ids"`
		All bool     `json:"all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send {ids:[…]} or {all:true}"})
		return
	}
	if !body.All && len(body.IDs) == 0 {
		// An empty request must never restore everything by accident.
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name the apps to restore, or send {all:true}"})
		return
	}
	ids := body.IDs
	if body.All {
		ids = nil
	}
	restored, err := restoreRemovedApps(s.cfg.DataDir, ids)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(restored) == 0 {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": "nothing to restore", "restored": []string{}})
		return
	}
	// Unpack the payload copies now: the record alone would only take effect
	// on the next boot.
	var failed, missing []string
	if embeddedBuild {
		registry := embeddedRegistryApps()
		for _, id := range restored {
			a := registry[id]
			if a == nil {
				missing = append(missing, id) // not carried by this build (any more)
				continue
			}
			if err := extractOneApp(s.cfg.DataDir, embeddedFS, a); err != nil {
				failed = append(failed, id)
			}
		}
	}
	if err := s.reloadApps(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	note := fmt.Sprintf("%d app(s) restored", len(restored))
	if len(failed) > 0 {
		note += " — not unpacked in this build: " + strings.Join(failed, ", ")
	}
	if len(missing) > 0 {
		note += " — no longer carried by this launcher: " + strings.Join(missing, ", ")
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note, "restored": restored})
}
