//go:build windows

package desktop

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"unsafe"

	"github.com/yohn-jp/matagi/internal/i18n"
	"golang.org/x/sys/windows"
)

const (
	wmCommand                   = 0x0111
	wmGetMinMaxInfo             = 0x0024
	commandReturnToTabs         = 1
	commandCloseView            = 2
	detachedToolbarHeightDIP    = int32(48)
	detachedWindowClass         = "MatagiServiceViewWindow"
	wsChild                     = 0x40000000
	wsVisible                   = 0x10000000
	wsTabStop                   = 0x00010000
	wsExControlParent           = 0x00010000
	buttonPush                  = 0x00000000
	monitorDefaultToNearest     = 2
	monitorEffectiveDPI         = 0
	moveFocusReasonProgrammatic = 0
	moveFocusReasonNext         = 1
	moveFocusReasonPrevious     = 2
	defaultDetachedWidthDIP     = int32(1024)
	defaultDetachedHeightDIP    = int32(768)
	detachedButtonWidthDIP      = int32(144)
	detachedButtonHeightDIP     = int32(32)
	detachedControlMarginDIP    = int32(8)
	detachedControlGapDIP       = int32(8)
	gaRoot                      = 2
)

var (
	moveGetWindowRect          = user32.NewProc("GetWindowRect")
	moveGetMonitorInfoW        = user32.NewProc("GetMonitorInfoW")
	moveMonitorFromRect        = user32.NewProc("MonitorFromRect")
	moveGetAncestor            = user32.NewProc("GetAncestor")
	moveIsDialogMessageW       = user32.NewProc("IsDialogMessageW")
	moveGetFocus               = user32.NewProc("GetFocus")
	moveAdjustWindowRectForDPI = user32.NewProc("AdjustWindowRectExForDpi")
	moveAdjustWindowRect       = user32.NewProc("AdjustWindowRectEx")
	moveShcore                 = windows.NewLazySystemDLL("shcore.dll")
	moveGetDpiForMonitor       = moveShcore.NewProc("GetDpiForMonitor")
)

type nativeDetachedHost struct {
	window       uintptr
	returnButton uintptr
	closeButton  uintptr
	bounds       DIPBounds
}

type moveMonitorInfo struct {
	size    uint32
	monitor dpiRect
	work    dpiRect
	flags   uint32
}

type movePoint struct{ x, y int32 }

type moveMinMaxInfo struct {
	reserved, maxSize, maxPosition, minTrackSize, maxTrackSize movePoint
}

type moveController struct{ controller *viewsController }

func (controller moveController) Hide() error { return controller.controller.PutIsVisible(false) }

func (controller moveController) SetParent(parent uintptr) error {
	return controller.controller.PutParentWindow(parent)
}

func (controller moveController) SetBounds(bounds moveBounds) error {
	return controller.controller.PutBounds(viewsRECT{Left: bounds.left, Top: bounds.top, Right: bounds.right, Bottom: bounds.bottom})
}

func (controller moveController) NotifyParentPosition() error {
	return controller.controller.NotifyParentWindowPositionChanged()
}

func (controller moveController) Show() error { return controller.controller.PutIsVisible(true) }

func (h *nativeViewsHost) preTranslateDetachedMessage(message *winMessage) bool {
	if message == nil || message.Hwnd == 0 {
		return false
	}
	root, _, _ := moveGetAncestor.Call(message.Hwnd, gaRoot)
	owner, ok := ownerForWindow(root)
	if !ok || owner.host != h || owner.view == nil {
		return false
	}
	processed, _, _ := moveIsDialogMessageW.Call(root, uintptr(unsafe.Pointer(message)))
	return processed != 0
}

