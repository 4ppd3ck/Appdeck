// featured.go — the featured-card store: a curated, one-click gallery for
// light users. Two kinds of entries:
//
//   - templates: always available, scaffolded locally (the `appdeck new`
//     machinery) — a working card with zero network.
//   - catalog cards: fetched from the deck's GitHub (featured.json at the
//     repo root), cached for a day, with a git/release source the dashboard
//     installs through the ordinary /api/import?url= flow. A missing or
//     stale file never blocks the gallery — it shows templates and the
//     cached cards, whatever exists.
//
// Every entry carries its `requires` list; /api/featured merges the live
// tool status so the gallery can green-light or warn per card before an
// install is even attempted.
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

// featuredCatalogURL is the curated list's home (the deck's own repo).
const featuredCatalogURL = "https://raw.githubusercontent.com/4ppd3ck/Appdeck/main/featured.json"

// featuredRefreshTTL bounds how often the gallery hits the network.
const featuredRefreshTTL = 24 * time.Hour

// featuredFetchTimeout keeps the gallery snappy: an unreachable GitHub must
// not stall the dialog.
const featuredFetchTimeout = 6 * time.Second

// featuredCard is one gallery entry (a template or a catalog card).
type featuredCard struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Icon     string `json:"icon,omitempty"`
	Desc     string `json:"desc"`
	Category string `json:"category,omitempty"`
	// Source is the import URL for catalog cards (empty for templates).
	Source string `json:"source,omitempty"`
	// Template names the scaffold kind for template cards ("go", "html").
	Template string   `json:"template,omitempty"`
	Requires []string `json:"requires,omitempty"`
	Shot     string   `json:"shot,omitempty"` // screenshot URL (lazy-loaded)
	Version  string   `json:"version,omitempty"`
}

// builtinTemplates are the zero-network entries every deck can install.
func builtinTemplates() []featuredCard {
	return []featuredCard{
		{
			ID: "tpl-go", Name: "Go web card", Icon: "🧩", Template: "go",
			Category: "Templates",
			Desc:     "A working Go app with the full security contract implemented (Host check, token'd API routes, loopback bind). Edit main.go — the deck builds it on first start.",
		},
		{
			ID: "tpl-html", Name: "Static page card", Icon: "📄", Template: "html",
			Category: "Templates",
			Desc:     "A single HTML page hosted inside the launcher itself — no process, no port, no build. Perfect for dashboards and cheat sheets.",
		},
	}
}

// featuredCachePath is <data>/featured-cache.json.
func (s *Server) featuredCachePath() string {
	return filepath.Join(s.cfg.DataDir, "featured-cache.json")
}

