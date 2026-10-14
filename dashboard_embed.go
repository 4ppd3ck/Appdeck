package main

import _ "embed"

// The dashboard is embedded in every build: folder-mode launchers fall back
// to it when index.html is missing beside the exe (bare single-exe
// distribution), so the deck always renders.
//
//go:embed index.html
var dashboardHTML []byte
