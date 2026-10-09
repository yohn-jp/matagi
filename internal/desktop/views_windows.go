//go:build windows

package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/webview2"
	"github.com/wailsapp/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
)

const (
	wmViewsCommand  = 0x8001
	wmViewsFinalize = 0x8002
	chromeHeightDIP = int32(96)
)

var (
	viewsGetClientRect = user32.NewProc("GetClientRect")
	viewsSetFocus      = user32.NewProc("SetFocus")
	activeViewsHost    *nativeViewsHost
	viewsWindowOwners  = struct {
		sync.RWMutex
		owners map[uintptr]viewWindowOwner
	}{owners: make(map[uintptr]viewWindowOwner)}
)

type viewWindowOwner struct {
	host *nativeViewsHost
	view *nativeView
}

func ownerForWindow(hwnd uintptr) (viewWindowOwner, bool) {
	viewsWindowOwners.RLock()
	owner, ok := viewsWindowOwners.owners[hwnd]
	viewsWindowOwners.RUnlock()
	return owner, ok
}

func addWindowOwner(hwnd uintptr, owner viewWindowOwner) {
	viewsWindowOwners.Lock()
	viewsWindowOwners.owners[hwnd] = owner
	viewsWindowOwners.Unlock()
}

func removeWindowOwner(hwnd uintptr) {
	viewsWindowOwners.Lock()
	delete(viewsWindowOwners.owners, hwnd)
	viewsWindowOwners.Unlock()
}

type nativeViewsHost struct {
	mu sync.Mutex

	views       map[ViewHandle]*nativeView
	profiles    map[string]ViewHandle
	trusted     ViewHandle
	selected    ViewHandle
	closing     bool
	root        uintptr
	activateMsg uint32

	commandMu sync.Mutex
	commands  map[uintptr]func() error
	nextCmd   uintptr

	eventMu   sync.Mutex
	events    []viewCallbackEvent
	eventWake chan struct{}
	eventDone chan struct{}

	started        chan error
	done           chan struct{}
	closeErr       error
	finalizePosted bool
}

type nativeView struct {
	handle        ViewHandle
	config        ViewConfig
	profile       string
	callbackState *viewCallbackState
	trusted       bool

	pendingEnvironment    bool
	pendingController     bool
	beginAccepted         bool
	ready                 bool
	failed                bool
	processFailurePending bool
	cancelled             bool
	closed                bool
	callbacksSuppressed   bool
	closeErr              error
	closedCh              chan struct{}

	// The following fields are read or changed only on the owning STA.
	environment           *webview2.ICoreWebView2Environment
	controller            *webview2.ICoreWebView2Controller
	webview               *webview2.ICoreWebView2
	guard                 *navigationGuard
	environmentHandler    *viewsEnvironmentHandler
	controllerHandler     *viewsControllerHandler
	processFailedHandler  *viewsProcessFailedHandler
	processFailedToken    webview2.EventRegistrationToken
	processFailedAttached bool
	parentWindow          uintptr
	controllerClosed      bool
}

type viewCallbackEvent struct {
	state *viewCallbackState
	kind  uint8
	err   error
}

// Callback state is deliberately separate from nativeView so no COM-owning
// view pointer crosses from the STA to the public callback dispatcher.
type viewCallbackState struct {
	mu                    sync.Mutex
	handle                ViewHandle
	callbacks             ViewCallbacks
	cancelled             bool
	closed                bool
	failed                bool
	processFailurePending bool
	suppressed            bool
}

const (
	viewEventReady uint8 = iota + 1
	viewEventFailed
	viewEventClosed
)

type viewsEnvironmentHandler struct {
	host *nativeViewsHost
	view *nativeView
}

func (h *viewsEnvironmentHandler) EnvironmentCompleted(result webviewloader.HRESULT, created *webviewloader.ICoreWebView2Environment) webviewloader.HRESULT {
	return h.host.environmentCompleted(h.view, result, created)
}

type viewsControllerHandler struct {
	host  *nativeViewsHost
	view  *nativeView
	iface *webview2.ICoreWebView2CreateCoreWebView2ControllerCompletedHandler
}

func newViewsControllerHandler(host *nativeViewsHost, view *nativeView) *viewsControllerHandler {
	handler := &viewsControllerHandler{host: host, view: view}
	handler.iface = webview2.NewICoreWebView2CreateCoreWebView2ControllerCompletedHandler(handler)
	return handler
}

func (h *viewsControllerHandler) QueryInterface(refiid, object uintptr) uintptr {
	if refiid == 0 || object == 0 {
		return uintptr(ePointer)
	}
	want := *(*windows.GUID)(unsafe.Pointer(refiid))
	if want != iidIUnknown && want != iidViewsControllerCompletedEH {
		*(*uintptr)(unsafe.Pointer(object)) = 0
		return uintptr(eNoInterface)
	}
	*(*uintptr)(unsafe.Pointer(object)) = uintptr(unsafe.Pointer(h.iface))
	return uintptr(sOK)
}

func (*viewsControllerHandler) AddRef() uintptr  { return 1 }
func (*viewsControllerHandler) Release() uintptr { return 1 }

func (h *viewsControllerHandler) CreateCoreWebView2ControllerCompleted(result uintptr, controller *webview2.ICoreWebView2Controller) uintptr {
	return h.host.controllerCompleted(h.view, result, controller)
}

type viewsProcessFailedHandler struct {
	host  *nativeViewsHost
	view  *nativeView
	iface *webview2.ICoreWebView2ProcessFailedEventHandler
}

func newViewsProcessFailedHandler(host *nativeViewsHost, view *nativeView) *viewsProcessFailedHandler {
	handler := &viewsProcessFailedHandler{host: host, view: view}
	handler.iface = webview2.NewICoreWebView2ProcessFailedEventHandler(handler)
	return handler
}

