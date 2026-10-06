// Package ui renders Matagi's desktop using Hachidori's shared visual-system
// tokens and primitives for first run and the operational workspace.
package ui

import (
	_ "embed"
	"html/template"
)

//go:embed system.css
var systemCSS string

//go:embed workspace.css
var workspaceCSS string

// CSS is the visual system stylesheet, for inlining into a page's <style>
// (the pages' Content-Security-Policy admits inline styles only).
func CSS() template.CSS { return template.CSS(systemCSS + "\n" + workspaceCSS) }