func (h *nativeViewsHost) moveOnSTA(ctx context.Context, handle ViewHandle, target ViewLocation, bounds DIPBounds) error {
	h.mu.Lock()
	v := h.views[handle]
	closing := h.closing
	cancelled := v != nil && (v.cancelled || v.closed)
	ready := v != nil && v.ready && v.controller != nil && !v.controllerClosed
	h.mu.Unlock()
	if v == nil {
		return errors.New("unknown WebView2 view handle")
	}
	if v.trusted {
		return ErrTrustedViewOwned
	}
	if closing || cancelled {
		return errors.New("WebView2 view move was cancelled")
	}
	if !ready {
		return ErrViewNotReady
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if target == v.location {
		return h.focusOnSTA(handle)
	}

	previousParent, err := v.controller.GetParentWindow()
	if err != nil {
		return fmt.Errorf("reading WebView2 parent before move: %w", err)
	}
	if previousParent != v.parentWindow {
		return fmt.Errorf("WebView2 parent window changed outside Matagi: got %#x, expected %#x", previousParent, v.parentWindow)
	}
	previousBounds, err := v.controller.GetBounds()
	if err != nil {
		return fmt.Errorf("reading WebView2 bounds before move: %w", err)
	}
	targetParent := h.root
	if targetParent == 0 {
		return errors.New("Matagi view host window is unavailable")
	}
	targetHost := nativeDetachedHost{}
	if target == ViewDetached {
		targetHost, err = h.createDetachedHost(v, bounds)
		if err != nil {
			return err
		}
		targetParent = targetHost.window
	}
	targetBounds, err := controllerBoundsForWindow(targetParent, target)
	if err != nil {
		if targetHost.window != 0 {
			_ = h.destroyDetachedWindowHandle(v, targetHost.window)
		}
		return err
	}
	oldLocation := v.location
	oldWindow := v.detachedWindow
	oldReturnButton := v.returnButton
	oldCloseButton := v.closeButton
	oldWindowBounds := v.windowBounds
	h.mu.Lock()
	previousSelected := h.selected
	trusted := h.views[h.trusted]
	h.mu.Unlock()
	controller := moveController{controller: v.controller}
	moveError := runControllerMove(controllerMovePlan{
		controller:     controller,
		previousParent: previousParent,
		targetParent:   targetParent,
		previousBounds: rectToMoveBounds(previousBounds),
		targetBounds:   rectToMoveBounds(targetBounds),
		revealTarget: func() error {
			if targetHost.window == 0 {
				return nil
			}
			procShowWindow.Call(targetHost.window, swShowNormal)
			procUpdateWindow.Call(targetHost.window)
			return nil
		},
		beforeShow: func() error { return h.prepareMoveVisibilityOnSTA(v, target, previousSelected) },
		restorePrevious: func() error {
			if oldLocation == ViewDetached {
				return showViewWindow(oldWindow)
			}
			return showViewWindow(h.root)
		},
		focusTarget: func() error { return focusWebViewWindow(targetParent, v.controller) },
		focusPrevious: func() error {
			if oldLocation == ViewDetached {
				return focusWebViewWindow(oldWindow, v.controller)
			}
			if previousSelected == v.handle {
				return focusWebViewWindow(h.root, v.controller)
			}
			h.mu.Lock()
			selected := h.views[previousSelected]
			h.mu.Unlock()
			if selected != nil && selected.ready && selected.controller != nil {
				return focusWebViewWindow(h.root, selected.controller)
			}
			return showViewWindow(h.root)
		},
		restorePeers: func() error {
			return h.restoreSelectionOnSTA(previousSelected, h.integratedViewsOnSTA(), trusted)
		},
		commit: func() error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.closing || v.cancelled || v.closed {
				return errors.New("WebView2 view move was cancelled")
			}
			v.parentWindow = targetParent
			v.location = target
			v.controllerBounds = targetBounds
			if target == ViewDetached {
				v.detachedWindow = targetHost.window
				v.returnButton = targetHost.returnButton
				v.closeButton = targetHost.closeButton
				v.windowBounds = targetHost.bounds
				if previousSelected == v.handle {
					h.selected = h.trusted
				}
			} else {
				v.detachedWindow = 0
				v.returnButton = 0
				v.closeButton = 0
				v.windowBounds = DIPBounds{}
				h.selected = v.handle
			}
			return nil
		},
		undoCommit: func() error {
			h.mu.Lock()
			defer h.mu.Unlock()
			v.parentWindow = previousParent
			v.location = oldLocation
			v.detachedWindow = oldWindow
			v.returnButton = oldReturnButton
			v.closeButton = oldCloseButton
			v.windowBounds = oldWindowBounds
			v.controllerBounds = previousBounds
			h.selected = previousSelected
			return nil
		},
		retirePrevious: func() error {
			if oldWindow == 0 || oldWindow == targetHost.window {
				return nil
			}
			return h.destroyDetachedWindowHandle(v, oldWindow)
		},
		cleanupTarget: func() error {
			if targetHost.window == 0 || targetHost.window == oldWindow {
				return nil
			}
			return h.destroyDetachedWindowHandle(v, targetHost.window)
		},
		closeController: func() error { return h.closeMoveResourcesOnSTA(v, oldWindow, targetHost.window) },
		cancelled: func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			h.mu.Lock()
			cancelled := h.closing || v.cancelled || v.closed
			h.mu.Unlock()
			if cancelled {
				return errors.New("WebView2 view move was cancelled")
			}
			return nil
		},
	})
	if errors.Is(moveError, ErrViewMoveRollbackFailed) {
		h.markMoveFailedOnSTA(v, moveError)
	}
	return moveError
}