// fetchFeaturedCards reads the curated list from GitHub (or the cache when
// the network disagrees). Missing upstream file, an offline machine or a
// malformed list all degrade to whatever was cached before.
func fetchFeaturedCards(cachePath string) ([]featuredCard, error) {
	type cacheDoc struct {
		FetchedAt float64        `json:"fetchedAt"`
		Cards     []featuredCard `json:"cards"`
	}
	readCache := func() []featuredCard {
		data, err := os.ReadFile(cachePath)
		if err != nil || !json.Valid(data) {
			return nil
		}
		var doc cacheDoc
		if json.Unmarshal(data, &doc) != nil {
			return nil
		}
		return doc.Cards
	}
	fresh := func() bool {
		data, err := os.ReadFile(cachePath)
		if err != nil {
			return false
		}
		var doc cacheDoc
		if json.Unmarshal(data, &doc) != nil {
			return false
		}
		return time.Since(time.Unix(int64(doc.FetchedAt), 0)) < featuredRefreshTTL
	}

	cards := readCache()
	if fresh() {
		return cards, nil
	}
	client := &http.Client{Timeout: featuredFetchTimeout}
	resp, err := client.Get(featuredCatalogURL)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var doc struct {
				Cards []featuredCard `json:"cards"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&doc); err == nil && len(doc.Cards) > 0 {
				if cleaned := sanitizeFeaturedCards(doc.Cards); len(cleaned) > 0 {
					if data, err := json.Marshal(cacheDoc{FetchedAt: float64(time.Now().Unix()), Cards: cleaned}); err == nil {
						_ = os.WriteFile(cachePath, data, 0o644)
					}
					return cleaned, nil
				}
			}
		}
	}
	// Stale-but-present beats empty.
	return cards, fmt.Errorf("featured catalog unavailable (using cached)")
}

// sanitizeFeaturedCards validates untrusted catalog input: ids and names
// must exist, sources must be http(s) git/archive URLs or repo tree paths —
// the import pipeline's own validation still runs at install time; this
// filter just keeps obvious junk out of the gallery.
func sanitizeFeaturedCards(in []featuredCard) []featuredCard {
	out := make([]featuredCard, 0, len(in))
	seen := map[string]bool{}
	for _, c := range in {
		c.ID = strings.TrimSpace(c.ID)
		c.Name = strings.TrimSpace(c.Name)
		c.Source = strings.TrimSpace(c.Source)
		if c.ID == "" || c.Name == "" || seen[c.ID] {
			continue
		}
		if c.Source != "" && !strings.HasPrefix(c.Source, "https://") {
			continue
		}
		var kept []string
		for _, r := range c.Requires {
			if validRequiresTool(r) {
				kept = append(kept, r)
			}
		}
		c.Requires = kept
		seen[c.ID] = true
		out = append(out, c)
	}
	return out
}

// validRequires mirrors the manifest's requires vocabulary (prereqs.go).
func validRequiresTool(id string) bool {
	for _, t := range toolPresets {
		if t.ID == id {
			return true
		}
	}
	return false
}

// serveFeatured answers GET /api/featured: templates + curated cards, each
// annotated with the live availability of its required tools, plus which
// ids are already installed (the gallery marks those "Installed").
func (s *Server) serveFeatured(w http.ResponseWriter) {
	cards, err := fetchFeaturedCards(s.featuredCachePath())
	templates := builtinTemplates()
	// Dependency status per unique tool id.
	need := map[string]bool{}
	for _, c := range append(append([]featuredCard{}, templates...), cards...) {
		for _, r := range c.Requires {
			need[r] = true
		}
	}
	type toolAvail struct {
		Installed bool   `json:"installed"`
		Version   string `json:"version,omitempty"`
	}
	tools := map[string]toolAvail{}
	for id := range need {
		for _, t := range toolPresets {
			if t.ID != id || t.probe == nil {
				continue
			}
			st := toolAvail{}
			if _, ver, ok := t.probe(); ok {
				st.Installed = true
				st.Version = ver
			}
			tools[id] = st
			break
		}
	}
	// Which catalog ids already sit on the deck.
	s.mu.Lock()
	installed := make(map[string]bool, len(s.apps))
	for _, a := range s.apps {
		installed[a.ID] = true
	}
	s.mu.Unlock()

	s.writeJSON(w, http.StatusOK, map[string]any{
		"templates": templates,
		"cards":     cards,
		"tools":     tools,
		"installed": installed,
		"catalogNote": func() string {
			if err != nil && len(cards) == 0 {
				return "The curated list is unreachable right now — template cards below still work offline."
			}
			return ""
		}(),
	})
}

// serveFeaturedTemplate answers POST /api/featured/template {name, template,
// desc}: scaffold a template card into the deck's Apps container and reload
// — the same machinery as `appdeck new`, driven from the gallery.
func (s *Server) serveFeaturedTemplate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Template string `json:"template"`
		Desc     string `json:"desc"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON body"})
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Template != "go" && body.Template != "html" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": `template must be "go" or "html"`})
		return
	}
	if body.Name == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	container, err := s.appsContainerDir()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	target := filepath.Join(container, slugify(body.Name))
	app, err := scaffoldApp(body.Name, body.Template, body.Desc, target)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.reloadApps(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	go s.snapshotDeck()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": app.ID, "name": app.Name})
}

// featuredSorted returns the gallery's catalog cards in a stable order —
// the curated file decides priority; ties break alphabetically by name.
func featuredSorted(cards []featuredCard) []featuredCard {
	out := append([]featuredCard(nil), cards...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Name < out[j].Name
	})
	return out
}
