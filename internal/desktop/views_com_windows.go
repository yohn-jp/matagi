//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/combridge"
	"golang.org/x/sys/windows"
)

// These slots follow the WebView2 IDL vtables in the pinned go-webview2
// module. IUnknown occupies slots 0-2. The generated pkg/webview2 bindings
// register callbacks with by-value structs at package initialization, which
// panics on supported Windows Go runners; this host uses scalar COM calls.
const (
	viewsEnvironmentCreateControllerSlot = 3

	viewsControllerPutIsVisibleSlot                      = 4
	viewsControllerPutBoundsSlot                         = 6
	viewsControllerMoveFocusSlot                         = 12
	viewsControllerNotifyParentWindowPositionChangedSlot = 23
	viewsControllerCloseSlot                             = 24
	viewsControllerGetCoreWebView2Slot                   = 25

	viewsCoreWebView2GetSettingsSlot         = 3
	viewsCoreWebView2NavigateSlot            = 5
	viewsCoreWebView2AddProcessFailedSlot    = 25
	viewsCoreWebView2RemoveProcessFailedSlot = 26

	viewsSettingsPutIsWebMessageEnabledSlot           = 6
	viewsSettingsPutIsStatusBarEnabledSlot            = 10
	viewsSettingsPutAreDevToolsEnabledSlot            = 12
	viewsSettingsPutAreDefaultContextMenusEnabledSlot = 14
	viewsSettingsPutAreHostObjectsAllowedSlot         = 16

	viewsProcessFailedGetKindSlot = 3
)

type viewsRECT struct {
	Left, Top, Right, Bottom int32
}

type viewsEnvironment struct{ pointer unsafe.Pointer }

func (e *viewsEnvironment) AddRef() {
	if e != nil && e.pointer != nil {
		comCall(e.pointer, 1)
	}
}

func (e *viewsEnvironment) CreateCoreWebView2Controller(parentWindow, completedHandler uintptr) error {
	if e == nil || e.pointer == nil {
		return errors.New("WebView2 environment is unavailable")
	}
	err := viewsCOMCall(e.pointer, viewsEnvironmentCreateControllerSlot, parentWindow, completedHandler)
	runtime.KeepAlive(e)
	runtime.KeepAlive(completedHandler)
	return err
}

type viewsController struct{ pointer unsafe.Pointer }

func (c *viewsController) AddRef() {
	if c != nil && c.pointer != nil {
		comCall(c.pointer, 1)
	}
}

func (c *viewsController) PutIsVisible(visible bool) error {
	err := viewsCOMCall(c.pointer, viewsControllerPutIsVisibleSlot, viewsBOOL(visible))
	runtime.KeepAlive(c)
	return err
}

func (c *viewsController) PutBounds(bounds viewsRECT) error {
	if err := viewsCOMCall(c.pointer, viewsControllerPutBoundsSlot, uintptr(unsafe.Pointer(&bounds))); err != nil {
		runtime.KeepAlive(&bounds)
		runtime.KeepAlive(c)
		return err
	}
	runtime.KeepAlive(&bounds)
	runtime.KeepAlive(c)
	return nil
}

func (c *viewsController) MoveFocus(reason uintptr) error {
	err := viewsCOMCall(c.pointer, viewsControllerMoveFocusSlot, reason)
	runtime.KeepAlive(c)
	return err
}

func (c *viewsController) NotifyParentWindowPositionChanged() error {
	err := viewsCOMCall(c.pointer, viewsControllerNotifyParentWindowPositionChangedSlot)
	runtime.KeepAlive(c)
	return err
}

func (c *viewsController) Close() error {
	err := viewsCOMCall(c.pointer, viewsControllerCloseSlot)
	runtime.KeepAlive(c)
	return err
}

func (c *viewsController) GetCoreWebView2() (*viewsCoreWebView2, error) {
	if c == nil || c.pointer == nil {
		return nil, errors.New("WebView2 controller is unavailable")
	}
	var pointer unsafe.Pointer
	err := viewsCOMCall(c.pointer, viewsControllerGetCoreWebView2Slot, uintptr(unsafe.Pointer(&pointer)))
	runtime.KeepAlive(&pointer)
	runtime.KeepAlive(c)
	if err != nil {
		if pointer != nil {
			_ = releaseWebView2Object(pointer)
		}
		return nil, err
	}
	if pointer == nil {
		return nil, errors.New("WebView2 controller returned no core view")
	}
	return &viewsCoreWebView2{pointer: pointer}, nil
}

type viewsCoreWebView2 struct{ pointer unsafe.Pointer }

func (v *viewsCoreWebView2) GetSettings() (*viewsSettings, error) {
	if v == nil || v.pointer == nil {
		return nil, errors.New("WebView2 surface is unavailable")
	}
	var pointer unsafe.Pointer
	err := viewsCOMCall(v.pointer, viewsCoreWebView2GetSettingsSlot, uintptr(unsafe.Pointer(&pointer)))
	runtime.KeepAlive(&pointer)
	runtime.KeepAlive(v)
	if err != nil {
		if pointer != nil {
			_ = releaseWebView2Object(pointer)
		}
		return nil, err
	}
	if pointer == nil {
		return nil, errors.New("WebView2 surface returned no settings")
	}
	return &viewsSettings{pointer: pointer}, nil
}

