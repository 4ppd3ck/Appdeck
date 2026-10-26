// `appdeck new` — scaffolds a new app folder from the embedded templates, so
// a third-party developer goes from zero to a discovered, running app in one
// command. Two kinds: "go" (a stdlib-only process app that demonstrates the
// full contract: ports, host binding, token check, data dir) and "html" (a
// static app hosted inside the launcher process).
//
//	appdeck new <name> [--kind=go|html] [--dir=<path>]
//
// The folder lands in the deck's app container (the Apps group folder beside
// the launcher) unless --dir names another location. Run the launcher
// afterwards and the new app is on the dashboard — nothing to register.
package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:templates
var scaffoldTemplates embed.FS

const scaffoldUsage = `usage: appdeck new <name> [--kind=go|html] [--dir=<path>] [--desc=<text>]

Scaffold a new AppDeck app folder. Kind defaults to "go".
The folder is created in the deck's Apps container unless --dir is given.
`

// runScaffoldCmd is the CLI entry: parses flags, renders the template, and
// prints the next steps. Exit code 1 with a message on any failure.
func runScaffoldCmd(args []string) {
	// Go's flag package stops at the first positional, so `appdeck new
	// "My App" --kind=html` would misparse; pull the flags out by hand so
	// flags and the name can come in any order.
	var flagArgs, positional []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" && a != "--" {
			flagArgs = append(flagArgs, a)
		} else {
			positional = append(positional, a)
		}
	}
	fs2 := flag.NewFlagSet("new", flag.ContinueOnError)
	kind := fs2.String("kind", "go", "template to use: go or html")
	dir := fs2.String("dir", "", "target folder (default: the deck's Apps container)")
	desc := fs2.String("desc", "", "one-line description for the dashboard card")
	if err := fs2.Parse(flagArgs); err != nil {
		fmt.Fprint(os.Stderr, scaffoldUsage)
		os.Exit(1)
	}
	if len(positional) != 1 || (*kind != "go" && *kind != "html") {
		fmt.Fprint(os.Stderr, scaffoldUsage)
		os.Exit(1)
	}
	name := strings.TrimSpace(positional[0])
	if name == "" {
		fmt.Fprint(os.Stderr, scaffoldUsage)
		os.Exit(1)
	}
	target := *dir
	if target == "" {
		cfg := DefaultConfig()
		container, err := (&Server{cfg: cfg}).appsContainerDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "appdeck new: %v\n", err)
			os.Exit(1)
		}
		target = filepath.Join(container, slugify(name))
	}
	app, err := scaffoldApp(name, *kind, *desc, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "appdeck new: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Scaffolded %s app %q in %s\n", *kind, app.Name, target)
	fmt.Println()
	fmt.Println("Next steps:")
	if *kind == "go" {
		fmt.Println("  1. Edit the app files (main.go is a working contract demo)")
		fmt.Println("  2. Start the launcher — the app builds itself on first start")
	} else {
		fmt.Println("  1. Edit index.html — the launcher hosts it in-process")
		fmt.Println("  2. Start the launcher — the app is discovered automatically")
	}
	fmt.Println("  3. The dashboard card shows it; Start/Open from there")
}

// scaffoldRenames maps template file names to their written names. The Go
// template's go.mod ships as go.mod.tmpl: a real go.mod in the tree would
// mark that subtree as a separate Go module, and the go tool then silently
// drops it from the //go:embed patterns.
var scaffoldRenames = map[string]string{
	"go.mod.tmpl": "go.mod",
}

// scaffoldApp renders the kind's template into target with the app's
// identity substituted. Refuses to touch a non-empty folder.
func scaffoldApp(name, kind, desc, target string) (*App, error) {
	if info, err := os.Stat(target); err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("%s exists and is not a folder", target)
		}
		if entries, _ := os.ReadDir(target); len(entries) > 0 {
			return nil, fmt.Errorf("%s already exists and is not empty", target)
		}
	}
	id := slugify(name)
	if id == "" || !validAppID(id) {
		return nil, fmt.Errorf("%q has no usable app id — use letters, digits, spaces", name)
	}
	if desc == "" {
		desc = kindDisplayName(kind) + " app scaffolded by `appdeck new`"
	}
	fields := map[string]string{
		"__APPID__":   id,
		"__APPNAME__": name,
		"__APPDESC__": strings.ReplaceAll(desc, "\"", "'"),
	}
	root := "templates/" + kind + "app"
	if err := fs.WalkDir(scaffoldTemplates, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(scaffoldTemplates, path)
		if err != nil {
			return err
		}
		out := filepath.Join(target, strings.TrimPrefix(strings.TrimPrefix(path, root), "/"))
		if renamed, ok := scaffoldRenames[filepath.Base(out)]; ok {
			out = filepath.Join(filepath.Dir(out), renamed)
		}
		for k, v := range fields {
			data = []byte(strings.ReplaceAll(string(data), k, v))
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, data, 0o644)
	}); err != nil {
		return nil, err
	}
	manifest, err := loadAppManifest(target)
	if err != nil {
		os.RemoveAll(target) // a scaffold must never ship half-broken
		return nil, fmt.Errorf("generated manifest is invalid: %v", err)
	}
	return manifest, nil
}

func kindDisplayName(kind string) string {
	if kind == "go" {
		return "Go"
	}
	return "Static web"
}
