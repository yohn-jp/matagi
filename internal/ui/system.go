// Package ui renders Matagi's desktop using shared visual-system tokens and
// primitives for first run and the operational workspace.
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

func mustParseTemplates(name, source string, funcs template.FuncMap, overlays ...string) *template.Template {
	parsed, err := template.New(name).Funcs(funcs).Parse(source)
	if err != nil {
		panic(err)
	}
	for _, overlay := range overlays {
		parsed, err = parsed.Parse(overlay)
		if err != nil {
			panic(err)
		}
	}
	return parsed
}
