const POLL_MS = 4000;
let apps = [];
// Boot-time frame (injected by serveIndex): the native darwin window gets
// the mac-frame layout from the first paint — /api/health answers too late.
try {
  if (window.__bootFrame && window.__bootFrame.os === "darwin" && window.__bootFrame.window === "native") {
    document.body.classList.add("mac-frame");
  }
} catch {}
// Filter state lives here from the very top: the seg-sync code below
// renders on init, and the sidebar reads all three variables — they must
// never sit in the temporal dead zone when a render fires.
let stateFilter = "all";
let sectionFilter = "all";
let categoryFilter = "";
let busy = new Set();      // ids with an action in flight
let logApp = null;         // id whose logs are shown in the modal
let hostWindow = "browser"; // how this dashboard is hosted: native|app|browser|none
let hostOS = "";            // the launcher's OS (darwin/windows/linux) — gates OS-specific buttons
let hostArch = "";          // the launcher's arch (amd64/arm64/386) — pre-flight platform checks
let hostMode = "";          // "single-file" when every app lives inside the executable

// Where apps open on "Open": as a tab inside this dashboard window (default),
// in a separate window, or as a tab in the default browser (from the native
// launcher window a browser-tab request goes through /api/open, because the
// WebView2 window itself has no tabs). The button and openApp() are wired
// further down, after the helpers are defined.
let openMode = "intab";
try {
  openMode = localStorage.getItem("launcher.openMode") || "intab";
  if (openMode === "tab") openMode = "browser"; // pre-in-window builds: "tab" meant the default browser
  if (!["intab", "window", "browser"].includes(openMode)) openMode = "intab";
} catch {}

// Card order inside each section: the registry's own order, alphabetical, or
// grouped by category (sub-headers, alphabetical inside). Cycled by the
// sort button next to Open-mode and remembered per browser.
const SORT_MODES = ["original", "az", "cat", "recent"];
let sortMode = "cat";
try {
  const savedSort = localStorage.getItem("launcher.sortMode");
  if (savedSort && SORT_MODES.includes(savedSort)) sortMode = savedSort;
} catch {}

// Pinned apps (★ on each card) float to the top of their section.
let pinnedApps = new Set();
try { pinnedApps = new Set(JSON.parse(localStorage.getItem("launcher.pinned") || "[]")); } catch {}
function savePins() {
  try { localStorage.setItem("launcher.pinned", JSON.stringify([...pinnedApps])); } catch {}
}

const $ = (s) => document.querySelector(s);
const groupsEl = $("#groups");
// Collapsible sections, remembered per browser ("launcher.collapsed").
const collapsedSections = (() => {
  try {
    const saved = JSON.parse(localStorage.getItem("launcher.collapsed") || "[]");
    return new Set(Array.isArray(saved) ? saved : []);
  } catch {
    return new Set();
  }
})();
const saveCollapsed = () => { try { localStorage.setItem("launcher.collapsed", JSON.stringify([...collapsedSections])); } catch {} };
document.querySelectorAll(".overlay .modal").forEach((modal, index) => {
  const heading = modal.querySelector(".modal-head .mh, .modal-head > span");
  modal.setAttribute("role", "dialog");
  modal.setAttribute("aria-modal", "true");
  if (heading) {
    if (!heading.id) heading.id = `dialog-title-${index + 1}`;
    modal.setAttribute("aria-labelledby", heading.id);
  } else {
    modal.setAttribute("aria-label", "AppDeck dialog");
  }
});
groupsEl.addEventListener("click", (e) => {
  const label = e.target.closest(".group-label[data-collapse]");
  if (!label) return;
  const key = label.dataset.collapse;
  if (collapsedSections.has(key)) collapsedSections.delete(key); else collapsedSections.add(key);
  saveCollapsed();
  label.closest(".group").classList.toggle("collapsed");
});
let appsLoaded = false;     // skeleton until the first /api/apps answers
let firstPaintRender = true; // stagger entrance only once

// Inline SVG glyphs for the UI chrome — one consistent set across macOS,
// Windows and Linux (emoji controls render differently per OS and size).
// App icons stay as authored (emoji/artwork): those are content, not chrome.
// Icon geometry: the Prism icon set (MIT — prism-download-manager), 24×24
// grid, 1.75 stroke, round caps/joins, currentColor. A few deck-specific
// glyphs (wifi, home, terminal, logs, themeAuto, starFilled, shieldCheck)
// keep hand-drawn paths in the same visual style.
const _gear = '<path d="M9.36 5.63A0.37 0.37 0 0 0 9.57 5.38L10.09 3.48A0.75 0.75 0 0 1 10.81 2.92L13.19 2.92A0.75 0.75 0 0 1 13.91 3.48L14.43 5.38A0.37 0.37 0 0 0 14.64 5.63L14.64 5.63A0.37 0.37 0 0 0 14.96 5.61L16.67 4.62A0.75 0.75 0 0 1 17.58 4.74L19.26 6.42A0.75 0.75 0 0 1 19.38 7.33L18.39 9.04A0.37 0.37 0 0 0 18.37 9.36L18.37 9.36A0.37 0.37 0 0 0 18.62 9.57L20.52 10.09A0.75 0.75 0 0 1 21.08 10.81L21.08 13.19A0.75 0.75 0 0 1 20.52 13.91L18.62 14.43A0.37 0.37 0 0 0 18.37 14.64L18.37 14.64A0.37 0.37 0 0 0 18.39 14.96L19.38 16.67A0.75 0.75 0 0 1 19.26 17.58L17.58 19.26A0.75 0.75 0 0 1 16.67 19.38L14.96 18.39A0.37 0.37 0 0 0 14.64 18.37L14.64 18.37A0.37 0.37 0 0 0 14.43 18.62L13.91 20.52A0.75 0.75 0 0 1 13.19 21.08L10.81 21.08A0.75 0.75 0 0 1 10.09 20.52L9.57 18.62A0.37 0.37 0 0 0 9.36 18.37L9.36 18.37A0.37 0.37 0 0 0 9.04 18.39L7.33 19.38A0.75 0.75 0 0 1 6.42 19.26L4.74 17.58A0.75 0.75 0 0 1 4.62 16.67L5.61 14.96A0.37 0.37 0 0 0 5.63 14.64L5.63 14.64A0.37 0.37 0 0 0 5.38 14.43L3.48 13.91A0.75 0.75 0 0 1 2.92 13.19L2.92 10.81A0.75 0.75 0 0 1 3.48 10.09L5.38 9.57A0.37 0.37 0 0 0 5.63 9.36L5.63 9.36A0.37 0.37 0 0 0 5.61 9.04L4.62 7.33A0.75 0.75 0 0 1 4.74 6.42L6.42 4.74A0.75 0.75 0 0 1 7.33 4.62L9.04 5.61A0.37 0.37 0 0 0 9.36 5.63Z"/><circle cx="12" cy="12" r="2.75"/>';
const _other = '<circle cx="8" cy="8" r="3.75"/><rect x="13" y="13" width="7" height="7" rx="1.75"/><path d="M15.81 4.66A0.8 0.8 0 0 1 17.19 4.66L19.78 9.04A0.8 0.8 0 0 1 19.1 10.25L13.9 10.25A0.8 0.8 0 0 1 13.22 9.04Z"/><path d="M4.5 16.5h7M8 13v7"/>';
const _options = '<path d="M4 7h9M17 7h3M4 17h3M11 17h9"/><circle cx="15" cy="7" r="2"/><circle cx="9" cy="17" r="2"/>';
const _more = '<circle cx="5" cy="12" r="1.5"/><circle cx="12" cy="12" r="1.5"/><circle cx="19" cy="12" r="1.5"/>';
const _star = '<path d="M11.28 4.76A0.8 0.8 0 0 1 12.72 4.76L14.41 8.3A0.8 0.8 0 0 0 15.04 8.75L18.81 9.2A0.8 0.8 0 0 1 19.27 10.57L16.51 13.2A0.8 0.8 0 0 0 16.28 13.93L16.99 17.74A0.8 0.8 0 0 1 15.82 18.59L12.38 16.71A0.8 0.8 0 0 0 11.62 16.71L8.18 18.59A0.8 0.8 0 0 1 7.01 17.74L7.72 13.93A0.8 0.8 0 0 0 7.49 13.2L4.73 10.57A0.8 0.8 0 0 1 5.19 9.2L8.96 8.75A0.8 0.8 0 0 0 9.59 8.3Z"/>';
const ICONS = {
  // actions
  play: '<circle cx="12" cy="12" r="9"/><path d="M10.4 10.11A0.9 0.9 0 0 1 11.77 9.34L14.85 11.23A0.9 0.9 0 0 1 14.85 12.77L11.77 14.66A0.9 0.9 0 0 1 10.4 13.89Z"/>',
  rocket: '<path d="M4.6 16.6c-1.4 1.2-2.1 4.8-2.1 4.8s3.6-.7 4.8-2.1a2 2 0 0 0-2.7-2.7Z"/><path d="m12 15-3-3c1.6-3.8 4.7-7.2 12-9-.6 7.3-4 10.4-7.8 12Z"/><path d="M9 12H4.5s.6-3.3 4.1-4.5M12 15v4.5s3.3-.6 4.5-4.1"/><circle cx="16.5" cy="7.5" r="1"/>',
  pause: '<rect x="6.25" y="5" width="4" height="14" rx="1.25"/><rect x="13.75" y="5" width="4" height="14" rx="1.25"/>',
  resume: '<path d="M8 7.56A1.6 1.6 0 0 1 10.41 6.18L17.92 10.62A1.6 1.6 0 0 1 17.92 13.38L10.41 17.82A1.6 1.6 0 0 1 8 16.44Z"/>',
  stop: '<rect x="5.5" y="5.5" width="13" height="13" rx="2.5"/>',
  restart: '<path d="M20 12a8 8 0 1 1-2.34-5.66"/><path d="M20.25 3.75V8H16"/>',
  retry: '<path d="M20 12a8 8 0 1 1-2.34-5.66"/><path d="M20.25 3.75V8H16"/>',
  power: '<path d="M12 3.5v8"/><path d="M7.05 6.4A7.85 7.85 0 1 0 16.95 6.4"/>',
  check: '<path d="M5 12.5l4.5 4.5L19 7.5"/>',
  close: '<path d="M6.5 6.5l11 11M17.5 6.5l-11 11"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  copy: '<rect x="8.5" y="8.5" width="12" height="12" rx="2.5"/><path d="M15.5 8.5V6A2.5 2.5 0 0 0 13 3.5H6A2.5 2.5 0 0 0 3.5 6v7A2.5 2.5 0 0 0 6 15.5h2.5"/>',
  external: '<path d="M14 4.5h5.5V10M19.5 4.5L11 13"/><path d="M18 14v3.5a2.5 2.5 0 0 1-2.5 2.5h-9A2.5 2.5 0 0 1 4 17.5v-9A2.5 2.5 0 0 1 6.5 6H10"/>',
  arrowDown: '<path d="M12 4.5v15M6 13.5l6 6 6-6"/>',
  chevronDown: '<path d="M6.5 9.25l5.5 5.5 5.5-5.5"/>',
  minus: '<path d="M5 12h14"/>',
  sortAsc: '<path d="M7 19.5v-15M3.5 8L7 4.5 10.5 8"/><path d="M13.5 6.5h3M13.5 11.5h5M13.5 16.5h7"/>',
  sortDesc: '<path d="M7 4.5v15M3.5 16L7 19.5l3.5-3.5"/><path d="M13.5 6.5h7M13.5 11.5h5M13.5 16.5h3"/>',
  // chrome
  gear: _gear,
  settings: _gear,
  tune: _options,
  options: _options,
  more: _more,
  shapes: _other,
  other: _other,
  search: '<circle cx="10.75" cy="10.75" r="6.5"/><path d="M15.5 15.5l4.75 4.75"/>',
  info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v5.5"/><circle cx="12" cy="7.9" r="1.15" fill="currentColor" stroke="none"/>',
  warning: '<path d="M10.26 6.6A2 2 0 0 1 13.74 6.6L19.32 16.52A2 2 0 0 1 17.58 19.5L6.42 19.5A2 2 0 0 1 4.68 16.52Z"/><path d="M12 9.5v4.25"/><circle cx="12" cy="16.6" r="1.1" fill="currentColor" stroke="none"/>',
  moon: '<path d="M19.73 13.03A8.25 8.25 0 1 1 10.97 4.27A6.75 6.75 0 0 0 19.73 13.03Z"/>',
  sun: '<circle cx="12" cy="12" r="3.75"/><path d="M12 5.4L12 3.25M16.67 7.33L18.19 5.81M18.6 12L20.75 12M16.67 16.67L18.19 18.19M12 18.6L12 20.75M7.33 16.67L5.81 18.19M5.4 12L3.25 12M7.33 7.33L5.81 5.81"/>',
  themeAuto: '<circle cx="12" cy="12" r="9"/><path d="M12 3a9 9 0 0 0 0 18z" fill="currentColor" stroke="none"/>',
  palette: _other,
  image: '<rect x="3.5" y="4.5" width="17" height="15" rx="2.5"/><circle cx="9" cy="9.75" r="1.75"/><path d="M3.75 16.75l4.4-4.4a1.2 1.2 0 0 1 1.7 0l6.65 6.65M14 15l1.65-1.65a1.2 1.2 0 0 1 1.7 0L20.25 16.25"/>',
  images: '<rect x="3.5" y="4.5" width="17" height="15" rx="2.5"/><circle cx="9" cy="9.75" r="1.75"/><path d="M3.75 16.75l4.4-4.4a1.2 1.2 0 0 1 1.7 0l6.65 6.65M14 15l1.65-1.65a1.2 1.2 0 0 1 1.7 0L20.25 16.25"/>',
  puzzle: '<path d="M6 6h3.75a2.25 2.25 0 0 1 4.5 0h2.25A1.5 1.5 0 0 1 18 7.5v2.25a2.25 2.25 0 0 1 0 4.5V18a1.5 1.5 0 0 1-1.5 1.5H6A1.5 1.5 0 0 1 4.5 18V7.5A1.5 1.5 0 0 1 6 6z"/>',
  programs: '<path d="M11.31 3.39A1.4 1.4 0 0 1 12.69 3.39L19.29 7.1A1.4 1.4 0 0 1 20 8.32L20 15.68A1.4 1.4 0 0 1 19.29 16.9L12.69 20.61A1.4 1.4 0 0 1 11.31 20.61L4.71 16.9A1.4 1.4 0 0 1 4 15.68L4 8.32A1.4 1.4 0 0 1 4.71 7.1Z"/><path d="M4.4 7.75L12 12l7.6-4.25M12 12v8.6"/>',
  import: '<path d="M14 4.5h3.5A2.5 2.5 0 0 1 20 7v10a2.5 2.5 0 0 1-2.5 2.5H14"/><path d="M4 12h11M11 8l4 4-4 4"/>',
  speedLimiter: '<path d="M4.64 16.75A8.5 8.5 0 1 1 19.36 16.75"/><path d="M12 12.5l3.75-4.25"/><circle cx="12" cy="12.5" r="1.25"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3.5 2"/>',
  history: '<path d="M3.5 12a8.5 8.5 0 1 0 2.5-6"/><path d="M6 2.75V6H2.75"/><path d="M12 8v4.5l3 1.75"/>',
  // places / views
  wifi: '<path d="M5 12.5a10 10 0 0 1 14 0"/><path d="M8.5 15.5a5.5 5.5 0 0 1 7 0"/><circle cx="12" cy="18.5" r="1" fill="currentColor" stroke="none"/>',
  home: '<path d="M3 11l9-8 9 8"/><path d="M5 10v10h14V10"/>',
  globe: '<circle cx="12" cy="12" r="9"/><ellipse cx="12" cy="12" rx="3.75" ry="9"/><path d="M3 12h18"/>',
  monitor: '<rect x="3" y="4" width="18" height="12.5" rx="2.5"/><path d="M8.5 20h7M12 16.5V20"/>',
  grid: '<rect x="4" y="4" width="6.75" height="6.75" rx="2"/><rect x="13.25" y="4" width="6.75" height="6.75" rx="2"/><rect x="4" y="13.25" width="6.75" height="6.75" rx="2"/><rect x="13.25" y="13.25" width="6.75" height="6.75" rx="2"/>',
  all: '<rect x="4" y="4" width="6.75" height="6.75" rx="2"/><rect x="13.25" y="4" width="6.75" height="6.75" rx="2"/><rect x="4" y="13.25" width="6.75" height="6.75" rx="2"/><rect x="13.25" y="13.25" width="6.75" height="6.75" rx="2"/>',
  list: '<circle cx="5" cy="6.5" r="1.15" fill="currentColor" stroke="none"/><circle cx="5" cy="12" r="1.15" fill="currentColor" stroke="none"/><circle cx="5" cy="17.5" r="1.15" fill="currentColor" stroke="none"/><path d="M9.5 6.5h10M9.5 12h10M9.5 17.5h10"/>',
  columns: '<rect x="3.5" y="4.5" width="17" height="15" rx="2.5"/><path d="M9.25 4.5v15M14.75 4.5v15"/>',
  maximize: '<rect x="5.5" y="5.5" width="13" height="13" rx="2"/>',
  sidebar: '<rect x="3" y="4.5" width="18" height="15" rx="2.5"/><path d="M9.5 4.5v15"/>',
  logs: '<path d="M4 6h16M4 12h10M4 18h13"/>',
  terminal: '<polyline points="4 17 10 11 4 5"/><line x1="12" y1="19" x2="20" y2="19"/>',
  folder: '<path d="M3.5 7A2.5 2.5 0 0 1 6 4.5h3.17a1.5 1.5 0 0 1 1.06.44L12 6.75h6a2.5 2.5 0 0 1 2.5 2.5v8.25A2.5 2.5 0 0 1 18 20H6a2.5 2.5 0 0 1-2.5-2.5z"/>',
  trash: '<path d="M4 6.5h16"/><path d="M9.5 6.5V5A1.5 1.5 0 0 1 11 3.5h2A1.5 1.5 0 0 1 14.5 5v1.5"/><path d="M6 6.5l.9 12.1a2 2 0 0 0 2 1.9h6.2a2 2 0 0 0 2-1.9L18 6.5"/>',
  disk: '<rect x="3" y="13" width="18" height="7" rx="2.5"/><path d="M3.6 13.4l2.3-6.9A2 2 0 0 1 7.8 5.2h8.4a2 2 0 0 1 1.9 1.3l2.3 6.9"/><circle cx="16.75" cy="16.5" r="1.05" fill="currentColor" stroke="none"/><path d="M6.75 16.5h4.5"/>',
  upload: '<path d="M12 14.5v-11M7.5 8L12 3.5 16.5 8"/><path d="M4.5 15.5V18A2.5 2.5 0 0 0 7 20.5h10a2.5 2.5 0 0 0 2.5-2.5v-2.5"/>',
  lock: '<rect x="5" y="10.5" width="14" height="10" rx="2.5"/><path d="M8 10.5V8a4 4 0 0 1 8 0v2.5"/><path d="M12 14.5v2"/>',
  lockOpen: '<rect x="5" y="10.5" width="14" height="10" rx="2.5"/><path d="M8 10.5V8a4 4 0 0 1 7.75-1.4"/>',
  tag: '<path d="M3.5 12.1V5.5a2 2 0 0 1 2-2h6.6a2 2 0 0 1 1.4.6l7.4 7.4a2 2 0 0 1 0 2.8l-6.6 6.6a2 2 0 0 1-2.8 0l-7.4-7.4a2 2 0 0 1-.6-1.4z"/><circle cx="8" cy="8" r="1.3" fill="currentColor" stroke="none"/>',
  pin: '<path d="M9 3.5h6M10 3.5v6L7 13h10l-3-3.5v-6M12 13v7.5"/>',
  // status / identity
  shield: '<path d="M12 3.25l7.25 2.9v5.6c0 4.3-3 7.9-7.25 9.25-4.25-1.35-7.25-4.95-7.25-9.25v-5.6z"/><path d="M9 12.25l2.1 2.1 4-4.1"/>',
  // Elevated: solid shield with the check knocked out in the backdrop color.
  // An outline shield + hairline tick was indistinguishable from the plain
  // shield at 15px — the filled silhouette is what carries the state.
  shieldCheck: '<path d="M12 3l7 3v6c0 4.5-3 7.5-7 9-4-1.5-7-4.5-7-9V6z" fill="currentColor" stroke="none"/><polyline points="9 12 11 14 15 10" style="stroke:var(--bg)"/>',
  star: _star,
  starFilled: _star.replace("/>", ' fill="currentColor" stroke="none"/>'),
  link: '<path d="M9 15l6-6"/><path d="M11 6.5l1.2-1.2a4 4 0 0 1 5.6 5.6L16.6 12.1"/><path d="M13 17.5l-1.2 1.2a4 4 0 0 1-5.6-5.6L7.4 11.9"/>',
  // brand
  prism: '<path d="M8.81 6.96A1.3 1.3 0 0 1 11.19 6.96L15.45 16.68A1.3 1.3 0 0 1 14.26 18.5L5.74 18.5A1.3 1.3 0 0 1 4.55 16.68Z"/><path d="M1.75 14L6 12.6"/><path d="M14.6 11.4L21.5 8.5M15.3 13.3H21.5M16 15.2l5.5 2.8"/>',
};
const svg = (n, s = 15) => `<svg class="ic" width="${s}" height="${s}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[n] || ""}</svg>`;
// Any element with [data-ic="name"] gets that glyph; static markup uses it
// instead of emoji chrome. Optional [data-ics="18"] sizes it.
document.querySelectorAll("[data-ic]").forEach(el => {
  el.innerHTML = svg(el.dataset.ic, parseInt(el.dataset.ics || "16", 10));
});
// Static chrome buttons trade their leading emoji for the shared glyphs.
for (const [id, ic, label] of [
  ["btn-install", "import", " Import app"], ["btn-prereqs", "programs", " Prerequisites"],
  ["btn-gear", "settings", ""], ["btn-themes", "other", ""], ["btn-backgrounds", "images", ""], ["btn-iconpacks", "puzzle", ""], ["btn-security", "shield", ""],
  ["btn-stop-all", "stop", " Stop All"], ["btn-start-all", "rocket", " Start All"],
  ["btn-select-mode", "check", " Select"],
  ["btn-quit", "power", " Quit"],
]) { const b = document.getElementById(id); if (b) b.innerHTML = svg(ic) + label; }
for (const b of document.querySelectorAll("#state-seg [data-state]")) {
  b.innerHTML = svg(b.dataset.state === "running" ? "play"
    : b.dataset.state === "stopped" ? "stop"
    : b.dataset.state === "pinned" ? "star" : "grid");
}
for (const b of document.querySelectorAll("#section-seg [data-section]")) {
  b.innerHTML = svg(b.dataset.section === "wifi" ? "wifi"
    : b.dataset.section === "local" ? "home"
    : b.dataset.section === "desktop" ? "monitor"
    : b.dataset.section === "cli" ? "terminal" : "grid");
}

// Color theme: one toggle, three states — 🌙 dark → ☀️ light → 🌗 auto
// (follows this Mac's appearance live). "auto" is the default on first
// visit; every choice is stored under "launcher.theme" so it sticks.
const themeBtn = $("#btn-theme");
const themeMq = window.matchMedia ? matchMedia("(prefers-color-scheme: light)") : null;
let themeMode = "auto";
try {
  const saved = localStorage.getItem("launcher.theme");
  if (saved === "dark" || saved === "light" || saved === "auto") themeMode = saved;
} catch {}
function syncThemeBtn() {
  const m = themeMode === "dark" ? [svg("moon"), "Dark theme — click for light"]
    : themeMode === "light" ? [svg("sun"), "Light theme — click to follow this Mac's appearance"]
    : [svg("themeAuto"), "Follows this Mac's appearance — click for dark"];
  themeBtn.innerHTML = m[0];
  themeBtn.title = m[1];
}
function applyTheme() {
  const resolved = themeMode === "auto" ? (themeMq && themeMq.matches ? "light" : "dark") : themeMode;
  document.documentElement.dataset.theme = resolved;
  syncThemeBtn();
  // The brightness slider in the 🖼 dialog follows the active mode.
  document.dispatchEvent(new Event("themechange"));
}
themeBtn.addEventListener("click", () => {
  themeMode = themeMode === "dark" ? "light" : themeMode === "light" ? "auto" : "dark";
  try { localStorage.setItem("launcher.theme", themeMode); } catch {}
  applyTheme();
});
if (themeMq) themeMq.addEventListener?.("change", () => { if (themeMode === "auto") applyTheme(); });
applyTheme();

function toast(msg, cls = "", appId = null) {
  const stack = $("#toasts");
  // Cap the stack: the oldest slides out rather than piling up.
  while (stack.children.length >= 3) stack.firstChild.remove();
  const t = document.createElement("div");
  t.className = "toast " + cls;
  if (cls === "ok") t.innerHTML = svg("check") + " ";
  t.appendChild(document.createTextNode(msg));
  if (cls === "err") {
    // Errors stay until dismissed — the reason often needs a second read,
    // and when an app is involved its log is one click away.
    const app = appId && apps.find(a => a.id === appId);
    if (app) {
      const view = document.createElement("button");
      view.className = "btn ghost";
      view.innerHTML = svg("logs") + " Log";
      view.title = "Open this app's log";
      view.addEventListener("click", () => { openLogs(app); t.remove(); });
      t.appendChild(view);
    }
    const x = document.createElement("button");
    x.className = "x";
    x.innerHTML = svg("close", 13);
    x.title = "Dismiss";
    x.addEventListener("click", () => t.remove());
    t.appendChild(x);
    stack.appendChild(t);
    return;
  }
  stack.appendChild(t);
  setTimeout(() => t.remove(), 4200);
}

async function api(path, body) {
  const res = await fetch(path, body
    ? { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }
    : undefined);
  return res.json();
}

