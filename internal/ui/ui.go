// Package ui holds the embedded web assets: HTML templates and static files.
package ui

import "embed"

// FS contains the templates and static assets served by the local web UI.
//
//go:embed templates/*.gohtml static/*
var FS embed.FS