func (h *viewsProcessFailedHandler) QueryInterface(refiid, object uintptr) uintptr {
	if refiid == 0 || object == 0 {
		return uintptr(ePointer)
	}
	want := *(*windows.GUID)(unsafe.Pointer(refiid))
	if want != iidIUnknown && want != iidViewsProcessFailedEH {
		*(*uintptr)(unsafe.Pointer(object)) = 0
		return uintptr(eNoInterface)
	}
	*(*uintptr)(unsafe.Pointer(object)) = uintptr(unsafe.Pointer(h.iface))
	return uintptr(sOK)
}

func (*viewsProcessFailedHandler) AddRef() uintptr  { return 1 }
func (*viewsProcessFailedHandler) Release() uintptr { return 1 }

func (h *viewsProcessFailedHandler) ProcessFailed(_ *webview2.ICoreWebView2, args *webview2.ICoreWebView2ProcessFailedEventArgs) uintptr {
	message := "WebView2 process failed"
	if args != nil {
		if kind, err := args.GetProcessFailedKind(); err == nil {
			message = fmt.Sprintf("WebView2 process failed (kind %d)", kind)
		}
	}
	h.host.queueProcessFailure(h.view, errors.New(message))
	return uintptr(windows.S_OK)
}

func prepareViewProfile(config ViewConfig) (ViewConfig, string, error) {
	policy, ok := config.Policy.(*ViewPolicy)
	if !ok || policy == nil {
		return ViewConfig{}, "", errors.New("view requires a Matagi-created origin policy")
	}
	if err := validateViewConfig(config, policy.role); err != nil {
		return ViewConfig{}, "", err
	}
	profile, err := filepath.Abs(config.ProfileFolder)
	if err != nil {
		return ViewConfig{}, "", fmt.Errorf("resolving WebView2 profile folder: %w", err)
	}
	profile = filepath.Clean(profile)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return ViewConfig{}, "", fmt.Errorf("creating WebView2 profile folder: %w", err)
	}
	config.ProfileFolder = profile
	return config, strings.ToUpper(profile), nil
}

func newNativeViewsHost() *nativeViewsHost {
	h := &nativeViewsHost{
		views:     make(map[ViewHandle]*nativeView),
		profiles:  make(map[string]ViewHandle),
		commands:  make(map[uintptr]func() error),
		eventWake: make(chan struct{}, 1),
		eventDone: make(chan struct{}),
		started:   make(chan error, 1),
		done:      make(chan struct{}),
	}
	return h
}

func startViewsHost(ctx context.Context, trusted ViewConfig, callbacks ViewCallbacks) (ViewsHost, ViewHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if version, err := (native{}).RuntimeVersion(); err != nil {
		return nil, "", fmt.Errorf("detecting the WebView2 Runtime: %w", err)
	} else if version == "" {
		return nil, "", ErrWebView2Missing
	}
	trusted, profile, err := prepareViewProfile(trusted)
	if err != nil {
		return nil, "", err
	}

	initialHandle, err := newViewHandle()
	if err != nil {
		return nil, "", err
	}
	h := newNativeViewsHost()
	initial := &nativeView{
		handle:        initialHandle,
		config:        trusted,
		profile:       profile,
		callbackState: &viewCallbackState{handle: initialHandle, callbacks: callbacks},
		trusted:       true,
		closedCh:      make(chan struct{}),
	}
	h.views[initialHandle] = initial
	h.profiles[profile] = initialHandle
	h.trusted = initialHandle
	h.selected = initialHandle
	activeShellMu.Lock()
	if activeShell != nil || activeViewsHost != nil {
		activeShellMu.Unlock()
		return nil, "", errors.New("a Matagi desktop host is already open")
	}
	activeViewsHost = h
	activeShellMu.Unlock()
	go h.dispatchCallbacks()
	go h.run(initial)

	select {
	case err := <-h.started:
		if err != nil {
			select {
			case <-h.done:
				return nil, "", err
			case <-ctx.Done():
				h.requestCloseAll()
				return nil, "", ctx.Err()
			}
		}
	case <-h.done:
		return nil, "", h.hostStoppedError()
	case <-ctx.Done():
		h.requestCloseAll()
		return nil, "", ctx.Err()
	}

	go func() {
		select {
		case <-ctx.Done():
			h.requestCloseAll()
		case <-h.done:
		}
	}()
	return h, initialHandle, nil
}

func newViewHandle() (ViewHandle, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("creating opaque WebView2 view handle: %w", err)
	}
	return ViewHandle(hex.EncodeToString(id[:])), nil
}

func (h *nativeViewsHost) dispatchCallbacks() {
	for {
		select {
		case <-h.eventWake:
		case <-h.eventDone:
		}
		for {
			h.eventMu.Lock()
			if len(h.events) == 0 {
				h.eventMu.Unlock()
				select {
				case <-h.eventDone:
					return
				default:
				}
				break
			}
			event := h.events[0]
			copy(h.events, h.events[1:])
			h.events[len(h.events)-1] = viewCallbackEvent{}
			h.events = h.events[:len(h.events)-1]
			h.eventMu.Unlock()
			h.deliverCallback(event)
		}
	}
}

func (h *nativeViewsHost) queueCallback(event viewCallbackEvent) {
	h.eventMu.Lock()
	h.events = append(h.events, event)
	h.eventMu.Unlock()
	select {
	case h.eventWake <- struct{}{}:
	default:
	}
}

func (h *nativeViewsHost) deliverCallback(event viewCallbackEvent) {
	state := event.state
	state.mu.Lock()
	allowed := !state.suppressed
	if event.kind != viewEventClosed {
		allowed = allowed && !state.cancelled && !state.closed
	}
	if event.kind == viewEventReady {
		allowed = allowed && !state.failed && !state.processFailurePending
	}
	callbacks := state.callbacks
	handle := state.handle
	state.mu.Unlock()
	if !allowed {
		return
	}
	switch event.kind {
	case viewEventReady:
		if callbacks.Ready != nil {
			callbacks.Ready(handle)
		}
	case viewEventFailed:
		if callbacks.Failed != nil {
			callbacks.Failed(handle, event.err)
		}
	case viewEventClosed:
		if callbacks.Closed != nil {
			callbacks.Closed(handle)
		}
	}
}

