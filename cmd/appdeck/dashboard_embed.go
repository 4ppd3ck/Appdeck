package main

import (
	"embed"
	"io/fs"
	"strings"
)

// Dashboard sources are embedded in every build. Folder mode serves the
// standalone assets beside index.html; the embedded fallback keeps the page
// available when the launcher is moved without its web files.
//
//go:embed index.html dashboard.css dashboard.js
var dashboardSources embed.FS

var dashboardHTML = buildDashboardFallback()

func buildDashboardFallback() []byte {
	index, err := fs.ReadFile(dashboardSources, "index.html")
	if err != nil {
		return nil
	}
	css, err := fs.ReadFile(dashboardSources, "dashboard.css")
	if err != nil {
		return index
	}
	link := `<link rel="stylesheet" href="/dashboard.css">`
	style := "<style>\n" + strings.ReplaceAll(string(css), "</style", "<\\/style") + "\n</style>"
	return []byte(strings.Replace(string(index), link, style, 1))
}

func readDashboardSource(name string) ([]byte, error) {
	return fs.ReadFile(dashboardSources, name)
}