func (v *viewsCoreWebView2) Navigate(uri string) error {
	if v == nil || v.pointer == nil {
		return errors.New("WebView2 surface is unavailable")
	}
	uriPointer, err := windows.UTF16PtrFromString(uri)
	if err != nil {
		return err
	}
	err = viewsCOMCall(v.pointer, viewsCoreWebView2NavigateSlot, uintptr(unsafe.Pointer(uriPointer)))
	runtime.KeepAlive(uriPointer)
	runtime.KeepAlive(v)
	return err
}

func (v *viewsCoreWebView2) AddProcessFailed(handler *eventHandler) (int64, error) {
	if v == nil || v.pointer == nil || handler == nil {
		return 0, errors.New("WebView2 process-failure registration is unavailable")
	}
	var token int64
	err := viewsCOMCall(v.pointer, viewsCoreWebView2AddProcessFailedSlot,
		uintptr(unsafe.Pointer(handler)), uintptr(unsafe.Pointer(&token)))
	runtime.KeepAlive(handler)
	runtime.KeepAlive(&token)
	runtime.KeepAlive(v)
	return token, err
}

func (v *viewsCoreWebView2) RemoveProcessFailed(token int64) error {
	if v == nil || v.pointer == nil {
		return errors.New("WebView2 surface is unavailable")
	}
	err := viewsCOMCall(v.pointer, viewsCoreWebView2RemoveProcessFailedSlot, uintptr(token))
	runtime.KeepAlive(v)
	return err
}

type viewsSettings struct{ pointer unsafe.Pointer }

func (s *viewsSettings) PutIsWebMessageEnabled(enabled bool) error {
	err := viewsCOMCall(s.pointer, viewsSettingsPutIsWebMessageEnabledSlot, viewsBOOL(enabled))
	runtime.KeepAlive(s)
	return err
}

func (s *viewsSettings) PutIsStatusBarEnabled(enabled bool) error {
	err := viewsCOMCall(s.pointer, viewsSettingsPutIsStatusBarEnabledSlot, viewsBOOL(enabled))
	runtime.KeepAlive(s)
	return err
}

func (s *viewsSettings) PutAreDevToolsEnabled(enabled bool) error {
	err := viewsCOMCall(s.pointer, viewsSettingsPutAreDevToolsEnabledSlot, viewsBOOL(enabled))
	runtime.KeepAlive(s)
	return err
}

func (s *viewsSettings) PutAreDefaultContextMenusEnabled(enabled bool) error {
	err := viewsCOMCall(s.pointer, viewsSettingsPutAreDefaultContextMenusEnabledSlot, viewsBOOL(enabled))
	runtime.KeepAlive(s)
	return err
}

func (s *viewsSettings) PutAreHostObjectsAllowed(enabled bool) error {
	err := viewsCOMCall(s.pointer, viewsSettingsPutAreHostObjectsAllowedSlot, viewsBOOL(enabled))
	runtime.KeepAlive(s)
	return err
}

func releaseWebView2Object(object unsafe.Pointer) error {
	if object == nil {
		return nil
	}
	if *(*unsafe.Pointer)(object) == nil {
		return errors.New("WebView2 COM object has no IUnknown vtable")
	}
	comCall(object, 2)
	runtime.KeepAlive(object)
	return nil
}

func viewsBOOL(value bool) uintptr {
	if value {
		return 1
	}
	return 0
}

func viewsCOMCall(object unsafe.Pointer, slot int, args ...uintptr) error {
	if object == nil {
		return errors.New("WebView2 COM object is unavailable")
	}
	if result := comCall(object, slot, args...); result != sOK {
		return fmt.Errorf("WebView2 COM slot %d failed: HRESULT 0x%08x", slot, uint32(result))
	}
	return nil
}

func viewsProcessFailedKind(args unsafe.Pointer) (uint32, error) {
	if args == nil {
		return 0, errors.New("WebView2 process-failure arguments are unavailable")
	}
	var kind uint32
	err := viewsCOMCall(args, viewsProcessFailedGetKindSlot, uintptr(unsafe.Pointer(&kind)))
	runtime.KeepAlive(&kind)
	runtime.KeepAlive(args)
	if err != nil {
		return 0, err
	}
	return kind, nil
}

type viewsControllerCompleted interface {
	combridge.IUnknown
	CreateCoreWebView2ControllerCompleted(result uintptr, controller unsafe.Pointer) uintptr
}

func init() {
	combridge.RegisterVTable[combridge.IUnknown, viewsControllerCompleted](
		"{6c4819f3-c9b7-4260-8127-c9f5bde7f68c}", viewsControllerCompletedInvoke)
}

func viewsControllerCompletedInvoke(this uintptr, result uintptr, controller unsafe.Pointer) uintptr {
	return combridge.Resolve[viewsControllerCompleted](this).CreateCoreWebView2ControllerCompleted(result, controller)
}

type viewsControllerHandler struct {
	host   *nativeViewsHost
	view   *nativeView
	object *combridge.ComObject[viewsControllerCompleted]
}

func newViewsControllerHandler(host *nativeViewsHost, view *nativeView) *viewsControllerHandler {
	handler := &viewsControllerHandler{host: host, view: view}
	handler.object = combridge.New[viewsControllerCompleted](handler)
	return handler
}

func (h *viewsControllerHandler) ref() uintptr { return h.object.Ref() }

func (h *viewsControllerHandler) close() {
	if h != nil && h.object != nil {
		_ = h.object.Close()
		h.object = nil
	}
}

func (h *viewsControllerHandler) CreateCoreWebView2ControllerCompleted(result uintptr, controller unsafe.Pointer) uintptr {
	defer h.close()
	return h.host.controllerCompleted(h.view, result, controller)
}
