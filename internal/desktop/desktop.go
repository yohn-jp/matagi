// Package desktop hosts the local Matagi interface in a native WebView2
// window. Its only application authority is the local HTTP boundary.
package desktop

import (
	"context"
	"errors"
)

var ErrUnsupported = errors.New("the Matagi desktop shell is only available on Windows")

type NavigationPolicy interface {
	AllowNavigation(uri string) bool
	AllowNewWindow(uri string) bool
}

type Window struct {
	Title   string
	URL     string
	DataDir string
	Policy  NavigationPolicy
}

type Platform interface {
	Open(context.Context, Window) error
	ReportError(title, message string)
}

// Open validates the initial URL against the same policy that protects later
// WebView2 navigations, then opens the platform shell.
func Open(ctx context.Context, p Platform, w Window) error {
	if p == nil {
		return errors.New("desktop platform is unavailable")
	}
	if ctx == nil {
		return errors.New("desktop context is required")
	}
	if w.Policy == nil || !w.Policy.AllowNavigation(w.URL) {
		return errors.New("desktop window URL is outside its navigation policy")
	}
	return p.Open(ctx, w)
}
