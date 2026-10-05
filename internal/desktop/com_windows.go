//go:build windows

package desktop

import (
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
	slotAddNewWindowRequested      = 44
	slotNavArgsGetURI              = 3
	slotNavArgsPutCancel           = 8
	slotNewWindowArgsGetURI        = 3
	slotNewWindowArgsPutHandled    = 6
	slotControllerClose            = 24
)

type hresult uint32

const (
	sOK          hresult = 0
	eNoInterface hresult = 0x80004002
)

var (
	iidIUnknown             = windows.GUID{Data1: 0, Data2: 0, Data3: 0, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidNavigationStartingEH = windows.GUID{Data1: 0x9adbe429, Data2: 0xf36d, Data3: 0x432b, Data4: [8]byte{0x9d, 0xdc, 0xf8, 0x88, 0x1f, 0xbd, 0x76, 0xe3}}
	iidNewWindowRequestedEH = windows.GUID{Data1: 0xd4c185fe, Data2: 0xc81c, Data3: 0x4989, Data4: [8]byte{0x97, 0xaf, 0x2d, 0x3f, 0xa7, 0xab, 0x56, 0x51}}
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
	tokens     [3]int64
}

func newNavigationGuard(policy NavigationPolicy, onBlocked func(string)) *navigationGuard {
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
	return g
}

func (g *navigationGuard) attach(webview unsafe.Pointer) error {
	for i, registration := range []struct {
		slot    int
		handler *eventHandler
	}{
		{slotAddNavigationStarting, g.navigation},
		{slotAddFrameNavigationStarting, g.navigation},
		{slotAddNewWindowRequested, g.newWindow},
	} {
		if hr := comCall(webview, registration.slot, uintptr(unsafe.Pointer(registration.handler)), uintptr(unsafe.Pointer(&g.tokens[i]))); hr != sOK {
			return syscall.Errno(hr)
		}
	}
	return nil
}