function fmtAge(ts) {
  if (!ts) return "";
  const s = Math.floor(Date.now() / 1000 - ts);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

function cardHtml(a) {
  const isBusy = busy.has(a.id);
  const starting = a.running && a.healthy === false;
  const statusKey = !a.running ? (a.lastError ? "error" : "stopped") : starting ? "starting" : (a.health ? "ready" : "running");
  const statusLabel = { stopped: "Stopped", error: "Error", starting: "Starting", ready: "Ready", running: "Running" }[statusKey];
  const appKind = a.cli ? "CLI tool" : a.desktop ? "Desktop app" : a.servedBy === "launcher"
    ? "Hosted page" : "Web app";
  const description = String(a.desc || "").trim() || (a.cli
    ? "Run this command-line tool from its app folder."
    : a.desktop ? "Launch this desktop application from AppDeck."
      : a.servedBy === "launcher" ? "Open this page hosted by AppDeck."
        : a.network ? "Start this web service for devices on your Wi-Fi."
          : "Start this web service on this computer.");
  const hasStatusDetails = Boolean(a.lastError && !a.running);
  const extBadge = a.external ? `<span class="badge ext" title="${escapeHtml(a.proc || "")}">external</span>` : "";
  const autoBadge = a.autostart ? `<span class="badge auto" title="Starts automatically with AppDeck">auto</span>` : "";
  const storageBadge = a.storage ? `<span class="badge storage" title="Lives on the location “${escapeHtml(a.storage)}” — that drive must be connected to start it (Storage on the card moves it back)">${escapeHtml(a.storage)}</span>` : "";
  // Contract lint (server-side probe): a this-computer-only app whose port
  // answers on the LAN anyway — it ignored the launcher's APPDECK_HOST bind.
  const exposedBadge = (a.lanExposed && !a.network) ? `<span class="badge ext" title="This app is reachable from other devices on this Wi-Fi even though it is marked this-computer-only — it ignored the launcher's loopback bind. Fix the app, or drag its card to the Wi-Fi section to make the sharing explicit.">exposed</span>` : "";
  // macOS code-seal state from import — so a later macOS security prompt
  // ("appdeck-tmp wants to…", a silently killed launch) has card context.
  const sig = a.signature ? {
    vendor:  ["signed",  "Developer-signed — its code signature was verified when it was imported."],
    adhoc:   ["ad-hoc",   "Its original code signature did not survive importing; AppDeck re-signed it ad hoc. It runs, but macOS no longer attributes it to its developer — if macOS shows a security prompt for this app, this is why."],
    invalid: ["unsigned", "Its code signature could not be verified when it was imported and could not be re-signed — macOS may block or kill it at launch. Re-import the vendor's original .dmg or .zip."],
  }[a.signature] : null;
  const sigBadge = sig ? `<span class="badge sig-${a.signature}" title="${escapeHtml(sig[1])}">${sig[0]}</span>` : "";
  const secBadge = a.security === "flagged"
    ? `<span class="badge error" title="Flagged by the security scan — open the security dialog for details, Start will ask first">flagged</span>`
    : a.security === "warn"
      ? `<span class="badge" title="The security scan noted warnings — open the security dialog for details">warn</span>` : "";
  // Permission badge: the card declares what it needs (permissions.go) —
  // the tooltip names them, the ⚠ variant marks ones never granted.
  const permBadge = a.permissions && a.permissions.length
    ? `<span class="badge${a.permissions.length === 5 ? "" : ""}" title="Declares permissions: ${escapeHtml(a.permissions.join(", "))} — Start asks once before the first launch">perm ${a.permissions.length}</span>`
    : "";
  // Sandbox badge: the card runs under the OS sandbox (seatbelt/bwrap/job).
  const sandboxBadge = a.sandboxed ? `<span class="badge" title="Runs under the OS sandbox — its file access is fenced to its own folder and data dir, and outbound network needs a declared permission (see AUTHORING.md)">sandbox</span>` : "";
  const accent = a.accent ? ` style="--app-accent:${escapeHtml(a.accent)}"` : "";
  const pinned = pinnedApps.has(a.id);
  // Lifecycle policy badge: the card's effective mode (manifest default or
  // operator override), so the way an app runs is visible without opening
  // the ⏱ dialog.
  const polBadge = a.policy && a.policy.mode ? {
    always: `<span class="badge auto" title="Always on — starts with AppDeck; the crash watchdog keeps it running">always on</span>`,
    onDemand: `<span class="badge auto" title="On-demand — stops ${a.policy.stopAfterIdle || 5} min after the last client disconnects">on-demand</span>`,
    scheduled: `<span class="badge auto" title="Scheduled — runs ${escapeHtml(a.policy.schedule || "")}">scheduled</span>`,
  }[a.policy.mode] || "" : "";
  const visibleFlags = [
    extBadge, autoBadge, storageBadge, sigBadge, permBadge, sandboxBadge,
    polBadge ? `<span class="card-flag policy">${escapeHtml(a.policy.mode === "always" ? "Always on" : a.policy.mode === "onDemand" ? "On demand" : "Scheduled")}</span>` : "",
    secBadge,
    exposedBadge,
  ].filter(Boolean).join("");
  const primaryAction = a.running
    ? `<button type="button" class="btn-stop icon-only-action" data-act="stop" data-id="${a.id}" aria-label="Stop ${escapeHtml(a.name)}" title="Stop ${escapeHtml(a.name)}" ${isBusy ? "disabled" : ""}>${isBusy ? '<span class="spinner"></span>' : svg("stop")}</button>`
    : `<button type="button" class="btn-start icon-only-action" data-act="start" data-id="${a.id}" aria-label="Start ${escapeHtml(a.name)}" title="Start ${escapeHtml(a.name)}" ${isBusy ? "disabled" : ""}>${isBusy ? '<span class="spinner"></span>' : svg("rocket")}</button>`;
  const openAction = a.running && !a.desktop
    ? `<button type="button" class="btn-primary icon-only-action" data-act="open" data-id="${a.id}" aria-label="Open ${escapeHtml(a.name)}" title="Open ${escapeHtml(a.name)}">${svg("external", 16)}</button>`
    : a.running && a.desktop && !a.cli
      ? `<button type="button" class="btn-primary icon-only-action" data-act="show" data-id="${a.id}" aria-label="Show ${escapeHtml(a.name)}" title="Show ${escapeHtml(a.name)}" ${isBusy ? "disabled" : ""}>${svg("monitor", 16)}</button>`
      : !a.running && !a.desktop && a.policy && a.policy.mode === "onDemand"
        ? `<button type="button" class="btn-ghost icon-only-action" data-act="open" data-id="${a.id}" aria-label="Open ${escapeHtml(a.name)} on demand" title="Open ${escapeHtml(a.name)} on demand">${svg("external", 16)}</button>`
        : "";
  const openSlot = openAction || `<span class="icon-only-action action-placeholder" aria-hidden="true"></span>`;
  const exposeLabel = a.exposed ? "Remove OS shortcut" : "Expose to the OS";
  const exposeAction = !hiddenCardActions.has("expose")
    ? `<button type="button" class="btn-ghost icon-only-action expose-action" data-act="expose" data-id="${a.id}" aria-label="${exposeLabel} for ${escapeHtml(a.name)}" title="${escapeHtml(exposeTip(a))}" ${a.exposeable ? "" : "disabled"}>${svg("link", 16)}</button>`
    : "";
  const privilegeAction = !hiddenCardActions.has("privilege") ? privilegeButton(a) : "";
  const uninstallAction = !hiddenCardActions.has("uninstall")
    ? `<button type="button" class="btn-ghost icon-only-action danger-action" data-act="uninstall" data-id="${a.id}" aria-label="Uninstall ${escapeHtml(a.name)}" title="Uninstall ${escapeHtml(a.name)}">${svg("trash", 16)}</button>`
    : "";
  const menuAction = (action, icon, label, title = label, disabled = false) =>
    `<button type="button" class="card-option" data-act="${action}" data-id="${a.id}" aria-label="${escapeHtml(label)} for ${escapeHtml(a.name)}" title="${escapeHtml(title)}" ${disabled ? "disabled" : ""}>${svg(icon, 16)}<span>${escapeHtml(label)}</span></button>`;
  const menuActions = [
    menuAction("verinfo", "info", "App details"),
    menuAction("logs", "logs", "Logs"),
    a.running ? menuAction("restart", "restart", "Restart", "Restart", isBusy) : "",
    a.cli && !hiddenCardActions.has("terminal") ? menuAction("terminal", "terminal", "Open terminal") : "",
    !a.desktop && !hiddenCardActions.has("network")
      ? menuAction("network-toggle", a.network ? "home" : "wifi", a.network ? "Keep on this computer" : "Share on Wi-Fi") : "",
    !hiddenCardActions.has("lifecycle") ? menuAction("lifecycle", "clock", "Lifecycle policy") : "",
    !hiddenCardActions.has("storage") ? menuAction("storage", "disk", "Storage location") : "",
    !hiddenCardActions.has("datafolder") ? menuAction("datafolder", "folder", "Open data folder") : "",
    !a.desktop && !a.running ? menuAction("port", "globe", "Start on another port") : "",
    a.origin && !hiddenCardActions.has("update") ? menuAction("update", "retry", "Update from origin", "Update from origin", isBusy) : "",
  ].filter(Boolean).join("");
  const icon = a.iconFile ? `<img src="${iconQuery(a.id)}" alt="">` : escapeHtml(a.icon || "📦");
  const wifiIndicator = a.network && !a.desktop
    ? `<span class="card-wifi-state" role="img" aria-label="Shared over Wi-Fi" title="Shared over Wi-Fi">${svg("wifi", 14)}</span>`
    : "";
  return `
  <article class="card ${a.running ? (starting ? "starting" : "running") : ""}${a.external ? " external" : ""}" id="card-${a.id}"${accent} data-id="${a.id}" data-net="${a.network ? "wifi" : "local"}"${a.desktop ? "" : ` draggable="true"`}>
    <div class="card-top">
      <div class="app-icon">${icon}</div>
      <div class="card-copy">
        <div class="card-title-row">
          <span class="card-state ${statusKey}" role="img" aria-label="Status: ${statusLabel}" title="${statusLabel}"><span class="dot" aria-hidden="true"></span></span>
          <div class="card-title">${escapeHtml(a.name)}</div>
          <button class="pin ${pinned ? "on" : ""}" data-act="pin" data-id="${a.id}" aria-label="${pinned ? "Unpin" : "Pin"} ${escapeHtml(a.name)}" title="${pinned ? "Pinned — click to unpin" : "Pin to the top of its section"}">${pinned ? svg("starFilled") : svg("star")}</button>
          ${wifiIndicator}
        </div>
        <div class="card-kind">${appKind}</div>
      </div>
      <label class="batch-check" title="Select for batch uninstall"><input type="checkbox" data-batch="${a.id}" ${batchSelected.has(a.id) ? "checked" : ""} aria-label="Select ${escapeHtml(a.name)} for batch uninstall"></label>
      <div class="actions" role="group" aria-label="Primary actions for ${escapeHtml(a.name)}">
        ${primaryAction}${exposeAction}${privilegeAction}${openSlot}${uninstallAction}
        <details class="card-options">
          <summary class="card-options-toggle icon-only-action" aria-label="Options for ${escapeHtml(a.name)}" aria-controls="card-options-${a.id}" title="Options for ${escapeHtml(a.name)}">${svg("more", 16)}</summary>
          <div class="card-options-menu" id="card-options-${a.id}" role="group" aria-label="More actions for ${escapeHtml(a.name)}">${menuActions}</div>
        </details>
      </div>
    </div>
    <div class="card-desc">${escapeHtml(description)}</div>
    ${hasStatusDetails ? `<div class="card-status-row">
      ${a.lastError && !a.running ? `<button type="button" class="card-alert" data-act="logs" data-id="${a.id}">${svg("warning", 13)} Last start failed · View log</button>` : ""}
    </div>` : ""}
    ${visibleFlags ? `<div class="card-flags">${visibleFlags}</div>` : ""}
  </article>`;
}

// iconArt prefers the app's icon.png/svg artwork (served by /api/icon) and
// falls back to its manifest emoji where no file exists.
const iconArt = (a) => a && a.iconFile
  ? `<img class="mini-icon" src="${iconQuery(a.id)}" alt="">`
  : `<span>${escapeHtml((a && a.icon) || "📦")}</span>`;

// privilegeButton is the per-app privilege toggle, icon-only: 👮 for an app
// under the launcher's identity, 💀 for one with its own elevation. One
// concept, three OS mechanisms — macOS TCC identity (Elevated/Inherited),
// Windows admin launch (Admin/Standard), Linux root launch (Root/User) — the
// tooltip carries the OS-specific wording. On every OS the elevation belongs
// to the app alone; the launcher never gains anything.
function privilegeButton(a) {
  // Elevation wraps the app's own binary (macOS identity bundle / Windows
  // UAC / Linux pkexec): static and shell-cmd apps run under the launcher
  // and have nothing to elevate. The button still renders (disabled, at the
  // row's faded opacity) so the action column lines up on every card — only
  // an old elevation that must stay revocable keeps it clickable.
  const applicable = a.elevated || a.hasBinary;
  const label = a.elevated ? svg("shieldCheck") : svg("shield");
  let tip;
  if (!applicable) {
    tip = a.servedBy === "launcher"
      ? "Hosted by the launcher itself — no separate binary, so there is nothing to elevate or inherit."
      : "Runs inside the launcher with no separate binary — nothing to elevate or inherit.";
  } else if (hostOS === "windows") {
    tip = a.elevated
      ? "Admin — starts through its own UAC elevation; the admin token is this app's alone. Click to run it as a standard app again."
      : "Standard — runs under your normal user token. Click to make it start as administrator: Windows shows the app its own UAC prompt at each start. AppDeck never gains admin rights.";
  } else if (hostOS === "linux") {
    tip = a.elevated
      ? "Root — starts through pkexec/sudo authorization; root access is this app's alone. Click to run it as your normal user again."
      : "User — runs under your normal user. Click to make it start as root: the desktop asks to authorize the app at each start. AppDeck never gains root.";
  } else {
    tip = a.elevated
      ? "Elevated — launches with its own macOS identity; its Full Disk Access grants are this app's alone. Click to lock it back to the launcher's inherited identity."
      : "Inherited — runs under the launcher's macOS identity, so its privacy grants are the launcher's. Click to elevate it: AppDeck builds the app its own .app (icon included) and opens it in Finder next to the Full Disk Access pane to drag in once. The launcher keeps no grants.";
  }
  const action = !applicable ? "Privilege unavailable" : a.elevated ? "Switch to standard identity" : "Run with elevation";
  return `<button type="button" class="btn-ghost icon-only-action privilege-action${a.elevated ? " priv-on" : ""}" data-act="privilege" data-id="${a.id}" aria-label="${action} for ${escapeHtml(a.name)}" ${applicable ? "" : "disabled"} title="${escapeHtml(tip)}">${label}</button>`;
}

// exposeTip is the 🔗 button's tooltip — the artifact name differs per OS,
// the trade-off (findable outside the deck, but no launcher environment)
// is the same everywhere. Disabled cards explain why, like the privilege
// button does.
function exposeTip(a) {
  if (!a.exposeable) {
    return "Nothing to expose — this app runs inside the launcher (a hosted page or a shell command) and has no program of its own to hand to the OS.";
  }
  const where = hostOS === "darwin" ? "/Applications" : hostOS === "windows" ? "the Start menu" : "the applications menu";
  const what = hostOS === "darwin" ? "Spotlight, Finder and Launchpad" : hostOS === "windows" ? "Windows search" : "your desktop's search";
  const gen = hostOS === "darwin" && !a.exposed
    ? " A bare binary is wrapped into its own .app first — the same wrapper the elevation feature builds, so the two share one bundle and never override each other."
    : "";
  if (a.exposed) return `Linked into ${where} right now — ${what} see it, and Start launches the app through that bundle. Click to remove the link (the app itself stays in the deck).`;
  return `Expose to ${where} — a link ${what} will find, so you can open the app without AppDeck.${gen} The deck keeps owning the folder: updates and uninstall stay here. Launched standalone the app runs without the launcher's environment (ports, data dir) — ideal for self-contained apps.`;
}

// shortLanUrl renders a LAN URL as :port/path — the port chip next to it
// already names the app, and full URLs wrapped mid-word in the cards.
function shortLanUrl(u) {
  try {
    const p = new URL(u);
    return `:${p.port}${p.pathname === "/" ? "" : p.pathname}`;
  } catch { return u; }
}


function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

// The 4 s status poll calls render() constantly; swapping in identical markup
// was the deck's flicker (icons re-decode, tooltips and selections die). The
// last rendered signature gates the DOM writes — see the tail of render().
let lastRenderSig;
function renderResourceSummary() {
  const measured = apps.filter(a => a.running && a.resources);
  const cpu = measured.reduce((n, a) => n + (Number(a.resources.cpuPercent) || 0), 0);
  const ram = measured.reduce((n, a) => n + (Number(a.resources.rssMB) || 0), 0);
  const cpuEl = document.getElementById("sb-usage-cpu");
  const ramEl = document.getElementById("sb-usage-ram");
  if (cpuEl) cpuEl.textContent = (cpu >= 10 ? Math.round(cpu) : cpu.toFixed(1)) + "%";
  if (ramEl) ramEl.textContent = ram >= 1024 ? (ram / 1024).toFixed(1) + " GB" : Math.round(ram) + " MB";
}

function render() {
  renderResourceSummary();
  const running = apps.filter(a => a.running).length;
  const wifiCount = apps.filter(a => a.network).length;

  // Filters: free-text (name/category/description) AND state (all/running/
  // stopped). The summary keeps counting every app so totals never lie.
  const q = filterText;
  const wantRunning = stateFilter === "running";
  const visible = apps.filter(a =>
    (!q || `${a.name} ${a.category} ${a.desc}`.toLowerCase().includes(q)) &&
    (!categoryFilter || ((a.category || "Tools").trim() || "Tools") === categoryFilter) &&
    (stateFilter === "pinned" ? pinnedApps.has(a.id)
      : stateFilter === "all" || a.running === wantRunning));

  const wifi = visible.filter(a => a.network && !a.desktop);
  // Hosted static pages get their own section (the sidebar's Web pages):
  // the launcher serves them in-process, so they are neither Wi-Fi-exposed
  // by a port of their own nor desktop apps.
  const web = visible.filter(a => a.servedBy === "launcher");
  const local = visible.filter(a => !a.network && !a.desktop && a.servedBy !== "launcher");
  // CLI tools (an MCP stdio server, a command toolkit) are desktop-lifecycle
  // apps without a window: their own section, their own action.
  const cli = visible.filter(a => a.cli);
  const desktop = visible.filter(a => a.desktop && !a.cli);
  const byName = (a, b) => String(a.name).localeCompare(String(b.name), undefined, { sensitivity: "base" });
  const byCategory = (a, b) => String(a.category || "Tools").localeCompare(String(b.category || "Tools"), undefined, { sensitivity: "base" }) || byName(a, b);
  const byRecent = (a, b) => ((b.lastStarted || 0) - (a.lastStarted || 0)) || byName(a, b);
  const pinFirst = (arr) => arr.sort((a, b) => (pinnedApps.has(b.id) ? 1 : 0) - (pinnedApps.has(a.id) ? 1 : 0));
  if (sortMode === "az") { wifi.sort(byName); local.sort(byName); desktop.sort(byName); web.sort(byName); }
  if (sortMode === "cat") { wifi.sort(byCategory); local.sort(byCategory); desktop.sort(byCategory); web.sort(byCategory); }
  if (sortMode === "recent") { wifi.sort(byRecent); local.sort(byRecent); desktop.sort(byRecent); web.sort(byRecent); }
  pinFirst(wifi);
  pinFirst(local);
  pinFirst(desktop);
  pinFirst(web);
  // Category sorting orders cards in place without repeating category headers;
  // the sidebar is the single place to browse and filter categories.
  const sectionInner = (list) => {
    if (!list.length) return `<div class="drop-here">drag a web app here</div>`;
    return `<div class="grid">${list.map(cardHtml).join("")}</div>`;
  };
  // Sections: the two web zones are always rendered as drop zones — dragging
  // a web app between them flips its Wi-Fi sharing (see the drop handler
  // below). The desktop section carries no drag semantics (desktop apps have
  // no port to share) and only renders when it has apps.
  const zone = (key, label, list, drop = true) => `
    <div class="group${collapsedSections.has(key) ? " collapsed" : ""}"${drop ? ` data-drop="${key}"` : ""}>
      <div class="group-label" data-collapse="${key}">${label}${list.length ? ` <span class="muted">(${list.length})</span>` : ""}<span class="chev">${svg("chevronDown", 12)}</span></div>
      ${sectionInner(list)}
    </div>`;
  let html = "";
  if (!appsLoaded) {
    // Skeleton first paint: instant feedback while /api/apps is in flight —
    // never a flash of the "deck is empty" state on a loaded deck.
    html = `<div class="grid">` +
      `<div class="card skel-card"><div class="skel skel-icon"></div><div class="skel skel-line w40"></div><div class="skel skel-line w90"></div><div class="skel skel-line w70"></div></div>`.repeat(6) +
      `</div>`;
  } else if (!apps.length) {
    html = `
      <div class="empty-deck">
        <div class="ed-icon">${svg("all", 44)}</div>
        <p class="ed-title">Start building your app deck</p>
        <p>Bring in an app you already use, or start with a featured card.</p>
        <div class="ed-actions"><button class="btn-primary" data-act="open-import">${svg("import", 15)} Import an app</button><button class="btn-ghost" data-act="open-featured">${svg("star", 15)} Browse featured</button></div>
        <p class="ed-hint">You can also drop an app folder containing <code>app.json</code> into <code>Apps/</code>.</p>
      </div>`;
  } else if (visible.length) {
    if (sectionFilter === "all") {
      html += zone("wifi", `${svg("wifi", 13)} Wi-Fi — open these from other devices`, wifi);
      html += zone("local", `${svg("home", 13)} This computer only`, local);
      if (web.length) html += zone("web", `${svg("globe", 13)} Web pages — hosted by the launcher`, web, false);
      if (desktop.length) html += zone("desktop", `${svg("monitor", 13)} Desktop apps`, desktop, false);
      if (cli.length) html += zone("cli", `${svg("terminal", 13)} CLI tools — run them from a terminal`, cli, false);
    } else {
      const bySection = { wifi, local, desktop, cli, web };
      const label = { wifi: `${svg("wifi", 13)} Wi-Fi — open these from other devices`,
                      local: `${svg("home", 13)} This computer only`,
                      desktop: `${svg("monitor", 13)} Desktop apps`,
                      cli: `${svg("terminal", 13)} CLI tools — run them from a terminal`,
                      web: `${svg("globe", 13)} Web pages — hosted by the launcher` }[sectionFilter];
      const list = bySection[sectionFilter];
      if (list.length) {
        html += zone(sectionFilter, label, list, sectionFilter !== "desktop" && sectionFilter !== "web" && sectionFilter !== "cli");
      } else {
        html += `<div class="group"><div class="group-label">${label}</div>` +
          `<div class="drop-here">no apps match this filter</div></div>`;
      }
    }
  } else {
    html += `<div class="group-label">No apps match the current filters — Esc to clear</div>`;
  }
  // Skip the DOM writes when nothing visible changed: html already carries
  // every input (running states, ages, badges, spinners, pins, filters), and
  // the counters below derive from the same data. A rebuild now happens only
  // when the deck would actually look different — e.g. a running age ticking
  // "59s" → "1m" — never just because the poll fired.
  const sig = `${running}/${apps.length}/${wifiCount}/${busy.size}\n${html}`;
  if (sig === lastRenderSig) return;
  lastRenderSig = sig;
  renderSidebar();
  renderContentHead(visible.length, { wifi: wifi.length, local: local.length,
    web: web.length, desktop: desktop.length, cli: cli.length });
  // Hero stat tiles: the deck's numbers where the content begins (the
  // sidebar navigates; these count). Hidden while the deck is empty.
  const heroEl = document.getElementById("hero-strip");
  if (heroEl) {
    if (!appsLoaded || !apps.length) heroEl.hidden = true;
    else {
      heroEl.hidden = false;
      const tile = (label, n, unit) =>
        `<div class="hero-tile"><span class="ht-label">${label}</span><b>${n}</b>${unit ? `<span class="ht-unit">${unit}</span>` : ""}</div>`;
      heroEl.innerHTML =
        tile("Running now", running, running === 1 ? "app" : "apps") +
        tile("Stopped", apps.length - running, (apps.length - running) === 1 ? "app" : "apps") +
        tile("Apps in deck", apps.length, "total") +
        tile("Shared on Wi-Fi", wifiCount, wifiCount === 1 ? "app" : "apps");
    }
  }
  // The hero tiles are the deck's counters now; the header carries none.
  $("#btn-stop-all").disabled = running === 0 || busy.size > 0;
  $("#btn-start-all").disabled = running === apps.length || busy.size > 0;
  groupsEl.innerHTML = html;
  if (!appsLoaded) return;
  // First paint with real apps: a short stagger gives the deck a finished
  // entrance; later re-renders (the 2 s poll) paint in place.
  if (firstPaintRender) {
    firstPaintRender = false;
    groupsEl.querySelectorAll(".grid > .card").forEach((c, i) => {
      if (i < 10) c.style.animation = `cardin .3s ${i * 25}ms ease backwards`;
    });
  }
}

// Filter box: typing filters instantly; "/" focuses it from anywhere and Esc
// leaves it (a second Esc clears the box via the native search behavior).
let filterText = "";
const filterInput = $("#filter");
filterInput.addEventListener("input", () => {
  filterText = filterInput.value.trim().toLowerCase();
  render();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && document.body.classList.contains("mobile-nav-open")) {
    closeMobileNav(true);
    return;
  }
  if (e.key === "/" && document.activeElement !== filterInput &&
      !e.metaKey && !e.ctrlKey && !e.altKey) {
    e.preventDefault();
    filterInput.focus();
  } else if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
    // ⌘K / Ctrl+K: the quick switcher, from anywhere.
    e.preventDefault();
    if ($("#palette-overlay").classList.contains("show")) closePalette();
    else openPalette();
  } else if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "r" && activeTabId) {
    // ⌘R / Ctrl+R: reload the active app tab's page. The iframe is
    // cross-origin (a different port), so the reload is a src reassignment —
    // no frame access needed.
    e.preventDefault();
    const f = document.getElementById("apptab-frame-" + activeTabId);
    if (f) f.src = f.src;
  } else if (e.key === "Escape") {
    const cardOptions = document.querySelector(".card-options[open]");
    if (cardOptions) {
      e.preventDefault();
      cardOptions.open = false;
      cardOptions.querySelector("summary")?.focus();
      return;
    }
    if (document.body.classList.contains("page-open")) { closePage(); return; }
    const sheet = document.getElementById("settings-overlay");
    if (sheet && sheet.classList.contains("show")) { closeSettings(); return; }
    if (categoryFilter) { applyCategory(""); return; }
    if (document.activeElement === filterInput) filterInput.blur();
    // The 🎨/🖼 pickers are side panels previewing over the live deck — Esc closes them.
    document.querySelectorAll(".overlay.side.show, .overlay.page.show").forEach(o => o.classList.remove("show"));
  }
});

// Category filter (the sidebar's CATEGORIES group and the clickable
// category labels): combines with the text and state filters like they do.
// Declared first — the seg syncs below render() on init, and the sidebar
// reads this variable.
categoryFilter = "";
// Sidebar category sort direction — alphabetical both ways, remembered.
let catSortDir = "asc";
try { if (localStorage.getItem("appdeck.catSort") === "desc") catSortDir = "desc"; } catch {}
const collapsedSidebar = new Set(["categories"]);
try {
  const saved = JSON.parse(localStorage.getItem("appdeck.sidebarCollapsed") || "null");
  if (Array.isArray(saved)) { collapsedSidebar.clear(); saved.forEach(key => collapsedSidebar.add(key)); }
} catch {}
const saveSidebarCollapse = () => {
  try { localStorage.setItem("appdeck.sidebarCollapsed", JSON.stringify([...collapsedSidebar])); } catch {}
};
const sbBlock = (key, label, items, extra = "") => {
  const closed = collapsedSidebar.has(key);
  return `<section class="sb-block${closed ? " collapsed" : ""}">` +
    `<div class="sb-cap"><span class="sb-cap-title">${label}</span>${extra}` +
    `<button type="button" class="sb-cap-toggle" data-sb-collapse="${key}" aria-label="${closed ? "Expand" : "Collapse"} ${label}" title="${closed ? "Expand" : "Collapse"} ${label}">${svg(closed ? "plus" : "minus", 12)}</button></div>` +
    `<div class="sb-block-items"${closed ? " hidden" : ""}>${items}</div></section>`;
};
try {
  const saved = localStorage.getItem("appdeck.categoryFilter");
  if (saved) categoryFilter = saved;
} catch {}
function applyCategory(next) {
  categoryFilter = next || "";
  try { localStorage.setItem("appdeck.categoryFilter", categoryFilter); } catch {}
  render();
}

// State filter segments (All / ▶ running / ■ stopped), remembered per device.
stateFilter = "all";
try {
  const saved = localStorage.getItem("appdeck.stateFilter");
  if (saved === "running" || saved === "stopped") stateFilter = saved;
} catch {}
function applyStateFilter(next) {
  stateFilter = next;
  try { localStorage.setItem("appdeck.stateFilter", next); } catch {}
  render();
}
applyStateFilter(stateFilter); // sync the pressed state with the loaded value

// Section filter: all sections, or just the Wi-Fi / This computer / Desktop
// group. Independent of the state filter above — both apply at once.
sectionFilter = "all";
try {
  const saved = localStorage.getItem("appdeck.sectionFilter");
  if (saved === "wifi" || saved === "local" || saved === "desktop" || saved === "cli" || saved === "web") sectionFilter = saved;
} catch {}
function applySectionFilter(next) {
  sectionFilter = next;
  try { localStorage.setItem("appdeck.sectionFilter", next); } catch {}
  render();
}
applySectionFilter(sectionFilter);



// ---- Sidebar: the Prism-style filter rail ----------------------------------------
// One glass rail that consolidates what the header segments do (state,
// section) and adds what they had no room for: per-category counts, the
// launcher-hosted web pages. Items set the SAME filter
// variables the header segments use — the two stay in sync by construction.

function sbItem(kind, val, icon, label, count, active) {
  return `<button class="sb-item${active ? " active" : ""}" data-sb="${kind}" data-val="${escapeHtml(val)}"` +
    `${count === null ? "" : ` data-count="${count}"`}>` +
    `<span class="sb-ic">${icon}</span><span>${escapeHtml(label)}</span>` +
    `${count === null ? "" : `<span class="sb-count">${count}</span>`}</button>`;
}

function renderSidebar() {
  // Looked up per call: the filter vars below initialize before this
  // function's const would, and the early seg-sync renders hit here first.
  const sbNav = document.getElementById("sb-nav");
  if (!sbNav) return;
  const n = (fn) => apps.filter(fn).length;
  const running = n(a => a.running);
  const heroN = $("#sb-hero-n"), heroU = $("#sb-hero-unit");
  if (heroN) heroN.textContent = running;
  if (heroU) heroU.textContent = running === 1 ? "app live" : "apps live";

  const counts = {
    all: apps.length,
    running,
    stopped: apps.length - running,
    pinned: apps.filter(a => pinnedApps.has(a.id)).length,
    wifi: n(a => a.network && !a.desktop),
    local: n(a => !a.network && !a.desktop && a.servedBy !== "launcher"),
    desktop: n(a => a.desktop && !a.cli),
    cli: n(a => a.cli),
    web: n(a => a.servedBy === "launcher" && !a.desktop),
  };
  const noCat = !categoryFilter;
  const plainState = noCat && sectionFilter === "all";

  let h = "";
  const appLinks = [
    sbItem("all", "all", svg("all", 16), "All apps", counts.all, plainState && stateFilter === "all"),
    sbItem("state", "running", svg("play", 16), "Running", counts.running, plainState && stateFilter === "running"),
    sbItem("state", "stopped", svg("stop", 16), "Stopped", counts.stopped, plainState && stateFilter === "stopped"),
    sbItem("state", "pinned", svg("pin", 16), "Pinned", counts.pinned, plainState && stateFilter === "pinned"),
  ].join("");
  h += sbBlock("apps", "Apps", appLinks);
  const sectionLinks = [
    sbItem("section", "wifi", svg("wifi", 16), "Wi-Fi", counts.wifi, noCat && sectionFilter === "wifi"),
    sbItem("section", "local", svg("home", 16), "This computer", counts.local, noCat && sectionFilter === "local"),
    sbItem("section", "web", svg("globe", 16), "Web pages", counts.web, noCat && sectionFilter === "web"),
    sbItem("section", "desktop", svg("monitor", 16), "Desktop apps", counts.desktop, noCat && sectionFilter === "desktop"),
    sbItem("section", "cli", svg("terminal", 16), "CLI tools", counts.cli, noCat && sectionFilter === "cli"),
  ].join("");
  h += sbBlock("sections", "Sections", sectionLinks);

  // Categories: plain alphabetical (the caption's ↑/↓ toggles direction) —
  // selecting one never reorders the list.
  const cats = new Map();
  for (const a of apps) {
    const c = (a.category || "Tools").trim() || "Tools";
    cats.set(c, (cats.get(c) || 0) + 1);
  }
  const catDir = catSortDir === "desc" ? -1 : 1;
  const catList = [...cats.entries()].sort((x, y) =>
    catDir * x[0].localeCompare(y[0], undefined, { sensitivity: "base" }));
  if (catList.length) {
    const categoryLinks = catList.map(([c, k]) =>
      sbItem("category", c, svg("tag", 16), c, k, categoryFilter === c)).join("");
    const sort = `<button type="button" class="sb-sort" data-sbsort title="Toggle category order" aria-label="Sort categories ${catSortDir === "desc" ? "ascending" : "descending"}">${svg(catSortDir === "desc" ? "sortDesc" : "sortAsc", 12)}</button>`;
    h += sbBlock("categories", "Categories", categoryLinks, sort);
  }

  sbNav.innerHTML = h;
}

// The brand is Home: an open page closes, back to the cards.
document.getElementById("sb-brand").addEventListener("click", () => {
  closeMobileNav();
  if (document.body.classList.contains("page-open")) closePage();
  closeSettings(); // the floating sheet leaves with any navigation
});

const mobileNavToggle = document.getElementById("mobile-nav-toggle");
function closeMobileNav(returnFocus = false) {
  document.body.classList.remove("mobile-nav-open");
  mobileNavToggle.setAttribute("aria-expanded", "false");
  mobileNavToggle.setAttribute("aria-label", "Open navigation");
  if (returnFocus) mobileNavToggle.focus();
}
mobileNavToggle.addEventListener("click", () => {
  const open = !document.body.classList.contains("mobile-nav-open");
  document.body.classList.toggle("mobile-nav-open", open);
  mobileNavToggle.setAttribute("aria-expanded", String(open));
  mobileNavToggle.setAttribute("aria-label", open ? "Close navigation" : "Open navigation");
  if (open) document.querySelector("#sb-nav button")?.focus();
});
document.getElementById("mobile-nav-scrim").addEventListener("click", () => closeMobileNav(true));
document.getElementById("mobile-nav-close").addEventListener("click", (e) => {
  e.stopPropagation();
  closeMobileNav(true);
});
document.querySelector(".sb-foot").addEventListener("click", (e) => {
  if (e.target.closest("button")) closeMobileNav();
});