func (h *nativeViewsHost) syncCallbackState(v *nativeView) {
	state := v.callbackState
	state.mu.Lock()
	state.cancelled = v.cancelled
	state.closed = v.closed
	state.failed = v.failed
	state.processFailurePending = v.processFailurePending
	state.suppressed = v.callbacksSuppressed
	state.mu.Unlock()
}

func (h *nativeViewsHost) run(initial *nativeView) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(h.done)
	defer close(h.eventDone)
	defer func() {
		activeShellMu.Lock()
		if activeViewsHost == h {
			activeViewsHost = nil
		}
		activeShellMu.Unlock()
	}()

	if err := enablePerMonitorDPI(); err != nil {
		h.started <- fmt.Errorf("enabling per-monitor DPI awareness: %w", err)
		return
	}
	switch err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); {
	case err == nil, errors.Is(err, syscall.Errno(1)):
		defer windows.CoUninitialize()
	default:
		h.started <- fmt.Errorf("initializing COM for WebView2: %w", err)
		return
	}

	instance, ownsClass, err := registerViewsWindowClass()
	if err != nil {
		h.started <- err
		return
	}
	if ownsClass {
		defer procUnregisterClassW.Call(uintptr(unsafe.Pointer(mustUTF16(windowClass))), uintptr(instance))
	}

	userID, err := currentUserID()
	if err != nil {
		h.started <- err
		return
	}
	messageName, err := windows.UTF16PtrFromString(ActivateMessageName(userID))
	if err != nil {
		h.started <- err
		return
	}
	activateMessage, _, callErr := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(messageName)))
	if activateMessage == 0 {
		h.started <- fmt.Errorf("registering desktop activation message: %w", callErr)
		return
	}
	h.activateMsg = uint32(activateMessage)

	title, err := windows.UTF16PtrFromString("Matagi")
	if err != nil {
		h.started <- err
		return
	}
	className := mustUTF16(windowClass)
	hwnd, _, callErr := procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow, cwUseDefault, cwUseDefault,
		uintptr(scaleForDPI(1100, systemDPI())), uintptr(scaleForDPI(780, systemDPI())),
		0, 0, uintptr(instance), 0)
	if hwnd == 0 {
		h.started <- fmt.Errorf("creating the Matagi view host window: %w", callErr)
		return
	}
	h.mu.Lock()
	h.root = hwnd
	closing := h.closing
	h.mu.Unlock()
	addWindowOwner(hwnd, viewWindowOwner{host: h})
	if closing {
		h.discardUnstartedView(initial)
		procDestroyWindow.Call(hwnd)
		h.started <- errors.New("WebView2 host startup was cancelled")
		return
	}
	procShowWindow.Call(hwnd, swShowNormal)
	procUpdateWindow.Call(hwnd)

	if err := h.beginCreate(initial); err != nil {
		h.discardUnstartedView(initial)
		h.mu.Lock()
		h.closing = true
		h.mu.Unlock()
		procDestroyWindow.Call(hwnd)
		h.started <- err
	} else {
		h.started <- nil
	}

	var message winMessage
	for {
		result, _, callErr := procGetMessageW.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		switch int32(result) {
		case 0:
			return
		case -1:
			h.mu.Lock()
			if h.closeErr == nil {
				h.closeErr = fmt.Errorf("reading the WebView2 host message loop: %w", callErr)
			}
			h.mu.Unlock()
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&message)))
	}
}

func registerViewsWindowClass() (windows.Handle, bool, error) {
	wndProcOnce.Do(func() { wndProcCallback = windows.NewCallback(windowProc) })
	var instance windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &instance); err != nil {
		return 0, false, fmt.Errorf("locating the application module: %w", err)
	}
	className := mustUTF16(windowClass)
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	wc := wndClassEx{
		WndProc:    wndProcCallback,
		Instance:   instance,
		Cursor:     windows.Handle(cursor),
		Background: windows.Handle(colorWindow + 1),
		ClassName:  className,
	}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if registered, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); registered == 0 {
		if callErr == syscall.Errno(1410) {
			return instance, false, nil
		}
		return 0, false, fmt.Errorf("registering the WebView2 host window class: %w", callErr)
	}
	return instance, true, nil
}

func mustUTF16(value string) *uint16 {
	ptr, _ := windows.UTF16PtrFromString(value)
	return ptr
}

func (h *nativeViewsHost) windowProc(hwnd uintptr, owner viewWindowOwner, message, wParam, lParam uintptr) uintptr {
	switch uint32(message) {
	case wmViewsCommand:
		h.runCommand(wParam)
		return 0
	case wmViewsFinalize:
		h.mu.Lock()
		h.finalizePosted = false
		h.mu.Unlock()
		h.finishCloseIfEmpty()
		return 0
	case wmDPIChanged:
		applyDPIChange(hwnd, lParam)
		h.resizeViews(hwnd)
		return 0
	case wmSize:
		h.resizeViews(hwnd)
		return 0
	case wmClose:
		if hwnd == h.root {
			h.closeAllOnSTA()
			return 0
		}
		if owner.view != nil {
			h.closeViewOnSTA(owner.view)
			return 0
		}
	case wmDestroy:
		removeWindowOwner(hwnd)
		if hwnd == h.root {
			h.mu.Lock()
			h.root = 0
			h.mu.Unlock()
			procPostQuitMessage.Call(0)
		}
		return 0
	}
	if hwnd == h.root && h.activateMsg != 0 && uint32(message) == h.activateMsg {
		procShowWindow.Call(hwnd, swShowNormal)
		procSetForegroundW.Call(hwnd)
		h.mu.Lock()
		selected := h.views[h.selected]
		h.mu.Unlock()
		if selected != nil && selected.ready && selected.controller != nil {
			_ = selected.controller.MoveFocus(webview2.COREWEBVIEW2_MOVE_FOCUS_REASON_PROGRAMMATIC)
		}
		return 0
	}
	result, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}