func (h *nativeViewsHost) prepareMoveVisibilityOnSTA(v *nativeView, target ViewLocation, previousSelected ViewHandle) error {
	h.mu.Lock()
	trusted := h.views[h.trusted]
	h.mu.Unlock()
	if target == ViewDetached {
		if previousSelected == v.handle && trusted != nil && trusted.ready && trusted.controller != nil {
			return trusted.controller.PutIsVisible(true)
		}
		return nil
	}
	if trusted != nil && trusted.ready && trusted.controller != nil {
		if err := trusted.controller.PutIsVisible(true); err != nil {
			return fmt.Errorf("showing trusted Matagi controller: %w", err)
		}
	}
	for _, candidate := range h.integratedViewsOnSTA() {
		if candidate.handle == v.handle || candidate.controller == nil {
			continue
		}
		if err := candidate.controller.PutIsVisible(false); err != nil {
			return fmt.Errorf("hiding integrated WebView2 sibling: %w", err)
		}
	}
	return nil
}

func (h *nativeViewsHost) integratedViewsOnSTA() []*nativeView {
	h.mu.Lock()
	defer h.mu.Unlock()
	views := make([]*nativeView, 0, len(h.views))
	for _, candidate := range h.views {
		if candidate.ready && !candidate.trusted && candidate.location == ViewIntegrated {
			views = append(views, candidate)
		}
	}
	return views
}

func rectToMoveBounds(rect viewsRECT) moveBounds {
	return moveBounds{left: rect.Left, top: rect.Top, right: rect.Right, bottom: rect.Bottom}
}