// ---- Content head: title + status segment + view toggles (Prism pattern) -----
// The title names what the sidebar picked; the segment applies the state
// filter with live counts; the view buttons switch between grid and list.
function renderContentHead(visibleCount, secCounts) {
  const head = document.getElementById("content-head");
  if (!head) return;
  head.hidden = !appsLoaded || !apps.length || document.body.classList.contains("page-open");
  if (head.hidden) return;
  const secName = { wifi: "Wi-Fi", local: "This computer", web: "Web pages",
                    desktop: "Desktop apps", cli: "CLI tools" }[sectionFilter];
  const stateName = { all: "All apps", running: "Running", stopped: "Stopped", pinned: "Pinned" }[stateFilter] || "All apps";
  $("#ch-title").textContent = categoryFilter || secName || stateName;
  // When a section is picked the count answers for that section, like
  // Prism's "Video — 0 items".
  const shown = secCounts && secCounts[sectionFilter] != null ? secCounts[sectionFilter] : visibleCount;
  $("#ch-sub").textContent = `${shown} item${shown === 1 ? "" : "s"}` +
    (shown !== apps.length ? ` of ${apps.length}` : "");
  const run = apps.filter(a => a.running).length;
  const pinnedN = apps.filter(a => pinnedApps.has(a.id)).length;
  const seg = document.getElementById("ch-seg");
  seg.innerHTML = [["all", "All", apps.length], ["running", "Running", run],
                   ["stopped", "Stopped", apps.length - run], ["pinned", "Pinned", pinnedN]]
    .map(([k, label, n]) =>
      `<button data-chs="${k}" class="${stateFilter === k ? "active" : ""}">${label} <span class="n">${n}</span></button>`)
    .join("");
  document.querySelectorAll("#ch-view [data-view]").forEach(b =>
    b.classList.toggle("active", b.dataset.view === viewMode));
}
document.getElementById("ch-seg").addEventListener("click", (e) => {
  const b = e.target.closest("[data-chs]");
  if (b) applyStateFilter(b.dataset.chs);
});
document.getElementById("ch-view").addEventListener("click", (e) => {
  const b = e.target.closest("[data-view]");
  if (!b || b.dataset.view === viewMode) return;
  viewMode = b.dataset.view;
  try { localStorage.setItem("launcher.viewMode", viewMode); } catch {}
  applyViewMode();
});

document.getElementById("sb-nav").addEventListener("click", (e) => {
  const collapse = e.target.closest("[data-sb-collapse]");
  if (collapse) {
    const key = collapse.dataset.sbCollapse;
    if (collapsedSidebar.has(key)) collapsedSidebar.delete(key); else collapsedSidebar.add(key);
    saveSidebarCollapse();
    renderSidebar();
    return;
  }
  if (e.target.closest("[data-sbsort]")) {
    catSortDir = catSortDir === "desc" ? "asc" : "desc";
    try { localStorage.setItem("appdeck.catSort", catSortDir); } catch {}
    renderSidebar();
    document.querySelector("[data-sbsort]")?.focus();
    return;
  }
  const it = e.target.closest("[data-sb]");
  if (!it) return;
  closeMobileNav();
  // The sidebar is the navigator: any item leaves an open page first.
  if (document.body.classList.contains("page-open")) closePage();
  closeSettings();
  const kind = it.dataset.sb, val = it.dataset.val;
  if (kind === "all") {
    applyStateFilter("all"); applySectionFilter("all"); applyCategory("");
    return;
  }
  if (kind === "state") {
    applyStateFilter(val); applySectionFilter("all"); applyCategory("");
    return;
  }
  if (kind === "section") {
    applySectionFilter(val); applyStateFilter("all"); applyCategory("");
    return;
  }
  if (kind === "category") {
    applyCategory(val); applySectionFilter("all"); applyStateFilter("all");
  }
});

// Category labels on the cards (Category sort) participate too — they only
// ever advertised this via their tooltip.
groupsEl.addEventListener("click", (e) => {
  const c = e.target.closest(".cat-label");
  if (c && c.dataset.cat) { applyCategory(c.dataset.cat); applySectionFilter("all"); applyStateFilter("all"); }
});

// Sidebar shortcuts mirror their existing toolbar actions.
$("#sb-theme").addEventListener("click", () => $("#btn-theme").click());
$("#sb-update").addEventListener("click", () => $("#btn-update-launcher").click());
$("#sb-settings").addEventListener("click", () => $("#btn-gear").click());
$("#sb-quit").addEventListener("click", () => $("#btn-quit").click());

// ---- Page dialogs: workspace tools open in the content flow -----------------
// The dialog's modal is moved into #page-area (beside the sidebar, where the
// cards normally live) and moved back on close. The overlay wrapper stays
// hidden — no popup layer at all.
let pageOpenId = null;
function openPage(id) {
  const ov = document.getElementById(id);
  const modal = ov && ov.querySelector(":scope > .modal");
  const slot = document.getElementById("page-area");
  if (!ov || !modal || !slot) { if (ov) ov.classList.add("show"); return; }
  if (pageOpenId && pageOpenId !== id) closePage();
  if (pageOpenId === id) return;
  pageOpenId = id;
  slot.appendChild(modal);
  slot.hidden = false;
  document.body.classList.add("page-open");
  const x = modal.querySelector(".modal-head .x");
  if (x) x.onclick = () => closePage();
  const ch = document.getElementById("content-head");
  if (ch) ch.hidden = true;
  window.scrollTo(0, 0);
}
function closePage() {
  if (!pageOpenId) return;
  const ov = document.getElementById(pageOpenId);
  const modal = document.querySelector("#page-area > .modal");
  if (ov && modal) ov.appendChild(modal);
  document.body.classList.remove("page-open");
  const slot = document.getElementById("page-area");
  if (slot) slot.hidden = true;
  const ch = document.getElementById("content-head");
  if (ch) ch.hidden = !appsLoaded || !apps.length;
  pageOpenId = null;
}

// ---- Settings: one page, Prism-style section nav ------------------------------
// Every deck-level tool lives here as a section. A section's content is the
// body of its donor dialog, adopted in place (moved into the slot on visit,
// back to its donor on switch) — so each tool's existing markup and scripts
// keep working unchanged.
const SETTINGS_SECTIONS = {
  general:     { donor: "deck-overlay",       groups: ["layout", "tray", "updates"], label: "General", sub: "Sorting, card layout, the menu bar companion and how apps open.", load: () => refreshTrayItem() },
  featured:    { donor: "featured-overlay",   label: "Featured", sub: "Browse curated apps and starter templates.", load: () => loadFeatured() },
  import:      { donor: "import-overlay",     label: "Import app", sub: "Add an app from a file, folder or URL." },
  downloads:   { donor: "deck-overlay",       groups: ["downloads"], label: "Downloads", sub: "Choose where downloads go and when to ask.", load: () => refreshDownloadSettings() },
  privacy:     { donor: "deck-overlay",       groups: ["privacy"], label: "Network & privacy", sub: "Choose what the dashboard exposes on your network.", load: () => api("/api/launcher-settings").then(cur => { updateLanMenuItem(cur); updateAppsVisibilityItem(cur); }).catch(() => {}) },
  startup:     { donor: "deck-overlay",       groups: ["startup"], label: "Startup & storage", sub: "Set launch behavior, app startup timing and storage.", load: () => { refreshLoginItem(); refreshStartWait(); } },
  backup:      { donor: "deck-overlay",       groups: ["backup"], label: "Backup & recovery", sub: "Export, import and recover your deck.", load: () => refreshRemovedApps() },
  appearance:  { donor: "theme-overlay",      label: "Theme packs",  sub: "Choose a theme, accent color and ambient appearance.",
                 load: () => loadThemePacks() },
  backgrounds: { donor: "background-overlay", label: "Backgrounds",   sub: "The backdrop behind the deck — packs, brightness, opacity.",
                 load: () => loadBackgrounds() },
  iconpacks:   { donor: "iconpack-overlay",   label: "Icon packs",    sub: "Artwork for the app cards — built-ins and imports.",
                 load: () => loadIconPacks() },
  security:    { donor: "security-overlay",   label: "Security",      sub: "Scan engines, findings and the self-test.",
                 load: () => loadSecurity() },
  prereqs:     { donor: "prereq-overlay",     label: "Prerequisites", sub: "The toolchains apps need — check, install or register them.",
                 load: () => loadPrereqs() },
  limits:      { donor: "limits-overlay",     label: "Import limits", sub: "Ceilings every import must fit — 0 = no limit.",
                 load: () => refreshLimits() },
  about:       { donor: null,                 label: "About",         sub: "This AppDeck — build, mode and where everything lives.",
                 load: null },
};
let settingsSection = "general";

function settingsSlotBody() {
  return document.querySelector("#settings-slot > .modal-body");
}

function homeSettingsBody() {
  // The visiting section body returns to its donor dialog (tagged when it
  // moved in), so each tool's markup always exists exactly once.
  const cur = settingsSlotBody();
  if (!cur) return;
  const donor = cur.getAttribute("data-sec-body");
  if (donor) {
    const donorModal = document.querySelector(`#${donor} > .modal`);
    if (donorModal) donorModal.appendChild(cur);
  }
}

function openSettings(section) {
  if (!SETTINGS_SECTIONS[section]) section = "general";
  // Leaving another page closes it first; the sheet itself floats.
  if (pageOpenId && pageOpenId !== "settings-overlay") closePage();
  homeSettingsBody();
  // About renders directly in the slot instead of borrowing a donor body.
  // Clear that markup when changing sections so "This deck" cannot linger
  // beneath General or any other settings page.
  const slot = document.getElementById("settings-slot");
  slot.replaceChildren();
  const def = SETTINGS_SECTIONS[section];
  if (def.donor) {
    const body = document.querySelector(`#${def.donor} > .modal > .modal-body`);
    if (body) {
      body.setAttribute("data-sec-body", def.donor);
      slot.appendChild(body);
      if (def.groups) body.querySelectorAll(".sgroup[data-settings-group]").forEach(group => {
        group.hidden = !def.groups.includes(group.dataset.settingsGroup);
      });
    }
  } else {
    slot.innerHTML = settingsAboutHTML();
  }
  settingsSection = section;
  document.body.classList.add("settings-open");
  $("#settings-title").textContent = def.label;
  $("#settings-sub").textContent = def.sub;
  document.querySelectorAll("#settings-nav [data-ssec]").forEach(b =>
    b.classList.toggle("active", b.dataset.ssec === section));
  if (def.load) def.load();
  // Prism-style floating sheet: the deck stays visible behind the scrim.
  document.getElementById("settings-overlay").classList.add("show");
}

function closeSettings() {
  const ov = document.getElementById("settings-overlay");
  if (!ov || !ov.classList.contains("show")) return;
  ov.classList.remove("show");
  document.body.classList.remove("settings-open");
  homeSettingsBody();
}
document.getElementById("settings-close").addEventListener("click", closeSettings);
document.getElementById("settings-overlay").addEventListener("click", (e) => {
  if (e.target === e.currentTarget) closeSettings(); // scrim click
});

function settingsAboutHTML() {
  const n = apps.length;
  const rows = [
    ["Build", escapeHtml(hostVersion)],
    ["Mode", hostMode === "single-file" ? "single-file — every app lives inside the launcher executable" : "folder — apps live in the Apps container beside the launcher"],
    ["Platform", `${hostOS} · ${hostArch}`],
    ["Deck", `${n} app${n === 1 ? "" : "s"} installed`],
  ];
  return `<div class="sgroup"><div class="sgroup-title">This deck</div><div class="scard pad">` +
    `<div class="kv">${rows.map(([k, v]) => `<div class="k">${k}</div><div class="v">${v}</div>`).join("")}</div>` +
    `<p class="muted" style="margin-top:14px">Everything else lives beside this page: the per-app logs and data directories, <code>apps.json</code>, the theme packs. Project and docs: <a href="https://github.com/4ppd3ck/Appdeck" target="_blank" rel="noopener">github.com/4ppd3ck/Appdeck</a>.</p>` +
    `</div></div>`;
}

// Settings search: filters the section list; Enter opens the first match.
$("#settings-search").addEventListener("input", (e) => {
  const q = e.target.value.trim().toLowerCase();
  document.querySelectorAll("#settings-nav [data-ssec]").forEach(b => {
    b.style.display = !q || b.textContent.toLowerCase().includes(q) ? "" : "none";
  });
});
$("#settings-search").addEventListener("keydown", (e) => {
  if (e.key !== "Enter") return;
  const first = [...document.querySelectorAll("#settings-nav [data-ssec]")]
    .find(b => b.style.display !== "none");
  if (first) { first.click(); e.target.value = ""; e.target.dispatchEvent(new Event("input")); }
});
document.getElementById("settings-nav").addEventListener("click", (e) => {
  const b = e.target.closest("[data-ssec]");
  if (b) openSettings(b.dataset.ssec);
});

// ---- Duration formatter (hero tile + inspector usage) ------------------------
function fmtDur(sec) {
  sec = Math.max(0, Math.round(sec || 0));
  if (sec < 60) return sec + "s";
  const m = Math.floor(sec / 60), h = Math.floor(m / 60);
  if (h) return `${h}h ${m % 60}m`;
  return `${m}m`;
}

// ---- Onboarding: the one-time start → open → pin strip -----------------------
function maybeTour() {
  const el = document.getElementById("tour-strip");
  if (!el) return;
  let done = false;
  try { done = !!localStorage.getItem("appdeck.tourDone"); } catch {}
  if (done || !apps.length) { el.hidden = true; return; }
  el.innerHTML = `<span>New deck? The whole loop:</span>` +
    `<span class="ts-step">${svg("play", 14)} <b>Start</b> a card</span>` +
    `<span class="ts-step">${svg("external", 14)} Open it in a tab</span>` +
    `<span class="ts-step">${svg("pin", 14)} <b>Pin</b> it to the top</span>` +
    `<button class="btn-ghost ts-x" id="tour-done">Got it</button>`;
  el.hidden = false;
  const b = document.getElementById("tour-done");
  if (b) b.addEventListener("click", () => {
    try { localStorage.setItem("appdeck.tourDone", "1"); } catch {}
    el.hidden = true;
  });
}

// ---- Spectral burst: a card that just came up healthy pulses the beam -------
function sparkle(id) {
  const el = document.getElementById("card-" + id);
  if (!el) return;
  el.classList.remove("just-ready");
  void el.offsetWidth; // restart the animation even on quick restarts
  el.classList.add("just-ready");
  setTimeout(() => el.classList.remove("just-ready"), 1800);
}

// ---- Card inspector (🔎): everything about one app in one rail ---------------
let inspectorAppId = null;
function openInspector(a) {
  inspectorAppId = a.id;
  $("#insp-name").textContent = a.name || a.id;
  const rows = [];
  const kv = (k, v) => { if (v !== "" && v != null) rows.push(`<div class="k">${k}</div><div class="v">${v}</div>`); };
  kv("State", a.running ? (a.healthy === false ? "Starting…" : "Running" + (a.startedAt ? ` · ${fmtAge(a.startedAt) || "just now"}` : "")) : "Stopped");
  if (!a.desktop) kv("Port", `:${a.portOverride || a.port}`);
  if (a.url) kv("URL", `<a href="${escapeHtml(a.url)}" target="_blank" rel="noopener">${escapeHtml(a.url)}</a>`);
  if (a.pid) kv("PID", String(a.pid));
  if (a.resources) kv("RAM · CPU",
    `${a.resources.rssMB >= 100 ? Math.round(a.resources.rssMB) : a.resources.rssMB.toFixed(1)} MB · ${a.resources.cpuPercent >= 1 ? Math.round(a.resources.cpuPercent) + "%" : "idle"}` +
    (a.resources.procs ? ` · ${a.resources.procs} proc` : ""));
  kv("Category", escapeHtml(a.category || "Tools"));
  if (a.version) kv("Version", escapeHtml(a.version));
  if (a.source) kv("Source", escapeHtml(a.source));
  if (a.origin) kv("Origin", `<a href="${escapeHtml(a.origin)}" target="_blank" rel="noopener">${escapeHtml(a.origin)}</a>`);
  if (a.signature) kv("Code seal", { vendor: "vendor-signed", adhoc: "ad-hoc re-signed", invalid: "unverifiable" }[a.signature] || escapeHtml(a.signature));
  if (a.security) kv("Security", a.security === "flagged" ? "flagged" : "warnings");
  if (a.permissions && a.permissions.length) kv("Permissions", escapeHtml(a.permissions.join(", ")));
  if (a.policy) kv("Lifecycle", escapeHtml(`${a.policy.mode}${a.policy.schedule ? ` (${a.policy.schedule})` : ""}${a.policy.stopAfterIdle ? ` · idle stop ${a.policy.stopAfterIdle} min` : ""}`));
  if (a.sandboxed) kv("Sandbox", "OS sandbox active");
  if (a.storage) kv("Storage", escapeHtml(a.storage));
  if (a.servedBy === "launcher") kv("Hosting", "in-process — a static page the launcher serves");
  if (a.lastStarted) kv("Last started", new Date(a.lastStarted * 1000).toLocaleString());
  if (a.runCount) kv("Usage", `${a.runCount} run(s) · ${fmtDur(a.runSeconds)} total`);
  if (a.lastError && !a.running) kv("Last error", escapeHtml(a.lastError));
  $("#inspector-body").innerHTML = `<div class="kv">${rows.join("")}</div>` +
    `<div style="display:flex;gap:8px;margin-top:14px">` +
    `<button class="btn-ghost" data-insp="logs">${svg("logs", 14)} Logs</button>` +
    (a.running
      ? `<button class="btn-primary" data-insp="open">${svg("external", 14)} Open</button>`
      : `<button class="btn-primary" data-insp="start">${svg("play", 14)} Start</button>`) +
    `</div>`;
  $("#inspector-overlay").classList.add("show");
}
$("#insp-close").addEventListener("click", () => $("#inspector-overlay").classList.remove("show"));
$("#inspector-body").addEventListener("click", (e) => {
  const b = e.target.closest("[data-insp]");
  if (!b || !inspectorAppId) return;
  $("#inspector-overlay").classList.remove("show");
  if (b.dataset.insp === "logs") {
    // Reuse the card's own Logs button — one code path for the log viewer.
    const card = document.querySelector(`[data-act="logs"][data-id="${inspectorAppId}"]`);
    if (card) card.click();
  } else if (b.dataset.insp === "start") {
    act(inspectorAppId, "start");
  } else if (b.dataset.insp === "open") {
    const a = apps.find(x => x.id === inspectorAppId);
    if (a) openApp(a);
  }
});

// Accent presets: the Prism six. A preset swaps the whole accent palette
// (including the rgb triplet the ambient light and tints read); a custom
// picked color replaces it.
function hexToRgbTriplet(hex) {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex || "");
  if (!m) return null;
  const n = parseInt(m[1], 16);
  return `${(n >> 16) & 255} ${(n >> 8) & 255} ${n & 255}`;
}
function renderAccentDots() {
  const cur = document.documentElement.dataset.accent || "prism";
  document.querySelectorAll("#accent-dots [data-apreset]").forEach(b =>
    b.classList.toggle("active", b.dataset.apreset === cur));
}
function applyAccentPreset(name) {
  const root = document.documentElement;
  if (!name || name === "prism") root.removeAttribute("data-accent");
  else root.dataset.accent = name;
  // A preset replaces any custom picked color — the inline overrides would
  // otherwise sit on top of the preset's palette.
  root.style.removeProperty("--accent");
  root.style.removeProperty("--accent-2");
  root.style.removeProperty("--accent-rgb");
  try {
    localStorage.removeItem("launcher.accent");
    localStorage.setItem("launcher.accentPreset", name || "prism");
  } catch {}
  const inp = document.getElementById("accent-pick");
  if (inp) inp.value = { prism: "#8e8cff", cyan: "#4fd3ee", emerald: "#46d99c", amber: "#f5bd55", rose: "#ff8aa5", graphite: "#d6d9e2" }[name || "prism"] || "#8e8cff";
  renderAccentDots();
}
document.getElementById("accent-dots").addEventListener("click", (e) => {
  const b = e.target.closest("[data-apreset]");
  if (b) applyAccentPreset(b.dataset.apreset);
});
renderAccentDots();

// Double-click the glass header (mac-frame): the transparent titlebar's own
// double-click-to-zoom is covered by the webview, so the page asks the
// native layer to perform the standard zoom — fit the screen.
document.querySelector("header").addEventListener("dblclick", (e) => {
  if (!document.body.classList.contains("mac-frame")) return;
  if (e.target.closest("button, input, a, select, label, details")) return;
  try { window.webkit?.messageHandlers?.appdeckWindow?.postMessage({ action: "zoom" }); } catch {}
});

async function refresh() {
  try {
    // Health first: OS-gated card buttons (the privilege toggle) need
    // hostOS at render time, not one refresh later.
    const h = await api("/api/health");
    if (h.window) hostWindow = h.window;
    if (h.os) hostOS = h.os;
    if (h.arch) hostArch = h.arch;
    if (h.mode) hostMode = h.mode;
    if (h.version) hostVersion = h.version;
    // macOS native window: transparent titlebar — the glass runs to the
    // window's very top and the traffic lights float over the sidebar.
    document.body.classList.toggle("mac-frame", h.os === "darwin" && h.window === "native");
    $("#btn-firewall").style.display = h.os === "windows" ? "" : "none";
    apps = await api("/api/apps");
    appsLoaded = true;
    render();
    maybeTour();
    if (h.mode === "single-file") {
      $("#foot").innerHTML =
        `Single-file build — every app lives inside the launcher executable.<br>` +
        `Extracted apps, logs &amp; data: <code>${escapeHtml(h.dataDir || "")}</code>`;
    }
    // Registry warnings (invalid third-party manifests, port exhaustion):
    // toast each new one once, so a fix clears the noise on the next reload.
    (h.warnings || []).forEach(w => {
      if (shownWarnings.has(w)) return;
      shownWarnings.add(w);
      toast(w, "err");
    });
    const note = $("#lan-note");
    if (h.lanUrl) {
      // Prefer the HTTPS twin when the launcher serves TLS: the Wi-Fi link
      // should be the encrypted one (it asks for a one-click certificate
      // trust on first visit — see SECURITY.md).
      let url = h.lanUrl;
      if (h.tlsPort) {
        try {
          const u = new URL(h.lanUrl);
          u.protocol = "https:";
          u.port = h.tlsPort;
          url = u.toString();
        } catch { /* keep the http link */ }
      }
      note.dataset.url = url;
      note.title = `Launcher address: ${url}. Click to copy.`;
      note.setAttribute("aria-label", `Launcher address ${url}; click to copy`);
      note.innerHTML = svg("info", 16);
      note.style.display = "inline-flex";
    } else {
      note.style.display = "none";
    }
  } catch {
    $("#summary").textContent = "Launcher offline — is it still running?";
  }
}
const shownWarnings = new Set();

// Import: one dialog, many sources. The launcher sniffs the bytes and routes
// to the right installer — archive, portable exe (wrapped), single html, or a
// downloaded file. Folders and URLs ride the same endpoint.
const importEl = (sel) => $(sel);
$("#btn-install").addEventListener("click", () => { $("#import-pkg-offer").hidden = true; openSettings("import"); });
$("#import-close").addEventListener("click", () => $("#import-overlay").classList.remove("show"));
$("#import-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#import-close").click(); });

