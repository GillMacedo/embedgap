// Command brokenembed is a small program with //go:embed mistakes that
// compile and pass go vet, then fail at run time. Run it to see each one
// fail, and run embedgap on this directory to see them found without
// running anything.
package main

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

//go:embed assets
var assets embed.FS

//go:embed templates/*.html
var templates embed.FS

//go:embed migrations/*.sql
var migrations embed.FS

//go:embed all:config
var config embed.FS

// appJS reads a file the assets pattern leaves out, because _app starts
// with an underscore.
func appJS() error {
	web, err := fs.Sub(assets, "assets")
	if err != nil {
		return err
	}
	_, err = fs.ReadFile(web, "_app/app.js")
	return err
}

// staticFiles points fs.Sub at the wrong directory. fs.Sub succeeds, and
// every read from the result fails.
func staticFiles() error {
	web, err := fs.Sub(assets, "static")
	if err != nil {
		return err
	}
	_, err = fs.ReadFile(web, "index.html")
	return err
}

// page uses the wrong extension in a template pattern.
func page() error {
	_, err := template.ParseFS(templates, "templates/*.tmpl")
	return err
}

// migration has a typo in the directory, so it fails for every name.
func migration(name string) error {
	_, err := migrations.ReadFile(path.Join("migration", name+".sql"))
	return err
}

// index works on Linux and macOS, and fails on Windows, where filepath.Join
// uses a backslash.
func index() error {
	_, err := assets.ReadFile(filepath.Join("assets", "index.html"))
	return err
}

// settings is correct.
func settings() error {
	_, err := config.ReadFile("config/app.yaml")
	return err
}

func main() {
	checks := []struct {
		name string
		fn   func() error
	}{
		{"appJS", appJS},
		{"staticFiles", staticFiles},
		{"page", page},
		{"migration", func() error { return migration("0001_init") }},
		{"index", index},
		{"settings", settings},
	}
	failed := 0
	for _, c := range checks {
		if err := c.fn(); err != nil {
			failed++
			fmt.Printf("%s: %v\n", c.name, err)
		} else {
			fmt.Printf("%s: ok\n", c.name)
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
}