func (h *nativeViewsHost) createDetachedHost(v *nativeView, bounds DIPBounds) (nativeDetachedHost, error) {
	title, returnLabel, closeLabel := detachedWindowCopy(v.config)
	titlePtr, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return nativeDetachedHost{}, fmt.Errorf("encoding detached WebView2 title: %w", err)
	}
	className, err := windows.UTF16PtrFromString(detachedWindowClass)
	if err != nil {
		return nativeDetachedHost{}, fmt.Errorf("encoding detached WebView2 window class: %w", err)
	}
	returnText, err := windows.UTF16PtrFromString(returnLabel)
	if err != nil {
		return nativeDetachedHost{}, fmt.Errorf("encoding Return to tabs label: %w", err)
	}
	closeText, err := windows.UTF16PtrFromString(closeLabel)
	if err != nil {
		return nativeDetachedHost{}, fmt.Errorf("encoding Close view label: %w", err)
	}
	dpi := systemDPI()
	hwnd, _, callErr := procCreateWindowExW.Call(wsExControlParent,
		uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(titlePtr)),
		wsOverlappedWindow, cwUseDefault, cwUseDefault,
		uintptr(scaleForDPI(defaultDetachedWidthDIP, dpi)), uintptr(scaleForDPI(defaultDetachedHeightDIP, dpi)),
		0, 0, uintptr(h.instance), 0)
	if hwnd == 0 {
		return nativeDetachedHost{}, fmt.Errorf("creating detached WebView2 window: %w", win32CallError(callErr))
	}
	host := nativeDetachedHost{window: hwnd}
	createButton := func(id uintptr, label *uint16) (uintptr, error) {
		buttonClass, classErr := windows.UTF16PtrFromString("BUTTON")
		if classErr != nil {
			return 0, classErr
		}
		button, _, buttonErr := procCreateWindowExW.Call(0,
			uintptr(unsafe.Pointer(buttonClass)), uintptr(unsafe.Pointer(label)),
			wsChild|wsVisible|wsTabStop|buttonPush,
			0, 0, uintptr(scaleForDPI(detachedButtonWidthDIP, viewWindowDPI(hwnd))), uintptr(scaleForDPI(detachedButtonHeightDIP, viewWindowDPI(hwnd))),
			hwnd, id, uintptr(h.instance), 0)
		if button == 0 {
			return 0, fmt.Errorf("creating detached view control: %w", win32CallError(buttonErr))
		}
		return button, nil
	}
	host.returnButton, err = createButton(commandReturnToTabs, returnText)
	if err == nil {
		host.closeButton, err = createButton(commandCloseView, closeText)
	}
	if err != nil {
		procDestroyWindow.Call(hwnd)
		return nativeDetachedHost{}, err
	}
	addWindowOwner(hwnd, viewWindowOwner{host: h, view: v})
	if bounds != (DIPBounds{}) {
		if err := setWindowBoundsFromDIP(hwnd, bounds); err != nil {
			_ = h.destroyDetachedWindowHandle(v, hwnd)
			return nativeDetachedHost{}, err
		}
	}
	if err := layoutDetachedButtons(hwnd, host.returnButton, host.closeButton); err != nil {
		_ = h.destroyDetachedWindowHandle(v, hwnd)
		return nativeDetachedHost{}, err
	}
	actualBounds, err := windowBoundsInDIP(hwnd)
	if err != nil {
		_ = h.destroyDetachedWindowHandle(v, hwnd)
		return nativeDetachedHost{}, err
	}
	host.bounds = actualBounds
	return host, nil
}

func detachedWindowCopy(config ViewConfig) (title, returnLabel, closeLabel string) {
	locale := i18n.Resolve("", i18n.HostLocales()...)
	returnLabel, closeLabel = "Return to tabs", "Close view"
	if locale == i18n.Japanese {
		returnLabel, closeLabel = "タブに戻る", "ビューを閉じる"
	}
	if strings.TrimSpace(config.WindowTitle) != "" {
		title = config.WindowTitle
	} else {
		title = "Matagi"
	}
	if strings.TrimSpace(config.ReturnToTabsLabel) != "" {
		returnLabel = config.ReturnToTabsLabel
	}
	if strings.TrimSpace(config.CloseViewLabel) != "" {
		closeLabel = config.CloseViewLabel
	}
	return title, returnLabel, closeLabel
}

func (h *nativeViewsHost) layoutDetachedControls(v *nativeView, hwnd uintptr) {
	if v == nil || hwnd == 0 || v.detachedWindow != hwnd {
		return
	}
	if err := layoutDetachedButtons(hwnd, v.returnButton, v.closeButton); err != nil {
		fmt.Fprintf(os.Stderr, "Matagi: positioning detached view controls failed: %v\n", err)
	}
	if bounds, err := windowBoundsInDIP(hwnd); err == nil {
		v.windowBounds = bounds
	}
}