let importFile = null;
function importFileName() {
  const f = importFile;
  if (f) return f.name;
  const url = $("#import-url").value.trim();
  if (url) return url.split(/[?#]/)[0].split(/[\/]/).pop() || "download.bin";
  return "";
}
function importAppName() {
  return $("#import-name").value.trim() ||
    importFileName().replace(/\.[^.]+$/, "").replace(/[._-]+/g, " ").trim();
}
function refreshImportDialog() {
  importFile = $("#import-file-input").files[0] || importFile;
  $("#import-filename").textContent = importFile ? importFile.name : "";
  const isExe = importFile && /\.(exe|msi)$/i.test(importFile.name);
  $("#exe-row").style.display = isExe || (!importFile && !$("#import-folder").value && !$("#import-url").value) ? "" : "";
  if (isExe && !$("#import-name").value) {
    $("#import-name").value = importFile.name.replace(/\.[^.]+$/, "").replace(/[._-]+/g, " ").trim();
  }
  $("#import-go").disabled = !importFile;
  $("#port-row").style.display = $("#import-kind").value === "web" ? "" : "none";
}
$("#import-choose").addEventListener("click", () => $("#import-file-input").click());
{
  const fi = document.createElement("input");
  fi.type = "file"; fi.id = "import-file-input"; fi.style.display = "none";
  fi.addEventListener("change", () => { importFile = fi.files[0] || null; refreshImportDialog(); if (importFile) { const w = platformMismatch(importFile.name); if (w) toast(w, "err"); } });
  $("#import-body").appendChild(fi);
}
$("#import-kind").addEventListener("change", refreshImportDialog);
$("#import-name").addEventListener("input", () => {});
const dz = $("#drop-zone");
["dragover", "dragenter"].forEach((ev) => dz.addEventListener(ev, (e) => { e.preventDefault(); dz.classList.add("over"); }));
["dragleave", "drop"].forEach((ev) => dz.addEventListener(ev, (e) => { e.preventDefault(); dz.classList.remove("over"); }));
dz.addEventListener("drop", (e) => {
  const file = e.dataTransfer.files[0];
  if (!file) return;
  importFile = file;
  refreshImportDialog();
});

// ---- import-progress dock ----
// The server streams milestone lines per import job (POST /api/import?jobid=
// … → GET /api/import/progress?id=…). watchImportJob() renders them live in
// the floating dock: auto-hide ~4s after success, stay open on failure.
const importDock = {
  id: null, timer: null, done: false, shown: 0,
  el: () => document.getElementById("import-dock"),
};
document.getElementById("import-dock-close").addEventListener("click", () => {
  const d = importDock.el(); if (d) d.hidden = true;
});
// Cancel while an import runs: the launcher kills the job's context (a
// running git clone or download dies at once) and every later step bails
// out, so the import request settles with "import canceled" quickly.
document.getElementById("import-dock-cancel").addEventListener("click", async () => {
  if (!importDock.id || importDock.done) return;
  const btn = document.getElementById("import-dock-cancel");
  btn.disabled = true;
  document.getElementById("import-dock-status").textContent = "canceling…";
  try { await api("/api/import/cancel", { id: importDock.id }); } catch { /* the import request settles on its own */ }
  // The cancel is honored locally no matter what the server answers: a job
  // the server no longer knows (it restarted mid-import) is by definition
  // not running — without this the dock used to spin on "working…" forever,
  // its cancel silently 404ing.
  importDock.done = true;
  importDockRender({ lines: [], done: true, ok: false, error: "canceled" },
    document.getElementById("import-dock-title").textContent);
});
function importDockRender(st, title) {
  const d = importDock.el(); if (!d) return;
  document.getElementById("import-dock-title").textContent = title;
  const log = document.getElementById("import-dock-log");
  // Replace the lines we have already shown; cheap for the ≤400-line cap.
  const cls = st.done ? (st.ok ? "ok" : "err") : "";
  // A WARNING milestone stays amber even in an all-green finished import —
  // e.g. an app whose code signature had to be re-signed ad hoc: installed,
  // but with a caveat the operator should know about.
  log.innerHTML = (st.lines || []).map(l => {
    let c = cls;
    if (cls !== "err" && /WARNING:/.test(l)) c = "warn";
    return `<div class="ln ${c}">${escapeHtml(l)}</div>`;
  }).join("");
  const spin = document.getElementById("import-dock-spin");
  spin.style.display = st.done ? "none" : "";
  const status = document.getElementById("import-dock-status");
  status.textContent = st.done ? (st.ok ? "done" : (st.error || "failed"))
    : (st.pendingSecurity ? "waiting for your decision…" : "working…");
  const cancel = document.getElementById("import-dock-cancel");
  cancel.hidden = st.done || st.pendingSecurity;
  cancel.disabled = false;
  // The paused security prompt: findings + the two explicit choices. The
  // buttons POST /api/security/confirm; the dock keeps polling the same
  // jobid, so "Install anyway" shows the resumed import finishing here.
  const secBox = document.getElementById("import-dock-security");
  if (st.pendingSecurity && !importDock.done) {
    document.getElementById("import-dock-findings").innerHTML = (st.findings || []).map(f =>
      `<div class="ln ${f.severity === "critical" ? "err" : "warn"}" style="white-space:normal">(${escapeHtml(f.severity)}) ${escapeHtml(f.detail)}${f.file ? ` <b>[${escapeHtml(f.file)}]</b>` : ""}</div>`).join("");
    secBox.hidden = false;
    document.getElementById("import-dock-install-anyway").onclick = async () => {
      secBox.hidden = true;
      status.textContent = "installing anyway…";
      try { await api("/api/security/confirm", { scanId: st.scanId, action: "install" }); } catch (e) { toast(`Confirm failed: ${e}`, "err"); }
    };
    document.getElementById("import-dock-discard").onclick = async () => {
      secBox.hidden = true;
      try { await api("/api/security/confirm", { scanId: st.scanId, action: "cancel" }); } catch { /* the job settles on its own */ }
    };
  } else {
    secBox.hidden = true;
  }
  d.hidden = false;
  log.scrollTop = log.scrollHeight;
}
// Starts watching a fresh job; returns the jobid to append to the request.
function watchImportJob(title) {
  const id = (crypto.randomUUID ? crypto.randomUUID() : `job-${Date.now()}-${Math.random().toString(36).slice(2)}`);
  watchImportJobById(id, title);
  return id;
}
// Watch a job someone else started: RepoLens hands its install to /api/import
// from its own port, so the dashboard never saw that response — the jobs list
// tells it, and the same dock renders the same live log.
function watchImportJobById(id, title) {
  importDock.id = id; importDock.done = false;
  importDockRender({ lines: [], done: false, ok: false }, title);
  clearInterval(importDock.timer);
  let misses = 0; // consecutive "unknown job" polls — 404s right after the
  // POST races job creation; sustained 404s mean the job is gone for good.
  importDock.timer = setInterval(async () => {
    if (importDock.id !== id || importDock.done) return clearInterval(importDock.timer);
    try {
      const res = await fetch(`/api/import/progress?id=${encodeURIComponent(id)}`);
      const st = await res.json().catch(() => ({}));
      if (res.status === 404) {
        if (++misses >= 5) {
          importDock.done = true;
          importDockRender({ lines: [], done: true, ok: false,
            error: "job not found — the launcher restarted while it ran; nothing is importing" }, title);
        }
        return;
      }
      misses = 0;
      importDockRender(st, title);
      if (st.done) {
        clearInterval(importDock.timer);
        importDock.done = true;
        if (st.ok) setTimeout(() => { if (importDock.id === id) importDock.el().hidden = true; }, 4000);
      }
    } catch { /* server busy mid-install — keep polling */ }
  }, 700);
  return id;
}

/* ---------- imports started elsewhere (RepoLens → AppDeck) ---------- */

setInterval(async () => {
  if (importDock.id && !importDock.done) return; // already watching one
  let jobs = [];
  try { jobs = (await api("/api/import/jobs")).jobs || []; } catch { return; }
  const live = jobs.filter(j => !j.done).sort((a, b) => (b.startedAt || 0) - (a.startedAt || 0));
  if (!live.length) return;
  const job = live[0];
  if (job.id === importDock.id) return;
  watchImportJobById(job.id, job.label ? `${job.label} — installing…` : "Installing…");
}, 2000);

/* ---------- downloads in the native window ---------- */

// The WKWebView download delegate calls window.__deckDownload for every
// browser-mode download (start → progress → done|error). Rendering lives here,
// so the deck owns the feedback instead of a silent beep.
const dlItems = new Map();
function dlBridge(msg) {
  try { window.webkit.messageHandlers.appdeckDownload.postMessage(msg); } catch { /* browser: no bridge */ }
}
function dlShort(n) {
  if (!n) return "";
  if (n > 1073741824) return (n / 1073741824).toFixed(1) + " GB";
  if (n > 1048576) return (n / 1048576).toFixed(1) + " MB";
  return Math.round(n / 1024) + " KB";
}
window.__deckDownload = function (ev) {
  if (!ev || !ev.id) return;
  const cur = dlItems.get(ev.id) || { id: ev.id, name: ev.name || "download", pct: 0 };
  Object.assign(cur, ev);
  dlItems.set(ev.id, cur);
  renderDownloads();
  if (ev.state === "done" || ev.state === "error") {
    setTimeout(() => { dlItems.delete(ev.id); renderDownloads(); }, 25000);
  }
};
function renderDownloads() {
  const box = document.getElementById("dl-stack");
  if (!box) return;
  const items = [...dlItems.values()];
  box.hidden = items.length === 0;
  box.innerHTML = items.map((d) => {
    const cls = d.state === "done" ? "done" : d.state === "error" ? "err" : "";
    const pct = d.state === "done" ? 100 : Math.max(0, Math.min(100, d.pct || 0));
    const running = d.state !== "done" && d.state !== "error";
    const sub = d.state === "error" ? escapeHtml(d.error || "failed")
      : d.state === "done" ? `saved · ${dlShort(d.total || d.done)}`
      : `${dlShort(d.done)}${d.total ? " / " + dlShort(d.total) : ""} · ${Math.round(pct)}%`;
    const action = d.state === "done"
      ? `<button class="btn-primary" data-dl-reveal="${escapeHtml(d.path || "")}" title="Show the file in Finder">Show in Finder</button>`
      : running ? `<button class="btn-ghost" data-dl-cancel="${d.id}" title="Stop this download">Cancel</button>` : "";
    return `<div class="dl-row ${cls}">
      <div class="dl-top">${svg("arrowDown", 13)}<span class="dl-name" title="${escapeHtml(d.name || "")}">${escapeHtml(d.name || "download")}</span>
        <span class="muted">${running ? "downloading" : d.state === "done" ? svg("check", 11) : svg("warning", 11)}</span></div>
      <div class="dl-bar"><div class="dl-fill" style="width:${pct}%"></div></div>
      <div class="dl-sub"><span>${sub}</span>${action}</div></div>`;
  }).join("");
}
document.getElementById("dl-stack").addEventListener("click", (e) => {
  const reveal = e.target.closest("[data-dl-reveal]");
  if (reveal && reveal.dataset.dlReveal) { dlBridge({ action: "reveal", path: reveal.dataset.dlReveal }); return; }
  const cancel = e.target.closest("[data-dl-cancel]");
  if (cancel) dlBridge({ action: "cancel", id: Number(cancel.dataset.dlCancel) });
});

/* ---------- download destination (Deck tools) ---------- */

async function refreshDownloadSettings() {
  let s = {};
  try { s = await api("/api/launcher-settings"); } catch { return; }
  const dir = s.downloadDir || "";
  const label = document.getElementById("menu-download-dir");
  if (label) {
    label.title = dir ? `Saved downloads go to ${dir}` : "Saved downloads go to ~/Downloads";
    const val = document.getElementById("download-dir-val");
    if (val) val.textContent = dir ? dir.replace(/^\/Users\/[^/]+/, "~") : "~/Downloads";
  }
  const ask = document.getElementById("menu-download-ask");
  if (ask) {
    ask.title = s.downloadAsk ? "Asking for a destination before every download — click to stop" : "Ask for a destination before every download";
    const val = document.getElementById("download-ask-val");
    if (val) { val.textContent = s.downloadAsk ? "On" : "Off"; val.classList.toggle("on", !!s.downloadAsk); }
  }
}
document.getElementById("menu-download-dir").addEventListener("click", async () => {
  try {
    const res = await api("/api/downloads/pick", {});
    if (res.canceled) return;
    if (!res.ok) { toast(res.error || "Could not set the folder", "err"); return; }
    toast(`Downloads now go to ${res.dir}`, "ok");
    refreshDownloadSettings();
  } catch (e) { toast(`Folder picker failed: ${e.message}`, "err"); }
});
document.getElementById("menu-download-ask").addEventListener("click", async () => {
  try {
    const cur = await api("/api/launcher-settings");
    const next = !cur.downloadAsk;
    await api("/api/launcher-settings", { downloadAsk: next });
    toast(next ? "Downloads will ask where to save" : "Downloads save straight to the folder", "ok");
    refreshDownloadSettings();
  } catch (e) { toast(`Could not change it: ${e.message}`, "err"); }
});
document.getElementById("btn-gear").addEventListener("click", () => { refreshDownloadSettings(); refreshRemovedApps(); refreshLoginItem(); refreshStartWait(); });
refreshDownloadSettings();

/* ---------- app start timeout (Deck tools) ---------- */

async function refreshStartWait() {
  let s = {};
  try { s = await api("/api/launcher-settings"); } catch { return; }
  const input = document.getElementById("menu-start-wait");
  if (input && document.activeElement !== input) {
    input.value = s.startWaitSeconds || 30;
  }
}
document.getElementById("menu-start-wait").addEventListener("change", async () => {
  const input = document.getElementById("menu-start-wait");
  const n = parseInt(input.value, 10);
  if (!Number.isFinite(n)) { refreshStartWait(); return; }
  try {
    const res = await api("/api/launcher-settings", { startWaitSeconds: n });
    if (res.ok === false) { toast(res.error || "Could not save the timeout", "err"); }
    else { toast(res.note || `App start timeout set to ${n} s`, "ok"); }
  } catch (e) { toast(`Could not change it: ${e.message}`, "err"); }
  refreshStartWait();
});

/* ---------- import limits (Deck tools) ---------- */

const LIMIT_UNITS = { KB: 1024, MB: 1024 * 1024, GB: 1024 * 1024 * 1024 };
const limitsState = { ranges: null, defaults: null, dirty: false };

// limitParts picks a display unit the value divides into cleanly (GB, then
// MB, then KB); one decimal covers everything in between.
function limitParts(bytes) {
  if (bytes >= LIMIT_UNITS.GB) {
    const gb = bytes / LIMIT_UNITS.GB;
    return { v: Number.isInteger(gb) ? gb : Math.round(gb * 10) / 10, u: "GB" };
  }
  if (bytes >= LIMIT_UNITS.MB) {
    const mb = bytes / LIMIT_UNITS.MB;
    return { v: Number.isInteger(mb) ? mb : Math.round(mb * 10) / 10, u: "MB" };
  }
  return { v: Math.ceil(bytes / LIMIT_UNITS.KB), u: "KB" };
}

function limitsRows() {
  return [
    ["importBody", "limits-body", "limits-body-unit"],
    ["expand", "limits-expand", "limits-expand-unit"],
    ["importHTML", "limits-html", "limits-html-unit"],
  ];
}

async function refreshLimits() {
  let d;
  try { d = await api("/api/limits"); } catch { return; }
  limitsState.ranges = d.ranges;
  limitsState.defaults = d.defaults;
  for (const [key, inId, selId] of limitsRows()) {
    const p = limitParts((d.current || {})[key] || 0);
    $(`#${inId}`).value = p.v;
    $(`#${selId}`).value = p.u;
  }
  limitsState.dirty = false;
  limitsError("");
  updateLimitsUI();
}

// collectLimitsBytes reads the three rows into bytes (null = absent never
// happens here — all three are always sent), or returns null after showing
// why the values are unusable.
function collectLimitsBytes() {
  const out = {};
  for (const [key, inId, selId] of limitsRows()) {
    const v = parseFloat($(`#${inId}`).value);
    if (!Number.isFinite(v) || v < 0) { limitsError("Enter a number (0 or more) in every row."); return null; }
    const bytes = Math.round(v * LIMIT_UNITS[$(`#${selId}`).value]);
    if (v > 0 && limitsState.ranges) {
      const r = limitsState.ranges[key];
      if (r && (bytes < r.min || bytes > r.max)) {
        limitsError(`That ${key === "expand" ? "expanded bundle" : key === "importHTML" ? "HTML app" : "import body"} limit must be 0 (no limit) or between ${limitParts(r.min).v} ${limitParts(r.min).u} and ${limitParts(r.max).v} ${limitParts(r.max).u}.`);
        return null;
      }
    }
    out[key] = bytes;
  }
  if (out.importBody > 0 && out.expand > 0 && out.expand < out.importBody) {
    limitsError("The expanded bundle limit cannot be below the import body limit — every archive expands to at least its own size.");
    return null;
  }
  return out;
}

function limitsError(msg) {
  const el = $("#limits-error");
  el.textContent = msg;
  el.style.display = msg ? "" : "none";
}

function updateLimitsUI() {
  const anyZero = limitsRows().some(([, inId]) => parseFloat($(`#${inId}`).value) === 0);
  $("#limits-unlimited-warn").style.display = anyZero ? "" : "none";
  $("#limits-save").disabled = !limitsState.dirty;
  $("#limits-hint").style.display = limitsState.dirty ? "" : "none";
}

$("#menu-limits").addEventListener("click", () => openSettings("limits"));
$("#limits-close").addEventListener("click", () => $("#limits-overlay").classList.remove("show"));
$("#limits-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#limits-close").click(); });
for (const [, inId, selId] of limitsRows()) {
  $(`#${inId}`).addEventListener("input", () => { limitsState.dirty = true; limitsError(""); updateLimitsUI(); });
  $(`#${selId}`).addEventListener("change", () => { limitsState.dirty = true; limitsError(""); updateLimitsUI(); });
}
$("#limits-save").addEventListener("click", async () => {
  const bytes = collectLimitsBytes();
  if (!bytes) return;
  try {
    const res = await api("/api/launcher-settings", {
      importBodyBytes: bytes.importBody,
      importExpandBytes: bytes.expand,
      importHTMLBytes: bytes.importHTML,
    });
    if (!res.ok) { limitsError(res.error || "Could not save the limits."); return; }
    toast("Import limits saved", "ok");
    refreshLimits();
  } catch (e) { limitsError(`Could not save the limits: ${e.message}`); }
});
$("#limits-reset").addEventListener("click", async () => {
  if (!limitsState.defaults) return;
  try {
    const res = await api("/api/launcher-settings", {
      importBodyBytes: limitsState.defaults.importBody,
      importExpandBytes: limitsState.defaults.expand,
      importHTMLBytes: limitsState.defaults.importHTML,
    });
    if (!res.ok) { limitsError(res.error || "Could not reset the limits."); return; }
    toast("Import limits restored to the defaults", "ok");
    refreshLimits();
  } catch (e) { limitsError(`Could not reset the limits: ${e.message}`); }
});

async function postImport(query, body) {
  // Pre-flight platform check: the file/URL name often tells the story
  // ("…-windows-amd64.zip" on a Mac). A mismatch asks before wasting the
  // upload; the server still verifies binary headers as the hard gate.
  const params = new URLSearchParams(query.replace(/^\?/, ""));
  const probeName = params.get("name") || (params.get("url") || "").split("/").filter(Boolean).pop() || "";
  if (!confirmPlatform(decodeURIComponent(probeName), probeName)) return null;
  const jobId = watchImportJob(`Importing…`);
  const sep = query.includes("?") ? "&" : "?";
  const res = await fetch(`/api/import${query}${sep}jobid=${encodeURIComponent(jobId)}`, { method: "POST", body });
  const out = await res.json();
  if (out.ok) {
    toast(`${out.name || out.id} imported — it appears on the dashboard now`, "ok");
    $("#import-overlay").classList.remove("show");
    $("#import-pkg-offer").hidden = true;
    importFile = null; $("#import-name").value = ""; $("#import-url").value = "";
    $("#import-folder").value = ""; $("#import-args").value = "";
    refreshImportDialog();
    await refresh();
  } else if (out.pendingSecurity) {
    // The dock is already showing the findings with Install anyway /
    // Cancel — no error toast, this is a pause, not a failure.
    $("#import-overlay").classList.remove("show");
  } else if (out.platformBlocked) {
    // The server's platform gate refused the import with the evidence in
    // out.error; the user can still force a source import, the same way
    // the pre-flight platform ask can be overridden.
    if (!confirm(`${out.error}\n\nImport it anyway and try from source?`)) return null;
    return postImport(`${query}${sep}force=1`, body);
  } else {
    toast(out.error || "Import failed", "err");
    maybeOfferPackage(params, out.error || "");
  }
  return out;
}

/* ---------- update-from-origin (↻ on cards that remember their import URL) ---------- */

// One app: re-run the same import the app originally arrived through. The
// server stops a running instance, swaps the folder in place, and the dock
// streams the milestones — the identical machinery as a manual re-import.
// force skips the server's platform gate, set after the user confirms it.
async function updateFromOrigin(app, wasRunning, force = false) {
  const jobId = watchImportJob(`Updating ${app.name}…`);
  try {
    const res = await fetch(`/api/import?jobid=${encodeURIComponent(jobId)}&appname=${encodeURIComponent(app.name)}&url=${encodeURIComponent(app.origin)}${force ? "&force=1" : ""}`, { method: "POST", body: "" });
    const out = await res.json();
    if (out.ok) {
      toast(`${app.name} updated from its origin`, "ok");
      await refresh();
      if (wasRunning) await act(app.id, "start");
    } else if (out.platformBlocked) {
      if (!confirm(`${out.error}\n\nImport it anyway and try from source?`)) return false;
      return updateFromOrigin(app, wasRunning, true);
    } else if (!out.pendingSecurity) {
      toast(out.error || `Update of ${app.name} failed`, "err");
    }
    return out.ok;
  } catch (e) {
    toast(`Update of ${app.name} failed: ${e}`, "err");
    return false;
  }
}

// Every app that remembers where it came from, one after another so the
// stop/update/swap steps never race each other.
async function updateAllFromOrigin() {
  const list = apps.filter(a => a.origin);
  if (!list.length) {
    toast("No apps remember an origin yet — git and URL imports do", "err");
    return;
  }
  const names = list.map(a => `  · ${a.name}`).join("\n");
  if (!confirm(`Update ${list.length} app(s) from their origins?\n\n${names}\n\nEach one is stopped, re-imported in place, and started again if it was running. Watch the progress dock.`)) return;
  let done = 0;
  for (const app of list) {
    const wasRunning = app.running;
    if (await updateFromOrigin(app, wasRunning)) done++;
  }
  toast(`Updated ${done} of ${list.length} app(s) from their origins`, done === list.length ? "ok" : "err");
}

// A source-only project the launcher cannot build (C/C++ autotools and
// friends) may still be packaged by the OS's package manager — probe by the
// project name and, when it is, offer the one-click consented install that
// ends as a ⌨ CLI app card. Nothing installs without the click.
async function maybeOfferPackage(params, errText) {  const box = $("#import-pkg-offer");
  box.hidden = true;
  box.innerHTML = "";
  if (!/cannot build C\/C\+\+|no recognizable layout/.test(errText)) return;
  const raw = params.get("appname") || params.get("name") ||
    (params.get("url") || "").split("/").filter(Boolean).pop() || "";
  const proj = decodeURIComponent(raw).replace(/\.git$/, "").replace(/[.\-](zip|tar\.gz|tgz|7z|tar\.xz)$/i, "");
  if (!proj) return;
  let res;
  try { res = await api(`/api/pkg/probe?name=${encodeURIComponent(proj)}`); }
  catch { return; /* probe failed — the plain refusal stands */ }
  if (!res.found || !res.package || !res.package.name) return;
  const p = res.package;
  box.innerHTML = `
    <hr>
    <div class="pkg-offer">
      <span><b>${escapeHtml(proj)}</b> is source AppDeck can't build — but it's packaged for this machine.</span>
      <span class="muted" style="font-size:12px">${escapeHtml(p.manager)}: <b>${escapeHtml(p.name)}</b>` +
        `${p.version ? ` ${escapeHtml(p.version)}` : ""}${p.desc ? ` — ${escapeHtml(p.desc)}` : ""}` +
        `${p.installed ? " (already installed — only the card is added)" : " — installs system-wide software; the package's version may differ from the imported tag"}.</span>
      <button class="btn-primary" id="import-pkg-go">${p.installed
        ? `Add ${escapeHtml(p.name)} as a CLI app`
        : `Install with ${escapeHtml(p.manager)} and add a CLI app`}</button>
    </div>`;
  box.hidden = false;
  $("#import-pkg-go").addEventListener("click", () => {
    const q = new URLSearchParams({ install: "pkg", pkgname: p.name, appname: proj });
    postImport(`?${q.toString().replace(/\+/g, "%20")}`);
  });
}

$("#import-go").addEventListener("click", async () => {
  if (!importFile) return;
  const q = new URLSearchParams({ name: importFile.name });
  const appname = importAppName();
  if (appname) q.set("appname", appname);
  const kindSel = $("#import-kind").value;
  if (kindSel === "web") q.set("kind", "web"), q.set("port", $("#import-port").value.trim());
  else q.set("kind", kindSel); // "auto" sniffs the binary server-side
  const args = $("#import-args").value.trim();
  if (args) q.set("args", args);
  const iconEmoji = $("#import-icon").value.trim();
  if (iconEmoji) q.set("icon", iconEmoji);
  if ($("#import-silent").checked) q.set("silent", "1");
  toast(`Importing ${importFile.name}…`);
  // URLSearchParams encodes spaces as "+" and the server keeps "+" literal
  // (file names may contain plus), so re-encode spaces as %20 — every real
  // plus already left as %2B, making the swap unambiguous.
  try { await postImport(`?${q.toString().replace(/\+/g, "%20")}`, await importFile.arrayBuffer()); }
  catch (e) { toast(`Import failed: ${e}`, "err"); }
});

$("#import-folder-go").addEventListener("click", async () => {
  const folder = $("#import-folder").value.trim();
  if (!folder) return;
  toast(`Importing folder ${folder}…`);
  try { await postImport(`?folder=${encodeURIComponent(folder)}`); }
  catch (e) { toast(`Import failed: ${e}`, "err"); }
});

$("#import-url-go").addEventListener("click", async () => {
  const url = $("#import-url").value.trim();
  if (!url) return;
  toast("Downloading…");
  // The no-progress watchdog: cancel automatically when nothing logs for
  // this long (0 = no limit — for repos/sources that legitimately stall).
  try { await postImport(`?url=${encodeURIComponent(url)}&stall=${encodeURIComponent($("#import-stall").value)}`); }
  catch (e) { toast(`Import failed: ${e}`, "err"); }
});


// ---- theme packs (🎨 header button + manager) ----
// Picking a pack previews it live: the pack's stylesheet (delivered with
// the pack list) rewrites the injected <style data-theme-pack> slot, so
// the page never reloads. Apply persists the choice; Cancel reverts.
let themePacks = [];
let themeApplied = "";  // pack the launcher injected at page load
let themePending = "";  // pack currently previewed
const themeSlot = () => {
  let el = document.querySelector("style[data-theme-pack]");
  if (!el) {
    el = document.createElement("style");
    el.setAttribute("data-theme-pack", "");
    document.head.appendChild(el);
  }
  return el;
};
function themeSelectHtml(packs) {
  let opts = `<option value="">Base dashboard (built-in)</option>`;
  for (const t of packs) opts += `<option value="${escapeHtml(t.name)}" ${t.applied ? "selected" : ""}>${escapeHtml(t.name)}${t.hasIcons ? " · has icons" : ""}</option>`;
  return opts;
}
function themeRefreshUi() {
  $("#theme-pick").value = themePending;
  $("#theme-pack-list").querySelectorAll("[data-theme-preview]").forEach(b =>
    b.classList.toggle("btn-primary", b.dataset.themePreview === themePending));
  const dirty = themePending !== themeApplied;
  $("#theme-apply").disabled = !dirty;
  $("#theme-cancel").disabled = !dirty;
  $("#theme-preview-hint").style.display = dirty ? "" : "none";
}
function themePreview(name) {
  themePending = name;
  themeSlot().textContent = (themePacks.find((p) => p.name === name) || {}).css || "";
  themeRefreshUi();
}
async function loadThemePacks() {
  const sel = $("#theme-pick");
  const list = $("#theme-pack-list");
  try {
    themePacks = await api("/api/themes");
    themeApplied = (themePacks.find((p) => p.applied) || {}).name || "";
    sel.innerHTML = themeSelectHtml(themePacks);
    if (themePacks.length) {
      // Button strip: preview on click, 🗑 for imported packs. A pack that
      // ships preview.png/svg shows it as a thumbnail (/api/themes/preview).
      list.innerHTML = `<div style="display:flex;flex-wrap:wrap;gap:8px">` + themePacks.map(t =>
        `<span style="display:inline-flex;gap:6px;align-items:center">` +
        `<button class="btn ${t.applied ? "btn-primary" : "ghost"}" data-theme-preview="${escapeHtml(t.name)}" title="Preview ${escapeHtml(t.name)}${t.hasIcons ? " (pack also has app icons — those land on Apply)" : ""}">${t.hasPreview ? `<img class="theme-thumb" src="/api/themes/preview/${encodeURIComponent(t.name)}" alt="">` : ""}${t.applied ? "● " : ""}${escapeHtml(t.name)}</button>` +
        (t.builtin ? "" : `<button class="btn ghost" data-theme-delete="${escapeHtml(t.name)}" title="Delete ${escapeHtml(t.name)}" aria-label="Delete">${svg("trash", 13)}</button>`) +
        `</span>`).join("") + `</div>`;
    } else {
      list.innerHTML = `<p class="muted">No custom packs yet — import one below.</p>`;
    }
    list.querySelectorAll("[data-theme-preview]").forEach(b => b.addEventListener("click", () => themePreview(b.dataset.themePreview)));
    list.querySelectorAll("[data-theme-delete]").forEach(b => b.addEventListener("click", async () => {
      if (!confirm(`Delete theme "${b.dataset.themeDelete}"?`)) return;
      const res = await api("/api/themes/delete", { name: b.dataset.themeDelete });
      toast(res.note || res.error || "removed", res.ok ? "ok" : "err");
      if (res.ok) await loadThemePacks(); // resets the preview to the applied state
    }));
    themePreview(themeApplied);
  } catch (e) {
    list.innerHTML = `<p class="muted">Could not load themes: ${e}</p>`;
  }
}
$("#theme-pick").addEventListener("change", (e) => themePreview(e.target.value));
$("#theme-apply").addEventListener("click", async () => {
  try {
    const res = await api("/api/themes/apply", { name: themePending });
    if (!res.ok) { toast(res.error || "apply failed", "err"); return; }
    themeApplied = themePending;
    themeRefreshUi();
    toast(res.note || "theme applied", "ok");
    $("#theme-close").click(); // done — back to the deck; 🎨 reopens any time
  } catch (e) { toast(`Apply failed: ${e}`, "err"); }
});
$("#theme-cancel").addEventListener("click", () => {
  themePreview(themeApplied); // revert the live preview, then leave
  $("#theme-close").click();
});
$("#btn-themes").addEventListener("click", () => openSettings("appearance"));
$("#theme-close").addEventListener("click", () => $("#theme-overlay").classList.remove("show"));
$("#theme-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#theme-close").click(); });
$("#theme-git-go").addEventListener("click", async () => {
  const url = $("#theme-git").value.trim();
  if (!url) return;
  toast("Cloning theme…");
  try {
    const res = await api(`/api/themes/import?url=${encodeURIComponent(url)}`);
    const note = $("#theme-note");
    note.style.display = ""; note.textContent = res.note || res.error || "";
    toast(res.note || res.error || "imported", res.ok ? "ok" : "err");
    if (res.ok) { $("#theme-git").value = ""; await loadThemePacks(); }
  } catch (e) { toast(`Import failed: ${e}`, "err"); }
});
{
  let themeFile = null;
  const fi = document.createElement("input");
  fi.type = "file"; fi.accept = ".zip"; fi.style.display = "none";
  fi.addEventListener("change", () => {
    themeFile = fi.files[0] || null;
    $("#theme-filename").textContent = themeFile ? themeFile.name : "";
    $("#theme-zip-go").disabled = !themeFile;
  });
  $("#theme-drop").appendChild(fi);
  $("#theme-choose").addEventListener("click", () => fi.click());
  const tdz = $("#theme-drop");
  ["dragover", "dragenter"].forEach((ev) => tdz.addEventListener(ev, (e) => { e.preventDefault(); tdz.classList.add("over"); }));
  ["dragleave", "drop"].forEach((ev) => tdz.addEventListener(ev, (e) => { e.preventDefault(); tdz.classList.remove("over"); }));
  tdz.addEventListener("drop", (e) => {
    const f = e.dataTransfer.files[0];
    if (!f) return;
    themeFile = f;
    $("#theme-filename").textContent = f.name;
    $("#theme-zip-go").disabled = false;
  });
  $("#theme-zip-go").addEventListener("click", async () => {
    if (!themeFile) return;
    toast("Importing theme zip…");
    try {
      const res = await fetch(`/api/themes/import?name=${encodeURIComponent(themeFile.name.replace(/\.zip$/i, ""))}`, { method: "POST", body: await themeFile.arrayBuffer() });
      const out = await res.json();
      const note = $("#theme-note");
      note.style.display = ""; note.textContent = out.note || out.error || "";
      toast(out.note || out.error || "imported", out.ok ? "ok" : "err");
      if (out.ok) { themeFile = null; $("#theme-filename").textContent = ""; $("#theme-zip-go").disabled = true; await loadThemePacks(); }
    } catch (e) { toast(`Import failed: ${e}`, "err"); }
  });
}

