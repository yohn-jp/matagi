//go:build !windows

package desktop

import "context"

func startViewsHost(context.Context, ViewConfig, ViewCallbacks) (ViewsHost, ViewHandle, error) {
	return nil, "", ErrUnsupported
}