func layoutDetachedButtons(hwnd, returnButton, closeButton uintptr) error {
	if hwnd == 0 || returnButton == 0 || closeButton == 0 {
		return errors.New("detached view controls are unavailable")
	}
	dpi := viewWindowDPI(hwnd)
	height := scaleForDPI(detachedButtonHeightDIP, dpi)
	width := scaleForDPI(detachedButtonWidthDIP, dpi)
	margin := scaleForDPI(detachedControlMarginDIP, dpi)
	gap := scaleForDPI(detachedControlGapDIP, dpi)
	var client dpiRect
	if ok, _, err := viewsGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&client))); ok == 0 {
		return fmt.Errorf("reading detached view client bounds: %w", win32CallError(err))
	}
	if client.right-client.left < margin+2*width+gap || client.bottom-client.top < margin+height {
		return errors.New("detached view window is too small to show its native controls")
	}
	set := func(button uintptr, x int32) error {
		if positioned, _, err := procSetWindowPos.Call(button, 0,
			uintptr(x), uintptr(margin), uintptr(width), uintptr(height), swpNoZOrder|swpNoActivate); positioned == 0 {
			return fmt.Errorf("positioning detached view button: %w", win32CallError(err))
		}
		return nil
	}
	if err := set(returnButton, margin); err != nil {
		return err
	}
	return set(closeButton, margin+width+gap)
}

func (h *nativeViewsHost) handleDetachedCommand(hwnd uintptr, v *nativeView, command, source uintptr) bool {
	if v == nil || hwnd == 0 || hwnd != v.detachedWindow || source == 0 {
		return false
	}
	id := command & 0xffff
	if command>>16&0xffff != 0 {
		return false
	}
	if id == commandReturnToTabs && source == v.returnButton {
		h.queueCallback(viewCallbackEvent{state: v.callbackState, kind: viewEventReturnToTabs})
		return true
	}
	if id == commandCloseView && source == v.closeButton {
		h.queueCallback(viewCallbackEvent{state: v.callbackState, kind: viewEventCloseRequested})
		return true
	}
	return false
}

func (h *nativeViewsHost) moveFocusRequested(v *nativeView, args unsafe.Pointer) {
	if v == nil || v.location != ViewDetached {
		return
	}
	reason, err := viewsMoveFocusReason(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Matagi: reading WebView2 focus direction failed: %v\n", err)
		return
	}
	target := detachedFocusTarget(v, reason)
	if target == 0 {
		return
	}
	viewsSetFocus.Call(target)
	if focused, _, _ := moveGetFocus.Call(); focused != target {
		return
	}
	if err := viewsMoveFocusSetHandled(args, true); err != nil {
		fmt.Fprintf(os.Stderr, "Matagi: handling WebView2 focus request failed: %v\n", err)
	}
}

func enforceDetachedWindowMinimum(hwnd, lParam uintptr) {
	if hwnd == 0 || lParam == 0 {
		return
	}
	minimum, err := detachedMinimumWindowSize(hwnd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Matagi: calculating detached-window minimum size failed: %v\n", err)
		return
	}
	info := *(**moveMinMaxInfo)(unsafe.Pointer(&lParam))
	if info == nil {
		return
	}
	info.minTrackSize.x = max(info.minTrackSize.x, minimum.x)
	info.minTrackSize.y = max(info.minTrackSize.y, minimum.y)
}

