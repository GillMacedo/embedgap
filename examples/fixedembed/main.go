// Command fixedembed is brokenembed with each //go:embed mistake fixed.
// embedgap stays quiet on it, and every read succeeds.
package main

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"os"
	"path"
)

//go:embed all:assets
var assets embed.FS

//go:embed templates/*.html
var templates embed.FS

//go:embed migrations/*.sql
var migrations embed.FS

//go:embed config
var config embed.FS

func appJS() error {
	web, err := fs.Sub(assets, "assets")
	if err != nil {
		return err
	}
	_, err = fs.ReadFile(web, "_app/app.js")
	return err
}

func staticFiles() error {
	web, err := fs.Sub(assets, "assets")
	if err != nil {
		return err
	}
	_, err = fs.ReadFile(web, "index.html")
	return err
}

func page() error {
	_, err := template.ParseFS(templates, "templates/*.html")
	return err
}

func migration(name string) error {
	_, err := migrations.ReadFile(path.Join("migrations", name+".sql"))
	return err
}

func index() error {
	_, err := assets.ReadFile(path.Join("assets", "index.html"))
	return err
}

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
