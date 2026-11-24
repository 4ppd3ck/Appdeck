// App icons: reading icon files and deriving accent colors.
package main

import (
	"bytes"
	"fmt"
	"image/png"
	"math"
)

// appIconNames are the two conventional icon file names (used by the
// theme-pack overwrite path and the git import icon picker; the general
// lookup in iconfile.go covers these plus <id>.* / <name>.* and ico/icns).
var appIconNames = []struct {
	name  string
	ctype string
}{
	{"icon.png", "image/png"},
	{"icon.svg", "image/svg+xml"},
}

// appIcon loads the app's folder icon: from the packaged embedded assets in
// single-file builds, from the app folder on disk otherwise. Imported apps
// exist only on disk in single-file builds — the embedded payload has no
// icon for them — so the app folder is the fallback there too.
func appIcon(a *App) (data []byte, ctype string, ok bool) {
	// The app folder wins over the packaged payload: theme packs overwrite
	// installed apps' icon files (icons/<app-id>.png), and the dashboard
	// must show the overwrite, not the packaged original. The payload is
	// the fallback for apps whose folder carries no icon file.
	if a.Dir != "" {
		if data, ctype, ok := lookupIconDir(a.Dir, a, nil); ok {
			return data, ctype, true
		}
	}
	if embeddedBuild {
		if data, ctype, ok := embeddedIcon(a.ID); ok {
			return data, ctype, true
		}
	}
	if a.Dir == "" {
		return nil, "", false
	}
	return lookupIconDir(a.Dir, a, nil)
}

// appHasIconFile is the cheap existence check for the /api/apps badge.
func appHasIconFile(a *App) bool {
	if embeddedBuild && embeddedIconExists(a.ID) {
		return true
	}
	if a.Dir == "" {
		return false
	}
	_, _, ok := lookupIconDir(a.Dir, a, nil)
	return ok
}

// -------------------------------------------------------- per-app accents ----

// accentFor returns the card accent color for an app: the explicit apps.json
// "accent" if set, else derived once from the icon.png's dominant hue, else
// "" (the dashboard's default accent). Derived values are cached per app —
// decoding on every /api/apps poll would be wasteful; restart to re-derive
// after changing an icon.
func (s *Server) accentFor(a *App) string {
	if a.Accent != "" {
		return a.Accent
	}
	if v, ok := s.accentCache.Load(a.ID); ok {
		return v.(string)
	}
	var hex string
	if data, ctype, ok := appIcon(a); ok && ctype == "image/png" {
		hex = dominantAccent(data)
	}
	s.accentCache.Store(a.ID, hex)
	return hex
}

// dominantAccent picks a vivid color representing the icon. It samples the
// center box (where the subject sits, not the tile background) and weights
// pixels by saturation² × value so a colorful emoji outranks a large but
// medium-saturated background. Transparent, gray and near-black pixels are
// skipped. Returns "" when the icon is effectively colorless.
func dominantAccent(data []byte) string {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	// Background reference: four inset corner samples. Skipping pixels near
	// these removes the tile background whatever it looks like — the shared
	// deck gradient or any hand-made icon's own backdrop.
	refs := [4][3]float64{}
	for i, p := range [][2]int{
		{b.Min.X + w/6, b.Min.Y + h/6}, {b.Max.X - w/6, b.Min.Y + h/6},
		{b.Min.X + w/6, b.Max.Y - h/6}, {b.Max.X - w/6, b.Max.Y - h/6},
	} {
		r, g, bl, _ := img.At(p[0], p[1]).RGBA()
		refs[i] = [3]float64{float64(r) / 0xffff, float64(g) / 0xffff, float64(bl) / 0xffff}
	}
	nearRef := func(fr, fg, fb float64) bool {
		for _, r := range refs {
			dr, dg, db := fr-r[0], fg-r[1], fb-r[2]
			if dr*dr+dg*dg+db*db < 0.0225 { // ≈ 0.15 RGB distance
				return true
			}
		}
		return false
	}
	x0 := b.Min.X + w/4 // center box: middle half of the image
	x1 := b.Max.X - w/4
	y0 := b.Min.Y + h/4
	y1 := b.Max.Y - h/4
	var buckets [36]float64
	any := false
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			if a < 0x8000 {
				continue
			}
			fr, fg, fb := float64(r)/0xffff, float64(g)/0xffff, float64(bl)/0xffff
			if nearRef(fr, fg, fb) {
				continue
			}
			mx := max(fr, max(fg, fb))
			mn := min(fr, min(fg, fb))
			v := mx
			if v <= 0 {
				continue
			}
			s := (mx - mn) / v
			if s < 0.28 || v < 0.25 { // gray / near-black: not an identity color
				continue
			}
			h := 0.0
			switch mx {
			case fr:
				h = (fg - fb) / (mx - mn)
			case fg:
				h = 2 + (fb-fr)/(mx-mn)
			default:
				h = 4 + (fr-fg)/(mx-mn)
			}
			h = math.Mod(h*60+360, 360)
			buckets[int(h/10)%36] += s * s * v
			any = true
		}
	}
	if !any {
		return ""
	}
	best := 0
	for i, w := range buckets {
		if w > buckets[best] {
			best = i
		}
	}
	hue := float64(best)*10 + 5
	return hsvToHex(hue, 0.62, 0.95)
}

// hsvToHex renders HSV (hue in degrees) as a #rrggbb string.
func hsvToHex(h, s, v float64) string {
	i := int(math.Floor(h/60)) % 6
	f := h/60 - math.Floor(h/60)
	p := v * (1 - s)
	q := v * (1 - f*s)
	t := v * (1 - (1-f)*s)
	var r, g, b float64
	switch i {
	case 0:
		r, g, b = v, t, p
	case 1:
		r, g, b = q, v, p
	case 2:
		r, g, b = p, v, t
	case 3:
		r, g, b = p, q, v
	case 4:
		r, g, b = t, p, v
	default:
		r, g, b = v, p, q
	}
	return fmt.Sprintf("#%02x%02x%02x", int(r*255+0.5), int(g*255+0.5), int(b*255+0.5))
}