func detachedMinimumWindowSize(hwnd uintptr) (movePoint, error) {
	dpi := viewWindowDPI(hwnd)
	clientWidth := scaleForDPI(2*detachedButtonWidthDIP+detachedControlGapDIP+2*detachedControlMarginDIP, dpi)
	clientHeight := scaleForDPI(detachedControlMarginDIP+detachedButtonHeightDIP, dpi)
	rect := dpiRect{right: clientWidth, bottom: clientHeight}
	style := uintptr(wsOverlappedWindow)
	exStyle := uintptr(wsExControlParent)
	if moveAdjustWindowRectForDPI.Find() == nil {
		if adjusted, _, callErr := moveAdjustWindowRectForDPI.Call(uintptr(unsafe.Pointer(&rect)), style, 0, exStyle, uintptr(dpi)); adjusted == 0 {
			return movePoint{}, fmt.Errorf("adjusting minimum detached-window client area: %w", win32CallError(callErr))
		}
	} else if adjusted, _, callErr := moveAdjustWindowRect.Call(uintptr(unsafe.Pointer(&rect)), style, 0, exStyle); adjusted == 0 {
		return movePoint{}, fmt.Errorf("adjusting minimum detached-window client area: %w", win32CallError(callErr))
	}
	return movePoint{x: rect.right - rect.left, y: rect.bottom - rect.top}, nil
}

func detachedFocusTarget(v *nativeView, reason uintptr) uintptr {
	if v == nil {
		return 0
	}
	switch reason {
	case moveFocusReasonNext:
		return v.returnButton
	case moveFocusReasonPrevious:
		return v.closeButton
	default:
		return 0
	}
}

func (h *nativeViewsHost) destroyDetachedWindow(v *nativeView, hwnd uintptr) error {
	return h.destroyDetachedWindowHandle(v, hwnd)
}

func (h *nativeViewsHost) destroyDetachedWindowHandle(v *nativeView, hwnd uintptr) error {
	if hwnd == 0 {
		return nil
	}
	if _, exists := ownerForWindow(hwnd); !exists {
		if v != nil && v.detachedWindow == hwnd {
			v.detachedWindow = 0
			v.returnButton = 0
			v.closeButton = 0
		}
		return nil
	}
	previousDestroying := false
	if v != nil {
		previousDestroying = v.destroyingDetached
		v.destroyingDetached = true
	}
	if destroyed, _, callErr := procDestroyWindow.Call(hwnd); destroyed == 0 {
		if v != nil {
			v.destroyingDetached = previousDestroying
		}
		return fmt.Errorf("destroying detached WebView2 window: %w", win32CallError(callErr))
	}
	if v != nil {
		if v.detachedWindow == hwnd {
			v.detachedWindow = 0
			v.returnButton = 0
			v.closeButton = 0
		}
		v.destroyingDetached = previousDestroying
	}
	return nil
}

func (h *nativeViewsHost) closeMoveResourcesOnSTA(v *nativeView, oldWindow, targetWindow uintptr) error {
	var closeErr error
	h.closeControllerOnSTA(v)
	if v != nil {
		v.detachedWindow = 0
		v.returnButton = 0
		v.closeButton = 0
		v.parentWindow = 0
	}
	for _, hwnd := range uniqueWindows(oldWindow, targetWindow) {
		if err := h.destroyDetachedWindowHandle(v, hwnd); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	return closeErr
}

func uniqueWindows(first, second uintptr) []uintptr {
	if first == 0 {
		if second == 0 {
			return nil
		}
		return []uintptr{second}
	}
	if second == 0 || second == first {
		return []uintptr{first}
	}
	return []uintptr{first, second}
}

func (h *nativeViewsHost) markMoveFailedOnSTA(v *nativeView, err error) {
	h.mu.Lock()
	v.ready = false
	v.failed = true
	v.parentWindow = 0
	v.controllerBounds = viewsRECT{}
	if h.profiles[v.profile] == v.handle {
		delete(h.profiles, v.profile)
	}
	h.syncCallbackState(v)
	h.mu.Unlock()
	h.queueCallback(viewCallbackEvent{state: v.callbackState, kind: viewEventFailed, err: err})
}

func controllerBoundsForWindow(hwnd uintptr, location ViewLocation) (viewsRECT, error) {
	if hwnd == 0 {
		return viewsRECT{}, errors.New("WebView2 parent window is unavailable")
	}
	var rect dpiRect
	if ok, _, err := viewsGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rect))); ok == 0 {
		return viewsRECT{}, fmt.Errorf("reading WebView2 target client bounds: %w", win32CallError(err))
	}
	top := chromeHeightDIP
	if location == ViewDetached {
		top = detachedToolbarHeightDIP
	}
	minimumTop := scaleForDPI(top, viewWindowDPI(hwnd))
	return viewsRECT{
		Left:   0,
		Top:    minimumTop,
		Right:  rect.right - rect.left,
		Bottom: max(rect.bottom-rect.top, minimumTop),
	}, nil
}