// ---- accent color (🎨 panel): an operator tint over any theme ----
// Inline on <html>, so it beats both the base stylesheet and the applied
// pack regardless of load order; stored per browser like the theme mode.
// --accent-2 is the darker companion most gradients pair --accent with.
function hexToRgb(h) {
  const n = parseInt(h.slice(1), 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}
function shadeAccent(hex, f) {
  const [r, g, b] = hexToRgb(hex);
  const dark = (v) => Math.round(v * (1 - f));
  return "#" + [r, g, b].map((v) => dark(v).toString(16).padStart(2, "0")).join("");
}
function applyAccent(hex) {
  const root = document.documentElement;
  if (!hex) {
    root.style.removeProperty("--accent");
    root.style.removeProperty("--accent-2");
    root.style.removeProperty("--accent-rgb");
    renderAccentDots();
    return;
  }
  root.style.setProperty("--accent", hex);
  root.style.setProperty("--accent-2", shadeAccent(hex, 0.15));
  const rgb = hexToRgbTriplet(hex);
  if (rgb) root.style.setProperty("--accent-rgb", rgb);
  // A custom color replaces the named preset.
  root.removeAttribute("data-accent");
  renderAccentDots();
}
$("#accent-pick").addEventListener("input", (e) => applyAccent(e.target.value));
$("#accent-pick").addEventListener("change", (e) => {
  try {
    localStorage.setItem("launcher.accent", e.target.value);
    localStorage.removeItem("launcher.accentPreset");
  } catch {}
  toast("Accent color saved");
});
$("#accent-reset").addEventListener("click", () => {
  applyAccent("");
  try {
    localStorage.removeItem("launcher.accent");
    const p = localStorage.getItem("launcher.accentPreset");
    if (p && p !== "prism") document.documentElement.dataset.accent = p;
  } catch {}
  toast("Accent color reset — the theme's own is back");
});
try {
  const savedAccent = localStorage.getItem("launcher.accent");
  if (savedAccent && /^#[0-9a-f]{6}$/i.test(savedAccent)) {
    applyAccent(savedAccent);
    $("#accent-pick").value = savedAccent;
  } else {
    const p = localStorage.getItem("launcher.accentPreset");
    if (p && p !== "prism") document.documentElement.dataset.accent = p;
  }
} catch {}

// ---- ⌘K quick switcher: keyboard-only start/open/logs ----
// ---- permission prompt (🔑) -----------------------------------------------------
// A card that declares permissions refuses its first start with
// {pending: …}; this dialog collects the verdict (mobile-style: allow once,
// always, or not now) and the caller retries the start (permissions.go).

const permLabels = {
  "network": "Talk to the internet (outbound requests)",
  "lan": "Reach other machines on this network",
  "fs-read": "Read files outside its own data folder",
  "fs-write": "Write files outside its own data folder",
  "exec": "Run other programs / shell commands",
};
let permRetry = null;
let permAppId = null;

function openPermPrompt(pending, onAllow) {
  permRetry = onAllow || null;
  permAppId = pending.id;
  $("#perm-app-name").textContent = pending.name || pending.id;
  $("#perm-app-name2").textContent = pending.name || pending.id;
  const list = $("#perm-list");
  list.innerHTML = (pending.needed || []).map(p => `
    <label style="display:flex;gap:8px;align-items:flex-start;padding:6px 0">
      <input type="checkbox" data-perm="${escapeHtml(p)}" checked style="margin-top:3px">
      <span><b>${escapeHtml(p)}</b> — ${escapeHtml(permLabels[p] || "Declared access")}</span>
    </label>`).join("");
  $("#perm-foot").textContent = pending.declared && pending.declared.length > pending.needed.length
    ? `Already granted: ${pending.declared.filter(p => !pending.needed.includes(p)).join(", ")}`
    : "Unchecked entries stay ungranted — the start refuses until they are allowed.";
  $("#perm-overlay").classList.add("show");
}

async function decidePerms(mode) {
  const allow = [...$("#perm-list").querySelectorAll("input[data-perm]:checked")].map(c => c.dataset.perm);
  if (mode !== "deny" && !allow.length) {
    toast("nothing selected — the start stays refused", "err");
    return;
  }
  try {
    if (allow.length) {
      const res = await api("/api/permissions", { id: permAppId, allow, mode });
      if (res.error) { toast(res.error, "err"); return; }
    }
  } catch (e) {
    toast(`Could not record the verdict: ${e}`, "err");
    return;
  }
  $("#perm-overlay").classList.remove("show");
  if (permRetry) {
    const retry = permRetry;
    permRetry = null;
    await retry();
  }
}

// ---- featured gallery (✨) -------------------------------------------------------
// Curated one-click store (featured.go): templates scaffolded locally,
// catalog cards installed through the ordinary import flow. Every entry's
// `requires` chips carry this machine's live tool status, so a card never
// installs half-working.

const featuredReqLabels = {
  go: "Go", rust: "Rust", git: "git", python: "Python", node: "Node.js",
  docker: "Docker", nginx: "nginx",
};

async function loadFeatured() {
  try {
    const data = await api("/api/featured");
    renderFeatured(data);
  } catch (e) {
    $("#featured-cards").innerHTML = "";
    $("#featured-note").hidden = false;
    $("#featured-note").textContent = `Could not load the gallery: ${e}`;
  }
}

async function openFeatured() {
  closeSettings();
  openPage("featured-overlay");
  await loadFeatured();
}

function featuredReqChips(requires, tools) {
  if (!requires || !requires.length) return "";
  return `<div class="fc-reqs">` + requires.map(r => {
    const ok = tools && tools[r] && tools[r].installed;
    const ver = ok && tools[r].version ? ` (${escapeHtml(tools[r].version)})` : "";
    return `<span class="fc-req ${ok ? "ok" : "miss"}" title="${ok ? "present on this machine" : "missing on this machine — see Prerequisites"}">${ok ? svg("check", 10) : svg("close", 10)} ${escapeHtml(featuredReqLabels[r] || r)}${ver}</span>`;
  }).join("") + `</div>`;
}

function renderFeatured(data) {
  const note = $("#featured-note");
  note.hidden = !data.catalogNote;
  note.textContent = data.catalogNote || "";
  // Templates: create with a name prompt.
  $("#featured-templates").innerHTML = (data.templates || []).map(c => `
    <div class="featured-card">
      <div class="fc-head"><span class="fc-icon">${escapeHtml(c.icon || "📦")}</span><span class="fc-name">${escapeHtml(c.name)}</span></div>
      <div class="fc-desc">${escapeHtml(c.desc)}</div>
      <div class="fc-actions"><button class="btn-primary" data-feat="template" data-template="${escapeHtml(c.template)}" data-name="${escapeHtml(c.name)}">Create…</button></div>
    </div>`).join("");
  // Catalog cards: install via import, or mark installed.
  const cards = data.cards || [];
  $("#featured-empty").hidden = cards.length > 0;
  $("#featured-cards").innerHTML = cards.map(c => {
    const installed = data.installed && data.installed[c.id];
    const missing = (c.requires || []).filter(r => !(data.tools && data.tools[r] && data.tools[r].installed));
    const blocked = missing.length > 0;
    return `
    <div class="featured-card">
      ${c.shot ? `<img class="fc-shot" loading="lazy" src="${escapeHtml(c.shot)}" alt="" onerror="this.remove()">` : ""}
      <div class="fc-head"><span class="fc-icon">${escapeHtml(c.icon || "📦")}</span><span class="fc-name">${escapeHtml(c.name)}</span>${c.version ? `<span class="badge">${escapeHtml(c.version)}</span>` : ""}</div>
      <div class="fc-desc">${escapeHtml(c.desc)}</div>
      ${featuredReqChips(c.requires, data.tools)}
      <div class="fc-actions">
        ${installed
          ? `<span class="badge auto" title="This card is already on the deck">${svg("check", 10)} Installed</span>`
          : blocked
            ? `<button class="btn-ghost" disabled title="Missing: ${escapeHtml(missing.map(r => featuredReqLabels[r] || r).join(", "))} — install the tools from Prerequisites first">Install</button>`
            : `<button class="btn-primary" data-feat="install" data-id="${escapeHtml(c.id)}" data-name="${escapeHtml(c.name)}" data-source="${escapeHtml(c.source)}">Install</button>`}
      </div>
    </div>`;
  }).join("");
}

document.body.addEventListener("click", async (e) => {
  const btn = e.target.closest("button[data-feat]");
  if (!btn) return;
  const kind = btn.dataset.feat;
  if (kind === "template") {
    const name = prompt(`Name for the new card (from the ${btn.dataset.name} template):`, btn.dataset.name);
    if (!name || !name.trim()) return;
    try {
      const res = await api("/api/featured/template", { name: name.trim(), template: btn.dataset.template });
      if (res.error) { toast(res.error, "err"); return; }
      toast(`${res.name} created — it appears on the deck now`, "ok");
      closePage();
      await refresh();
    } catch (err) {
      toast(`Could not scaffold: ${err}`, "err");
    }
  } else if (kind === "install") {
    const id = btn.dataset.id, name = btn.dataset.name, source = btn.dataset.source;
    if (!source) { toast("this entry has no installable source", "err"); return; }
    const jobId = watchImportJob(`Installing ${name}…`);
    try {
      const res = await fetch(`/api/import?jobid=${encodeURIComponent(jobId)}&appname=${encodeURIComponent(name)}&url=${encodeURIComponent(source)}`, { method: "POST", body: "" });
      const out = await res.json();
      if (out.ok) {
        toast(`${name} installed`, "ok");
        closePage();
        await refresh();
      } else if (!out.pendingSecurity) {
        toast(out.error || `Install of ${name} failed`, "err");
      }
    } catch (err) {
      toast(`Install of ${name} failed: ${err}`, "err");
    }
  }
});

$("#btn-featured").addEventListener("click", openFeatured);
$("#featured-close").addEventListener("click", () => {
  closePage();
  try { localStorage.setItem("featuredSeen", "1"); } catch (_) {}
});

// Guided onboarding: a brand-new deck (no cards at all) opens the gallery
// once — the fastest path from "empty dashboard" to a working card.
function maybeOnboardFeatured() {
  if (apps.length === 0 && !$("#featured-overlay").classList.contains("show")) {
    let seen = false;
    try { seen = !!localStorage.getItem("featuredSeen"); } catch (_) {}
    if (!seen) {
      try { localStorage.setItem("featuredSeen", "1"); } catch (_) {}
      openFeatured();
    }
  }
}

$("#perm-once").addEventListener("click", () => decidePerms("once"));
$("#perm-always").addEventListener("click", () => decidePerms("always"));
$("#perm-deny").addEventListener("click", () => {
  $("#perm-overlay").classList.remove("show");
  permRetry = null;
  toast("start refused — the card's permissions stay ungranted", "err");
});
$("#perm-close").addEventListener("click", () => {
  $("#perm-overlay").classList.remove("show");
  permRetry = null;
});

// ---- lifecycle (⏱): per-card policy overrides ---------------------------------
// The card's effective policy arrives on /api/apps (a.policy); the dialog
// edits the operator override (POST /api/lifecycle) — empty fields inherit
// the manifest, "Back to the manifest" removes the override entirely. The
// enforcement loop picks the change up on its next 30 s tick.

let lifecycleAppId = null;

function openLifecycle(app) {
  lifecycleAppId = app.id;
  $("#lifecycle-app-name").textContent = app.name;
  const p = app.policy || {};
  $("#lifecycle-mode").value = p.mode || "";
  $("#lifecycle-idle").value = p.source === "override" && p.stopAfterIdle ? p.stopAfterIdle : "";
  $("#lifecycle-schedule").value = p.schedule || "";
  syncLifecycleRows(p);
  $("#lifecycle-overlay").classList.add("show");
}

function syncLifecycleRows(p) {
  const mode = $("#lifecycle-mode").value;
  $("#lifecycle-idle-row").hidden = mode !== "onDemand";
  $("#lifecycle-schedule-row").hidden = mode !== "scheduled";
  const note = $("#lifecycle-note");
  if (mode === "onDemand") {
    note.hidden = false;
    note.textContent = "Client presence = an open TCP connection on the app's port. An open browser tab keeps it busy; a closed one goes idle and the app stops after the budget. Static pages with no live connection can go idle immediately.";
  } else if (mode === "scheduled") {
    note.hidden = false;
    note.textContent = "Weekdays are Mon Tue Wed Thu Fri Sat Sun (ranges like Mon-Fri, lists like Sat,Sun). The window can cross midnight (22:00-02:00). Inside the window the app starts; outside it stops.";
  } else if (mode === "always") {
    note.hidden = false;
    note.textContent = "Starts when AppDeck starts and restarts when the process crashes — up to the manifest's keepAliveLimit (default 5) so a crash loop stays visible. A Stop you click stays stopped until the next window/app start.";
  } else {
    note.hidden = true;
    note.textContent = "";
  }
  if (p && p.source === "override") {
    note.hidden = false;
    note.textContent += (note.textContent ? " " : "") + "An operator override is active.";
  }
}

$("#lifecycle-mode").addEventListener("change", () => {
  const app = apps.find(a => a.id === lifecycleAppId);
  syncLifecycleRows(app && app.policy || {});
});

async function saveLifecycle(clear) {
  if (!lifecycleAppId) return;
  const body = { id: lifecycleAppId };
  if (!clear) {
    body.mode = $("#lifecycle-mode").value;
    const idle = parseInt($("#lifecycle-idle").value, 10);
    if (Number.isInteger(idle)) body.stopAfterIdle = idle;
    body.schedule = $("#lifecycle-schedule").value.trim();
  } else {
    body.clear = true;
  }
  try {
    const res = await api("/api/lifecycle", body);
    if (res.error) { toast(res.error, "err"); return; }
    toast(clear ? "back to the manifest's policy" : "lifecycle saved — the deck applies it within 30 s", "ok");
    $("#lifecycle-overlay").classList.remove("show");
    await refresh();
  } catch (e) {
    toast(`Could not save the policy: ${e}`, "err");
  }
}

$("#lifecycle-save").addEventListener("click", () => saveLifecycle(false));
$("#lifecycle-clear").addEventListener("click", () => saveLifecycle(true));
$("#lifecycle-close").addEventListener("click", () => $("#lifecycle-overlay").classList.remove("show"));

// A palette over the live app list; fuzzy subsequence match on name (then
// category), Enter fires the primary action — start when stopped, open (or
// Show for desktop apps) when running, ⌥↵ always opens the log.
function fuzzyScore(q, text) {
  q = q.toLowerCase();
  const t = text.toLowerCase();
  if (!q) return 1;
  let qi = 0, score = 0, streak = 0;
  for (const ch of t) {
    if (qi < q.length && ch === q[qi]) { qi++; streak++; score += 2 + streak; }
    else streak = 0;
  }
  return qi === q.length ? score : 0;
}
let paletteRows = [], paletteSel = 0;
function paletteRender() {
  const q = $("#palette-input").value.trim();
  const scored = apps
    .map((a) => ({ a, s: Math.max(fuzzyScore(q, a.name), fuzzyScore(q, a.category || "") * 0.5) }))
    .filter((x) => x.s > 0)
    .sort((x, y) => y.s - x.s || byNameCmp(x.a, y.a));
  paletteRows = scored.map((x) => x.a).slice(0, 12);
  paletteSel = 0;
  $("#palette-list").innerHTML = paletteRows.length
    ? paletteRows.map((a, i) => `
      <div class="pal-row${i === paletteSel ? " sel" : ""}" data-pal="${a.id}">
        <span class="app-icon">${a.iconFile ? `<img src="${iconQuery(a.id)}" alt="">` : escapeHtml(a.icon)}</span>
        <span class="pal-name">${escapeHtml(a.name)}</span>
        <span class="pal-hint">${a.running ? (a.desktop && !a.cli ? "show" : "open") : "start"}</span>
      </div>`).join("")
    : `<p class="muted" style="padding:8px 10px">no apps match — esc to close</p>`;
}
function paletteSyncSel() {
  $("#palette-list").querySelectorAll(".pal-row").forEach((r, i) => r.classList.toggle("sel", i === paletteSel));
  const sel = $("#palette-list .pal-row.sel");
  if (sel) sel.scrollIntoView({ block: "nearest" });
}
async function paletteGo(app, logs) {
  closePalette();
  if (logs) return void openLogs(app);
  if (app.desktop) {
    if (app.cli) return void document.querySelector(`#card-${app.id} [data-act="terminal"]`)?.click();
    if (app.running) {
      try {
        const res = await api("/api/show", { id: app.id });
        if (res.error) toast(`${app.name}: ${res.error}`, "err");
      } catch (e) { toast(`Could not focus ${app.name}: ${e}`, "err"); }
      return;
    }
    return void act(app.id, "start");
  }
  if (app.running) return void openApp(app);
  return void act(app.id, "start");
}
function openPalette() {
  $("#palette-overlay").classList.add("show");
  const input = $("#palette-input");
  input.value = "";
  paletteRender();
  input.focus();
}
function closePalette() { $("#palette-overlay").classList.remove("show"); }
const byNameCmp = (a, b) => String(a.name).localeCompare(String(b.name), undefined, { sensitivity: "base" });
$("#palette-input").addEventListener("input", paletteRender);
$("#palette-input").addEventListener("keydown", (e) => {
  if (e.key === "ArrowDown") { e.preventDefault(); paletteSel = Math.min(paletteSel + 1, paletteRows.length - 1); paletteSyncSel(); }
  else if (e.key === "ArrowUp") { e.preventDefault(); paletteSel = Math.max(paletteSel - 1, 0); paletteSyncSel(); }
  else if (e.key === "Enter") {
    e.preventDefault();
    const app = paletteRows[paletteSel];
    if (app) paletteGo(app, e.altKey);
  } else if (e.key === "Escape") closePalette();
});
$("#palette-list").addEventListener("click", (e) => {
  const row = e.target.closest("[data-pal]");
  if (row) { const app = apps.find((a) => a.id === row.dataset.pal); if (app) paletteGo(app, e.altKey); }
});
$("#palette-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) closePalette(); });

// ---- backgrounds (🖼 header button: packs + brightness/opacity/colors) ----
// Everything previews live: the pack's stylesheet (delivered with the pack
// list) rewrites the injected <style data-background-pack> slot and the
// knobs ride on CSS variables — the page never reloads. Apply persists
// (settings choice in one go); Cancel reverts to the last applied state.
function backgroundSelectHtml(packs) {
  let opts = `<option value="">None — flat theme background</option>`;
  for (const b of packs) opts += `<option value="${escapeHtml(b.name)}" ${b.applied ? "selected" : ""}>${escapeHtml(b.name)}</option>`;
  return opts;
}
let bgPacks = [];
let bgApplied = "";  // pack the launcher injected at page load
let bgPending = "";  // pack currently previewed
let bgSaved = null;  // last applied knob values
let bgConf = { brightnessDark: 100, brightnessLight: 100, opacity: 100, transparency: 0, color1: "", color2: "" };
const bgKeys = ["brightnessDark", "brightnessLight", "opacity", "transparency", "color1", "color2"];
const bgSlot = () => {
  let el = document.querySelector("style[data-background-pack]");
  if (!el) {
    el = document.createElement("style");
    el.setAttribute("data-background-pack", "");
    document.head.appendChild(el);
  }
  return el;
};
function bgApplyLive() {
  const rs = document.documentElement.style;
  rs.setProperty("--bg-opacity", bgConf.opacity / 100);
  rs.setProperty("--bg-brightness-dark", bgConf.brightnessDark / 100);
  rs.setProperty("--bg-brightness-light", bgConf.brightnessLight / 100);
  // Inline style beats the pack stylesheet — custom colors override packs.
  $("#bg-layer").style.background = (bgConf.color1 && bgConf.color2)
    ? `linear-gradient(135deg, ${bgConf.color1}, ${bgConf.color2})` : "";
}
function bgDirty() {
  if (bgPending !== bgApplied) return true;
  if (!bgSaved) return false;
  return bgKeys.some((k) => bgConf[k] !== bgSaved[k]);
}
let bgApplying = false;
function bgRefreshButtons() {
  const dirty = bgDirty();
  $("#background-apply").disabled = !dirty || bgApplying;
  $("#background-cancel").disabled = !dirty || bgApplying;
  $("#background-preview-hint").style.display = dirty ? "" : "none";
  document.querySelectorAll("#background-pack-list [data-bg-preview]").forEach(button => {
    const selected = button.dataset.bgPreview === bgPending;
    button.classList.toggle("btn-primary", selected);
    button.classList.toggle("ghost", !selected);
    button.setAttribute("aria-pressed", String(selected));
    button.textContent = `${selected ? "✓ " : ""}${button.dataset.bgPreview}`;
  });
}
function bgPreviewPack() {
  bgSlot().textContent = (bgPacks.find((p) => p.name === bgPending) || {}).css || "";
}
// One brightness bar for both modes: it edits whichever mode the 🌙/☀️
// toggle is in right now — the label says which.
function bgModeKey() {
  return document.documentElement.dataset.theme === "light" ? "brightnessLight" : "brightnessDark";
}
function bgSyncMode() {
  const key = bgModeKey();
  $("#bg-br-mode").textContent = key === "brightnessLight" ? "light mode" : "dark mode";
  $("#bg-br").value = bgConf[key];
  $("#bg-br-val").textContent = bgConf[key] + "%";
  bgSliderFill($("#bg-br"));
}
document.addEventListener("themechange", bgSyncMode);
function bgConfUi() {
  bgSyncMode();
  $("#bg-op").value = bgConf.opacity;
  $("#bg-op-val").textContent = bgConf.opacity + "%";
  bgSliderFill($("#bg-op"));
  $("#bg-tr").value = bgConf.transparency || 0;
  $("#bg-tr-val").textContent = (bgConf.transparency || 0) + "%";
  bgSliderFill($("#bg-tr"));
  const trOff = !bgWindowBridge;
  $("#bg-tr-row").classList.toggle("bg-knob-off", trOff);
  $("#bg-tr").disabled = trOff;
  if (trOff) $("#bg-tr-hint").textContent = "(native app window only — a browser window cannot go transparent)";
  $("#background-c1").value = bgConf.color1 || "#2a1660";
  $("#background-c2").value = bgConf.color2 || "#a12d64";
  $("#background-colors-off").style.display = (bgConf.color1 && bgConf.color2) ? "" : "none";
}
async function loadBackgrounds() {
  const sel = $("#background-pick");
  const list = $("#background-pack-list");
  try {
    const res = await api("/api/backgrounds");
    bgPacks = res.packs || [];
    if (res.settings) bgConf = { ...bgConf, ...res.settings };
    bgApplied = (bgPacks.find((p) => p.applied) || {}).name || "";
    bgPending = bgApplied;
    bgSaved = { ...bgConf };
    sel.innerHTML = backgroundSelectHtml(bgPacks);
    if (bgPacks.length) {
      // Button strip: preview on click, 🗑 for imported packs.
      list.innerHTML = `<div style="display:flex;flex-wrap:wrap;gap:8px">` + bgPacks.map(b =>
        `<span style="display:inline-flex;gap:6px;align-items:center">` +
        `<button class="btn ${b.applied ? "btn-primary" : "ghost"}" data-bg-preview="${escapeHtml(b.name)}" aria-pressed="${b.applied}" title="Preview ${escapeHtml(b.name)}">${b.applied ? "✓ " : ""}${escapeHtml(b.name)}</button>` +
        (b.builtin ? "" : `<button class="btn ghost" data-bg-delete="${escapeHtml(b.name)}" title="Delete ${escapeHtml(b.name)}" aria-label="Delete">${svg("trash", 13)}</button>`) +
        `</span>`).join("") + `</div>`;
    } else {
      list.innerHTML = `<p class="muted">No background packs installed.</p>`;
    }
    list.querySelectorAll("[data-bg-preview]").forEach(b => b.addEventListener("click", () => {
      bgPending = b.dataset.bgPreview;
      $("#background-pick").value = bgPending;
      bgPreviewPack(); bgRefreshButtons();
    }));
    list.querySelectorAll("[data-bg-delete]").forEach(b => b.addEventListener("click", async () => {
      if (!confirm(`Delete background "${b.dataset.bgDelete}"?`)) return;
      const res = await api("/api/backgrounds/delete", { name: b.dataset.bgDelete });
      toast(res.note || res.error || "removed", res.ok ? "ok" : "err");
      if (res.ok) await loadBackgrounds(); // resets the preview to the applied state
    }));
    bgPreviewPack(); bgConfUi(); bgRefreshButtons();
  } catch (e) {
    list.innerHTML = `<p class="muted">Could not load backgrounds: ${e}</p>`;
  }
}
$("#background-pick").addEventListener("change", (e) => {
  bgPending = e.target.value;
  bgPreviewPack(); bgRefreshButtons();
});
// Sliders: fill the accent track up to the thumb, and while one is dragged
// mark #bg-layer .bg-live (transition: none) so the preview tracks the
// thumb 1:1 — no easing lag. Release (change) restores the ease.
function bgSliderFill(el) {
  el.style.setProperty("--fill", ((el.value - el.min) / (el.max - el.min) * 100) + "%");
}
function bgSliderWire(el) {
  el.addEventListener("pointerdown", () => $("#bg-layer").classList.add("bg-live"));
  el.addEventListener("input", () => $("#bg-layer").classList.add("bg-live"));
  el.addEventListener("change", () => $("#bg-layer").classList.remove("bg-live"));
  bgSliderFill(el);
}
$("#bg-br").addEventListener("input", (e) => {
  bgConf[bgModeKey()] = Number(e.target.value);
  $("#bg-br-val").textContent = e.target.value + "%";
  bgSliderFill(e.target); bgApplyLive(); bgRefreshButtons();
});
$("#bg-op").addEventListener("input", (e) => {
  bgConf.opacity = Number(e.target.value);
  $("#bg-op-val").textContent = e.target.value + "%";
  bgSliderFill(e.target); bgApplyLive(); bgRefreshButtons();
});
// Window transparency rides a native bridge: the WKWebView script message
// handler on macOS, a webview2 binding on Windows. The slider pushes every
// drag step through it; browser and --app windows have no such bridge and
// the knob renders disabled (bgConfUi). Declared here, after bgConfUi's
// definition but before first use at dialog-open time.
const bgWindowBridge = (() => {
  try {
    const h = window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.appdeckTransparency;
    if (h) return (pct) => h.postMessage(pct);
  } catch {}
  try {
    if (typeof window.appdeckSetTransparency === "function")
      return (pct) => { try { window.appdeckSetTransparency(pct); } catch {} };
  } catch {}
  return null;
})();
$("#bg-tr").addEventListener("input", (e) => {
  bgConf.transparency = Number(e.target.value);
  $("#bg-tr-val").textContent = e.target.value + "%";
  bgSliderFill(e.target);
  if (bgWindowBridge) bgWindowBridge(bgConf.transparency);
  bgRefreshButtons();
});
bgSliderWire($("#bg-br"));
bgSliderWire($("#bg-op"));
bgSliderWire($("#bg-tr"));
function bgColorsChanged() {
  bgConf.color1 = $("#background-c1").value;
  bgConf.color2 = $("#background-c2").value;
  $("#background-colors-off").style.display = "";
  bgApplyLive(); bgRefreshButtons();
}
$("#background-c1").addEventListener("input", bgColorsChanged);
$("#background-c2").addEventListener("input", bgColorsChanged);
$("#background-colors-off").addEventListener("click", () => {
  bgConf.color1 = ""; bgConf.color2 = "";
  bgConfUi(); bgApplyLive(); bgRefreshButtons();
});
$("#background-apply").addEventListener("click", async () => {
  if (bgApplying) return;
  bgApplying = true;
  bgRefreshButtons();
  try {
    const res = await api("/api/backgrounds/apply", { name: bgPending, settings: bgConf });
    if (!res.ok) { toast(res.error || "Background could not be applied", "err"); return; }
    bgApplied = bgPending;
    bgSaved = { ...bgConf };
    toast(res.note || (bgApplied ? "Background applied" : "Background settings saved"), "ok");
    bgRefreshButtons();
    if (document.getElementById("settings-overlay").classList.contains("show") && settingsSection === "backgrounds") closeSettings();
    else $("#background-close").click();
  } catch (e) { toast(`Apply failed: ${e}`, "err"); }
  finally { bgApplying = false; bgRefreshButtons(); }
});
$("#background-cancel").addEventListener("click", () => {
  if (bgSaved) bgConf = { ...bgSaved };
  bgPending = bgApplied;
  bgApplyLive(); bgPreviewPack(); bgConfUi(); bgRefreshButtons();
  if (bgWindowBridge) bgWindowBridge(bgConf.transparency || 0); // revert the live window blend too
  if (document.getElementById("settings-overlay").classList.contains("show") && settingsSection === "backgrounds") closeSettings();
  else $("#background-close").click();
});
$("#btn-backgrounds").addEventListener("click", () => openSettings("backgrounds"));
$("#background-close").addEventListener("click", () => $("#background-overlay").classList.remove("show"));
$("#background-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#background-close").click(); });

// ---- icon packs (🧩 header button: deck-wide sets + a per-app picker) ----
// iconQuery lets a pack preview ride the normal icon endpoint (?pack=name)
// so the deck shows the candidate artwork live — render() and iconArt()
// build every icon URL through it, and only this section flips the preview.
let iconPackPreview = null; // pack name shown on the deck without applying
let iconPacks = [];
let iconPackChoice = "";
const iconQuery = (id) => `/api/icon?id=${encodeURIComponent(id)}` +
  (iconPackPreview ? `&pack=${encodeURIComponent(iconPackPreview)}` : "");
function iconPackRefreshButtons() {
  const previewing = !!iconPackPreview;
  $("#iconpack-apply").disabled = !(iconPackPreview && iconPackPreview !== iconPackChoice);
  $("#iconpack-apply-name").textContent = iconPackPreview ? `"${iconPackPreview}"` : "";
  $("#iconpack-preview-hint").style.display = previewing ? "" : "none";
}
function iconPackRenderVariants() {
  const box = $("#iconpack-variants");
  const id = $("#iconpack-app").value;
  if (!id) { box.innerHTML = `<span class="muted">pick an app above</span>`; return; }
  const bust = Date.now();
  const tile = (pack, label) => `<button class="btn ghost" data-ip-one="${escapeHtml(pack)}" title="Give this app the ${escapeHtml(label)} icon" style="padding:6px;line-height:0">` +
    `<img src="/api/icon?id=${encodeURIComponent(id)}${pack ? `&pack=${encodeURIComponent(pack)}` : ""}&t=${bust}" alt="" style="width:40px;height:40px;border-radius:9px;display:block" onerror="this.replaceWith(document.createTextNode('📦'))">` +
    `</button>`;
  let html = tile("original", "original (backed-up)");
  for (const p of iconPacks) html += tile(p.name, `"${p.name}"`);
  box.innerHTML = html;
  box.querySelectorAll("[data-ip-one]").forEach(b => b.addEventListener("click", async () => {
    const res = await api("/api/iconpacks/apply-one", { id, pack: b.dataset.ipOne });
    toast(res.note || res.error || "done", res.ok ? "ok" : "err");
    render();
    iconPackRenderVariants();
  }));
}
function iconPackFillAppSelect() {
  const sel = $("#iconpack-app");
  const cur = sel.value;
  sel.innerHTML = [...apps].sort((a, b) => a.name.localeCompare(b.name))
    .map(a => `<option value="${escapeHtml(a.id)}">${escapeHtml(a.name)} (${escapeHtml(a.id)})</option>`).join("");
  if (cur && apps.some(a => a.id === cur)) sel.value = cur;
}
async function loadIconPacks() {
  const list = $("#iconpack-list");
  try {
    const res = await api("/api/iconpacks");
    iconPacks = res.packs || [];
    iconPackChoice = res.choice || "";
    if (!iconPacks.length) {
      list.innerHTML = `<p class="muted">No icon packs installed.</p>`;
    } else {
      list.innerHTML = `<div style="display:flex;flex-wrap:wrap;gap:8px">` + iconPacks.map(p =>
        `<span style="display:inline-flex;gap:6px;align-items:center">` +
        `<button class="btn ${iconPackPreview === p.name || (!iconPackPreview && p.applied) ? "btn-primary" : "ghost"}" data-ip-preview="${escapeHtml(p.name)}" title="Preview ${escapeHtml(p.name)} — ${p.count} icons">${(!iconPackPreview && p.applied) ? "● " : ""}${escapeHtml(p.name)}</button>` +
        (p.builtin ? "" : `<button class="btn ghost" data-ip-delete="${escapeHtml(p.name)}" title="Delete ${escapeHtml(p.name)}" aria-label="Delete">${svg("trash", 13)}</button>`) +
        `</span>`).join("") + `</div>`;
      list.querySelectorAll("[data-ip-preview]").forEach(b => b.addEventListener("click", () => {
        iconPackPreview = iconPackPreview === b.dataset.ipPreview ? null : b.dataset.ipPreview;
        render();          // the deck re-renders its icon <img>s through iconQuery
        loadIconPacks();   // refresh the button strip's active state
        iconPackRenderVariants();
      }));
      list.querySelectorAll("[data-ip-delete]").forEach(b => b.addEventListener("click", async () => {
        if (!confirm(`Delete icon pack "${b.dataset.ipDelete}"?`)) return;
        const res = await api("/api/iconpacks/delete", { name: b.dataset.ipDelete });
        toast(res.note || res.error || "removed", res.ok ? "ok" : "err");
        if (res.ok) await loadIconPacks();
      }));
    }
    iconPackFillAppSelect();
    iconPackRenderVariants();
    iconPackRefreshButtons();
  } catch (e) {
    list.innerHTML = `<p class="muted">Could not load icon packs: ${e}</p>`;
  }
}
$("#iconpack-app").addEventListener("change", iconPackRenderVariants);
$("#iconpack-apply").addEventListener("click", async () => {
  if (!iconPackPreview) return;
  const res = await api("/api/iconpacks/apply", { name: iconPackPreview });
  toast(res.note || res.error || "applied", res.ok ? "ok" : "err");
  if (res.ok) { iconPackChoice = iconPackPreview; iconPackPreview = null; render(); await loadIconPacks(); }
});
$("#iconpack-original").addEventListener("click", async () => {
  const res = await api("/api/iconpacks/apply", { name: "" });
  toast(res.note || res.error || "restored", res.ok ? "ok" : "err");
  if (res.ok) { iconPackChoice = ""; iconPackPreview = null; render(); await loadIconPacks(); }
});
$("#iconpack-git-go").addEventListener("click", async () => {
  const u = $("#iconpack-git").value.trim();
  if (!u) return;
  toast("Cloning icon pack…");
  try {
    const res = await api(`/api/iconpacks/import?url=${encodeURIComponent(u)}`, {});
    const note = $("#iconpack-note");
    note.style.display = ""; note.textContent = res.note || res.error || "";
    toast(res.note || res.error || "imported", res.ok ? "ok" : "err");
    if (res.ok) { $("#iconpack-git").value = ""; await loadIconPacks(); }
  } catch (e) { toast(`Import failed: ${e}`, "err"); }
});
{
  let packFile = null;
  const fi = document.createElement("input");
  fi.type = "file"; fi.accept = ".zip"; fi.style.display = "none";
  fi.addEventListener("change", () => {
    packFile = fi.files[0] || null;
    $("#iconpack-filename").textContent = packFile ? packFile.name : "";
    $("#iconpack-zip-go").disabled = !packFile;
  });
  $("#iconpack-drop").appendChild(fi);
  $("#iconpack-choose").addEventListener("click", () => fi.click());
  const dz = $("#iconpack-drop");
  ["dragover", "dragenter"].forEach((ev) => dz.addEventListener(ev, (e) => { e.preventDefault(); dz.classList.add("over"); }));
  ["dragleave", "drop"].forEach((ev) => dz.addEventListener(ev, (e) => { e.preventDefault(); dz.classList.remove("over"); }));
  dz.addEventListener("drop", (e) => {
    const f = e.dataTransfer.files[0];
    if (!f) return;
    packFile = f;
    $("#iconpack-filename").textContent = f.name;
    $("#iconpack-zip-go").disabled = false;
  });
  $("#iconpack-zip-go").addEventListener("click", async () => {
    if (!packFile) return;
    toast("Importing icon pack…");
    try {
      const res = await fetch(`/api/iconpacks/import?name=${encodeURIComponent(packFile.name.replace(/\.zip$/i, ""))}`, { method: "POST", body: await packFile.arrayBuffer() });
      const out = await res.json();
      const note = $("#iconpack-note");
      note.style.display = ""; note.textContent = out.note || out.error || "";
      toast(out.note || out.error || "imported", out.ok ? "ok" : "err");
      if (out.ok) { packFile = null; $("#iconpack-filename").textContent = ""; $("#iconpack-zip-go").disabled = true; await loadIconPacks(); }
    } catch (e) { toast(`Import failed: ${e}`, "err"); }
  });
}
$("#btn-iconpacks").addEventListener("click", () => openSettings("iconpacks"));
$("#iconpack-close").addEventListener("click", () => {
  $("#iconpack-overlay").classList.remove("show");
  if (iconPackPreview) { iconPackPreview = null; render(); } // drop the preview, keep applied icons
});
$("#iconpack-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#iconpack-close").click(); });

