//go:build windows

package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/edge"
	"github.com/wailsapp/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
)

var user32 = windows.NewLazySystemDLL("user32.dll")
var procRegisterClassExW = user32.NewProc("RegisterClassExW")
var procUnregisterClassW = user32.NewProc("UnregisterClassW")
var procCreateWindowExW = user32.NewProc("CreateWindowExW")
var procDefWindowProcW = user32.NewProc("DefWindowProcW")
var procDestroyWindow = user32.NewProc("DestroyWindow")
var procShowWindow = user32.NewProc("ShowWindow")
var procUpdateWindow = user32.NewProc("UpdateWindow")
var procGetMessageW = user32.NewProc("GetMessageW")
var procTranslateMessage = user32.NewProc("TranslateMessage")
var procDispatchMessageW = user32.NewProc("DispatchMessageW")
var procPostMessageW = user32.NewProc("PostMessageW")
var procPostQuitMessage = user32.NewProc("PostQuitMessage")
var procFindWindowW = user32.NewProc("FindWindowW")
var procSetForegroundW = user32.NewProc("SetForegroundWindow")
var procRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")
var procAllowSetForegroundWindow = user32.NewProc("AllowSetForegroundWindow")
var procLoadCursorW = user32.NewProc("LoadCursorW")
var procMessageBoxW = user32.NewProc("MessageBoxW")

var (
	activeShellMu   sync.Mutex
	activeShell     *shell
	wndProcOnce     sync.Once
	wndProcCallback uintptr
)

const (
	wsOverlappedWindow = 0x00CF0000
	cwUseDefault       = 0x80000000
	swHide             = 0
	swShowNormal       = 1
	idcArrow           = 32512
	colorWindow        = 5
	wmDestroy          = 0x0002
	wmSize             = 0x0005
	wmClose            = 0x0010
	windowClass        = "MatagiWebView2Window"
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type winMessage struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Point   struct{ X, Y int32 }
	Private uint32
}

type shell struct {
	hwnd            uintptr
	chromium        *edge.Chromium
	controller      *edge.ICoreWebView2Controller
	guard           *navigationGuard
	closeOnce       sync.Once
	initializing    bool
	quitPending     bool
	activatePending bool
	closed          bool
	activateMsg     uint32
}

type native struct{}

func Native() Platform { return native{} }

func (native) RuntimeVersion() (string, error) {
	return webviewloader.GetAvailableCoreWebView2BrowserVersionString("")
}

func currentUserID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("resolving current user: %w", err)
	}
	return user.User.Sid.String(), nil
}

func (native) AcquireInstance() (func(), error) {
	userID, err := currentUserID()
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(InstanceName(userID))
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return nil, ErrAlreadyRunning
	}
	if err != nil {
		return nil, fmt.Errorf("creating desktop single-instance guard: %w", err)
	}
	return func() { _ = windows.CloseHandle(handle) }, nil
}

func (native) Activate() error {
	userID, err := currentUserID()
	if err != nil {
		return err
	}
	class, err := windows.UTF16PtrFromString(windowClass)
	if err != nil {
		return err
	}
	messageName, err := windows.UTF16PtrFromString(ActivateMessageName(userID))
	if err != nil {
		return err
	}
	message, _, callErr := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(messageName)))
	if message == 0 {
		return fmt.Errorf("registering desktop activation message: %w", callErr)
	}
	procAllowSetForegroundWindow.Call(^uintptr(0))
	deadline := time.Now().Add(10 * time.Second)
	for {
		hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(class)), 0)
		if hwnd != 0 {
			if ok, _, err := procPostMessageW.Call(hwnd, message, 0, 0); ok == 0 {
				return fmt.Errorf("posting desktop activation message: %w", err)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("running Matagi desktop has no window to activate")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (native) ReportError(title, message string) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(message)
	const mbOKIconError = 0x00000000 | 0x00000010
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), mbOKIconError)
}

