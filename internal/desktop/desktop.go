// Package desktop hosts the local Matagi interface in a native WebView2
// window. Its only application authority is the local HTTP boundary.
package desktop

import (
	"context"
	"errors"
)

var ErrUnsupported = errors.New("the Matagi desktop shell is only available on Windows")

var ErrAlreadyRunning = errors.New("Matagi desktop is already running for this user")

var ErrWebView2Missing = errors.New("the Microsoft Edge WebView2 Runtime is not installed")

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
	RuntimeVersion() (string, error)
	AcquireInstance() (release func(), err error)
	Activate() error
	Open(context.Context, Window) error
	ReportError(title, message string)
}

// Preflight performs Windows prerequisites before Matagi composes runtime or
// tunnel authority.
func Preflight(p Platform) (version string, release func(), err error) {
	if p == nil {
		return "", nil, errors.New("desktop platform is unavailable")
	}
	version, err = p.RuntimeVersion()
	if err != nil {
		return "", nil, err
	}
	if version == "" {
		return "", nil, ErrWebView2Missing
	}
	release, err = p.AcquireInstance()
	if err != nil {
		return "", nil, err
	}
	return version, release, nil
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