// ---- app security (🛡 header button: engines, self-test, deck scan) ----
async function loadSecurity() {
  try {
    const res = await api("/api/security");
    $("#security-engines").innerHTML = (res.engines || []).map(e =>
      `<div style="display:flex;gap:8px;align-items:baseline;padding:4px 0">` +
      `<b style="min-width:130px">${e.ok ? svg("check", 13) : svg("minus", 13)} ${escapeHtml(e.name)}</b>` +
      `<span class="muted">${escapeHtml(e.detail)}</span></div>`).join("");
  } catch (e) { $("#security-engines").innerHTML = `<p class="muted">Could not load: ${e}</p>`; }
}
$("#btn-security").addEventListener("click", () => openSettings("security"));
$("#security-close").addEventListener("click", () => $("#security-overlay").classList.remove("show"));
$("#security-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#security-close").click(); });
$("#security-vtkey-save").addEventListener("click", async () => {
  try {
    await api("/api/launcher-settings", { virusTotalKey: $("#security-vtkey").value.trim() });
    toast("VirusTotal key saved", "ok");
    loadSecurity();
  } catch (e) { toast(`Save failed: ${e}`, "err"); }
});
$("#security-selftest-go").addEventListener("click", async () => {
  const btn = $("#security-selftest-go");
  btn.disabled = true;
  $("#security-selftest-result").textContent = "running…";
  try {
    const res = await api("/api/security/selftest", {});
    const all = res.passed === res.total;
    $("#security-selftest-result").textContent = `passed ${res.passed}/${res.total} tests` + (all ? " ✓" : " — FAILURES BELOW");
    $("#security-selftest-cases").innerHTML = (res.cases || []).map(c =>
      `<div style="display:flex;gap:8px;align-items:baseline;padding:2px 0">` +
      `<span>${c.pass ? svg("check", 13) : svg("close", 13)}</span><b style="min-width:230px">${escapeHtml(c.name)}</b>` +
      `<span class="muted">${c.pass ? escapeHtml(c.expect) : escapeHtml(c.error || "failed")}</span></div>`).join("");
    toast(`Self-test: passed ${res.passed}/${res.total} tests`, all ? "ok" : "err");
  } catch (e) {
    $("#security-selftest-result").textContent = "";
    toast(`Self-test failed to run: ${e}`, "err");
  }
  btn.disabled = false;
});
$("#security-scanall-go").addEventListener("click", async () => {
  const btn = $("#security-scanall-go");
  btn.disabled = true;
  $("#security-scanall-status").textContent = "scanning…";
  try {
    const res = await api("/api/security/scan", {});
    const apps = res.apps || [];
    const flagged = apps.filter(a => a.level === "flagged").length;
    const warned = apps.filter(a => a.level === "warn").length;
    $("#security-scanall-status").textContent =
      `${apps.length} scanned · ${flagged} flagged · ${warned} with warnings`;
    $("#security-results").innerHTML = apps.map(a => {
      const ic = a.level === "flagged" ? svg("warning", 14) : a.level === "warn" ? svg("shield", 14) : svg("check", 14);
      const det = (a.findings || []).map(f => escapeHtml(`(${f.severity}) ${f.detail}`)).join("<br>") || "no findings";
      return `<details style="padding:3px 0;border-bottom:1px solid var(--border)">` +
        `<summary style="cursor:pointer">${ic} <b>${escapeHtml(a.name)}</b> <span class="muted">— ${escapeHtml(a.level)}</span></summary>` +
        `<div class="muted" style="padding:4px 0 8px 24px;font-size:12.5px">${det}</div></details>`;
    }).join("");
    toast(`Deck scan: ${apps.length} apps, ${flagged} flagged`, flagged ? "err" : "ok");
    refresh(); // refresh the cards so the failed badges match
  } catch (e) { $("#security-scanall-status").textContent = ""; toast(`Scan failed: ${e}`, "err"); }
  btn.disabled = false;
});
$("#iconpack-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#iconpack-close").click(); });
$("#background-git-go").addEventListener("click", async () => {
  const url = $("#background-git").value.trim();
  if (!url) return;
  toast("Cloning background…");
  try {
    const res = await api(`/api/backgrounds/import?url=${encodeURIComponent(url)}`);
    const note = $("#background-note");
    note.style.display = ""; note.textContent = res.note || res.error || "";
    toast(res.note || res.error || "imported", res.ok ? "ok" : "err");
    if (res.ok) { $("#background-git").value = ""; await loadBackgrounds(); }
  } catch (e) { toast(`Import failed: ${e}`, "err"); }
});
{
  let backgroundFile = null;
  const fi = document.createElement("input");
  fi.type = "file"; fi.accept = ".zip"; fi.style.display = "none";
  fi.addEventListener("change", () => {
    backgroundFile = fi.files[0] || null;
    $("#background-filename").textContent = backgroundFile ? backgroundFile.name : "";
    $("#background-zip-go").disabled = !backgroundFile;
  });
  $("#background-drop").appendChild(fi);
  $("#background-choose").addEventListener("click", () => fi.click());
  const bdz = $("#background-drop");
  ["dragover", "dragenter"].forEach((ev) => bdz.addEventListener(ev, (e) => { e.preventDefault(); bdz.classList.add("over"); }));
  ["dragleave", "drop"].forEach((ev) => bdz.addEventListener(ev, (e) => { e.preventDefault(); bdz.classList.remove("over"); }));
  bdz.addEventListener("drop", (e) => {
    const f = e.dataTransfer.files[0];
    if (!f) return;
    backgroundFile = f;
    $("#background-filename").textContent = f.name;
    $("#background-zip-go").disabled = false;
  });
  $("#background-zip-go").addEventListener("click", async () => {
    if (!backgroundFile) return;
    toast("Importing background zip…");
    try {
      const res = await fetch(`/api/backgrounds/import?name=${encodeURIComponent(backgroundFile.name.replace(/\.zip$/i, ""))}`, { method: "POST", body: await backgroundFile.arrayBuffer() });
      const out = await res.json();
      const note = $("#background-note");
      note.style.display = ""; note.textContent = out.note || out.error || "";
      toast(out.note || out.error || "imported", out.ok ? "ok" : "err");
      if (out.ok) { backgroundFile = null; $("#background-filename").textContent = ""; $("#background-zip-go").disabled = true; await loadBackgrounds(); }
    } catch (e) { toast(`Import failed: ${e}`, "err"); }
  });
}

// ---- launcher self-update (⚙ ▸ ⬆ Update launcher…) — two ways ----
{
  let updateFile = null;
  // platformMismatch inspects a file or asset name for OS/arch markers and
// returns a warning string when it clearly was not built for this launcher
// ("", or an empty/unknown name, means no opinion). Best-effort pre-flight:
// the server still verifies the actual binary headers after upload.
function platformMismatch(name) {
  if (!name) return "";
  const n = name.toLowerCase();
  const markers = {
    darwin:  ["macos", "darwin", "osx", ".dmg"],
    windows: ["windows", "win32", "win64", ".exe", ".msi"],
    linux:   ["linux", ".appimage", ".deb", ".rpm"],
  };
  let fileOS = null;
  for (const [os, marks] of Object.entries(markers)) {
    if (marks.some(m => n.includes(m))) { fileOS = os; break; }
  }
  if (fileOS && hostOS && fileOS !== hostOS) {
    return `This looks like a ${fileOS} build, but this launcher runs on ${hostOS}.`;
  }
  // Arch markers (checked only within the right OS — a "windows-arm64"
  // name on a darwin host is already reported above).
  const isArm = n.includes("arm64") || n.includes("aarch64");
  const isAmd = n.includes("amd64") || n.includes("x86_64") || n.includes("x64");
  const is386 = n.includes("386") || n.includes("i686");
  const universal = n.includes("universal");
  if (hostArch && (isArm || isAmd || is386) && !universal) {
    const arm64 = hostArch === "arm64", amd64 = hostArch === "amd64";
    const compatible = (isArm && arm64) || (isAmd && amd64) ||
      (is386 && amd64) ||                    // 32-bit x86 runs on amd64
      (isAmd && arm64 && hostOS === "darwin"); // Rosetta 2 runs amd64 on arm64 Macs
    if (!compatible) {
      return `This looks like a ${fileOS || hostOS} ${isArm ? "arm64" : is386 ? "386" : "amd64"} build, but this launcher runs on ${hostOS}/${hostArch}.`;
    }
  }
  return "";
}

// confirmPlatform checks a file name pre-flight and, on a mismatch, asks
// the user whether to continue anyway. Returns false when declined.
function confirmPlatform(name, what) {
  const warn = platformMismatch(name);
  if (!warn) return true;
  return confirm(`${warn}\n\nIt will most likely fail to install or run.\n\nContinue with ${what || name} anyway?`);
}

let ghInfo = null; // last /api/update/check response
const updateCheckDay = 24 * 60 * 60 * 1000;
const updateCheckPreferenceKey = "launcher.updateCheckEnabled";
const updateCheckTimeKey = "launcher.updateCheckAt";
const updateCheckResultKey = "launcher.updateCheckResult";
let updateCheckPromise = null;
const readCachedUpdate = () => {
  try {
    const info = JSON.parse(localStorage.getItem(updateCheckResultKey) || "null");
    return info && typeof info === "object" ? info : null;
  } catch { return null; }
};
const applyUpdateBadge = (info) => {
  const badge = $("#sb-update-badge"), button = $("#sb-update");
  const available = !!(info && info.found && info.asset && !info.upToDate);
  badge.hidden = !available;
  button.title = available ? `Update available: ${info.tag}` : "Update AppDeck launcher";
  button.setAttribute("aria-label", available ? `Update launcher; version ${info.tag} available` : "Update launcher");
};
const saveUpdateCheck = (info) => {
  if (!info || info.error) return;
  const checked = { ...info, checkedAt: Date.now() };
  try {
    localStorage.setItem(updateCheckResultKey, JSON.stringify(checked));
    localStorage.setItem(updateCheckTimeKey, String(checked.checkedAt));
  } catch {}
  applyUpdateBadge(checked);
};
let stagedUpdateToken = "";
let stagedUpdateSource = "";
let stagedUpdateButton = null;
let stagedUpdateButtonLabel = "";
let stagedUpdateFile = null;
const resetStagedUpdate = () => {
  if (stagedUpdateButton) stagedUpdateButton.textContent = stagedUpdateButtonLabel;
  stagedUpdateButton = null;
  stagedUpdateButtonLabel = "";
  stagedUpdateFile = null;
  stagedUpdateToken = "";
  stagedUpdateSource = "";
};
const updateProgress = (message, state = "busy", cancellable = false) => {
  const box = $("#update-progress");
  box.hidden = !message;
  box.classList.remove("busy", "error", "success");
  $("#update-cancel").hidden = !message || !cancellable;
  if (!message) {
    box.removeAttribute("aria-busy");
    return;
  }
  box.classList.add(state);
  box.setAttribute("aria-busy", state === "busy" ? "true" : "false");
  $("#update-progress-text").textContent = message;
};
const acceptStagedUpdate = (result, button, file = null) => {
  if (!result.ok || !result.token) throw new Error(result.error || "The update package could not be staged.");
  stagedUpdateToken = result.token;
  stagedUpdateSource = result.source || "";
  stagedUpdateButton = button;
  stagedUpdateButtonLabel = button.textContent;
  stagedUpdateFile = file;
  button.textContent = "Install now";
  const source = result.source ? ` (${result.source})` : "";
  updateProgress(`Package downloaded and validated${source}. Install now to replace the launcher, or cancel to discard it.`, "success", true);
};
const prepareUpdate = async (request, button, message, file = null) => {
  if (stagedUpdateToken) {
    await cancelStagedUpdate();
    if (stagedUpdateToken) return false;
  }
  button.disabled = true;
  updateProgress(message);
  try {
    acceptStagedUpdate(await request(), button, file);
    return true;
  } catch (e) {
    updateProgress(`Could not prepare update: ${e}`, "error");
    toast(`Update preparation failed: ${e}`, "err");
    return false;
  } finally { button.disabled = false; }
};
const cancelStagedUpdate = async () => {
  if (!stagedUpdateToken) return;
  const token = stagedUpdateToken;
  const cancel = $("#update-cancel");
  cancel.disabled = true;
  try {
    const result = await api("/api/update?phase=cancel", { token });
    if (!result.ok) throw new Error(result.error || "The staged update could not be canceled.");
    resetStagedUpdate();
    updateProgress("Staged update canceled. The installed launcher was not changed.", "success");
    return true;
  } catch (e) {
    updateProgress(`Could not cancel staged update: ${e}`, "error", true);
    return false;
  } finally { cancel.disabled = false; }
};
const commitStagedUpdate = async (button) => {
  if (!stagedUpdateToken) return;
  const token = stagedUpdateToken;
  const file = stagedUpdateFile;
  button.disabled = true;
  updateProgress("Installing the validated launcher. This final step cannot be canceled.");
  try {
    const result = await api("/api/update?phase=commit", { token });
    if (!result.ok) {
      updateProgress(`Install failed: ${result.error || "unknown error"}. You can retry or cancel the staged package.`, "error", true);
      return;
    }
    if (button.id === "update-go" && file && updateFile === file) {
      updateFile = null;
      $("#update-filename").textContent = "";
    }
    resetStagedUpdate();
    updateProgress("Update installed. Choose Restart Now or Later to finish.", "success");
    await offerUpdateRestart(result.note || "Launcher updated.");
  } catch (e) {
    updateProgress(`Could not confirm the update result. Check the running version before retrying. (${e})`, "error", true);
    toast(`Update failed: ${e}`, "err");
  } finally { button.disabled = false; }
};
$("#update-cancel").addEventListener("click", cancelStagedUpdate);
const updateCheckInput = $("#update-auto-check");
try { updateCheckInput.checked = localStorage.getItem(updateCheckPreferenceKey) === "1"; } catch {}
const cachedUpdateInfo = readCachedUpdate();
if (cachedUpdateInfo) applyUpdateBadge(cachedUpdateInfo);

  // offerUpdateRestart: after a successful self-update, hand the dashboard
  // to the new launcher. Confirms (skip in the native window — its WKWebView
  // shows no confirm dialogs), asks the dying launcher to spawn its
  // replacement, then polls until the fresh one answers and reloads.
  // Cancel = "Later": the running launcher's process identity (its boot
  // time — the version string alone never changes across a dev restart)
  // is remembered per browser, and a reminder bar (restartFlow behind ⏻)
  // returns on every load while that same process is still serving.
  const pendingKey = "launcher.pendingRestart";
  const restartFlow = async () => {
    try { localStorage.removeItem(pendingKey); } catch {}
    $("#update-pending").hidden = true;
    toast("Restarting AppDeck…", "ok");
    try { await api("/api/restart", {}); } catch { /* server already going away */ }
    // The child launcher waits for the port, then boots; poll until it is
    // up (or long enough that the user should reopen by hand).
    let back = false;
    for (let i = 0; i < 60 && !back; i++) {
      await new Promise((res) => setTimeout(res, 1000));
      try { await api("/api/apps"); back = true; } catch { /* still down */ }
    }
    if (back) { toast("New launcher is up — reloading the dashboard", "ok"); location.reload(); }
    else toast("Still waiting for the new launcher — reopen the dashboard in a moment", "err");
  };
  const offerUpdateRestart = async (note) => {
    // A styled dialog instead of confirm(): the native window's WKWebView
    // shows no confirm dialogs, so asking there used to auto-restart
    // without offering Later. The dialog that triggered this (⬆ Update
    // launcher) is still open and stacks above this prompt — both are
    // fixed overlays at z-index 50 and this one sits earlier in the DOM —
    // so it is closed here first, and #restart-overlay carries a higher
    // z-index for any other caller.
    $("#update-overlay").classList.remove("show");
    $("#restart-note").textContent = note || "Launcher updated.";
    $("#restart-overlay").classList.add("show");
  };
  $("#restart-now").addEventListener("click", () => {
    $("#restart-overlay").classList.remove("show");
    restartFlow();
  });
  const restartLater = async () => {
    $("#restart-overlay").classList.remove("show");
    try {
      const h = await api("/api/health");
      localStorage.setItem(pendingKey, String(h.started || h.version || "unknown"));
    } catch { /* the reminder bar needs the identity only to clear itself later */ }
    $("#update-pending").hidden = false;
  };
  $("#restart-later").addEventListener("click", restartLater);
  $("#restart-close").addEventListener("click", restartLater);
  $("#restart-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) restartLater(); });
  const checkPendingRestart = async () => {
    let pending = null;
    try { pending = localStorage.getItem(pendingKey); } catch {}
    if (!pending) return;
    try {
      const h = await api("/api/health");
      const current = String(h.started || h.version || "unknown");
      if (current === pending) { $("#update-pending").hidden = false; return; }
      try { localStorage.removeItem(pendingKey); } catch {} // a different launcher process is running — stale reminder
    } catch { /* launcher down — nothing to show */ }
  };
  checkPendingRestart();
  $("#update-pending-go").addEventListener("click", restartFlow);
  $("#update-pending-later").addEventListener("click", () => { $("#update-pending").hidden = true; });
  const setNote = (t) => { const n = $("#update-note"); n.style.display = t ? "" : "none"; n.textContent = t || ""; };
  const setChannel = async (ch, save) => {
    $("#update-github-pane").style.display = ch === "github" ? "" : "none";
    $("#update-local-pane").style.display = ch === "local" ? "" : "none";
    $("#update-ch-github").classList.toggle("on", ch === "github");
    $("#update-ch-local").classList.toggle("on", ch === "local");
    if (!save) return;
    try {
      const cur = await api("/api/launcher-settings");
      await api("/api/launcher-settings", { ...cur, updateChannel: ch });
    } catch { /* persistence is best-effort */ }
  };
  const openUpdate = async () => {
    closeSettings();
    openPage("update-overlay");
    ghInfo = null;
    $("#update-gh-box").style.display = "none";
    $("#update-check").disabled = false;
    $("#update-tag").value = "";
    setNote("");
    if (stagedUpdateToken) {
      const source = stagedUpdateSource ? ` (${stagedUpdateSource})` : "";
      updateProgress(`A validated package is staged${source}. Install now to replace the launcher, or cancel to discard it.`, "success", true);
    } else updateProgress("");
    try { const h = await api("/api/health"); $("#update-current").textContent = `Running version: ${h.version || "dev"}`; } catch {}
    try {
      const s = await api("/api/launcher-settings");
      await setChannel(s.updateChannel === "local" ? "local" : "github", false);
      const cached = readCachedUpdate();
      if (s.updateChannel !== "local" && cached) {
        ghInfo = cached;
        $("#update-gh-box").style.display = "";
        $("#update-tag").value = ghInfo.tag || "";
        ghRender();
      }
    } catch { await setChannel("github", false); }
  };
  $("#btn-update-launcher").addEventListener("click", openUpdate);
  $("#update-close").addEventListener("click", () => $("#update-overlay").classList.remove("show"));
  $("#update-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#update-close").click(); });
  $("#update-ch-github").addEventListener("click", () => setChannel("github", true));
  $("#update-ch-local").addEventListener("click", () => setChannel("local", true));

  // GitHub channel: check → pick a tag (dropdown or typed, downgrades
  // included) → load its asset → download & install.
  const ghRender = () => {
    const info = $("#update-gh-info"), inst = $("#update-gh-install");
    inst.disabled = true;
    if (!ghInfo) { info.textContent = ""; return; }
    if (ghInfo.error) { info.textContent = ghInfo.error; return; }
    if (!ghInfo.found) { info.textContent = "No releases published yet."; return; }
    const a = ghInfo.asset;
    const pm = a ? platformMismatch(a.name) : "";
    const latest = ghInfo.latest || ghInfo.tag;
    info.innerHTML = `AppDeck ${escapeHtml(ghInfo.current || "?")} → <b>${escapeHtml(ghInfo.tag)}</b>` +
      `<br>${escapeHtml(ghInfo.platform || "?")}/${escapeHtml(ghInfo.architecture || "?")}` +
      (a ? ` · ${escapeHtml(a.name)} · ${(a.size / 1048576).toFixed(1)} MB` : "") +
      (ghInfo.isLatest === false ? `<br><span class="update-warning">Older release selected; latest is ${escapeHtml(latest)}. Installing may downgrade AppDeck.</span>` : "") +
      (ghInfo.notes
        ? `<details class="update-notes"><summary>What's changed in ${escapeHtml(ghInfo.tag)}</summary><pre>${escapeHtml(ghInfo.notes)}</pre></details>`
        : "") +
      (ghInfo.releaseURL ? `<br><a href="${escapeHtml(ghInfo.releaseURL)}" target="_blank" rel="noopener">Release notes on GitHub</a>` : "") +
      (pm ? `<br><span class="update-error">${svg("warning", 12)} ${escapeHtml(pm)}</span>` : "");
    inst.disabled = !a || !!ghInfo.upToDate;
    if (ghInfo.upToDate) info.innerHTML += "<br>This release is already installed.";
  };
  const ghCheck = async (tag) => {
    if (stagedUpdateToken && !(await cancelStagedUpdate())) return;
    $("#update-gh-box").style.display = "";
    $("#update-gh-info").textContent = "Checking for a release…";
    $("#update-check").disabled = true;
    setNote("");
    updateProgress("Checking GitHub releases…");
    try {
      ghInfo = await api("/api/update/check" + (tag ? `?tag=${encodeURIComponent(tag)}` : ""));
      if (ghInfo.releases) {
        $("#update-tags").innerHTML = ghInfo.releases.map((t) => `<option value="${escapeHtml(t)}">`).join("");
        if (!$("#update-tag").value && ghInfo.tag) $("#update-tag").value = ghInfo.tag;
      }
      ghRender();
      if (!tag) saveUpdateCheck(ghInfo);
      if (ghInfo.error) updateProgress(ghInfo.error, "error");
      else updateProgress("");
    } catch (e) {
      setNote(`Check failed: ${e}`);
      updateProgress(`Check failed: ${e}`, "error");
    } finally { $("#update-check").disabled = false; }
  };
  const checkLauncherUpdates = async (force = false) => {
    if (!updateCheckInput.checked) return;
    if (updateCheckPromise) return updateCheckPromise;
    let lastCheck = 0;
    try { lastCheck = Number(localStorage.getItem(updateCheckTimeKey)) || 0; } catch {}
    if (!force && Date.now() - lastCheck < updateCheckDay) return;
    updateCheckPromise = (async () => {
      try {
        const settings = await api("/api/launcher-settings");
        if (settings.updateChannel !== "local") {
          const result = await api("/api/update/check");
          if (!result.error) {
            ghInfo = result;
            saveUpdateCheck(result);
            if ($("#update-overlay").classList.contains("show")) {
              $("#update-gh-box").style.display = "";
              $("#update-tag").value = result.tag || "";
              ghRender();
            }
          }
        }
      } catch {
        // A failed automatic check is quiet and can be retried on the next daily check.
      } finally {
        try { localStorage.setItem(updateCheckTimeKey, String(Date.now())); } catch {}
        updateCheckPromise = null;
      }
    })();
    return updateCheckPromise;
  };
  updateCheckInput.addEventListener("change", () => {
    try { localStorage.setItem(updateCheckPreferenceKey, updateCheckInput.checked ? "1" : "0"); } catch {}
    if (updateCheckInput.checked) void checkLauncherUpdates(true);
  });
  setTimeout(() => { void checkLauncherUpdates(); }, 1200);
  $("#update-check").addEventListener("click", () => { $("#update-tag").value = ""; ghCheck(""); });
  $("#update-tag-fetch").addEventListener("click", () => ghCheck($("#update-tag").value.trim()));
  $("#update-tag").addEventListener("change", () => ghCheck($("#update-tag").value.trim()));
  $("#update-gh-install").addEventListener("click", async () => {
    const button = $("#update-gh-install");
    if (stagedUpdateToken) { await commitStagedUpdate(button); return; }
    if (!ghInfo || !ghInfo.asset) return;
    if (!confirmPlatform(ghInfo.asset.name, `release ${ghInfo.tag}`)) return;
    await prepareUpdate(
      () => api("/api/update?phase=prepare", { source: "github", tag: $("#update-tag").value.trim(), stall: parseInt($("#update-stall").value, 10) || 0 }),
      button,
      "Downloading and validating the selected release…",
    );
  });

  // Deck rebuild: regenerate missing app.json manifests from the installed
  // folders (after an update shrank the deck), optionally importing app
  // folders from an external path.
  $("#rebuild-go").addEventListener("click", async () => {
    const btn = $("#rebuild-go");
    btn.disabled = true;
    const note = $("#rebuild-note");
    note.style.display = "";
    note.textContent = "Rebuilding…";
    try {
      const res = await api("/api/deck/rebuild", { path: $("#rebuild-path").value.trim() });
      if (res.ok) {
        const parts = [];
        if (res.generated) parts.push(`${res.generated} manifest(s) generated`);
        if (res.imported) parts.push(`${res.imported} folder(s) imported`);
        note.textContent = parts.length
          ? `Deck rebuilt: ${parts.join(", ")}.`
          : "Nothing to do — every installed folder already has an app.json.";
        if (res.notes && res.notes.length) note.textContent += "\n" + res.notes.join("\n");
        await refresh();
        toast("Deck rebuilt", "ok");
      } else {
        note.textContent = res.error || "Rebuild failed";
        toast(res.error || "Rebuild failed", "err");
      }
    } catch (e) { note.textContent = `Rebuild failed: ${e}`; }
    btn.disabled = false;
  });

  // …or the classic upload: drop / choose a zip.
  const udz = $("#update-drop");
  const fi = document.createElement("input");
  fi.type = "file"; fi.accept = ".zip"; fi.style.display = "none";
  fi.addEventListener("change", async () => {
    const chosen = fi.files[0] || null;
    if (stagedUpdateFile && stagedUpdateFile !== chosen && !(await cancelStagedUpdate())) return;
    updateFile = chosen;
    $("#update-filename").textContent = updateFile ? updateFile.name : "";
    $("#update-go").disabled = !updateFile;
    if (updateFile) { const w = platformMismatch(updateFile.name); if (w) toast(w + " — it will fail at install time", "err"); }
  });
  $("#update-drop").appendChild(fi);
  $("#update-choose").addEventListener("click", () => fi.click());
  ["dragover", "dragenter"].forEach((ev) => udz.addEventListener(ev, (e) => { e.preventDefault(); udz.classList.add("over"); }));
  ["dragleave", "drop"].forEach((ev) => udz.addEventListener(ev, (e) => { e.preventDefault(); udz.classList.remove("over"); }));
  udz.addEventListener("drop", async (e) => {
    const f = e.dataTransfer.files[0];
    if (!f) return;
    if (stagedUpdateFile && stagedUpdateFile !== f && !(await cancelStagedUpdate())) return;
    updateFile = f;
    $("#update-filename").textContent = f.name;
    $("#update-go").disabled = false;
    { const w = platformMismatch(f.name); if (w) toast(w + " — it will fail at install time", "err"); }
  });
  $("#update-go").addEventListener("click", async () => {
    const button = $("#update-go");
    if (stagedUpdateToken) { await commitStagedUpdate(button); return; }
    if (!updateFile) return;
    if (!confirmPlatform(updateFile.name, updateFile.name)) return;
    const file = updateFile;
    await prepareUpdate(
      async () => {
        const res = await fetch("/api/update?phase=prepare", { method: "POST", body: await file.arrayBuffer() });
        return res.json();
      },
      button,
      "Uploading, validating, and staging the selected launcher…",
      file,
    );
  });
}