func focusWebViewWindow(hwnd uintptr, controller *viewsController) error {
	if err := showViewWindow(hwnd); err != nil {
		return err
	}
	if activated, _, err := procSetForegroundW.Call(hwnd); activated == 0 {
		return fmt.Errorf("activating WebView2 host window: %w", win32CallError(err))
	}
	viewsSetFocus.Call(hwnd)
	if controller != nil {
		return controller.MoveFocus(0)
	}
	return nil
}

func showViewWindow(hwnd uintptr) error {
	if hwnd == 0 {
		return errors.New("WebView2 host window is unavailable")
	}
	procAllowSetForegroundWindow.Call(^uintptr(0))
	procShowWindow.Call(hwnd, swShowNormal)
	procUpdateWindow.Call(hwnd)
	return nil
}

func windowRect(hwnd uintptr) (dpiRect, error) {
	if hwnd == 0 {
		return dpiRect{}, errors.New("window handle is unavailable")
	}
	var rect dpiRect
	if ok, _, err := moveGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rect))); ok == 0 {
		return dpiRect{}, fmt.Errorf("reading window bounds: %w", win32CallError(err))
	}
	return rect, nil
}

func setWindowRect(hwnd uintptr, rect dpiRect) error {
	if hwnd == 0 || !validDPIChangeRect(&rect) {
		return errors.New("window bounds are unavailable")
	}
	if positioned, _, err := procSetWindowPos.Call(hwnd, 0,
		uintptr(rect.left), uintptr(rect.top), uintptr(rect.right-rect.left), uintptr(rect.bottom-rect.top), swpNoZOrder|swpNoActivate); positioned == 0 {
		return fmt.Errorf("setting window bounds: %w", win32CallError(err))
	}
	return nil
}

func setWindowBoundsFromDIP(hwnd uintptr, bounds DIPBounds) error {
	if hwnd == 0 || bounds.Width <= 0 || bounds.Height <= 0 {
		return errors.New("detached WebView2 bounds require a window and positive size")
	}
	dpi := viewWindowDPI(hwnd)
	initial, err := dipBoundsRect(bounds, dpi)
	if err != nil {
		return err
	}
	monitor, _, _ := moveMonitorFromRect.Call(uintptr(unsafe.Pointer(&initial)), monitorDefaultToNearest)
	monitorDPI := effectiveMonitorDPI(monitor, dpi)
	rect, err := dipBoundsRect(bounds, monitorDPI)
	if err != nil {
		return err
	}
	monitor, _, _ = moveMonitorFromRect.Call(uintptr(unsafe.Pointer(&rect)), monitorDefaultToNearest)
	monitorDPI = effectiveMonitorDPI(monitor, monitorDPI)
	rect, err = dipBoundsRect(bounds, monitorDPI)
	if err != nil {
		return err
	}
	info, err := monitorInfo(monitor)
	if err != nil {
		return err
	}
	rect = clampWindowRect(rect, info.work)
	return setWindowRect(hwnd, rect)
}