func (h *nativeViewsHost) Create(ctx context.Context, config ViewConfig, callbacks ViewCallbacks) (ViewHandle, error) {
	if ctx == nil {
		return "", errors.New("desktop context is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	config, profile, err := prepareViewProfile(config)
	if err != nil {
		return "", err
	}
	if config.Policy.(*ViewPolicy).role != serviceViewPolicy {
		return "", errors.New("service controller requires an exact ensured-origin policy")
	}
	handle, err := newViewHandle()
	if err != nil {
		return "", err
	}
	v := &nativeView{
		handle:        handle,
		config:        config,
		profile:       profile,
		callbackState: &viewCallbackState{handle: handle, callbacks: callbacks},
		closedCh:      make(chan struct{}),
	}
	h.mu.Lock()
	if h.closing || h.root == 0 {
		h.mu.Unlock()
		return "", errors.New("WebView2 host is closing")
	}
	if _, exists := h.profiles[profile]; exists {
		h.mu.Unlock()
		return "", errors.New("WebView2 profile folder is already owned by this host")
	}
	h.views[handle] = v
	h.profiles[profile] = handle
	h.mu.Unlock()

	completion, err := h.enqueue(false, func() error { return h.beginCreate(v) })
	if err != nil {
		h.discardUnstartedView(v)
		return "", err
	}
	select {
	case err := <-completion:
		if err != nil {
			h.discardUnstartedView(v)
			return "", err
		}
	case <-h.done:
		h.mu.Lock()
		v.cancelled = true
		v.callbacksSuppressed = true
		h.syncCallbackState(v)
		started := v.beginAccepted
		h.mu.Unlock()
		if !started {
			h.discardUnstartedView(v)
		}
		return "", h.hostStoppedError()
	case <-ctx.Done():
		h.mu.Lock()
		v.cancelled = true
		v.callbacksSuppressed = true
		h.syncCallbackState(v)
		started := v.beginAccepted
		h.mu.Unlock()
		if _, closeErr := h.enqueue(true, func() error { h.closeViewOnSTA(v); return nil }); closeErr != nil {
			h.mu.Lock()
			h.closeErr = errors.Join(h.closeErr, closeErr)
			h.mu.Unlock()
			if !started {
				h.discardUnstartedView(v)
			}
		}
		return "", ctx.Err()
	}
	return handle, nil
}

func (h *nativeViewsHost) Select(ctx context.Context, handle ViewHandle) error {
	return h.invoke(ctx, false, func() error { return h.selectOnSTA(handle) })
}

func (h *nativeViewsHost) Hide(ctx context.Context, handle ViewHandle) error {
	return h.invoke(ctx, false, func() error { return h.hideOnSTA(handle) })
}

func (h *nativeViewsHost) Focus(ctx context.Context, handle ViewHandle) error {
	return h.invoke(ctx, false, func() error { return h.focusOnSTA(handle) })
}

func (h *nativeViewsHost) Move(ctx context.Context, handle ViewHandle, target ViewLocation, bounds DIPBounds) error {
	if ctx == nil {
		return errors.New("desktop context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	_, exists := h.views[handle]
	h.mu.Unlock()
	if !exists {
		return errors.New("unknown WebView2 view handle")
	}
	if target != ViewIntegrated && target != ViewDetached {
		return errors.New("invalid WebView2 view location")
	}
	_ = bounds
	return ErrViewMoveDeferred
}

func (h *nativeViewsHost) Close(ctx context.Context, handle ViewHandle) error {
	if ctx == nil {
		return errors.New("desktop context is required")
	}
	h.mu.Lock()
	v := h.views[handle]
	if v == nil {
		h.mu.Unlock()
		return errors.New("unknown WebView2 view handle")
	}
	if v.trusted {
		h.mu.Unlock()
		return ErrTrustedViewOwned
	}
	v.cancelled = true
	h.syncCallbackState(v)
	h.mu.Unlock()
	completion, err := h.enqueue(true, func() error { h.closeViewOnSTA(v); return nil })
	if err != nil {
		return err
	}
	select {
	case err := <-completion:
		if err != nil {
			return err
		}
	case <-h.done:
		if h.viewClosed(v) {
			return h.viewCloseError(v)
		}
		return h.hostStoppedError()
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-v.closedCh:
	case <-h.done:
		if h.viewClosed(v) {
			return h.viewCloseError(v)
		}
		return h.hostStoppedError()
	case <-ctx.Done():
		return ctx.Err()
	}
	return h.viewCloseError(v)
}

func (h *nativeViewsHost) CloseAll(ctx context.Context) error {
	if ctx == nil {
		return errors.New("desktop context is required")
	}
	select {
	case <-h.done:
		h.mu.Lock()
		err := h.closeErr
		h.mu.Unlock()
		return err
	default:
	}
	h.requestCloseAll()
	select {
	case <-h.done:
		h.mu.Lock()
		err := h.closeErr
		h.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *nativeViewsHost) requestCloseAll() {
	select {
	case <-h.done:
		return
	default:
	}
	h.mu.Lock()
	h.closing = true
	for _, v := range h.views {
		v.cancelled = true
		h.syncCallbackState(v)
	}
	root := h.root
	h.mu.Unlock()
	if root == 0 {
		return
	}
	if _, err := h.enqueue(true, func() error { h.closeAllOnSTA(); return nil }); err != nil {
		h.mu.Lock()
		h.closeErr = errors.Join(h.closeErr, err)
		h.mu.Unlock()
	}
}

func (h *nativeViewsHost) invoke(ctx context.Context, allowClosing bool, command func() error) error {
	if ctx == nil {
		return errors.New("desktop context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := h.enqueue(allowClosing, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return command()
	})
	if err != nil {
		return err
	}
	select {
	case err := <-result:
		return err
	case <-h.done:
		return h.hostStoppedError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *nativeViewsHost) hostStoppedError() error {
	h.mu.Lock()
	closeErr := h.closeErr
	h.mu.Unlock()
	return errors.Join(ErrViewsHostStopped, closeErr)
}

func (h *nativeViewsHost) viewClosed(v *nativeView) bool {
	select {
	case <-v.closedCh:
		return true
	default:
		return false
	}
}

func (h *nativeViewsHost) viewCloseError(v *nativeView) error {
	h.mu.Lock()
	err := v.closeErr
	h.mu.Unlock()
	return err
}

func (h *nativeViewsHost) enqueue(allowClosing bool, command func() error) (<-chan error, error) {
	result := make(chan error, 1)
	h.mu.Lock()
	if h.root == 0 || (h.closing && !allowClosing) {
		h.mu.Unlock()
		return nil, errors.New("WebView2 host is unavailable")
	}
	root := h.root
	h.mu.Unlock()

	h.commandMu.Lock()
	h.nextCmd++
	if h.nextCmd == 0 {
		h.nextCmd++
	}
	id := h.nextCmd
	h.commands[id] = func() error {
		err := command()
		result <- err
		return nil
	}
	h.commandMu.Unlock()
	if posted, _, err := procPostMessageW.Call(root, wmViewsCommand, id, 0); posted == 0 {
		h.commandMu.Lock()
		delete(h.commands, id)
		h.commandMu.Unlock()
		return nil, fmt.Errorf("posting WebView2 host command: %w", err)
	}
	return result, nil
}

func (h *nativeViewsHost) runCommand(id uintptr) {
	h.commandMu.Lock()
	command := h.commands[id]
	delete(h.commands, id)
	h.commandMu.Unlock()
	if command != nil {
		_ = command()
	}
}

func (h *nativeViewsHost) beginCreate(v *nativeView) error {
	h.mu.Lock()
	if h.closing || v.cancelled || v.closed {
		h.mu.Unlock()
		return errors.New("WebView2 view creation was cancelled")
	}
	v.beginAccepted = true
	v.pendingEnvironment = true
	h.mu.Unlock()

	handler := &viewsEnvironmentHandler{host: h, view: v}
	v.environmentHandler = handler
	err := webviewloader.CreateCoreWebView2EnvironmentWithOptions(
		handler,
		webviewloader.WithUserDataFolder(v.config.ProfileFolder),
	)
	if err != nil {
		v.pendingEnvironment = false
		v.environmentHandler = nil
		return fmt.Errorf("creating WebView2 environment: %w", err)
	}
	return nil
}

func (h *nativeViewsHost) environmentCompleted(v *nativeView, result webviewloader.HRESULT, created *webviewloader.ICoreWebView2Environment) webviewloader.HRESULT {
	v.pendingEnvironment = false
	v.environmentHandler = nil
	if result < 0 || created == nil {
		if h.isCancelled(v) {
			h.finishViewClose(v, nil)
		} else {
			h.failView(v, fmt.Errorf("creating WebView2 environment failed: HRESULT 0x%08x", uint32(result)))
		}
		return webviewloader.HRESULT(windows.S_OK)
	}
	if h.isCancelled(v) {
		h.finishViewClose(v, nil)
		return webviewloader.HRESULT(windows.S_OK)
	}

	env := (*webview2.ICoreWebView2Environment)(unsafe.Pointer(created))
	env.AddRef()
	v.environment = env
	parent := h.root
	if parent == 0 {
		_ = releaseWebView2Object(unsafe.Pointer(env))
		v.environment = nil
		h.finishViewClose(v, errors.New("WebView2 host window was destroyed during creation"))
		return webviewloader.HRESULT(windows.S_OK)
	}
	handler := newViewsControllerHandler(h, v)
	v.controllerHandler = handler
	v.pendingController = true
	if err := env.CreateCoreWebView2Controller(webview2.HWND(parent), handler.iface); err != nil {
		v.pendingController = false
		v.controllerHandler = nil
		_ = releaseWebView2Object(unsafe.Pointer(env))
		v.environment = nil
		if h.isCancelled(v) {
			h.finishViewClose(v, nil)
		} else {
			h.failView(v, fmt.Errorf("creating WebView2 controller: %w", err))
		}
		return webviewloader.HRESULT(windows.S_OK)
	}
	return webviewloader.HRESULT(windows.S_OK)
}

func (h *nativeViewsHost) controllerCompleted(v *nativeView, result uintptr, controller *webview2.ICoreWebView2Controller) uintptr {
	v.pendingController = false
	v.controllerHandler = nil
	if v.environment != nil {
		_ = releaseWebView2Object(unsafe.Pointer(v.environment))
		v.environment = nil
	}
	if controller == nil || int32(result) < 0 {
		if controller != nil {
			controller.AddRef()
			v.controller = controller
			h.closeControllerOnSTA(v)
		}
		if h.isCancelled(v) {
			h.finishViewClose(v, v.closeErr)
		} else {
			h.failView(v, fmt.Errorf("creating WebView2 controller failed: HRESULT 0x%08x", uint32(result)))
		}
		return uintptr(windows.S_OK)
	}
	controller.AddRef()
	v.controller = controller
	if h.isCancelled(v) {
		h.closeControllerOnSTA(v)
		h.finishViewClose(v, v.closeErr)
		return uintptr(windows.S_OK)
	}
	if err := h.initializeController(v); err != nil {
		h.closeControllerOnSTA(v)
		if h.isCancelled(v) {
			h.finishViewClose(v, v.closeErr)
		} else {
			h.failView(v, err)
		}
		return uintptr(windows.S_OK)
	}
	h.mu.Lock()
	if h.closing || v.cancelled || v.closed {
		h.mu.Unlock()
		h.closeControllerOnSTA(v)
		h.finishViewClose(v, v.closeErr)
		return uintptr(windows.S_OK)
	}
	if v.processFailurePending {
		h.mu.Unlock()
		return uintptr(windows.S_OK)
	}
	v.ready = true
	selected := h.selected == v.handle
	h.mu.Unlock()
	if v.trusted || selected {
		if err := v.controller.PutIsVisible(true); err != nil {
			h.closeControllerOnSTA(v)
			h.failView(v, fmt.Errorf("showing WebView2 controller: %w", err))
			return uintptr(windows.S_OK)
		}
	}
	if v.trusted {
		h.mu.Lock()
		selectedView := h.views[h.selected]
		showSelected := selectedView != nil && !selectedView.trusted && selectedView.ready && !selectedView.failed && !selectedView.cancelled && selectedView.controller != nil
		h.mu.Unlock()
		if showSelected {
			if err := selectedView.controller.PutIsVisible(true); err != nil {
				h.failView(selectedView, fmt.Errorf("showing selected WebView2 controller: %w", err))
			}
		}
	}
	h.queueCallback(viewCallbackEvent{state: v.callbackState, kind: viewEventReady})
	return uintptr(windows.S_OK)
}

func (h *nativeViewsHost) initializeController(v *nativeView) error {
	if v.controller == nil {
		return errors.New("WebView2 controller creation returned no controller")
	}
	if err := v.controller.PutIsVisible(false); err != nil {
		return fmt.Errorf("hiding WebView2 controller during setup: %w", err)
	}
	webview, err := v.controller.GetCoreWebView2()
	if err != nil {
		return fmt.Errorf("getting WebView2 surface: %w", err)
	}
	v.webview = webview
	settings, err := webview.GetSettings()
	if err != nil {
		return fmt.Errorf("getting WebView2 settings: %w", err)
	}
	lockErr := lockDownViewSettings(settings)
	if lockErr != nil {
		return fmt.Errorf("restricting WebView2 features: %w", lockErr)
	}
	v.guard = newScopedNavigationGuard(v.config.Policy, func(uri string) {
		fmt.Fprintf(os.Stderr, "Matagi blocked WebView2 navigation to %q\n", uri)
	}, true)
	if err := v.guard.attach(unsafe.Pointer(webview)); err != nil {
		return fmt.Errorf("installing WebView2 navigation and permission policy: %w", err)
	}
	processFailedHandler := newViewsProcessFailedHandler(h, v)
	token, err := webview.AddProcessFailed(processFailedHandler.iface)
	if err != nil {
		return fmt.Errorf("monitoring WebView2 process failures: %w", err)
	}
	v.processFailedHandler = processFailedHandler
	v.processFailedToken = token
	v.processFailedAttached = true
	v.parentWindow = h.root
	if err := h.setBounds(v); err != nil {
		return err
	}
	if err := webview.Navigate(v.config.URL); err != nil {
		return fmt.Errorf("navigating WebView2 to its admitted origin: %w", err)
	}
	return nil
}

func (h *nativeViewsHost) selectOnSTA(handle ViewHandle) error {
	h.mu.Lock()
	v := h.views[handle]
	if v == nil {
		h.mu.Unlock()
		return errors.New("unknown WebView2 view handle")
	}
	if !v.ready || v.controller == nil || v.cancelled {
		h.mu.Unlock()
		return ErrViewNotReady
	}
	previous := h.selected
	views := make([]*nativeView, 0, len(h.views))
	for _, candidate := range h.views {
		if candidate.ready && !candidate.trusted {
			views = append(views, candidate)
		}
	}
	trusted := h.views[h.trusted]
	root := h.root
	h.mu.Unlock()

	if root == 0 {
		return errors.New("WebView2 host window is unavailable")
	}
	if trusted != nil && trusted.ready && trusted.controller != nil {
		if err := trusted.controller.PutIsVisible(true); err != nil {
			return fmt.Errorf("showing trusted Matagi controller: %w", err)
		}
	}
	for _, candidate := range views {
		if candidate.handle == handle {
			continue
		}
		if err := candidate.controller.PutIsVisible(false); err != nil {
			return errors.Join(fmt.Errorf("hiding sibling WebView2 controller: %w", err), h.restoreSelectionOnSTA(previous, views, trusted))
		}
	}
	if !v.trusted {
		if err := h.setBounds(v); err != nil {
			return errors.Join(err, h.restoreSelectionOnSTA(previous, views, trusted))
		}
	}
	if err := v.controller.PutIsVisible(true); err != nil {
		return errors.Join(fmt.Errorf("showing selected WebView2 controller: %w", err), h.restoreSelectionOnSTA(previous, views, trusted))
	}
	h.mu.Lock()
	if h.closing || v.cancelled || v.closed {
		h.mu.Unlock()
		return errors.Join(ErrViewNotReady, h.restoreSelectionOnSTA(previous, views, trusted))
	}
	h.selected = handle
	h.mu.Unlock()
	return nil
}

func (h *nativeViewsHost) restoreSelectionOnSTA(handle ViewHandle, views []*nativeView, trusted *nativeView) error {
	var restoreErr error
	for _, candidate := range views {
		visible := candidate.handle == handle && !candidate.trusted
		if err := candidate.controller.PutIsVisible(visible); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restoring WebView2 sibling visibility: %w", err))
		}
	}
	if trusted != nil && trusted.ready && trusted.controller != nil {
		if err := trusted.controller.PutIsVisible(handle == trusted.handle); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restoring trusted Matagi visibility: %w", err))
		}
	}
	return restoreErr
}

func (h *nativeViewsHost) hideOnSTA(handle ViewHandle) error {
	h.mu.Lock()
	v := h.views[handle]
	if v == nil {
		h.mu.Unlock()
		return errors.New("unknown WebView2 view handle")
	}
	if v.trusted {
		h.mu.Unlock()
		return ErrTrustedViewOwned
	}
	if !v.ready || v.controller == nil {
		h.mu.Unlock()
		return ErrViewNotReady
	}
	selected := h.selected == handle
	trusted := h.views[h.trusted]
	h.mu.Unlock()
	if err := v.controller.PutIsVisible(false); err != nil {
		return fmt.Errorf("hiding WebView2 controller: %w", err)
	}
	if selected && trusted != nil && trusted.ready && trusted.controller != nil {
		if err := trusted.controller.PutIsVisible(true); err != nil {
			_ = v.controller.PutIsVisible(true)
			return fmt.Errorf("showing trusted Matagi controller: %w", err)
		}
	}
	if selected {
		h.mu.Lock()
		if h.selected == handle {
			h.selected = h.trusted
		}
		h.mu.Unlock()
	}
	return nil
}

func (h *nativeViewsHost) focusOnSTA(handle ViewHandle) error {
	h.mu.Lock()
	v := h.views[handle]
	root := h.root
	h.mu.Unlock()
	if v == nil {
		return errors.New("unknown WebView2 view handle")
	}
	if !v.ready || v.controller == nil {
		return ErrViewNotReady
	}
	if err := h.selectOnSTA(handle); err != nil {
		return err
	}
	procShowWindow.Call(root, swShowNormal)
	if activated, _, err := procSetForegroundW.Call(root); activated == 0 {
		return fmt.Errorf("activating Matagi view host window: %w", err)
	}
	viewsSetFocus.Call(root)
	return v.controller.MoveFocus(webview2.COREWEBVIEW2_MOVE_FOCUS_REASON_PROGRAMMATIC)
}

func (h *nativeViewsHost) closeAllOnSTA() {
	h.mu.Lock()
	h.closing = true
	views := make([]*nativeView, 0, len(h.views))
	for _, v := range h.views {
		v.cancelled = true
		h.syncCallbackState(v)
		views = append(views, v)
	}
	h.mu.Unlock()
	for _, v := range views {
		h.closeViewOnSTA(v)
	}
	h.finishCloseIfEmpty()
}

func (h *nativeViewsHost) closeViewOnSTA(v *nativeView) {
	if v == nil || v.closed {
		return
	}
	if v.pendingEnvironment || v.pendingController {
		return
	}
	h.mu.Lock()
	var trusted *nativeView
	if h.selected == v.handle && !v.trusted {
		h.selected = h.trusted
		trusted = h.views[h.trusted]
	}
	h.mu.Unlock()
	if trusted != nil && trusted.ready && trusted.controller != nil {
		_ = trusted.controller.PutIsVisible(true)
	}
	h.closeControllerOnSTA(v)
	h.finishViewClose(v, v.closeErr)
}

func (h *nativeViewsHost) closeControllerOnSTA(v *nativeView) {
	if v.controller == nil || v.controllerClosed {
		return
	}
	v.controllerClosed = true
	guard := v.guard
	if guard != nil && v.webview != nil {
		if err := guard.detach(unsafe.Pointer(v.webview)); err != nil {
			v.closeErr = errors.Join(v.closeErr, fmt.Errorf("removing WebView2 navigation handlers: %w", err))
		}
	}
	if v.processFailedAttached && v.webview != nil {
		if err := v.webview.RemoveProcessFailed(v.processFailedToken); err != nil {
			v.closeErr = errors.Join(v.closeErr, fmt.Errorf("removing WebView2 process failure handler: %w", err))
		}
		v.processFailedAttached = false
	}
	if err := v.controller.Close(); err != nil {
		v.closeErr = errors.Join(v.closeErr, fmt.Errorf("closing WebView2 controller: %w", err))
	}
	if v.webview != nil {
		if err := releaseWebView2Object(unsafe.Pointer(v.webview)); err != nil {
			v.closeErr = errors.Join(v.closeErr, fmt.Errorf("releasing WebView2 surface: %w", err))
		}
		v.webview = nil
	}
	if err := releaseWebView2Object(unsafe.Pointer(v.controller)); err != nil {
		v.closeErr = errors.Join(v.closeErr, fmt.Errorf("releasing WebView2 controller: %w", err))
	}
	v.controller = nil
	runtime.KeepAlive(guard)
	v.guard = nil
	v.processFailedHandler = nil
}

func (h *nativeViewsHost) finishViewClose(v *nativeView, err error) {
	if v.closed {
		return
	}
	if v.controller != nil {
		h.closeControllerOnSTA(v)
	}
	if v.environment != nil {
		_ = releaseWebView2Object(unsafe.Pointer(v.environment))
		v.environment = nil
	}
	h.mu.Lock()
	if v.closed {
		h.mu.Unlock()
		return
	}
	v.closed = true
	v.pendingEnvironment = false
	v.pendingController = false
	h.syncCallbackState(v)
	if err != nil && !errors.Is(v.closeErr, err) {
		v.closeErr = errors.Join(v.closeErr, err)
	}
	h.closeErr = errors.Join(h.closeErr, v.closeErr)
	delete(h.views, v.handle)
	delete(h.profiles, v.profile)
	close(v.closedCh)
	h.mu.Unlock()
	h.queueCallback(viewCallbackEvent{state: v.callbackState, kind: viewEventClosed})
	h.scheduleCloseFinalization()
}

func (h *nativeViewsHost) failView(v *nativeView, err error) {
	if v.controller != nil {
		h.closeControllerOnSTA(v)
	}
	if v.environment != nil {
		_ = releaseWebView2Object(unsafe.Pointer(v.environment))
		v.environment = nil
	}
	h.mu.Lock()
	cancelled := h.closing || v.cancelled || v.closed
	v.failed = true
	if cancelled && !v.closed {
		v.closeErr = errors.Join(v.closeErr, err)
	}
	h.syncCallbackState(v)
	h.mu.Unlock()
	if cancelled {
		h.finishViewClose(v, v.closeErr)
		return
	}
	h.queueCallback(viewCallbackEvent{state: v.callbackState, kind: viewEventFailed, err: err})
}

func (h *nativeViewsHost) isCancelled(v *nativeView) bool {
	h.mu.Lock()
	cancelled := h.closing || v.cancelled || v.closed
	h.mu.Unlock()
	return cancelled
}

func (h *nativeViewsHost) queueProcessFailure(v *nativeView, failure error) {
	h.mu.Lock()
	if h.closing || v.cancelled || v.closed || v.failed || v.processFailurePending {
		h.mu.Unlock()
		return
	}
	v.processFailurePending = true
	h.syncCallbackState(v)
	h.mu.Unlock()
	if _, err := h.enqueue(false, func() error {
		h.mu.Lock()
		if h.closing || v.cancelled || v.closed || v.failed {
			v.processFailurePending = false
			h.syncCallbackState(v)
			h.mu.Unlock()
			return nil
		}
		v.processFailurePending = false
		v.failed = true
		v.ready = false
		h.syncCallbackState(v)
		selected := h.selected == v.handle
		if selected && !v.trusted {
			h.selected = h.trusted
		}
		trusted := h.views[h.trusted]
		showTrusted := selected && !v.trusted && trusted != nil && trusted.ready && trusted.controller != nil
		h.mu.Unlock()

		if v.controller != nil {
			if err := v.controller.PutIsVisible(false); err != nil {
				failure = errors.Join(failure, fmt.Errorf("hiding failed WebView2 controller: %w", err))
			}
		}
		if showTrusted {
			if err := trusted.controller.PutIsVisible(true); err != nil {
				failure = errors.Join(failure, fmt.Errorf("showing trusted Matagi controller: %w", err))
			}
		}
		h.queueCallback(viewCallbackEvent{state: v.callbackState, kind: viewEventFailed, err: failure})
		return nil
	}); err != nil {
		h.mu.Lock()
		v.processFailurePending = false
		h.syncCallbackState(v)
		if !h.closing && !v.closed {
			h.closeErr = errors.Join(h.closeErr, fmt.Errorf("queueing WebView2 process failure: %w", err))
		}
		h.mu.Unlock()
	}
}

func (h *nativeViewsHost) discardUnstartedView(v *nativeView) {
	h.mu.Lock()
	if !v.closed {
		v.closed = true
		v.callbacksSuppressed = true
		h.syncCallbackState(v)
		delete(h.views, v.handle)
		delete(h.profiles, v.profile)
		close(v.closedCh)
	}
	h.mu.Unlock()
}

func (h *nativeViewsHost) finishCloseIfEmpty() {
	h.mu.Lock()
	if !h.closing || len(h.views) != 0 {
		h.mu.Unlock()
		return
	}
	root := h.root
	h.mu.Unlock()
	if root != 0 {
		if destroyed, _, err := procDestroyWindow.Call(root); destroyed == 0 {
			h.mu.Lock()
			h.closeErr = errors.Join(h.closeErr, fmt.Errorf("destroying Matagi view host window: %w", err))
			h.mu.Unlock()
		}
	}
}

func (h *nativeViewsHost) scheduleCloseFinalization() {
	h.mu.Lock()
	if !h.closing || len(h.views) != 0 || h.root == 0 || h.finalizePosted {
		h.mu.Unlock()
		return
	}
	h.finalizePosted = true
	root := h.root
	h.mu.Unlock()
	if posted, _, err := procPostMessageW.Call(root, wmViewsFinalize, 0, 0); posted == 0 {
		h.mu.Lock()
		h.finalizePosted = false
		h.closeErr = errors.Join(h.closeErr, fmt.Errorf("posting WebView2 host close completion: %w", err))
		h.mu.Unlock()
	}
}

func (h *nativeViewsHost) resizeViews(hwnd uintptr) {
	var rect dpiRect
	if ok, _, err := viewsGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rect))); ok == 0 {
		h.mu.Lock()
		h.closeErr = errors.Join(h.closeErr, fmt.Errorf("reading Matagi view host bounds: %w", err))
		h.mu.Unlock()
		return
	}
	width := rect.right - rect.left
	height := rect.bottom - rect.top
	dpi := viewWindowDPI(hwnd)
	h.mu.Lock()
	views := make([]*nativeView, 0, len(h.views))
	for _, v := range h.views {
		if v.ready && v.parentWindow == hwnd && v.controller != nil {
			views = append(views, v)
		}
	}
	h.mu.Unlock()
	for _, v := range views {
		bounds := webview2.RECT{Left: 0, Top: 0, Right: width, Bottom: height}
		if !v.trusted {
			bounds.Top = scaleForDPI(chromeHeightDIP, dpi)
			if bounds.Bottom < bounds.Top {
				bounds.Bottom = bounds.Top
			}
		}
		if err := v.controller.PutBounds(bounds); err != nil {
			h.failView(v, fmt.Errorf("resizing WebView2 controller: %w", err))
			continue
		}
		if err := v.controller.NotifyParentWindowPositionChanged(); err != nil {
			h.failView(v, fmt.Errorf("notifying WebView2 controller of parent position: %w", err))
		}
	}
}

func (h *nativeViewsHost) setBounds(v *nativeView) error {
	var rect dpiRect
	if ok, _, err := viewsGetClientRect.Call(h.root, uintptr(unsafe.Pointer(&rect))); ok == 0 {
		return fmt.Errorf("reading Matagi view host bounds: %w", err)
	}
	dpi := viewWindowDPI(h.root)
	bounds := webview2.RECT{Left: 0, Top: 0, Right: rect.right - rect.left, Bottom: rect.bottom - rect.top}
	if !v.trusted {
		bounds.Top = scaleForDPI(chromeHeightDIP, dpi)
		if bounds.Bottom < bounds.Top {
			bounds.Bottom = bounds.Top
		}
	}
	if err := v.controller.PutBounds(bounds); err != nil {
		return fmt.Errorf("setting WebView2 controller bounds: %w", err)
	}
	if err := v.controller.NotifyParentWindowPositionChanged(); err != nil {
		return fmt.Errorf("updating WebView2 parent position: %w", err)
	}
	return nil
}

func viewWindowDPI(hwnd uintptr) uint32 {
	proc := user32.NewProc("GetDpiForWindow")
	if proc.Find() == nil {
		if dpi, _, _ := proc.Call(hwnd); dpi != 0 {
			return uint32(dpi)
		}
	}
	return systemDPI()
}

func releaseWebView2Object(object unsafe.Pointer) error {
	if object == nil {
		return nil
	}
	unknown := (*webview2.IUnknown)(object)
	if unknown.Vtbl == nil {
		return errors.New("WebView2 COM object has no IUnknown vtable")
	}
	unknown.Vtbl.Release.Call(uintptr(object))
	return nil
}

func lockDownViewSettings(settings *webview2.ICoreWebView2Settings) error {
	if settings == nil {
		return errors.New("WebView2 settings are unavailable")
	}
	defer releaseWebView2Object(unsafe.Pointer(settings))
	for _, disable := range []func(bool) error{
		settings.PutIsWebMessageEnabled,
		settings.PutAreHostObjectsAllowed,
		settings.PutAreDevToolsEnabled,
		settings.PutAreDefaultContextMenusEnabled,
		settings.PutIsStatusBarEnabled,
	} {
		if err := disable(false); err != nil {
			return err
		}
	}
	return nil
}