// ---- storage (💾) ----
{
  const storageBytes = (n) => {
    if (!Number.isFinite(n) || n <= 0) return "0 B";
    const units = ["B", "KB", "MB", "GB", "TB"];
    let v = n;
    let i = 0;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return `${v >= 10 || i === 0 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
  };
  const refreshStorage = async () => {
    try {
      const out = await api("/api/storage");
      const node = $("#storage-status");
      const warn = $("#storage-warning");
      node.textContent = `Current data dir: ${out.dataDir || "(unset)"}\nDefault dir: ${out.defaultDir || "(unset)"}\nSize: ${storageBytes(out.bytes || 0)}`;
      if (out.warning) {
        warn.textContent = out.warning;
        warn.style.display = "";
      } else {
        warn.textContent = "";
        warn.style.display = "none";
      }
      $("#storage-note").style.display = "none";
      $("#storage-note").textContent = "";
    } catch (e) {
      $("#storage-status").textContent = `Storage status unavailable: ${e}`;
    }
  };
  const openStorage = async () => {
    $("#deck-overlay").classList.remove("show");
    closeSettings();
    $("#storage-overlay").classList.add("show");
    await refreshStorage();
  };
  $("#menu-storage").addEventListener("click", () => openStorage());
  $("#storage-close").addEventListener("click", () => $("#storage-overlay").classList.remove("show"));
  $("#storage-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#storage-close").click(); });
  $("#storage-move").addEventListener("click", async () => {
    try {
      toast("Choose the destination folder in the dialog…");
      const out = await api("/api/pick-path", { tool: "storage", kind: "install" });
      if (out.error) {
        if (out.error !== "cancelled") toast(out.error, "err");
        return;
      }
      const res = await api("/api/storage/move", { path: out.path });
      const note = $("#storage-note");
      note.style.display = "";
      note.textContent = res.note || res.error || "Storage move failed.";
      if (res.ok) {
        toast("Storage move queued — restart AppDeck to use the new location", "ok");
      } else {
        toast(res.error || "Storage move failed", "err");
      }
      await refreshStorage();
    } catch (e) {
      const note = $("#storage-note");
      note.style.display = "";
      note.textContent = `Move failed: ${e}`;
      toast(`Storage move failed: ${e}`, "err");
    }
  });
}

// ---- prerequisites (🧰) ----
function prereqRowHtml(t) {
  const status = t.installed
    ? `<span class="muted">${svg("check", 12)} ${t.version || "installed"}${t.custom ? " · registered path" : ""}</span><br><code class="muted" style="font-size:11px">${t.path}</code>`
    : `<span class="muted">not installed</span>`;
  const installRow = t.installable
    ? `<div style="display:flex;gap:6px;flex-wrap:wrap;margin:6px 0">
         <button class="btn ghost" data-prereq-install="${t.id}">Install — default location</button>
         <button class="btn ghost" data-prereq-install-pick="${t.id}">Install to folder…</button>
       </div>`
    : (t.installNote ? `<p class="muted" style="margin:4px 0">${t.installNote}</p>` : "");
  const reqBy = t.requiredBy && t.requiredBy.length
    ? `<p class="muted" style="margin:4px 0">needed by: ${t.requiredBy.join(", ")}</p>` : "";
  const pathRow = t.installed || true
    ? `<div style="display:flex;gap:6px;flex-wrap:wrap;align-items:center;margin-top:6px">
         <button class="btn ghost" data-prereq-register-pick="${t.id}">Register path…</button>
         ${t.custom ? `<button class="btn ghost" data-prereq-clear="${t.id}">Clear</button>` : ""}
         ${t.removable ? `<button class="btn ghost" data-prereq-remove="${t.id}" title="Delete the copy the launcher installed and clear its path"><span data-ic="trash" data-ics="13"></span> Remove installed copy</button>` : ""}
       </div>
       <p class="muted" style="margin:4px 0 0;font-size:11px">Registering: ${t.pointHint}</p>`
    : "";
  return `<div style="border:1px solid var(--btn-border,#333);border-radius:8px;padding:10px;margin:8px 0">
    <div style="display:flex;justify-content:space-between;gap:8px;align-items:baseline">
      <b>${t.name}</b>${status}
    </div>
    <p class="muted" style="margin:4px 0">${t.desc}</p>
    ${reqBy}
    ${installRow}
    ${pathRow}
  </div>`;
}
async function prereqPick(tool, kind) {
  toast("Choose the folder in the dialog on your screen…");
  const out = await api("/api/pick-path", { tool, kind });
  if (out.error) {
    if (out.error !== "cancelled") toast(out.error, "err");
    return null;
  }
  return out.path;
}
async function loadPrereqs() {
  const list = $("#prereq-list");
  try {
    const res = await fetch("/api/presets");
    const tools = await res.json();
    list.innerHTML = tools.map(prereqRowHtml).join("") || "<p class='muted'>No prerequisites defined.</p>";
    list.querySelectorAll("[data-prereq-install]").forEach((btn) => btn.addEventListener("click", async () => {
      const id = btn.dataset.prereqInstall;
      toast(`Installing ${id} to the default location… (this can take a while)`);
      btn.disabled = true;
      try {
        const out = await api("/api/presets/install", { tool: id });
        toast(out.ok ? (out.note || "installed") : (out.error || "install failed"), out.ok ? "ok" : "err");
      } catch (e) { toast(`Install failed: ${e}`, "err"); }
      await loadPrereqs();
    }));
    list.querySelectorAll("[data-prereq-install-pick]").forEach((btn) => btn.addEventListener("click", async () => {
      const id = btn.dataset.prereqInstallPick;
      const dir = await prereqPick(id, "install");
      if (!dir) return;
      toast(`Installing ${id} to ${dir}… (this can take a while)`);
      btn.disabled = true;
      try {
        const out = await api("/api/presets/install", { tool: id, dir });
        toast(out.ok ? (out.note || "installed") : (out.error || "install failed"), out.ok ? "ok" : "err");
      } catch (e) { toast(`Install failed: ${e}`, "err"); }
      await loadPrereqs();
    }));
    list.querySelectorAll("[data-prereq-register-pick]").forEach((btn) => btn.addEventListener("click", async () => {
      const id = btn.dataset.prereqRegisterPick;
      const path = await prereqPick(id, "register");
      if (!path) return;
      try {
        const out = await api("/api/presets/path", { tool: id, path });
        toast(out.ok ? `${id}: ${out.note}` : (out.error || "failed"), out.ok ? "ok" : "err");
      } catch (e) { toast(`Failed: ${e}`, "err"); }
      await loadPrereqs();
    }));
    list.querySelectorAll("[data-prereq-clear]").forEach((btn) => btn.addEventListener("click", async () => {
      try { await api("/api/presets/path", { tool: btn.dataset.prereqClear, path: "" }); }
      catch (e) { toast(`Failed: ${e}`, "err"); }
      await loadPrereqs();
    }));
    list.querySelectorAll("[data-prereq-remove]").forEach((btn) => btn.addEventListener("click", async () => {
      const id = btn.dataset.prereqRemove;
      if (!confirm(`Remove the ${id} copy the launcher installed (delete its files and clear the path)?`)) return;
      try {
        const out = await api("/api/presets/remove", { tool: id });
        toast(out.ok ? (out.note || "removed") : (out.error || "remove failed"), out.ok ? "ok" : "err");
      } catch (e) { toast(`Remove failed: ${e}`, "err"); }
      await loadPrereqs();
    }));
  } catch (e) {
    list.innerHTML = `<p class="muted">Could not load prerequisites: ${e}</p>`;
  }
}
$("#btn-prereqs").addEventListener("click", () => openSettings("prereqs"));
$("#prereq-close").addEventListener("click", () => $("#prereq-overlay").classList.remove("show"));
$("#prereq-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#prereq-close").click(); });

// Drag a web app between the two sections to flip its Wi-Fi sharing. The
// server saves a per-app override (network.json) and restarts a running app
// so the new bind host takes effect immediately. Desktop apps can't be
// dragged — they have no web page to share. Cards also carry a 🛜/모 toggle
// button that flips the same setting (drag-and-drop does not fire inside the
// native WKWebView window, so the button is the reliable path there).
async function flipNetwork(id, enabled) {
  busy.add(id);
  render();
  try {
    const res = await api("/api/network", { id, enabled });
    if (res.busy) toast(`${id}: another action is still running`, "");
    else if (res.error) toast(`${id}: ${res.error}`, "err");
    else toast(res.note || (enabled ? "Shared on the Wi-Fi" : "Back to this computer only"), "ok");
  } catch (err) { toast(`Could not change Wi-Fi sharing: ${err}`, "err"); }
  busy.delete(id);
  await refresh();
}
// Auto-scroll while dragging: with many apps per section the other drop
// zone can be a full page away, and WebKit does not auto-scroll during an
// HTML5 drag — so nudge the window toward the pointer whenever it nears a
// viewport edge. dragover keeps firing for the whole drag session (WebKit
// emits it on a timer, even for a stationary pointer); the rAF loop scrolls
// between events and stops when the drag ends or the stream goes quiet.
let dragPointerY = -1, dragLastSeen = 0, dragScrollRaf = 0;
function endDragSession() {
  dragPointerY = -1;
  if (dragScrollRaf) { cancelAnimationFrame(dragScrollRaf); dragScrollRaf = 0; }
  $("#drag-hint").hidden = true;
}
document.addEventListener("dragover", (e) => { dragPointerY = e.clientY; dragLastSeen = performance.now(); }, true);
document.addEventListener("dragstart", (e) => {
  const card = e.target.closest ? e.target.closest(".card[data-net]") : null;
  const app = card && apps.find(a => a.id === card.dataset.id);
  $("#drag-hint").textContent = app
    ? `Dragging “${app.name}” — drop it in the other section to change Wi-Fi sharing · hold near a window edge to auto-scroll`
    : "Drop the card in the other section to change Wi-Fi sharing · hold near a window edge to auto-scroll";
  $("#drag-hint").hidden = false;
  if (!dragScrollRaf) dragScrollRaf = requestAnimationFrame(dragScrollTick);
}, true);
function dragScrollTick() {
  const fresh = performance.now() - dragLastSeen < 1500;
  if (dragPointerY < 0 || !fresh) { dragScrollRaf = 0; return; }
  const margin = 90, h = innerHeight;
  let v = 0;
  if (dragPointerY < margin) v = -Math.ceil((margin - dragPointerY) / 6) - 2;
  else if (dragPointerY > h - margin) v = Math.ceil((dragPointerY - (h - margin)) / 6) + 2;
  if (v) window.scrollBy(0, v);
  dragScrollRaf = requestAnimationFrame(dragScrollTick);
}
document.addEventListener("drop", endDragSession, true);
document.addEventListener("dragend", endDragSession, true);
groupsEl.addEventListener("dragstart", (e) => {
  const card = e.target.closest(".card[data-net]");
  if (!card) { e.preventDefault(); return; }
  e.dataTransfer.setData("text/plain", card.dataset.id);
  e.dataTransfer.effectAllowed = "move";
});
groupsEl.addEventListener("dragover", (e) => {
  const zone = e.target.closest(".group[data-drop]");
  if (!zone) return;
  e.preventDefault();
  e.dataTransfer.dropEffect = "move";
  zone.classList.add("dragover");
});
groupsEl.addEventListener("dragleave", (e) => {
  const zone = e.target.closest(".group[data-drop]");
  if (zone) zone.classList.remove("dragover");
});
groupsEl.addEventListener("drop", (e) => {
  const zone = e.target.closest(".group[data-drop]");
  if (!zone) return;
  e.preventDefault();
  document.querySelectorAll(".group.dragover").forEach(z => z.classList.remove("dragover"));
  const id = e.dataTransfer.getData("text/plain");
  const card = document.getElementById(`card-${id}`);
  if (!card || card.dataset.net === zone.dataset.drop) return; // no change
  const app = apps.find(a => a.id === id);
  if (!app || app.desktop) return;
  const enabled = zone.dataset.drop === "wifi";
  if (enabled && !confirm(`Put “${app.name}” on the Wi-Fi?\nEvery device on this network will be able to open it.`)) return;
  flipNetwork(id, enabled);
});
groupsEl.addEventListener("click", (e) => {
  const cat = e.target.closest(".cat-label");
  if (cat) {
    const f = $("#filter");
    f.value = cat.dataset.cat;
    f.dispatchEvent(new Event("input", { bubbles: true }));
    return;
  }
});

$("#lan-note").addEventListener("click", async () => {
  const url = $("#lan-note").dataset.url;
  if (!url) return;
  try {
    await navigator.clipboard.writeText(url);
    toast(url, "ok");
  } catch { toast(url, ""); }
});

async function act(id, action, port, securityOverride) {
  busy.add(id);
  render();
  try {
    const body = port ? { id, port } : { id };
    if (securityOverride) body.securityOverride = true;
    const res = await api(`/api/${action}`, body);
    if (res.busy) { toast(`${id}: another action is still running`, ""); }
    else if (res.pending) {
      // Permission gate: the card declares permissions nobody granted yet —
      // show the checklist; a verdict retries the same start automatically.
      busy.delete(id);
      render();
      openPermPrompt(res.pending, () => act(id, action, port));
      return;
    }
    else if (res.securityHold) {
      // Start-gate: the app is flagged — show what was found and ask.
      const why = (res.findings || []).map(f => `• (${f.severity}) ${f.detail}`).join("\n") || "critical security findings";
      if (confirm(`"${res.name || id}" is flagged by the security scan:\n\n${why}\n\nStart it anyway?`)) {
        busy.delete(id);
        return act(id, action, port, true);
      }
    }
    else if (res.removed) {
      // The server uninstalled the app after a platform-incompatible start
      // failure; the card vanishes on the refresh below.
      toast(res.note || `${id} was removed — it cannot run on this machine`, "err", id);
    }
    else if (res.error) { toast(`${id}: ${res.error}`, "err", id); }
    else if (action === "start" && res.ok === false) {
      toast(`Start failed for ${id} — see Logs`, "err", id);
    } else if (action === "start") {
      const on = res.status && res.status.portOverride;
      toast(res.note || (res.alreadyRunning ? `${id} was already running`
        : res.slow ? `${id} starting slowly — check logs`
        : `${id} started${on ? ` on port ${on}` : ""}`), res.ok ? "ok" : "err");
      if (res.ok && !res.alreadyRunning && !res.slow) sparkle(id);
    } else {
      if (res.ok) closeAppWindow(id);
      toast(res.ok ? `${id} stopped` : `${id}: could not fully stop (still up)`, res.ok ? "ok" : "err", id);
    }
  } catch (e) {
    toast(`Launcher unreachable: ${e}`, "err");
  }
  busy.delete(id);
  await refresh();
}

// Custom tooltip for the ⓘ tag — delegated, so card re-renders (the 4-second
// poll) cannot kill it the way they kill native title tooltips.
const verTip = document.createElement("div");
verTip.className = "tip";
verTip.hidden = true;
document.body.appendChild(verTip);
document.addEventListener("mouseover", (e) => {
  const el = e.target.closest ? e.target.closest(".ver-info") : null;
  if (!el) { verTip.hidden = true; return; }
  const app = apps.find(a => a.id === el.dataset.id);
  verTip.textContent = app && app.version ? `${app.name} · version ${app.version}` : "version unknown";
  const rect = el.getBoundingClientRect();
  verTip.style.left = Math.max(8, Math.min(rect.left + rect.width / 2, innerWidth - verTip.offsetWidth - 8)) + "px";
  verTip.style.top = (rect.bottom + 6) + "px";
  verTip.hidden = false;
});
document.addEventListener("scroll", () => { verTip.hidden = true; }, true);

// Card actions are delegated from the stable deck container, including the
// per-card options that are rebuilt during status polling.
document.addEventListener("click", (e) => {
  document.querySelectorAll(".card-options[open]").forEach((details) => {
    if (!details.contains(e.target)) details.open = false;
  });
});
groupsEl.addEventListener("click", async (e) => {
  const btn = e.target.closest("button[data-act]");
  if (!btn) return;
  const options = btn.closest(".card-options");
  if (options) {
    options.open = false;
    options.querySelector("summary")?.focus();
  }
  const { act: action, id } = btn.dataset;
  if (action === "open-import") { $("#btn-install").click(); return; }
  if (action === "open-featured") { $("#btn-featured").click(); return; }
  const app = apps.find(a => a.id === id);
  if (!app) return;
  if (action === "verinfo") { openInspector(app); return; }
  if (action === "update") { await updateFromOrigin(app); return; }
  if (action === "storage") { openStorage(app); return; }
  if (action === "lifecycle") { openLifecycle(app); return; }
  if (action === "terminal") {
    // ⌨ CLI tools: their home is a shell in the app's folder.
    try {
      const res = await api("/api/terminal", { id });
      toast(res.note || res.error || "", res.ok ? "ok" : "err");
    } catch (e) { toast(`Could not open a terminal: ${e}`, "err"); }
    return;
  }
  if (action === "open") {
    // On-demand cards spin up on click: a stopped on-demand app starts
    // first, then opens — the tray's menu rows already behave this way.
    if (!app.running && app.policy && app.policy.mode === "onDemand") {
      await act(id, "start");
      app = apps.find(a => a.id === id) || app;
    }
    openApp(app);
  } else if (action === "restart") {
    // Composed from the existing stop/start actions — the server needs no
    // restart endpoint and keeps its busy/toast/reporting behavior. A custom
    // port the app is running on is kept across the restart.
    const keep = app.portOverride;
    await act(id, "stop");
    await act(id, "start", keep || undefined);
  } else if (action === "pin") {
    if (pinnedApps.has(id)) pinnedApps.delete(id);
    else pinnedApps.add(id);
    savePins();
    render();
    return;
  } else if (action === "start" || action === "stop") {
    await act(id, action);
  } else if (action === "port") {
    const raw = prompt(`Custom port for ${app.name} (configured: ${app.port})`, "");
    if (raw == null) return;
    const p = parseInt(raw, 10);
    if (!Number.isInteger(p) || p < 1024 || p > 65535) {
      toast(`${id}: port must be a number between 1024 and 65535`, "err");
      return;
    }
    await act(id, "start", p);
  } else if (action === "network-toggle") {
    // The Wi-Fi option flips sharing state and moves the card between
    // sections, just like dragging the card to the other section.
    if (app.desktop) return;
    const enabled = !app.network;
    if (enabled && !confirm(`Put “${app.name}” on the Wi-Fi?\nEvery device on this network will be able to open it.`)) return;
    await flipNetwork(id, enabled);
  } else if (action === "privilege") {
    // 🔓 per-app privilege: on = the app launches with its own OS-level
    // elevation (macOS TCC identity bundle / Windows UAC / Linux
    // pkexec-sudo); off = back under the launcher's identity. AppDeck
    // itself never gains anything on any OS.
    const enable = !app.elevated;
    let msg;
    if (hostOS === "windows") {
      msg = `Make “${app.name}” start as administrator?\n\nWindows will show the app its own UAC prompt at every start — approve it there. The admin token belongs to the app alone; AppDeck itself never gains admin rights, and other apps keep running as your normal user.`;
    } else if (hostOS === "linux") {
      msg = `Make “${app.name}” start as root?\n\nThe desktop will ask to authorize the app (pkexec, or sudo with NOPASSWD) at every start. Root access belongs to the app alone; AppDeck itself never gains root, and other apps keep running as your user.`;
    } else {
      msg = `Elevate “${app.name}” out of the launcher's macOS identity?\n\nIts privacy grants (Full Disk Access etc.) will belong to the app alone. macOS never asks for these — AppDeck opens the app's own ${app.id}.app in Finder next to the Full Disk Access pane; drag it in there once. AppDeck itself never gains access, and other apps stay Inherited under the launcher's identity.`;
    }
    if (app.running) msg += `\n\n${app.name} is running — it will be stopped and started again right away, so the change applies now.`;
    if (enable && !confirm(msg)) return;
    const res = await api("/api/privilege", { id, enabled: enable });
    toast(res.note || res.error || "saved", res.ok ? "ok" : "err");
    await refresh();
  } else if (action === "show") {
    // Desktop equivalent of Open: bring the app's window to the front.
    if (!app.running) { toast(`${app.name} is not running`, "err"); return; }
    try {
      const res = await api("/api/show", { id });
      if (res.error) toast(`${app.name}: ${res.error}`, "err");
    } catch (e) { toast(`Could not focus ${app.name}: ${e}`, "err"); }
  } else if (action === "expose") {
    // 🔗 expose to the OS: the /Applications link, the Start-menu pin or the
    // .desktop entry (per OS). One toggle icon; the glyph's brightness
    // carries the state, like the Wi-Fi toggle beside it.
    const off = !!app.exposed;
    try {
      const res = await api("/api/expose", { id, off });
      toast(res.note || res.error || "", res.ok ? "ok" : "err");
    } catch (e) { toast(`Could not ${off ? "remove" : "create"} the link: ${e}`, "err"); }
    await refresh();
  } else if (action === "logs") {
    openLogs(app);
  } else if (action === "datafolder") {
    const res = await api("/api/datafolder", { id });
    if (res.error) toast(res.error, "err");
  } else if (action === "uninstall") {
    // Registry apps: in a folder build the launcher drops their apps.json
    // entry itself; a payload app cannot be deleted from the executable, so
    // there uninstall means the deck stops offering it (restorable from
    // ⚙ Deck tools). Say which one is about to happen.
    const ask = app.source === "registry"
      ? (hostMode === "single-file"
          ? `Uninstall ${app.name}?\n\nThis app is packed inside the launcher. It is removed from the deck and stays removed — restore it any time from Deck tools.`
          : `Uninstall ${app.name}?\n\nThis app is registered in apps.json. Uninstalling removes its entry there and deletes the app folder.`)
      : `Uninstall ${app.name}?\n\nThe app folder will be removed.`;
    if (!confirm(ask)) return;
    const removeData = confirm("Also delete the app's data folder?\n(databases/settings under AppDeck\\appdata)\n\nOK = delete data too   ·   Cancel = keep data");
    const res = await api("/api/uninstall", { id, removeData });
    if (res.error) { toast(res.error, "err"); return; }
    toast(res.note || `${id} uninstalled`, "ok");
    await refresh();
  }
});

const OPEN_MODE_TOAST = {
  intab: "Apps will open as tabs inside this window",
  window: "Apps will open in their own window",
  browser: "Apps will open as tabs in your default browser"
};
function applyOpenModeBtn() {
  $("#openmode-ic").innerHTML = svg({ intab: "columns", window: "maximize", browser: "globe" }[openMode], 15);
  $("#setting-openmode").value = openMode;
}
$("#setting-openmode").addEventListener("change", (e) => {
  openMode = e.target.value;
  try { localStorage.setItem("launcher.openMode", openMode); } catch {}
  applyOpenModeBtn();
  toast(OPEN_MODE_TOAST[openMode]);
});
applyOpenModeBtn();

// Sort-order button: cycles original → A–Z → category → recently used,
// remembered per browser like the open-mode setting.
const SORT_TOAST = {
  original: "Apps in their original order",
  az: "Apps sorted alphabetically",
  cat: "Apps sorted by category within each section",
  recent: "Most recently used first (usage recorded by this launcher)"
};
function applySortBtn() {
  $("#sortmode-ic").innerHTML = svg({ original: "list", az: "sortAsc", cat: "tag", recent: "history" }[sortMode], 15);
  $("#setting-sortmode").value = sortMode;
}
$("#setting-sortmode").addEventListener("change", (e) => {
  sortMode = e.target.value;
  try { localStorage.setItem("launcher.sortMode", sortMode); } catch {}
  applySortBtn();
  toast(SORT_TOAST[sortMode]);
  render();
});
applySortBtn();

// View selection only supports the two maintained layouts.
const VIEW_TOAST = {
  grid: "Grid view — full cards",
  list: "List view — one row per app"
};
let hostVersion = "dev";
let viewMode = "grid";
try {
  const savedView = localStorage.getItem("launcher.viewMode");
  if (savedView === "list") viewMode = "list";
} catch {}
function applyViewMode() {
  groupsEl.classList.remove("view-grid", "view-list");
  groupsEl.classList.add("view-" + viewMode);
  $("#viewmode-ic").innerHTML = svg({ grid: "grid", list: "list" }[viewMode], 15);
  $("#setting-viewmode").value = viewMode;
  document.querySelectorAll("#ch-view [data-view]").forEach(b =>
    b.classList.toggle("active", b.dataset.view === viewMode));
}
$("#setting-viewmode").addEventListener("change", (e) => {
  viewMode = e.target.value;
  try { localStorage.setItem("launcher.viewMode", viewMode); } catch {}
  applyViewMode();
  toast(VIEW_TOAST[viewMode]);
});
applyViewMode();

// Card buttons: hide what you don't use (Wi-Fi sharing, Elevated, Expose,
// Storage, Data folder, Update, Terminal, Uninstall). Per browser, like the
// view/theme settings — hidden keys persist in launcher.cardActions; hidden
// just removes the card button, the feature itself stays put.
const CARD_ACTIONS = [
  ["network",    "Wi-Fi sharing toggle", "The Wi-Fi button — share an app on the Wi-Fi or keep it on this computer"],
  ["privilege",  "Elevated (privilege)", "The privilege toggle — shows only on apps that can use it"],
  ["expose",     "Expose to the OS", "The link button — /Applications link, Start-menu or .desktop pin"],
  ["storage",    "Storage", "The storage button — move app data or the whole app to another drive"],
  ["datafolder", "Data folder", "The folder button — open this app's data folder"],
  ["update",     "Update from origin", "The restart button — only exists on apps imported from a URL"],
  ["terminal",   "Terminal (CLI apps)", "The terminal button — opens a terminal in CLI apps' folders"],
  ["uninstall",  "Uninstall", "The trash button — remove the app from the deck"],
];
let hiddenCardActions = new Set();
try { hiddenCardActions = new Set(JSON.parse(localStorage.getItem("launcher.cardActions") || "[]")); } catch {}
function applyCardActionChecks() {
  document.querySelectorAll("#cardact-details [data-cardact]").forEach(cb => {
    cb.checked = !hiddenCardActions.has(cb.dataset.cardact);
  });
  const shown = CARD_ACTIONS.length - hiddenCardActions.size;
  $("#btn-cardact .mi-label").textContent = `Card buttons — ${shown} of ${CARD_ACTIONS.length} shown`;
}
$("#cardact-details").addEventListener("change", (e) => {
  const cb = e.target.closest("[data-cardact]");
  if (!cb) return;
  cb.checked ? hiddenCardActions.delete(cb.dataset.cardact) : hiddenCardActions.add(cb.dataset.cardact);
  try { localStorage.setItem("launcher.cardActions", JSON.stringify([...hiddenCardActions])); } catch {}
  applyCardActionChecks();
  render();
});
applyCardActionChecks();

// App tab display: brightness + opacity over every embedded app tab (one
// CSS filter on the iframe stage — the apps themselves stay untouched).
// Persisted per browser like the theme. Transparency is the 🖼 dialog's
// window knob (bgConf.transparency) surfaced here for quick access while
// an app tab fills the window: the same native bridge drives it live and the same
// background.conf holds it — saved at once, since the popup has no Apply.
(() => {
  const pop = $("#display-pop"), frames = $("#apptab-frames");
  const bright = $("#dp-bright"), opacity = $("#dp-opacity");
  const bv = $("#dp-bright-v"), ov = $("#dp-opacity-v");
  const trans = $("#dp-trans"), transV = $("#dp-trans-v");
  let saved = { brightness: 100, opacity: 100 };
  try { Object.assign(saved, JSON.parse(localStorage.getItem("launcher.display") || "{}")); } catch {}
  function apply() {
    frames.style.transition = "filter .15s ease";
    frames.style.filter = `brightness(${saved.brightness}%) opacity(${saved.opacity}%)`;
    bright.value = saved.brightness; opacity.value = saved.opacity;
    bv.textContent = saved.brightness + "%"; ov.textContent = saved.opacity + "%";
    try { localStorage.setItem("launcher.display", JSON.stringify(saved)); } catch {}
  }
  function transUi() {
    trans.value = bgConf.transparency || 0;
    transV.textContent = (bgConf.transparency || 0) + "%";
  }
  // Without the native bridge the knob cannot work — grey it out with the
  // same hint the 🖼 dialog shows (bgConfUi does the matching there).
  if (!bgWindowBridge) {
    $("#dp-trans-row").classList.add("bg-knob-off");
    trans.disabled = true;
    $("#dp-trans-hint").hidden = false;
  }
  async function setTransparency(pct) {
    bgConf.transparency = pct;
    transUi();
    if (bgWindowBridge) bgWindowBridge(pct);
    const bg = $("#bg-tr"); // keep the 🖼 dialog's own slider in step
    if (bg) { bg.value = pct; $("#bg-tr-val").textContent = pct + "%"; bgSliderFill(bg); }
    // The settings POST replaces the whole conf, so it carries the applied
    // state (bgSaved) — a pending 🖼 preview must not leak into the save.
    const conf = { ...(bgSaved || bgConf), transparency: pct };
    try {
      const res = await api("/api/backgrounds/settings", conf);
      if (res.ok) bgSaved = conf;
    } catch {}
  }
  trans.addEventListener("input", () => setTransparency(+trans.value));
  $("#dp-reset").addEventListener("click", () => {
    saved = { brightness: 100, opacity: 100 };
    apply();
    if ((bgConf.transparency || 0) !== 0) setTransparency(0);
  });
  $("#apptab-display").addEventListener("click", async (e) => {
    e.stopPropagation();
    if (pop.hidden && bgWindowBridge && bgSaved === null) await loadBackgrounds(); // first open: fetch the applied conf
    transUi();
    pop.hidden = !pop.hidden;
  });
  pop.addEventListener("click", (e) => e.stopPropagation());
  document.addEventListener("click", () => { pop.hidden = true; });
  apply();
})();

// Deck tools (⚙): export the whole setup as a zip; import merges a deck
// export (existing installs are never overwritten). A modal, like Import and
// Logs — no floating menu over the cards.
$("#btn-gear").addEventListener("click", () => openSettings("general"));
// The reachability item doubles as a status light: the value text reads
// green while the dashboard is shared on the Wi-Fi, muted while it is
// limited to this computer. Re-read after every toggle so the light never lies.
function updateLanMenuItem(cur) {
  const val = $("#lan-mode-val");
  val.textContent = cur.dashboardLoopback ? "This computer only" : "Shared on the Wi-Fi";
  val.classList.toggle("on", !cur.dashboardLoopback);
}
function updateAppsVisibilityItem(cur) {
  const val = $("#apps-vis-val");
  val.textContent = cur.hideAppsInventory ? "This computer only" : "Visible on the Wi-Fi";
  val.classList.toggle("on", !cur.hideAppsInventory);
}
// Launch-at-login item doubles as a status light, like the reachability one.
function updateLoginItem(cur) {
  const val = $("#login-val");
  val.textContent = cur.launchAtLogin ? "On" : "Off";
  val.classList.toggle("on", cur.launchAtLogin);
}
async function refreshLoginItem() {
  try { updateLoginItem(await api("/api/launcher-settings")); } catch { /* older launcher */ }
}
$("#menu-launch-login").addEventListener("click", async () => {
  try {
    const cur = await api("/api/launcher-settings");
    const next = !cur.launchAtLogin;
    const res = await api("/api/launcher-settings", { launchAtLogin: next });
    toast(res.note || res.error || "", res.ok ? "ok" : "err");
    if (res.settings) updateLoginItem(res.settings);
  } catch (e) { toast(`Could not change it: ${e}`, "err"); }
});
// Menu bar companion item doubles as a status light, like the others. The
// launcher's GET never echoes trayIcon when it is absent — absent means on.
function updateTrayItem(cur) {
  const val = $("#tray-val");
  const on = cur.trayIcon === undefined ? true : !!cur.trayIcon;
  val.textContent = on ? "On" : "Off";
  val.classList.toggle("on", on);
}
async function refreshTrayItem() {
  try { updateTrayItem(await api("/api/launcher-settings")); } catch { /* older launcher */ }
}
$("#menu-tray-icon").addEventListener("click", async () => {
  try {
    const cur = await api("/api/launcher-settings");
    const next = cur.trayIcon === undefined ? false : !cur.trayIcon;
    const res = await api("/api/launcher-settings", { trayIcon: next });
    toast(res.note || res.error || "", res.ok ? "ok" : "err");
    if (res.settings) updateTrayItem(res.settings);
  } catch (e) { toast(`Could not change it: ${e}`, "err"); }
});
$("#menu-update-apps").addEventListener("click", () => { closeSettings(); updateAllFromOrigin(); });
// Removed payload apps: shown only when this machine uninstalled some. The
// record lives in the data dir and survives launcher updates; restoring
// clears it and unpacks the copy the executable already carries. Forgetting
// (per app, never in bulk) stops offering a removal back — the record itself
// stays for payload apps, so the app stays off the deck.
let removedShelf = { apps: [], forgotten: [] };
async function refreshRemovedApps() {
  const btn = $("#menu-restore-apps"), hint = $("#menu-restore-apps-hint");
  const fbtn = $("#menu-forgotten-apps"), fhint = $("#menu-forgotten-apps-hint");
  removedShelf = { apps: [], forgotten: [] };
  try { removedShelf = await api("/api/apps/removed"); } catch { /* older launcher */ }
  const list = removedShelf.apps || [];
  const flist = removedShelf.forgotten || [];
  const n = list.length, fn = flist.length;
  btn.style.display = n ? "" : "none";
  hint.style.display = n ? "" : "none";
  fbtn.style.display = fn ? "" : "none";
  fhint.style.display = fn ? "" : "none";
  if (n) { const t = document.getElementById("menu-restore-apps-t"); if (t) t.textContent = `Restore removed apps (${n})…`; }
  if (fn) { const t = document.getElementById("menu-forgotten-apps-t"); if (t) t.textContent = `Forgotten apps (${fn})…`; }
}
function openRemovedShelf() { renderRemovedShelf(); $("#removed-overlay").classList.add("show"); }
function removedRowHtml(a, isForgotten) {
  const name = escapeHtml(a.name || a.id);
  const forget = isForgotten ? "" : ` <button class="btn-ghost" data-act="forget" title="Keep it uninstalled — stop offering it here">Forget</button>`;
  return `<div class="removed-row" data-id="${escapeHtml(a.id)}"><span class="removed-name" title="${escapeHtml(a.id)}">${name}</span><button class="btn-ghost" data-act="restore">${svg("history", 14)} Restore</button>${forget}</div>`;
}
function renderRemovedShelf() {
  const list = removedShelf.apps || [], flist = removedShelf.forgotten || [];
  $("#removed-list").innerHTML = list.map((a) => removedRowHtml(a, false)).join("") || `<p class="muted">Nothing removed.</p>`;
  $("#forgotten-section").hidden = !flist.length;
  $("#forgotten-list").innerHTML = flist.map((a) => removedRowHtml(a, true)).join("");
}
async function removedShelfAction(act, id) {
  try {
    const res = await api(`/api/apps/${act}`, { ids: [id] });
    toast(res.note || res.error || act, res.ok ? "ok" : "err");
    if (res.ok) {
      await refreshRemovedApps();
      renderRemovedShelf();
      if (act === "restore") await refresh();
    }
  } catch (e) { toast(`Launcher unreachable: ${e}`, "err"); }
}
$("#menu-restore-apps").addEventListener("click", openRemovedShelf);
$("#menu-forgotten-apps").addEventListener("click", openRemovedShelf);
$("#removed-close").addEventListener("click", () => $("#removed-overlay").classList.remove("show"));
$("#removed-close2").addEventListener("click", () => $("#removed-overlay").classList.remove("show"));
$("#removed-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#removed-overlay").classList.remove("show"); });
$("#removed-list").addEventListener("click", (e) => {
  const act = e.target.closest("button[data-act]")?.dataset.act, row = e.target.closest(".removed-row");
  if (act && row) removedShelfAction(act, row.dataset.id);
});
$("#forgotten-list").addEventListener("click", (e) => {
  const act = e.target.closest("button[data-act]")?.dataset.act, row = e.target.closest(".removed-row");
  if (act && row) removedShelfAction(act, row.dataset.id);
});
$("#removed-restore-all").addEventListener("click", async () => {
  try {
    const res = await api("/api/apps/restore", { all: true });
    toast(res.note || res.error || "restored", res.ok ? "ok" : "err");
    if (res.ok) { await refreshRemovedApps(); renderRemovedShelf(); await refresh(); }
  } catch (e) { toast(`Launcher unreachable: ${e}`, "err"); }
});