func dipBoundsRect(bounds DIPBounds, dpi uint32) (dpiRect, error) {
	left, err := scaleDIPToPixels(bounds.X, dpi)
	if err != nil {
		return dpiRect{}, err
	}
	top, err := scaleDIPToPixels(bounds.Y, dpi)
	if err != nil {
		return dpiRect{}, err
	}
	width, err := scaleDIPToPixels(bounds.Width, dpi)
	if err != nil {
		return dpiRect{}, err
	}
	height, err := scaleDIPToPixels(bounds.Height, dpi)
	if err != nil {
		return dpiRect{}, err
	}
	right := int64(left) + int64(width)
	bottom := int64(top) + int64(height)
	if right > math.MaxInt32 || right < math.MinInt32 || bottom > math.MaxInt32 || bottom < math.MinInt32 {
		return dpiRect{}, errors.New("detached WebView2 bounds exceed Windows coordinate range")
	}
	return dpiRect{left: left, top: top, right: int32(right), bottom: int32(bottom)}, nil
}

func scaleDIPToPixels(value int32, dpi uint32) (int32, error) {
	product := int64(value) * int64(dpi)
	if product >= 0 {
		product += defaultDPI / 2
	} else {
		product -= defaultDPI / 2
	}
	result := product / defaultDPI
	if result < math.MinInt32 || result > math.MaxInt32 {
		return 0, errors.New("detached WebView2 bounds exceed Windows coordinate range")
	}
	return int32(result), nil
}

func effectiveMonitorDPI(monitor uintptr, fallback uint32) uint32 {
	if monitor == 0 || moveGetDpiForMonitor.Find() != nil {
		return fallback
	}
	var x, y uint32
	if result, _, _ := moveGetDpiForMonitor.Call(monitor, monitorEffectiveDPI, uintptr(unsafe.Pointer(&x)), uintptr(unsafe.Pointer(&y))); result != 0 || x == 0 {
		return fallback
	}
	return x
}

func monitorInfo(monitor uintptr) (moveMonitorInfo, error) {
	if monitor == 0 {
		return moveMonitorInfo{}, errors.New("no monitor contains detached WebView2 bounds")
	}
	info := moveMonitorInfo{size: uint32(unsafe.Sizeof(moveMonitorInfo{}))}
	if ok, _, err := moveGetMonitorInfoW.Call(monitor, uintptr(unsafe.Pointer(&info))); ok == 0 {
		return moveMonitorInfo{}, fmt.Errorf("reading monitor work area: %w", win32CallError(err))
	}
	return info, nil
}

func clampWindowRect(rect, work dpiRect) dpiRect {
	workWidth := work.right - work.left
	workHeight := work.bottom - work.top
	width := min(rect.right-rect.left, workWidth)
	height := min(rect.bottom-rect.top, workHeight)
	left := rect.left
	top := rect.top
	if left < work.left {
		left = work.left
	}
	if top < work.top {
		top = work.top
	}
	if int64(left)+int64(width) > int64(work.right) {
		left = work.right - width
	}
	if int64(top)+int64(height) > int64(work.bottom) {
		top = work.bottom - height
	}
	return dpiRect{left: left, top: top, right: int32(int64(left) + int64(width)), bottom: int32(int64(top) + int64(height))}
}

func windowBoundsInDIP(hwnd uintptr) (DIPBounds, error) {
	rect, err := windowRect(hwnd)
	if err != nil {
		return DIPBounds{}, err
	}
	dpi := viewWindowDPI(hwnd)
	return DIPBounds{
		X:      unscalePixelsToDIP(rect.left, dpi),
		Y:      unscalePixelsToDIP(rect.top, dpi),
		Width:  unscalePixelsToDIP(rect.right-rect.left, dpi),
		Height: unscalePixelsToDIP(rect.bottom-rect.top, dpi),
	}, nil
}

func unscalePixelsToDIP(value int32, dpi uint32) int32 {
	product := int64(value) * defaultDPI
	if product >= 0 {
		product += int64(dpi) / 2
	} else {
		product -= int64(dpi) / 2
	}
	return int32(product / int64(dpi))
}

func win32CallError(err error) error {
	if err == nil || errors.Is(err, windows.ERROR_SUCCESS) {
		return errors.New("Windows API call failed")
	}
	return err
}