func (native) Open(ctx context.Context, w Window) error {
	if w.Policy == nil || !w.Policy.AllowNavigation(w.URL) {
		return errors.New("initial WebView2 URL is outside the navigation policy")
	}
	if ctx == nil {
		return errors.New("desktop context is required")
	}
	version, err := (native{}).RuntimeVersion()
	if err != nil {
		return fmt.Errorf("detecting the WebView2 Runtime: %w", err)
	}
	if version == "" {
		return ErrWebView2Missing
	}
	dataDir := w.DataDir
	if dataDir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("locating the WebView2 profile folder: %w", err)
		}
		dataDir = filepath.Join(cache, "Matagi", "WebView2")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("creating the WebView2 profile folder: %w", err)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	switch err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); {
	case err == nil, errors.Is(err, syscall.Errno(1)):
		defer windows.CoUninitialize()
	default:
		return fmt.Errorf("initializing COM for WebView2: %w", err)
	}

	s := &shell{}
	userID, err := currentUserID()
	if err != nil {
		return err
	}
	messageName, err := windows.UTF16PtrFromString(ActivateMessageName(userID))
	if err != nil {
		return err
	}
	activateMessageID, _, callErr := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(messageName)))
	if activateMessageID == 0 {
		return fmt.Errorf("registering desktop activation message: %w", callErr)
	}
	s.activateMsg = uint32(activateMessageID)
	activeShellMu.Lock()
	if activeShell != nil {
		activeShellMu.Unlock()
		return errors.New("a Matagi desktop window is already open")
	}
	activeShell = s
	activeShellMu.Unlock()
	defer func() {
		activeShellMu.Lock()
		activeShell = nil
		activeShellMu.Unlock()
	}()

	var instance windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &instance); err != nil {
		return fmt.Errorf("locating the application module: %w", err)
	}
	className, err := windows.UTF16PtrFromString(windowClass)
	if err != nil {
		return err
	}
	title := w.Title
	if title == "" {
		title = "Matagi"
	}
	titlePtr, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return err
	}
	wndProcOnce.Do(func() { wndProcCallback = windows.NewCallback(windowProc) })
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
		return fmt.Errorf("registering the WebView2 window class: %w", callErr)
	}
	defer procUnregisterClassW.Call(uintptr(unsafe.Pointer(className)), uintptr(instance))

	hwnd, _, callErr := procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(titlePtr)),
		wsOverlappedWindow, cwUseDefault, cwUseDefault, 1100, 780,
		0, 0, uintptr(instance), 0)
	if hwnd == 0 {
		return fmt.Errorf("creating the Matagi window: %w", callErr)
	}
	s.hwnd = hwnd
	procShowWindow.Call(hwnd, swShowNormal)
	procUpdateWindow.Call(hwnd)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			procPostMessageW.Call(hwnd, wmClose, 0, 0)
		case <-done:
		}
	}()
	defer func() {
		s.closeWebView()
		if s.hwnd != 0 {
			procDestroyWindow.Call(s.hwnd)
		}
	}()

	c := edge.NewChromium()
	c.DataPath = dataDir
	c.SetErrorCallback(func(err error) {
		message := "WebView2 failed: " + err.Error()
		fmt.Fprintln(os.Stderr, "Matagi:", message)
		(native{}).ReportError("Matagi", message)
		if s.hwnd != 0 {
			procPostMessageW.Call(s.hwnd, wmClose, 0, 0)
		}
	})
	c.SetGlobalPermission(edge.CoreWebView2PermissionStateDeny)
	s.chromium = c
	s.initializing = true
	if !c.Embed(hwnd) {
		return errors.New("creating the WebView2 controller failed")
	}
	s.initializing = false
	if s.quitPending {
		s.closeWebView()
		procDestroyWindow.Call(hwnd)
		return nil
	}
	if s.closed {
		return nil
	}
	s.controller = c.GetController()
	if s.controller == nil {
		return errors.New("WebView2 did not create a controller")
	}
	if s.activatePending {
		s.activatePending = false
		procShowWindow.Call(hwnd, swShowNormal)
		procSetForegroundW.Call(hwnd)
		c.Resize()
		c.Focus()
	}
	webview, err := s.controller.GetCoreWebView2()
	if err != nil {
		return fmt.Errorf("getting the WebView2 surface: %w", err)
	}
	if err := lockDown(c); err != nil {
		return fmt.Errorf("restricting WebView2 features: %w", err)
	}
	s.guard = newNavigationGuard(w.Policy, func(uri string) {
		fmt.Fprintf(os.Stderr, "Matagi blocked WebView2 navigation to %q\n", uri)
	})
	if err := s.guard.attach(unsafe.Pointer(webview)); err != nil {
		return fmt.Errorf("installing WebView2 navigation policy: %w", err)
	}
	c.ProcessFailedCallback = func(_ *edge.ICoreWebView2, args *edge.ICoreWebView2ProcessFailedEventArgs) {
		kind, err := args.GetProcessFailedKind()
		if err != nil {
			(native{}).ReportError("Matagi WebView2 failure", "A WebView2 process failed and its failure type could not be read.")
			return
		}
		(native{}).ReportError("Matagi WebView2 failure", fmt.Sprintf("A WebView2 process failed (kind %d).", kind))
	}
	c.Navigate(w.URL)
	c.Resize()
	c.Focus()

	var message winMessage
	for {
		result, _, callErr := procGetMessageW.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		switch int32(result) {
		case 0:
			return nil
		case -1:
			return fmt.Errorf("reading the WebView2 window message loop: %w", callErr)
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&message)))
	}
}

func windowProc(hwnd, message, wParam, lParam uintptr) uintptr {
	activeShellMu.Lock()
	s := activeShell
	activeShellMu.Unlock()
	if s != nil {
		switch uint32(message) {
		case wmSize:
			if s.chromium != nil {
				s.chromium.Resize()
			}
		case wmClose:
			if s.initializing {
				// The WebView2 controller is created asynchronously. Keep its
				// parent window alive until Embed completes, then honor the close.
				s.quitPending = true
				return 0
			}
			s.closeWebView()
			procDestroyWindow.Call(hwnd)
			return 0
		case wmDestroy:
			s.hwnd = 0
			procPostQuitMessage.Call(0)
			return 0
		}
		if s.activateMsg != 0 && uint32(message) == s.activateMsg {
			procShowWindow.Call(hwnd, swShowNormal)
			procSetForegroundW.Call(hwnd)
			if s.initializing || s.controller == nil {
				s.activatePending = true
				return 0
			}
			if s.chromium != nil {
				s.chromium.Resize()
				s.chromium.Focus()
			}
			return 0
		}
	}
	result, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return result
}

func (s *shell) closeWebView() {
	s.closeOnce.Do(func() {
		s.closed = true
		if s.chromium != nil {
			s.chromium.ShuttingDown()
		}
		if s.controller != nil {
			comCall(unsafe.Pointer(s.controller), slotControllerClose)
		}
		runtime.KeepAlive(s.guard)
	})
}

func lockDown(c *edge.Chromium) error {
	settings, err := c.GetSettings()
	if err != nil {
		return err
	}
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
