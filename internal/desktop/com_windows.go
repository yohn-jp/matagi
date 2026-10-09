//go:build windows

package desktop

import (
	"errors"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The WebView2 Go binding does not expose navigation-starting or new-window
// events. These three COM slots install the same deny-by-default guard used by
// the Hachidori WebView2 host.
const (
	slotAddNavigationStarting      = 7
	slotAddFrameNavigationStarting = 17
	slotAddPermissionRequested     = 23
	slotAddNewWindowRequested      = 44
	slotNavArgsGetURI              = 3
	slotNavArgsPutCancel           = 8
	slotPermissionArgsPutState     = 7
	slotRemoveNavigationStarting   = 8
	slotRemoveFrameNavigation      = 18
	slotRemovePermissionRequested  = 24
	slotRemoveNewWindowRequested   = 45
	slotNewWindowArgsGetURI        = 3
	slotNewWindowArgsPutHandled    = 6
	slotControllerClose            = 24
	permissionStateDeny            = 2
)

type hresult uint32

const (
	sOK          hresult = 0
	eNoInterface hresult = 0x80004002
	ePointer     hresult = 0x80004003
)

var (
	iidIUnknown                   = windows.GUID{Data1: 0, Data2: 0, Data3: 0, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidNavigationStartingEH       = windows.GUID{Data1: 0x9adbe429, Data2: 0xf36d, Data3: 0x432b, Data4: [8]byte{0x9d, 0xdc, 0xf8, 0x88, 0x1f, 0xbd, 0x76, 0xe3}}
	iidNewWindowRequestedEH       = windows.GUID{Data1: 0xd4c185fe, Data2: 0xc81c, Data3: 0x4989, Data4: [8]byte{0x97, 0xaf, 0x2d, 0x3f, 0xa7, 0xab, 0x56, 0x51}}
	iidPermissionRequestedEH      = windows.GUID{Data1: 0x15e1c6a3, Data2: 0xc72a, Data3: 0x4df3, Data4: [8]byte{0x91, 0xd7, 0xd0, 0x97, 0xfb, 0xec, 0x6b, 0xfd}}
	iidViewsControllerCompletedEH = windows.GUID{Data1: 0x6c4819f3, Data2: 0xc9b7, Data3: 0x4260, Data4: [8]byte{0x81, 0x27, 0xc9, 0xf5, 0xbd, 0xe7, 0xf6, 0x8c}}
	iidViewsProcessFailedEH       = windows.GUID{Data1: 0x79e0aea4, Data2: 0x990b, Data3: 0x42d9, Data4: [8]byte{0xaa, 0x1d, 0x0f, 0xcc, 0x2e, 0x5b, 0xc7, 0xf1}}
)

type eventHandler struct {
	vtbl   *eventHandlerVtbl
	iid    windows.GUID
	invoke func(unsafe.Pointer)
}

type eventHandlerVtbl struct {
	queryInterface, addRef, release, invoke uintptr
}

var (
	handlerVtblOnce sync.Once
	handlerVtbl     *eventHandlerVtbl
)

func newEventHandler(iid windows.GUID, invoke func(unsafe.Pointer)) *eventHandler {
	handlerVtblOnce.Do(func() {
		handlerVtbl = &eventHandlerVtbl{
			queryInterface: windows.NewCallback(func(this *eventHandler, riid *windows.GUID, out *unsafe.Pointer) uintptr {
				if *riid == iidIUnknown || *riid == this.iid {
					*out = unsafe.Pointer(this)
					return uintptr(sOK)
				}
				*out = nil
				return uintptr(eNoInterface)
			}),
			addRef:  windows.NewCallback(func(*eventHandler) uintptr { return 1 }),
			release: windows.NewCallback(func(*eventHandler) uintptr { return 1 }),
			invoke: windows.NewCallback(func(this *eventHandler, _, args unsafe.Pointer) uintptr {
				this.invoke(args)
				return uintptr(sOK)
			}),
		}
	})
	return &eventHandler{vtbl: handlerVtbl, iid: iid, invoke: invoke}
}

//go:uintptrescapes
func comCall(obj unsafe.Pointer, slot int, args ...uintptr) hresult {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return hresult(uint32(r))
}

func eventURI(args unsafe.Pointer, slot int) (string, bool) {
	var value *uint16
	if comCall(args, slot, uintptr(unsafe.Pointer(&value))) != sOK || value == nil {
		return "", false
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(value))
	return windows.UTF16PtrToString(value), true
}

type navigationGuard struct {
	policy     NavigationPolicy
	onBlocked  func(string)
	navigation *eventHandler
	newWindow  *eventHandler
	permission *eventHandler
	tokens     [4]int64
	attached   [4]bool
}

func newNavigationGuard(policy NavigationPolicy, onBlocked func(string)) *navigationGuard {
	return newScopedNavigationGuard(policy, onBlocked, false)
}

// newScopedNavigationGuard installs the per-controller permission denial in
// addition to the navigation rules. The legacy shell already owns its
// permission event through the pinned Chromium wrapper.
func newScopedNavigationGuard(policy NavigationPolicy, onBlocked func(string), denyPermissions bool) *navigationGuard {
	g := &navigationGuard{policy: policy, onBlocked: onBlocked}
	g.navigation = newEventHandler(iidNavigationStartingEH, func(args unsafe.Pointer) {
		uri, readable := eventURI(args, slotNavArgsGetURI)
		if readable && g.policy.AllowNavigation(uri) {
			return
		}
		comCall(args, slotNavArgsPutCancel, 1)
		if g.onBlocked != nil {
			g.onBlocked(uri)
		}
	})
	g.newWindow = newEventHandler(iidNewWindowRequestedEH, func(args unsafe.Pointer) {
		uri, _ := eventURI(args, slotNewWindowArgsGetURI)
		comCall(args, slotNewWindowArgsPutHandled, 1)
		if g.onBlocked != nil {
			g.onBlocked(uri)
		}
	})
	if denyPermissions {
		g.permission = newEventHandler(iidPermissionRequestedEH, func(args unsafe.Pointer) {
			comCall(args, slotPermissionArgsPutState, permissionStateDeny)
		})
	}
	return g
}

func (g *navigationGuard) attach(webview unsafe.Pointer) error {
	registrations := []struct {
		slot    int
		handler *eventHandler
	}{
		{slotAddNavigationStarting, g.navigation},
		{slotAddFrameNavigationStarting, g.navigation},
		{slotAddNewWindowRequested, g.newWindow},
	}
	if g.permission != nil {
		registrations = append(registrations, struct {
			slot    int
			handler *eventHandler
		}{slotAddPermissionRequested, g.permission})
	}
	for i, registration := range registrations {
		if hr := comCall(webview, registration.slot, uintptr(unsafe.Pointer(registration.handler)), uintptr(unsafe.Pointer(&g.tokens[i]))); hr != sOK {
			return errors.Join(syscall.Errno(hr), g.detach(webview))
		}
		g.attached[i] = true
	}
	return nil
}

func (g *navigationGuard) detach(webview unsafe.Pointer) error {
	if g == nil || webview == nil {
		return nil
	}
	registrations := [...]int{slotRemoveNavigationStarting, slotRemoveFrameNavigation, slotRemoveNewWindowRequested, slotRemovePermissionRequested}
	var detachErr error
	for i := len(registrations) - 1; i >= 0; i-- {
		if !g.attached[i] {
			continue
		}
		if hr := comCall(webview, registrations[i], uintptr(g.tokens[i])); hr != sOK {
			detachErr = errors.Join(detachErr, syscall.Errno(hr))
			continue
		}
		g.attached[i] = false
	}
	runtime.KeepAlive(g)
	return detachErr
}
