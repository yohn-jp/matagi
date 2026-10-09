package desktop

import (
	"context"
	"errors"

	"github.com/yohn-jp/matagi/internal/ui"
)

// ViewHandle is an opaque, process-local identity for one WebView2 controller.
type ViewHandle string

// ViewCallbacks report controller lifecycle changes. They are invoked outside
// the WebView2 STA and never contain COM objects or native window handles.
type ViewCallbacks struct {
	Ready  func(ViewHandle)
	Failed func(ViewHandle, error)
	Closed func(ViewHandle)
}

// ViewConfig contains only the URL admitted for this controller, its isolated
// WebView2 user-data folder, and its origin policy.
type ViewConfig struct {
	URL           string
	ProfileFolder string
	Policy        NavigationPolicy
}

// DIPBounds uses device-independent pixels for native window placement.
type DIPBounds struct {
	X, Y          int32
	Width, Height int32
}

// ViewLocation is the host location a controller may occupy.
type ViewLocation uint8

const (
	ViewIntegrated ViewLocation = iota
	ViewDetached
)

// ViewsHost owns the main native window and every controller created through
// it. Create returns after the asynchronous request is accepted; Ready or
// Failed reports the actual WebView2 completion. Other operations return only
// after the owning STA has completed them.
type ViewsHost interface {
	Create(context.Context, ViewConfig, ViewCallbacks) (ViewHandle, error)
	Select(context.Context, ViewHandle) error
	Hide(context.Context, ViewHandle) error
	Focus(context.Context, ViewHandle) error
	Move(context.Context, ViewHandle, ViewLocation, DIPBounds) error
	Close(context.Context, ViewHandle) error
	CloseAll(context.Context) error
}

var ErrViewNotReady = errors.New("WebView2 controller is not ready")

var ErrViewsHostStopped = errors.New("WebView2 host message loop stopped")

var ErrTrustedViewOwned = errors.New("the trusted Matagi view is owned by the host")

// ErrViewMoveDeferred marks the move operation reserved for the detached-view
// integration leaf. The host exposes the frozen seam while integrated views
// remain the only supported location in this leaf.
var ErrViewMoveDeferred = errors.New("detached view movement is not available")

// ViewPolicy allows exactly one loopback HTTP origin. A trusted policy admits
// only the Matagi UI origin; a service policy admits only the ensured endpoint
// origin. Policies from NewPolicy retain their legacy multi-origin behavior
// for Platform.Open and cannot be used by ViewsHost.
type ViewPolicy struct {
	origin string
	role   viewPolicyRole
}

type viewPolicyRole uint8

const (
	trustedViewPolicy viewPolicyRole = iota + 1
	serviceViewPolicy
)

// NewTrustedViewPolicy creates a UI-only policy from the Matagi loopback URL.
func NewTrustedViewPolicy(uiURL string) (*ViewPolicy, error) {
	origin, err := ui.LoopbackHTTPOrigin(uiURL)
	if err != nil {
		return nil, errors.New("Matagi UI URL must be an IPv4 loopback HTTP URL with a port")
	}
	return &ViewPolicy{origin: origin, role: trustedViewPolicy}, nil
}

// NewServiceViewPolicy creates a policy for the exact loopback origin returned
// by a successful endpoint-ensure operation.
func NewServiceViewPolicy(ensuredURL string) (*ViewPolicy, error) {
	origin, err := ui.LoopbackHTTPOrigin(ensuredURL)
	if err != nil {
		return nil, errors.New("ensured endpoint URL must be an IPv4 loopback HTTP URL with a port")
	}
	return &ViewPolicy{origin: origin, role: serviceViewPolicy}, nil
}

func (p *ViewPolicy) AllowNavigation(uri string) bool {
	if p == nil {
		return false
	}
	origin, err := ui.LoopbackHTTPOrigin(uri)
	return err == nil && origin == p.origin
}

func (*ViewPolicy) AllowNewWindow(string) bool { return false }

func validateViewConfig(config ViewConfig, role viewPolicyRole) error {
	policy, ok := config.Policy.(*ViewPolicy)
	if !ok || policy == nil || policy.role != role {
		return errors.New("view requires a Matagi-created origin policy for its role")
	}
	origin, err := ui.LoopbackHTTPOrigin(config.URL)
	if err != nil || origin != policy.origin || !policy.AllowNavigation(config.URL) {
		return errors.New("view URL is outside its exact loopback origin policy")
	}
	if config.ProfileFolder == "" {
		return errors.New("view requires an endpoint-specific WebView2 profile folder")
	}
	return nil
}

// StartViewsHost creates the main Matagi window and its trusted UI controller.
// The trusted config must use NewTrustedViewPolicy and a dedicated profile
// folder. Service controllers are added later through ViewsHost.Create and
// must use NewServiceViewPolicy with the freshly ensured endpoint URL.
func StartViewsHost(ctx context.Context, trusted ViewConfig, callbacks ViewCallbacks) (ViewsHost, ViewHandle, error) {
	if ctx == nil {
		return nil, "", errors.New("desktop context is required")
	}
	if err := validateViewConfig(trusted, trustedViewPolicy); err != nil {
		return nil, "", err
	}
	return startViewsHost(ctx, trusted, callbacks)
}
