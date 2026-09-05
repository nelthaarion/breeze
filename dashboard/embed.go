package dashboard

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nelthaarion/breeze"
)

//go:embed templates/views/*.html
var viewsFS embed.FS

//go:embed templates/components/*.html
var componentsFS embed.FS

//go:embed templates/public/*
var publicFS embed.FS

// writeTemplates extracts the embedded template files to a temporary directory
// and returns the directory path. The directory is removed by the OS on next
// reboot (or when the process exits, on some platforms).
func writeTemplates() (string, error) {
	dir, err := os.MkdirTemp("", "breeze-dashboard-*")
	if err != nil {
		return "", fmt.Errorf("dashboard: failed to create temp dir: %w", err)
	}

	// Write views
	viewsDir := filepath.Join(dir, "views")
	if err := os.MkdirAll(viewsDir, 0755); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	entries, err := viewsFS.ReadDir("templates/views")
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	for _, e := range entries {
		data, err := viewsFS.ReadFile("templates/views/" + e.Name())
		if err != nil {
			os.RemoveAll(dir)
			return "", err
		}
		if err := os.WriteFile(filepath.Join(viewsDir, e.Name()), data, 0644); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}

	// Write components
	compDir := filepath.Join(dir, "components")
	if err := os.MkdirAll(compDir, 0755); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	entries, err = componentsFS.ReadDir("templates/components")
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	for _, e := range entries {
		data, err := componentsFS.ReadFile("templates/components/" + e.Name())
		if err != nil {
			os.RemoveAll(dir)
			return "", err
		}
		if err := os.WriteFile(filepath.Join(compDir, e.Name()), data, 0644); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}

	// Write public assets
	pubDir := filepath.Join(dir, "public")
	if err := os.MkdirAll(pubDir, 0755); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	err = fs.WalkDir(publicFS, "templates/public", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := publicFS.ReadFile(path)
		if err != nil {
			return err
		}
		relPath := strings.TrimPrefix(path, "templates/public/")
		return os.WriteFile(filepath.Join(pubDir, relPath), data, 0644)
	})
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}

	return dir, nil
}

// templateEngine creates a Breeze TemplateEngine pointing at the extracted
// template directory. The engine uses the standard Breeze SPA runtime,
// layout system, and component system.
func templateEngine(dir string) *breeze.TemplateEngine {
	return breeze.NewTemplateEngine(breeze.TemplateConfig{
		ViewsDir:      filepath.Join(dir, "views"),
		ComponentsDir: filepath.Join(dir, "components"),
		LayoutFile:    filepath.Join(dir, "views", "layout.html"),
		DevMode:       false,
	})
}