$("#menu-apps-visibility").addEventListener("click", async () => {
  let cur = { hideAppsInventory: false };
  try { cur = await api("/api/launcher-settings"); } catch { /* launcher answers below anyway */ }
  const next = !cur.hideAppsInventory;
  const ask = next
    ? "Limit the app list to this computer?\n\nDevices on the Wi-Fi will no longer be able to read which apps this deck runs (the dashboard itself keeps working; cross-port tools like Wi-Fi Guard lose the list from other devices)."
    : "Let the Wi-Fi read the app list again?\n\nAny device on this network will be able to see which apps this deck runs (names and ports only — no logs).";
  if (!confirm(ask)) return;
  try {
    const res = await api("/api/launcher-settings", { hideAppsInventory: next });
    toast(res.note || res.error || "saved", res.ok ? "ok" : "err");
    if (res.ok) updateAppsVisibilityItem({ hideAppsInventory: next });
  } catch (e) { toast(`Launcher unreachable: ${e}`, "err"); }
});
api("/api/launcher-settings").then((cur) => { updateLanMenuItem(cur); updateAppsVisibilityItem(cur); }).catch(() => { /* light stays default until the first toggle */ });
$("#menu-lan-mode").addEventListener("click", async () => {
  let cur = { dashboardLoopback: false };
  try { cur = await api("/api/launcher-settings"); } catch { /* launcher answers below anyway */ }
  const next = !cur.dashboardLoopback;
  const ask = next
    ? "Limit the dashboard to this computer?\n\nFrom the Wi-Fi, the app list and app LOGS will no longer be visible — shared apps keep their own addresses and stay reachable."
    : "Make the dashboard reachable from the Wi-Fi again?\n\nAnyone on this network will be able to see the app list and read app logs.";
  if (!confirm(ask)) return;
  try {
    const res = await api("/api/launcher-settings", { dashboardLoopback: next });
    toast(res.note || res.error || "saved", res.ok ? "ok" : "err");
    if (res.ok) updateLanMenuItem({ dashboardLoopback: next });
  } catch (e) { toast(`Launcher unreachable: ${e}`, "err"); }
});
$("#deck-close").addEventListener("click", () => $("#deck-overlay").classList.remove("show"));
$("#deck-overlay").addEventListener("click", (e) => {
  if (e.target === e.currentTarget) $("#deck-close").click();
});
$("#menu-deck-export").addEventListener("click", () => {
  $("#deck-overlay").classList.remove("show");
  toast("Packing your deck…");
  location.href = "/api/deck/export";
});
$("#menu-deck-import").addEventListener("click", () => {
  $("#deck-overlay").classList.remove("show");
  $("#deck-import-file").click();
});
{
  const di = document.createElement("input");
  di.type = "file";
  di.accept = ".zip";
  di.style.display = "none";
  di.addEventListener("change", async () => {
    const f = di.files[0];
    di.value = "";
    if (!f) return;
    toast(`Importing deck ${f.name}…`);
    busy.add("__deck__");
    try {
      const res = await fetch("/api/deck/import", { method: "POST", body: f });
      const out = await res.json();
      if (out.ok) toast(`Deck imported — ${out.applied} file(s) added, ${out.skipped} already present`, "ok");
      else toast(out.error || "Deck import failed", "err");
    } catch (err) { toast(`Deck import failed: ${err}`, "err"); }
    busy.delete("__deck__");
    await refresh();
  });
  document.body.appendChild(di);
}

// Windows/tabs this dashboard opened, per app id, so Stop / Stop All can
// close them along with the app's server. (Tabs the launcher sent to the
// default browser — browser mode from the native window — live in another
// browser process and cannot be closed from here.)
const openWins = {};

// In-window tabs: each opened app gets an iframe plus a chip in the stage's
// tab bar. Closing a tab closes only the view — the app keeps running until
// Stop / Stop All, mirroring the separate-window behavior.
const openTabs = [];
let activeTabId = null;
// Set by the tab drag below so the click that ends a drag doesn't also
// switch tabs (same pattern as pillDragged on the floating pill).
let apptabDragged = false;

function closeAppWindow(id) {
  const w = openWins[id];
  if (w) {
    delete openWins[id];
    try { w.close(); } catch { /* browser refused — leave the tab */ }
  }
  closeAppTab(id);
}

function openApp(app) {
  if (openMode === "intab") { openInWindowTab(app); return; }
  if (openMode === "browser") {
    // Only the page inside the launcher's own window (PC loopback) has no
    // tabs — hand the URL to the default browser instead. Phones/dashboards
    // opened over Wi-Fi just open a normal tab locally.
    const onPC = location.hostname === "127.0.0.1" || location.hostname === "[::1]";
    if (hostWindow === "native" && onPC) {
      // Default browser: untrackable from here; launcher-hosted apps close
      // such tabs themselves via the injected watchdog once stopped.
      api("/api/open", { url: app.url }).catch(() => window.open(app.url, "_blank"));
      return;
    }
    openWins[app.id] = window.open(app.url, "_blank");
    return;
  }
  // No "noopener" on purpose: keeping the opener reference is what lets
  // Stop / Stop All close this window later.
  openWins[app.id] = window.open(app.url, "_blank", "width=1280,height=880");
}

function openInWindowTab(app) {
  let t = openTabs.find(t => t.id === app.id);
  if (!t) {
    t = { id: app.id, name: app.name, icon: app.icon, url: app.url };
    openTabs.push(t);
    const frame = document.createElement("iframe");
    frame.id = "apptab-frame-" + t.id;
    // Same-origin embed: WebKit blank-frames cross-origin apps (XFO/COEP
    // servers, storage- and service-worker-denied third-party frames), so
    // the tab loads the app through /embed/<id>/ on this origin. The token
    // busts WebKit's HTTP cache per page load: a launcher update that
    // changes embed headers must never be shadowed by a cached copy of the
    // old response (a 304 revalidation keeps stale headers alive forever).
    // Open-in-browser keeps the app's real URL — that path needs no shim.
    frame.src = "/embed/" + encodeURIComponent(app.id) + "/?_deck=" + Math.random().toString(36).slice(2);
    frame.title = t.name;
    $("#apptab-frames").appendChild(frame);
  }
  activeTabId = t.id;
  renderApptabs();
}

function closeAppTab(id) {
  const i = openTabs.findIndex(t => t.id === id);
  if (i < 0) return;
  openTabs.splice(i, 1);
  const f = document.getElementById("apptab-frame-" + id);
  if (f) f.remove();
  if (activeTabId === id) activeTabId = openTabs.length ? openTabs[openTabs.length - 1].id : null;
  renderApptabs();
}

function renderApptabs() {
  // The tab bar is persistent: "⌂ Dashboard" is its first tab, open app tabs
  // sit beside it in the dragged order — switching works from both the
  // dashboard and the stage.
  const bar = $("#apptab-bar");
  bar.hidden = openTabs.length === 0;
  document.body.classList.toggle("tabbed", openTabs.length > 0);
  var pillEl = $("#apptab-pill");
  if (pillEl) pillEl.hidden = true; // superseded by the persistent bar
  const showStage = openTabs.length > 0 && activeTabId !== null;
  $("#apptab-stage").hidden = !showStage;
  if (!openTabs.length) return;
  const list = $("#apptab-list");
  list.innerHTML =
    '<button class="apptab ' + (activeTabId === null ? "active" : "") + '" data-tab="dashboard" title="AppDeck dashboard — double-click to reload">⌂ Dashboard</button>' +
    orderedOpenTabs().map(t => `
    <button class="apptab ${t.id === activeTabId ? "active" : ""}" data-tab="${t.id}" title="${escapeHtml(t.name)} — double-click to reload">
      <span>${iconArt(apps.find(a => a.id === t.id) || t)}</span><span class="name">${escapeHtml(t.name)}</span><span class="x" data-tabclose="${t.id}" title="Close tab">✕</span>
    </button>`).join("");
  list.querySelectorAll(".apptab").forEach(el => {
    el.addEventListener("click", (e) => {
      if (e.target.closest("[data-tabclose]")) return;
      if (apptabDragged) { apptabDragged = false; return; } // a drag must not count as a click
      activeTabId = el.dataset.tab === "dashboard" ? null : el.dataset.tab;
      renderApptabs();
    });
    el.addEventListener("dblclick", (e) => {
      if (e.target.closest("[data-tabclose]")) return;
      if (el.dataset.tab === "dashboard") { location.reload(); return; }
      const f = document.getElementById("apptab-frame-" + el.dataset.tab);
      if (f) { f.src = f.src; toast(el.dataset.tab + " page reloaded", "ok"); }
    });
  });
  list.querySelectorAll("[data-tabclose]").forEach(el => {
    el.addEventListener("click", (e) => { e.stopPropagation(); closeAppTab(el.dataset.tabclose); });
  });
  document.querySelectorAll("#apptab-frames iframe").forEach(f => {
    f.style.display = f.id === "apptab-frame-" + activeTabId ? "" : "none";
  });
  // Switching to a tab scrolled out of the bar must reveal it, not leave
  // the user staring at whichever chips happen to be in view.
  const act = list.querySelector(".apptab.active");
  if (act) act.scrollIntoView({ block: "nearest", inline: "nearest" });
}

// Dragged tab order, saved per browser (same store pattern as the pill
// position). Ids of tabs that are no longer open are ignored; tabs opened
// after the last drag keep their open order behind the saved ones.
function savedTabOrder() {
  try {
    const v = JSON.parse(localStorage.getItem("launcher.apptabOrder") || "[]");
    return Array.isArray(v) ? v.filter(x => typeof x === "string") : [];
  } catch { return []; }
}

function orderedOpenTabs() {
  const byId = new Map(openTabs.map(t => [t.id, t]));
  const saved = savedTabOrder().map(id => byId.get(id)).filter(Boolean);
  return saved.concat(openTabs.filter(t => !saved.includes(t)));
}

// Drag app tabs sideways to reorder them; Dashboard stays pinned first.
// Pointer events (like the pill drag above) rather than HTML5 drag-and-drop,
// so it behaves the same in the native window; mouse only — touch keeps
// tap-to-switch and the bar's horizontal scroll.
(() => {
  const list = $("#apptab-list");
  if (!list) return;
  let drag = null;

  const tabButtons = () =>
    Array.from(list.querySelectorAll('.apptab[data-tab]:not([data-tab="dashboard"])'));

  list.addEventListener("pointerdown", (e) => {
    const btn = e.target.closest('.apptab[data-tab]');
    // the close ✕ is a click affordance, never a drag handle: arming the
    // drag here would pointer-capture the chip and retarget the click away
    // from the ✕'s own handler, so the tab would switch instead of close
    if (!btn || e.target.closest("[data-tabclose]")) return;
    if (e.button !== 0 || e.pointerType !== "mouse" || btn.dataset.tab === "dashboard") return;
    drag = { id: btn.dataset.tab, btn, sx: e.clientX, sy: e.clientY, moved: false };
    try { btn.setPointerCapture(e.pointerId); } catch { /* synthetic pointer — fine without capture */ }
  });

  list.addEventListener("pointermove", (e) => {
    if (!drag) return;
    if (!drag.moved && Math.hypot(e.clientX - drag.sx, e.clientY - drag.sy) < 5) return;
    drag.moved = true;
    drag.btn.classList.add("dragging");
    // swap with whichever neighbour the pointer has crossed
    for (const b of tabButtons()) {
      if (b === drag.btn) continue;
      const r = b.getBoundingClientRect();
      if (e.clientX < r.left || e.clientX > r.right) continue;
      if (b.compareDocumentPosition(drag.btn) & Node.DOCUMENT_POSITION_FOLLOWING) {
        list.insertBefore(drag.btn, b); // dragged sits after b: moving left
      } else {
        list.insertBefore(drag.btn, b.nextSibling); // dragged before b: moving right
      }
      break;
    }
  });

  const finish = () => {
    if (!drag) return;
    const moved = drag.moved;
    drag.btn.classList.remove("dragging");
    drag = null;
    if (!moved) return;
    // persist and sync the model so future renders keep the dragged order
    const order = tabButtons().map(b => b.dataset.tab);
    try { localStorage.setItem("launcher.apptabOrder", JSON.stringify(order)); } catch {}
    const byId = new Map(openTabs.map(t => [t.id, t]));
    openTabs.length = 0;
    order.forEach(id => { const t = byId.get(id); if (t) openTabs.push(t); });
    renderApptabs();
    apptabDragged = true;
    // the re-render detaches the dragged chip, so the gesture's trailing
    // click usually never fires; expire the flag so a later genuine click
    // (e.g. keyboard Enter, which has no pointerdown to reset it) isn't eaten
    setTimeout(() => { apptabDragged = false; }, 0);
  };
  list.addEventListener("pointerup", finish);
  list.addEventListener("pointercancel", finish);
})();

// Drag the pill anywhere and it stays there (saved per browser). A plain
// click still returns to the open tabs; double-click resets the position.
let pillDragged = false;
(() => {
  const pill = $("#apptab-pill");
  const clamp = (x, y) => [
    Math.min(Math.max(0, x), innerWidth - pill.offsetWidth),
    Math.min(Math.max(0, y), innerHeight - pill.offsetHeight),
  ];
  try {
    const pos = JSON.parse(localStorage.getItem("launcher.tabPill") || "null");
    if (pos && Number.isFinite(pos.x) && Number.isFinite(pos.y)) {
      const [x, y] = clamp(pos.x, pos.y);
      pill.style.left = x + "px";
      pill.style.top = y + "px";
      pill.style.right = "auto";
      pill.style.bottom = "auto";
    }
  } catch {}
  let drag = null;
  pill.addEventListener("pointerdown", (e) => {
    if (e.button !== 0) return;
    const r = pill.getBoundingClientRect();
    drag = { dx: e.clientX - r.left, dy: e.clientY - r.top, sx: e.clientX, sy: e.clientY, moved: false };
    pill.setPointerCapture(e.pointerId);
  });
  pill.addEventListener("pointermove", (e) => {
    if (!drag) return;
    if (!drag.moved && Math.hypot(e.clientX - drag.sx, e.clientY - drag.sy) < 5) return;
    drag.moved = true;
    pill.style.cursor = "grabbing";
    const [x, y] = clamp(e.clientX - drag.dx, e.clientY - drag.dy);
    pill.style.left = x + "px";
    pill.style.top = y + "px";
    pill.style.right = "auto";
    pill.style.bottom = "auto";
  });
  const finish = () => {
    if (!drag) return;
    pillDragged = drag.moved;
    pill.style.cursor = "";
    if (drag.moved) {
      try {
        localStorage.setItem("launcher.tabPill", JSON.stringify({
          x: parseFloat(pill.style.left) || 0, y: parseFloat(pill.style.top) || 0,
        }));
      } catch {}
    }
    drag = null;
  };
  pill.addEventListener("pointerup", finish);
  pill.addEventListener("pointercancel", finish);
  pill.addEventListener("dblclick", () => {
    try { localStorage.removeItem("launcher.tabPill"); } catch {}
    pill.style.left = pill.style.top = pill.style.right = pill.style.bottom = "";
  });
})();

$("#apptab-pill").addEventListener("click", () => {
  if (pillDragged) { pillDragged = false; return; } // a drag must not count as a click
  activeTabId = openTabs.length ? openTabs[openTabs.length - 1].id : null;
  renderApptabs();
});

$("#btn-start-all").addEventListener("click", async () => {
  toast("Starting all stopped apps…");
  await api("/api/all/start", {});
  await refresh();
  toast("Start All done", "ok");
});
$("#btn-stop-all").addEventListener("click", async () => {
  toast("Stopping all running apps…");
  await api("/api/all/stop", {});
  await refresh();
  // Close the windows/tabs of everything that is no longer running.
  apps.forEach(a => { if (!a.running) closeAppWindow(a.id); });
  toast("Stop All done", "ok");
});

// Quit: stops the apps the launcher started, then the launcher exits. The
// launcher window closes with it in window mode.
$("#btn-quit").addEventListener("click", async () => {
  // The launcher's own WKWebView window shows no JS confirm() dialogs (no
  // JS UIDelegate is implemented), so confirm() would silently return false
  // and the button would do nothing; browser/app-window hosts keep the
  // confirmation.
  if (hostWindow !== "native" &&
      !confirm("Quit the launcher?\n\nApps it started will be stopped.")) return;
  // Best effort: close the app windows/tabs this page opened before quitting.
  Object.keys(openWins).forEach(closeAppWindow);
  openTabs.slice().forEach(t => closeAppTab(t.id));
  try { await api("/api/shutdown", {}); } catch { /* already gone */ }
  toast("Launcher stopping…", "ok");
  setTimeout(() => window.close(), 600);
});

// One-time firewall fix: adds allow-rules for the launcher + every network
// app through a single Windows admin prompt, so individual apps stop asking.
$("#btn-firewall").addEventListener("click", async () => {
  toast("Approve the admin prompt to whitelist every app at once…");
  try {
    const res = await api("/api/firewall", {});
    if (res.ok) toast(res.note || "Done — apps will stop asking for network permission", "ok");
    else toast(res.error || "Could not start the firewall fix", "err");
  } catch (e) {
    toast(`Launcher unreachable: ${e}`, "err");
  }
});

// Window-mode keepalive: an open dashboard holds the launcher alive; closing
// the last one (pagehide) fires a bye beacon so the launcher shuts down.
setInterval(() => { fetch("/api/heartbeat", { method: "POST" }).catch(() => {}); }, 4000);
addEventListener("pagehide", () => {
  navigator.sendBeacon?.("/api/heartbeat?bye=1");
});

// ---- per-app storage (💾 card action, between privilege and datafolder) ----
// Local (default), Data only, or Whole app onto a named location. The
// backend swaps the canonical path to a link; this dialog reports the live
// sizes (so the right choice is obvious) and posts the user's pick.
let storageApp = null;
const fmtMB = (n) => !n ? "0" : n >= 1024 ** 3 ? (n / 1024 ** 3).toFixed(1) + " GB"
  : n >= 1024 ** 2 ? Math.round(n / 1024 ** 2) + " MB" : Math.max(1, Math.round(n / 1024)) + " KB";
async function openStorage(a) {
  storageApp = a;
  $("#app-storage-title").innerHTML = `${svg("disk", 16)} Storage — ${iconArt(a)} ${escapeHtml(a.name)}`;
  openPage("app-storage-overlay");
  await refreshStorage();
}
async function refreshStorage() {
  const body = $("#app-storage-body");
  body.innerHTML = `<p class="muted">Reading sizes…</p>`;
  try {
    renderStorage(await api(`/api/appstorage?id=${encodeURIComponent(storageApp.id)}`));
  } catch (e) {
    body.innerHTML = `<p class="muted">Could not read storage info: ${e}</p>`;
  }
}
function renderStorage(st) {
  const ext = st.mode && st.mode !== "local";
  const haveLocs = st.locations && st.locations.length;
  const locOpts = (haveLocs ? st.locations : []).map(l =>
    `<option value="${escapeHtml(l.name)}" ${l.name === st.location ? "selected" : ""}>${escapeHtml(l.name)} — ${escapeHtml(l.path)}</option>`).join("");
  const radio = (v, lab, sub, dis) =>
    `<label class="chk" style="align-items:flex-start;gap:8px"><input type="radio" name="st-mode" value="${v}" ${dis ? "disabled" : ""}>` +
    `<span><b>${lab}</b><br><span class="muted" style="font-size:12px">${sub}</span></span></label>`;
  $("#app-storage-body").innerHTML = `
    <p class="muted">Now: <b>${ext ? escapeHtml(st.location) : "local disk"}</b> · program folder ${fmtMB(st.programBytes)} · app data ${fmtMB(st.dataBytes)} · the app log always stays local.</p>
    ${radio("local", "Local (default)", "everything on this disk")}
    ${radio("data", "Data only → chosen location", "the binary stays local; the app's data dir (APPDECK_DATA) moves", !haveLocs)}
    ${radio("app", "Whole app → chosen location", "program folder + data move; the drive must be connected to start the app", !haveLocs)}
    ${haveLocs
      ? `<select id="st-loc" class="modal-input" style="width:100%" aria-label="Location">${locOpts}</select>`
      : `<p class="muted">No locations yet — add one below (an external drive, another volume).</p>`}
    <div class="preview-bar">
      <button class="btn-primary" id="st-go"><span data-ic="disk" data-ics="14"></span> Apply</button>
      ${ext ? `<button class="btn-ghost" id="st-reset" title="Move everything back to this disk (the external copy stays on its drive)"><span data-ic="retry" data-ics="14"></span> Reset to local</button>` : ""}
      ${st.savedCopies && st.savedCopies.length ? `<button class="btn-ghost" id="st-cleanup" title="Delete the kept copies of the previous placement"><span data-ic="trash" data-ics="14"></span> Saved copies (${st.savedCopies.length})</button>` : ""}
    </div>
    <hr>
    <label>Add a location — a short name and its folder (mount the drive first)
      <span style="display:flex;gap:8px">
        <input id="st-newname" class="modal-input" placeholder="T7" style="flex:1">
        <input id="st-newpath" class="modal-input" placeholder="/Volumes/T7/…" style="flex:2">
        <button class="btn ghost" id="st-addloc">Add</button>
      </span>
    </label>`;
  document.querySelector(`input[name=st-mode][value=${ext ? st.mode : "local"}]`)?.click();
  const storageOp = async (payload, what) => {
    try {
      const res = await api("/api/appstorage", Object.assign({ id: storageApp.id }, payload));
      toast(res.note || res.error || what, res.ok ? "ok" : "err");
      if (res.ok) await refreshStorage();
    } catch (e) { toast(`${what} failed: ${e}`, "err"); }
  };
  $("#st-go").addEventListener("click", () => {
    const picked = document.querySelector("input[name=st-mode]:checked");
    if (!picked) return;
    if (picked.value !== "local" && !$("#st-loc")) { toast("Add a location first", "err"); return; }
    storageOp({ op: "move", mode: picked.value, location: $("#st-loc") ? $("#st-loc").value : "" }, "Move");
  });
  $("#st-reset")?.addEventListener("click", () => storageOp({ op: "reset" }, "Reset"));
  $("#st-cleanup")?.addEventListener("click", () => {
    if (confirm("Delete the saved copies of this app's previous placement?"))
      storageOp({ op: "cleanup" }, "Cleanup");
  });
  $("#st-addloc").addEventListener("click", async () => {
    try {
      const res = await api("/api/appstorage", { op: "location", name: $("#st-newname").value, path: $("#st-newpath").value });
      toast(res.note || res.error || "", res.ok ? "ok" : "err");
      if (res.ok) await refreshStorage();
    } catch (e) { toast(`Adding the location failed: ${e}`, "err"); }
  });
}
$("#app-storage-close").addEventListener("click", () => $("#app-storage-overlay").classList.remove("show"));
$("#app-storage-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#app-storage-close").click(); });

let logLines = [];             // the raw tail, kept for the filter
let logFollow = true;          // autoscroll only while the user is at the bottom anyway
function openLogs(app) {
  logApp = app.id;
  $("#log-filter").value = "";
  $("#log-title").innerHTML = `${iconArt(app)} ${escapeHtml(app.name)} — log`;
  openPage("overlay");
  loadLog();
}
async function loadLog() {
  if (!logApp) return;
  try {
    const res = await api(`/api/logs?id=${logApp}`);
    logLines = res.lines || [];
    renderLog();
  } catch { /* launcher restarted */ }
}
function renderLog() {
  const pre = $("#log-body");
  const q = $("#log-filter").value.trim().toLowerCase();
  const lines = q ? logLines.filter(l => l.toLowerCase().includes(q)) : logLines;
  // Follow only when the reader was at the bottom — scrolling up to read
  // history must not be yanked back down by the 2 s refresh.
  const atBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
  pre.textContent = lines.length ? lines.join("\n")
    : (q ? "(no lines match the filter)" : "(no log output yet)");
  if (logFollow && atBottom) pre.scrollTop = pre.scrollHeight;
}
$("#log-filter").addEventListener("input", renderLog);
$("#log-wrap").addEventListener("click", (e) => {
  $("#log-body").classList.toggle("nowrap");
  e.currentTarget.classList.toggle("on");
});
$("#log-follow").addEventListener("click", (e) => {
  logFollow = !logFollow;
  e.currentTarget.classList.toggle("on", logFollow);
  if (logFollow) $("#log-body").scrollTop = $("#log-body").scrollHeight;
});
$("#log-copy").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(logLines.join("\n"));
    toast("Log copied to the clipboard", "ok");
  } catch { toast("Copy failed — the browser blocked clipboard access", "err"); }
});
$("#log-close").addEventListener("click", () => { logApp = null; $("#overlay").classList.remove("show"); });
$("#overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#log-close").click(); });

/* ---------- batch uninstall (select mode) ---------- */
let selectMode = false;
const batchSelected = new Set();

$("#btn-select-mode").addEventListener("click", () => {
  selectMode = !selectMode;
  document.body.classList.toggle("select-mode", selectMode);
  $("#btn-select-mode").innerHTML = selectMode ? svg("close") + " Exit select" : svg("check") + " Select";
  if (!selectMode) batchSelected.clear();
  renderSidebar();
  syncBatchUI();
});

document.addEventListener("change", (e) => {
  const cb = e.target.closest("input[data-batch]");
  if (!cb) return;
  if (cb.checked) batchSelected.add(cb.dataset.batch); else batchSelected.delete(cb.dataset.batch);
  syncBatchUI();
});

function syncBatchUI() {
  const bar = $("#batch-bar");
  const n = batchSelected.size;
  // The bar mirrors select mode itself: out the moment Select is clicked,
  // gone the moment it exits — not tied to whether anything is checked yet.
  bar.hidden = !selectMode;
  $("#batch-count").textContent = n === 0 ? "Select apps to uninstall" : n === 1 ? "1 app selected" : `${n} apps selected`;
  $("#batch-uninstall").disabled = n === 0;
}

function exitSelectMode() {
  selectMode = false;
  document.body.classList.remove("select-mode");
  $("#btn-select-mode").innerHTML = svg("check") + " Select";
  batchSelected.clear();
  renderSidebar();
  syncBatchUI();
}
$("#batch-cancel").addEventListener("click", exitSelectMode);

$("#batch-uninstall").addEventListener("click", () => {
  if (!batchSelected.size) return;
  $("#batch-list").innerHTML = [...batchSelected].map((id) => {
    const a = apps.find((x) => x.id === id);
    return `<div class="batch-row"><span>${a ? escapeHtml(a.name) : escapeHtml(id)}</span>
      <span class="muted">${a && a.running ? "running — will be stopped first" : "stopped"}</span></div>`;
  }).join("");
  $("#batch-data-modal").checked = false;
  $("#batch-overlay").classList.add("show");
});
$("#batch-close").addEventListener("click", () => $("#batch-overlay").classList.remove("show"));
$("#batch-cancel2").addEventListener("click", () => $("#batch-overlay").classList.remove("show"));
$("#batch-overlay").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#batch-overlay").classList.remove("show"); });
$("#batch-go").addEventListener("click", async () => {
  const removeData = $("#batch-data-modal").checked;
  $("#batch-overlay").classList.remove("show");
  const ids = [...batchSelected];
  try {
    const res = await api("/api/uninstall/batch", { ids, removeData });
    const skipped = (res.results || []).filter((r) => !r.ok);
    const firstErr = skipped[0] ? ` — ${skipped[0].name || skipped[0].id}: ${skipped[0].error}` : "";
    toast(`Uninstalled ${res.uninstalled} app${res.uninstalled === 1 ? "" : "s"}${skipped.length ? ` · ${skipped.length} skipped${firstErr}` : ""}`,
      skipped.length ? "err" : "ok");
    exitSelectMode();
    refresh();
  } catch (e) { toast("Batch uninstall failed: " + e.message, "err"); }
});

refresh();

// The tray's "Search apps…" item opens the dashboard with #palette: the
// quick switcher auto-opens (and the hash is cleaned so a reload doesn't
// re-pop it).
if (location.hash === "#palette") {
  history.replaceState(null, "", location.pathname + location.search);
  setTimeout(openPalette, 150);
}

// Guided onboarding runs after the first load knows whether the deck is empty.
setTimeout(maybeOnboardFeatured, 800);

setInterval(() => { if (!busy.size) refresh(); }, POLL_MS);
setInterval(() => { if (logApp && $("#overlay").classList.contains("show")) loadLog(); }, 2000);
