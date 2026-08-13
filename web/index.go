package web

import (
	"embed"
	"fmt"
	"io/fs"
)

// ShellTmpl is the html template for the web UI shell (all pages).
//
//go:embed templates/shell.html
var ShellTmpl string

//go:embed all:static
var static embed.FS

//go:embed robots.txt
var WebFs embed.FS

func Assets() (fs.FS, error) {
	subFS, err := fs.Sub(static, "static")
	if err != nil {
		return nil, fmt.Errorf("failed to get static assets sub-filesystem: %w", err)
	}

	return subFS, nil
}
