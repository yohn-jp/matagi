//go:build windows

package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	testExecuteScriptSlot = 29
	keyEventUnicode       = 0x0004
	keyEventKeyUp         = 0x0002
)

var (
	testIIDExecuteScriptCompleted = windows.GUID{Data1: 0x49511172, Data2: 0xcc67, Data3: 0x4bca, Data4: [8]byte{0x99, 0x23, 0x13, 0x71, 0x12, 0xf4, 0xc4, 0xcc}}
	testSendMessageW              = user32.NewProc("SendMessageW")
	testKeybdEvent                = user32.NewProc("keybd_event")
	testIsWindow                  = user32.NewProc("IsWindow")
	testGetWindowTextW            = user32.NewProc("GetWindowTextW")
)

type moveWindowsSnapshot struct {
	controller       uintptr
	webview          uintptr
	guard            uintptr
	profile          string
	parent           uintptr
	location         ViewLocation
	detachedWindow   uintptr
	returnButton     uintptr
	closeButton      uintptr
	windowBounds     DIPBounds
	controllerBounds viewsRECT
	selected         ViewHandle
	visible          bool
}

func TestDetachedFocusRequestsUseToolbarTabOrder(t *testing.T) {
	view := &nativeView{returnButton: 11, closeButton: 22}
	if got := detachedFocusTarget(view, moveFocusReasonNext); got != view.returnButton {
		t.Fatalf("forward WebView tab focus = %#x, want Return button %#x", got, view.returnButton)
	}
	if got := detachedFocusTarget(view, moveFocusReasonPrevious); got != view.closeButton {
		t.Fatalf("reverse WebView tab focus = %#x, want Close button %#x", got, view.closeButton)
	}
	for _, reason := range []uintptr{moveFocusReasonProgrammatic, 99} {
		if got := detachedFocusTarget(view, reason); got != 0 {
			t.Errorf("focus reason %d selected toolbar control %#x, want no traversal", reason, got)
		}
	}
}

func startMoveTestHost(t *testing.T, ctx context.Context, profileHome string, callbacks ViewCallbacks) (*nativeViewsHost, ViewHandle, nativeViewEvents) {
	t.Helper()
	trustedEvents := make(nativeViewEvents, 16)
	if callbacks.Ready == nil && callbacks.Failed == nil && callbacks.Closed == nil {
		callbacks = callbacksFor(trustedEvents)
	} else {
		base := callbacksFor(trustedEvents)
		if callbacks.Ready == nil {
			callbacks.Ready = base.Ready
		}
		if callbacks.Failed == nil {
			callbacks.Failed = base.Failed
		}
		if callbacks.Closed == nil {
			callbacks.Closed = base.Closed
		}
	}
	trustedServer := testViewServer()
	t.Cleanup(trustedServer.Close)
	policy, err := NewTrustedViewPolicy(trustedServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	views, trusted, err := StartViewsHost(ctx, ViewConfig{
		URL:           trustedServer.URL,
		ProfileFolder: filepath.Join(profileHome, "trusted"),
		Policy:        policy,
	}, callbacks)
	if err != nil {
		t.Fatalf("starting real WebView2 host: %v", err)
	}
	host, ok := views.(*nativeViewsHost)
	if !ok {
		t.Fatalf("desktop host type is %T, want *nativeViewsHost", views)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := host.CloseAll(cleanupCtx); err != nil {
			t.Errorf("closing real WebView2 host: %v", err)
		}
	})
	waitNativeViewEvent(t, ctx, trustedEvents, "ready")
	return host, trusted, trustedEvents
}

func createMoveTestView(t *testing.T, ctx context.Context, host *nativeViewsHost, url, profile string, callbacks ViewCallbacks) (ViewHandle, nativeViewEvents) {
	t.Helper()
	return createConfiguredMoveTestView(t, ctx, host, ViewConfig{URL: url, ProfileFolder: profile}, callbacks)
}

func createConfiguredMoveTestView(t *testing.T, ctx context.Context, host *nativeViewsHost, config ViewConfig, callbacks ViewCallbacks) (ViewHandle, nativeViewEvents) {
	t.Helper()
	events := make(nativeViewEvents, 16)
	if callbacks.Ready == nil {
		callbacks.Ready = func(ViewHandle) { events <- nativeViewEvent{kind: "ready", at: time.Now()} }
	}
	if callbacks.Failed == nil {
		callbacks.Failed = func(_ ViewHandle, err error) { events <- nativeViewEvent{kind: "failed", err: err, at: time.Now()} }
	}
	if callbacks.Closed == nil {
		callbacks.Closed = func(ViewHandle) { events <- nativeViewEvent{kind: "closed", at: time.Now()} }
	}
	policy, err := NewServiceViewPolicy(config.URL)
	if err != nil {
		t.Fatal(err)
	}
	config.Policy = policy
	handle, err := host.Create(ctx, config, callbacks)
	if err != nil {
		t.Fatalf("creating real WebView2 service view: %v", err)
	}
	waitNativeViewEvent(t, ctx, events, "ready")
	return handle, events
}

func snapshotMoveTestView(ctx context.Context, host *nativeViewsHost, handle ViewHandle) (moveWindowsSnapshot, error) {
	var snapshot moveWindowsSnapshot
	err := host.invoke(ctx, false, func() error {
		host.mu.Lock()
		view := host.views[handle]
		snapshot.selected = host.selected
		host.mu.Unlock()
		if view == nil || view.controller == nil || view.webview == nil {
			return errors.New("WebView2 view is unavailable")
		}
		parent, err := view.controller.GetParentWindow()
		if err != nil {
			return err
		}
		bounds, err := view.controller.GetBounds()
		if err != nil {
			return err
		}
		visible, err := view.controller.GetIsVisible()
		if err != nil {
			return err
		}
		snapshot.controller = uintptr(view.controller.pointer)
		snapshot.webview = uintptr(view.webview.pointer)
		snapshot.guard = uintptr(unsafe.Pointer(view.guard))
		snapshot.profile = view.profile
		snapshot.parent = parent
		snapshot.location = view.location
		snapshot.detachedWindow = view.detachedWindow
		snapshot.returnButton = view.returnButton
		snapshot.closeButton = view.closeButton
		snapshot.windowBounds = view.windowBounds
		snapshot.controllerBounds = bounds
		snapshot.visible = visible
		return nil
	})
	return snapshot, err
}

func executeMoveTestScript(t *testing.T, ctx context.Context, host *nativeViewsHost, handle ViewHandle, script string) string {
	t.Helper()
	scriptPointer, err := windows.UTF16PtrFromString(script)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan string, 1)
	handler := newEventHandler(testIIDExecuteScriptCompleted, func(pointer unsafe.Pointer) {
		if pointer == nil {
			result <- "null"
			return
		}
		value := windows.UTF16PtrToString((*uint16)(pointer))
		// ExecuteScript passes an input LPCWSTR borrowed for this callback;
		// unlike a WebView2 out-parameter it must not be CoTaskMemFree'd.
		result <- value
	})
	err = host.invoke(ctx, false, func() error {
		host.mu.Lock()
		view := host.views[handle]
		host.mu.Unlock()
		if view == nil || view.webview == nil {
			return errors.New("WebView2 page is unavailable")
		}
		callErr := viewsCOMCall(view.webview.pointer, testExecuteScriptSlot, uintptr(unsafe.Pointer(scriptPointer)), uintptr(unsafe.Pointer(handler)))
		runtime.KeepAlive(scriptPointer)
		return callErr
	})
	if err != nil {
		t.Fatalf("submitting page-state script: %v", err)
	}
	select {
	case value := <-result:
		runtime.KeepAlive(handler)
		return value
	case <-ctx.Done():
		t.Fatalf("waiting for page-state script: %v", ctx.Err())
		return ""
	}
}

func waitForMoveTestScript(t *testing.T, ctx context.Context, host *nativeViewsHost, handle ViewHandle, script, want string) string {
	t.Helper()
	for {
		got := executeMoveTestScript(t, ctx, host, handle, script)
		if got == want {
			return got
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("page script result = %q, want %q: %v", got, want, ctx.Err())
		}
	}
}

func typeMoveTestInput(t *testing.T, value string) {
	t.Helper()
	for _, r := range value {
		if r > 0xffff {
			t.Fatalf("test input rune %q is outside the BMP", r)
		}
		testKeybdEvent.Call(0, uintptr(r), keyEventUnicode, 0)
		testKeybdEvent.Call(0, uintptr(r), keyEventUnicode|keyEventKeyUp, 0)
		time.Sleep(5 * time.Millisecond)
	}
}

func moveTestWindowText(hwnd uintptr) string {
	buffer := make([]uint16, 256)
	if len(buffer) == 0 {
		return ""
	}
	length, _, _ := testGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	return windows.UTF16ToString(buffer[:length])
}

func TestWindowsControllerMovePreservesTypedPageAndControllerIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	profileHome, err := os.MkdirTemp("", "matagi-move-82-profile-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profileHome) })

	var navigations atomic.Int32
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			navigations.Add(1)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html><html><body><label for="entry">Input</label><input id="entry" autofocus></body></html>`)
	}))
	t.Cleanup(page.Close)
	returned := make(chan uint32, 1)
	var host *nativeViewsHost
	callbacks := ViewCallbacks{ReturnToTabs: func(handle ViewHandle) {
		threadID := windows.GetCurrentThreadId()
		if moveErr := host.Move(context.Background(), handle, ViewIntegrated, DIPBounds{}); moveErr != nil {
			returned <- 0
			return
		}
		returned <- threadID
	}}
	host, _, _ = startMoveTestHost(t, ctx, profileHome, callbacks)
	view, events := createConfiguredMoveTestView(t, ctx, host, ViewConfig{
		URL:               page.URL,
		ProfileFolder:     filepath.Join(profileHome, "service"),
		WindowTitle:       "Service view",
		ReturnToTabsLabel: "タブに戻る",
		CloseViewLabel:    "ビューを閉じる",
	}, callbacks)
	if err := host.Focus(ctx, view); err != nil {
		t.Fatalf("focusing service page before typing: %v", err)
	}
	waitForMoveTestScript(t, ctx, host, view, `document.readyState === "complete" && !!document.getElementById("entry")`, "true")
	if got := executeMoveTestScript(t, ctx, host, view, `document.getElementById("entry").focus(); true`); got != "true" {
		t.Fatalf("focusing page input returned %q", got)
	}
	const typed = "matagi-82"
	typeMoveTestInput(t, typed)
	wantTyped, _ := json.Marshal(typed)
	waitForMoveTestScript(t, ctx, host, view, `document.getElementById("entry").value`, string(wantTyped))
	before, err := snapshotMoveTestView(ctx, host, view)
	if err != nil {
		t.Fatal(err)
	}

	if err := host.Move(ctx, view, ViewDetached, DIPBounds{X: 40, Y: 50, Width: 800, Height: 600}); err != nil {
		t.Fatalf("detaching real WebView2 controller: %v", err)
	}
	detached, err := snapshotMoveTestView(ctx, host, view)
	if err != nil {
		t.Fatal(err)
	}
	assertSameMoveTestController(t, before, detached)
	if detached.location != ViewDetached || detached.detachedWindow == 0 || detached.parent != detached.detachedWindow {
		t.Fatalf("detached view location/ParentWindow = %#v", detached)
	}
	if detached.windowBounds.Width <= 0 || detached.windowBounds.Height <= 0 || detached.controllerBounds.Top != scaleForDPI(detachedToolbarHeightDIP, viewWindowDPI(detached.detachedWindow)) {
		t.Fatalf("detached DIP/controller bounds = window:%#v controller:%#v", detached.windowBounds, detached.controllerBounds)
	}
	if detached.selected != host.trusted {
		t.Fatalf("selected view after detach = %q, want trusted workspace %q", detached.selected, host.trusted)
	}
	if moveTestWindowText(detached.returnButton) != "タブに戻る" || moveTestWindowText(detached.closeButton) != "ビューを閉じる" {
		t.Fatalf("detached native controls are not localized: %q / %q", moveTestWindowText(detached.returnButton), moveTestWindowText(detached.closeButton))
	}
	if moveTestWindowText(detached.detachedWindow) != "Service view" {
		t.Fatalf("detached native window title = %q, want configured service title", moveTestWindowText(detached.detachedWindow))
	}
	if got := executeMoveTestScript(t, ctx, host, view, `document.getElementById("entry").value`); got != string(wantTyped) {
		t.Fatalf("typed page value after detach = %s, want %s", got, wantTyped)
	}
	if got := navigations.Load(); got != 1 {
		t.Fatalf("page navigation count after detach = %d, want one initial navigation", got)
	}
	// WM_COMMAND is handled with LRESULT 0; syscall.Call reports ERROR_SUCCESS as a non-nil error.
	testSendMessageW.Call(detached.detachedWindow, wmCommand, commandReturnToTabs, detached.returnButton)
	select {
	case callbackThread := <-returned:
		if callbackThread == 0 {
			t.Fatal("native Return to tabs callback could not enqueue the controller move")
		}
		if callbackThread == host.staThreadID {
			t.Fatalf("native action callback ran on WebView2 STA thread %d", callbackThread)
		}
	case <-ctx.Done():
		t.Fatalf("waiting for native Return to tabs callback: %v", ctx.Err())
	}
	after, err := snapshotMoveTestView(ctx, host, view)
	if err != nil {
		t.Fatal(err)
	}
	assertSameMoveTestController(t, before, after)
	if after.location != ViewIntegrated || after.parent != host.root || after.detachedWindow != 0 {
		t.Fatalf("returned view location/ParentWindow = %#v", after)
	}
	if after.selected != view || after.controllerBounds.Top != scaleForDPI(chromeHeightDIP, viewWindowDPI(host.root)) {
		t.Fatalf("integrated selection/controller bounds after return = selected:%q bounds:%#v", after.selected, after.controllerBounds)
	}
	if got := executeMoveTestScript(t, ctx, host, view, `document.getElementById("entry").value`); got != string(wantTyped) {
		t.Fatalf("typed page value after return = %s, want %s", got, wantTyped)
	}
	if got := navigations.Load(); got != 1 {
		t.Fatalf("page navigation count after roundtrip = %d, want one initial navigation", got)
	}
	if err := host.Move(ctx, view, ViewDetached, DIPBounds{X: 60, Y: 70, Width: 800, Height: 600}); err != nil {
		t.Fatalf("detaching again to exercise native Close view button: %v", err)
	}
	closeSnapshot, err := snapshotMoveTestView(ctx, host, view)
	if err != nil {
		t.Fatal(err)
	}
	testSendMessageW.Call(closeSnapshot.detachedWindow, wmCommand, commandCloseView, closeSnapshot.closeButton)
	waitNativeViewEvent(t, ctx, events, "closed")
	select {
	case <-host.done:
		t.Fatal("native Close view button stopped the Matagi host")
	default:
	}
}

func TestWindowsControllerMoveRollsBackRealParentWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	profileHome, err := os.MkdirTemp("", "matagi-move-82-rollback-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profileHome) })
	page := testViewServer()
	t.Cleanup(page.Close)
	host, _, _ := startMoveTestHost(t, ctx, profileHome, ViewCallbacks{})
	view, _ := createMoveTestView(t, ctx, host, page.URL, filepath.Join(profileHome, "service"), ViewCallbacks{})
	sibling, _ := createMoveTestView(t, ctx, host, page.URL, filepath.Join(profileHome, "sibling"), ViewCallbacks{})
	if err := host.Select(ctx, sibling); err != nil {
		t.Fatal(err)
	}
	before, err := snapshotMoveTestView(ctx, host, view)
	if err != nil {
		t.Fatal(err)
	}
	if before.visible || before.selected != sibling {
		t.Fatalf("rollback setup visibility/selection = visible:%t selected:%q, want hidden view with sibling selected", before.visible, before.selected)
	}
	rollbackCause := errors.New("injected controller show failure after reparent")
	var targetWindow uintptr
	err = host.invoke(ctx, false, func() error {
		host.mu.Lock()
		v := host.views[view]
		previousSelected := host.selected
		trusted := host.views[host.trusted]
		host.mu.Unlock()
		previousBounds, err := v.controller.GetBounds()
		if err != nil {
			return err
		}
		target, err := host.createDetachedHost(v, DIPBounds{X: 80, Y: 90, Width: 800, Height: 600})
		if err != nil {
			return err
		}
		targetWindow = target.window
		targetBounds, err := controllerBoundsForWindow(target.window, ViewDetached)
		if err != nil {
			_ = host.destroyDetachedWindowHandle(v, target.window)
			return err
		}
		operations := &injectedShowFailure{moveController: moveController{controller: v.controller}, cause: rollbackCause}
		return runControllerMove(controllerMovePlan{
			controller:     operations,
			previousParent: before.parent,
			targetParent:   target.window,
			previousBounds: rectToMoveBounds(previousBounds),
			targetBounds:   rectToMoveBounds(targetBounds),
			revealTarget: func() error {
				procShowWindow.Call(target.window, swShowNormal)
				procUpdateWindow.Call(target.window)
				return nil
			},
			beforeShow:      func() error { return host.prepareMoveVisibilityOnSTA(v, ViewDetached, previousSelected) },
			restorePrevious: func() error { return showViewWindow(host.root) },
			focusTarget:     func() error { return focusWebViewWindow(target.window, v.controller) },
			focusPrevious:   func() error { return focusWebViewWindow(host.root, v.controller) },
			restorePeers: func() error {
				return host.restoreSelectionOnSTA(previousSelected, host.integratedViewsOnSTA(), trusted)
			},
			cleanupTarget:   func() error { return host.destroyDetachedWindowHandle(v, target.window) },
			closeController: func() error { return host.closeMoveResourcesOnSTA(v, 0, target.window) },
		})
	})
	if !errors.Is(err, rollbackCause) || errors.Is(err, ErrViewMoveRollbackFailed) {
		t.Fatalf("injected real-controller move error = %v, want recoverable show failure after rollback", err)
	}
	after, snapshotErr := snapshotMoveTestView(ctx, host, view)
	if snapshotErr != nil {
		t.Fatal(snapshotErr)
	}
	assertSameMoveTestController(t, before, after)
	if after.location != ViewIntegrated || after.parent != host.root || after.detachedWindow != 0 {
		t.Fatalf("real ParentWindow/location after rollback = %#v", after)
	}
	if after.controllerBounds != before.controllerBounds {
		t.Fatalf("real controller bounds after rollback = %#v, want %#v", after.controllerBounds, before.controllerBounds)
	}
	if after.visible != before.visible || after.selected != before.selected {
		t.Fatalf("integrated visibility/selection after rollback = visible:%t selected:%q, want visible:%t selected:%q", after.visible, after.selected, before.visible, before.selected)
	}
	if targetWindow == 0 || testWindowExists(targetWindow) {
		t.Fatalf("unused detached target HWND %#x was not retired after rollback", targetWindow)
	}
}

func TestWindowsRollbackFailureClosesRealControllerAndTargetOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	profileHome, err := os.MkdirTemp("", "matagi-move-82-rollback-failure-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profileHome) })
	page := testViewServer()
	t.Cleanup(page.Close)
	host, _, _ := startMoveTestHost(t, ctx, profileHome, ViewCallbacks{})
	view, events := createMoveTestView(t, ctx, host, page.URL, filepath.Join(profileHome, "service"), ViewCallbacks{})
	if err := host.Select(ctx, view); err != nil {
		t.Fatal(err)
	}
	before, err := snapshotMoveTestView(ctx, host, view)
	if err != nil {
		t.Fatal(err)
	}
	showFailure := errors.New("injected target show failure")
	parentFailure := errors.New("injected ParentWindow rollback failure")
	var targetWindow uintptr
	var viewEntry *nativeView
	var closeCount int
	err = host.invoke(ctx, false, func() error {
		host.mu.Lock()
		viewEntry = host.views[view]
		previousSelected := host.selected
		trusted := host.views[host.trusted]
		host.mu.Unlock()
		previousBounds, err := viewEntry.controller.GetBounds()
		if err != nil {
			return err
		}
		target, err := host.createDetachedHost(viewEntry, DIPBounds{X: 100, Y: 110, Width: 800, Height: 600})
		if err != nil {
			return err
		}
		targetWindow = target.window
		targetBounds, err := controllerBoundsForWindow(target.window, ViewDetached)
		if err != nil {
			_ = host.destroyDetachedWindowHandle(viewEntry, target.window)
			return err
		}
		operations := &injectedRollbackFailure{
			moveController: moveController{controller: viewEntry.controller},
			oldParent:      before.parent,
			showCause:      showFailure,
			parentCause:    parentFailure,
		}
		moveErr := runControllerMove(controllerMovePlan{
			controller:     operations,
			previousParent: before.parent,
			targetParent:   target.window,
			previousBounds: rectToMoveBounds(previousBounds),
			targetBounds:   rectToMoveBounds(targetBounds),
			revealTarget: func() error {
				procShowWindow.Call(target.window, swShowNormal)
				procUpdateWindow.Call(target.window)
				return nil
			},
			beforeShow:      func() error { return host.prepareMoveVisibilityOnSTA(viewEntry, ViewDetached, previousSelected) },
			restorePrevious: func() error { return showViewWindow(host.root) },
			focusTarget:     func() error { return focusWebViewWindow(target.window, viewEntry.controller) },
			focusPrevious:   func() error { return focusWebViewWindow(host.root, viewEntry.controller) },
			restorePeers: func() error {
				return host.restoreSelectionOnSTA(previousSelected, host.integratedViewsOnSTA(), trusted)
			},
			cleanupTarget: func() error { return host.destroyDetachedWindowHandle(viewEntry, target.window) },
			closeController: func() error {
				closeCount++
				return host.closeMoveResourcesOnSTA(viewEntry, 0, target.window)
			},
		})
		if errors.Is(moveErr, ErrViewMoveRollbackFailed) {
			host.markMoveFailedOnSTA(viewEntry, moveErr)
		}
		return moveErr
	})
	if !errors.Is(err, ErrViewMoveRollbackFailed) || !errors.Is(err, showFailure) || !errors.Is(err, parentFailure) {
		t.Fatalf("real-controller rollback failure = %v, want explicit target and rollback failures", err)
	}
	if closeCount != 1 {
		t.Fatalf("real-controller cleanup count = %d, want exactly one", closeCount)
	}
	if viewEntry.controller != nil || !viewEntry.controllerClosed || viewEntry.webview != nil || viewEntry.guard != nil {
		t.Fatalf("controller resources remain after rollback failure: controller=%p closed=%t webview=%p guard=%p", viewEntry.controller, viewEntry.controllerClosed, viewEntry.webview, viewEntry.guard)
	}
	if targetWindow == 0 || testWindowExists(targetWindow) {
		t.Fatalf("target HWND %#x leaked after real-controller rollback failure", targetWindow)
	}
	select {
	case event := <-events:
		if event.kind != "failed" || !errors.Is(event.err, ErrViewMoveRollbackFailed) {
			t.Fatalf("rollback failure callback = %#v, want explicit retryable failure", event)
		}
	case <-ctx.Done():
		t.Fatalf("waiting for retryable failure callback: %v", ctx.Err())
	}
	if err := host.Close(ctx, view); err != nil {
		t.Fatalf("closing failed placeholder after rollback: %v", err)
	}
}

type injectedShowFailure struct {
	moveController
	cause  error
	failed bool
}

func (operations *injectedShowFailure) Show() error {
	if !operations.failed {
		operations.failed = true
		return operations.cause
	}
	return operations.moveController.Show()
}

type injectedRollbackFailure struct {
	moveController
	oldParent   uintptr
	showCause   error
	parentCause error
	showFailed  bool
}

func (operations *injectedRollbackFailure) SetParent(parent uintptr) error {
	if parent == operations.oldParent {
		return operations.parentCause
	}
	return operations.moveController.SetParent(parent)
}

func (operations *injectedRollbackFailure) Show() error {
	if !operations.showFailed {
		operations.showFailed = true
		return operations.showCause
	}
	return operations.moveController.Show()
}

func assertSameMoveTestController(t *testing.T, before, after moveWindowsSnapshot) {
	t.Helper()
	if after.controller != before.controller || after.webview != before.webview || after.guard != before.guard || after.profile != before.profile {
		t.Fatalf("controller/page/security/profile identity changed: before=%#v after=%#v", before, after)
	}
}

func testWindowExists(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	exists, _, _ := testIsWindow.Call(hwnd)
	return exists != 0
}

func TestWindowsDetachedWMCloseAndDestroyOnlyCloseTheirView(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	profileHome, err := os.MkdirTemp("", "matagi-move-82-close-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profileHome) })
	page := testViewServer()
	t.Cleanup(page.Close)
	host, _, _ := startMoveTestHost(t, ctx, profileHome, ViewCallbacks{})
	closeView, closeEvents := createMoveTestView(t, ctx, host, page.URL, filepath.Join(profileHome, "wm-close"), ViewCallbacks{})
	destroyView, destroyEvents := createMoveTestView(t, ctx, host, page.URL, filepath.Join(profileHome, "wm-destroy"), ViewCallbacks{})
	for _, handle := range []ViewHandle{closeView, destroyView} {
		if err := host.Move(ctx, handle, ViewDetached, DIPBounds{X: 70, Y: 80, Width: 800, Height: 600}); err != nil {
			t.Fatalf("detaching service view for close-message test: %v", err)
		}
	}
	closeSnapshot, err := snapshotMoveTestView(ctx, host, closeView)
	if err != nil {
		t.Fatal(err)
	}
	testSendMessageW.Call(closeSnapshot.detachedWindow, wmClose, 0, 0)
	waitNativeViewEvent(t, ctx, closeEvents, "closed")
	select {
	case <-host.done:
		t.Fatal("detached WM_CLOSE stopped the Matagi host")
	default:
	}
	if err := host.Focus(ctx, destroyView); err != nil {
		t.Fatalf("sibling controller unavailable after detached WM_CLOSE: %v", err)
	}
	destroySnapshot, err := snapshotMoveTestView(ctx, host, destroyView)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.invoke(ctx, false, func() error {
		if destroyed, _, callErr := procDestroyWindow.Call(destroySnapshot.detachedWindow); destroyed == 0 {
			return fmt.Errorf("destroying detached HWND for WM_DESTROY test: %w", win32CallError(callErr))
		}
		return nil
	}); err != nil {
		t.Fatalf("destroying detached host window: %v", err)
	}
	waitNativeViewEvent(t, ctx, destroyEvents, "closed")
	select {
	case <-host.done:
		t.Fatal("detached WM_DESTROY stopped the Matagi host")
	default:
	}
	if err := host.Focus(ctx, host.trusted); err != nil {
		t.Fatalf("trusted main controller unavailable after detached WM_DESTROY: %v", err)
	}
}

func TestWindowsShutdownDuringControllerMoveRollsBackThenClosesOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	profileHome, err := os.MkdirTemp("", "matagi-move-82-shutdown-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profileHome) })
	page := testViewServer()
	t.Cleanup(page.Close)
	host, _, _ := startMoveTestHost(t, ctx, profileHome, ViewCallbacks{})
	view, events := createMoveTestView(t, ctx, host, page.URL, filepath.Join(profileHome, "service"), ViewCallbacks{})
	if err := host.Select(ctx, view); err != nil {
		t.Fatal(err)
	}
	before, err := snapshotMoveTestView(ctx, host, view)
	if err != nil {
		t.Fatal(err)
	}
	var targetWindow uintptr
	var parentAfterRollback uintptr
	var viewEntry *nativeView
	err = host.invoke(ctx, false, func() error {
		host.mu.Lock()
		viewEntry = host.views[view]
		previousSelected := host.selected
		trusted := host.views[host.trusted]
		host.mu.Unlock()
		previousBounds, err := viewEntry.controller.GetBounds()
		if err != nil {
			return err
		}
		target, err := host.createDetachedHost(viewEntry, DIPBounds{X: 120, Y: 130, Width: 800, Height: 600})
		if err != nil {
			return err
		}
		targetWindow = target.window
		targetBounds, err := controllerBoundsForWindow(target.window, ViewDetached)
		if err != nil {
			_ = host.destroyDetachedWindowHandle(viewEntry, target.window)
			return err
		}
		operations := shutdownAfterReparent{
			moveController: moveController{controller: viewEntry.controller},
			targetParent:   target.window,
			shutdown:       host.requestCloseAll,
		}
		moveErr := runControllerMove(controllerMovePlan{
			controller:     operations,
			previousParent: before.parent,
			targetParent:   target.window,
			previousBounds: rectToMoveBounds(previousBounds),
			targetBounds:   rectToMoveBounds(targetBounds),
			revealTarget: func() error {
				procShowWindow.Call(target.window, swShowNormal)
				procUpdateWindow.Call(target.window)
				return nil
			},
			beforeShow:      func() error { return host.prepareMoveVisibilityOnSTA(viewEntry, ViewDetached, previousSelected) },
			restorePrevious: func() error { return showViewWindow(host.root) },
			focusTarget:     func() error { return focusWebViewWindow(target.window, viewEntry.controller) },
			focusPrevious:   func() error { return focusWebViewWindow(host.root, viewEntry.controller) },
			restorePeers: func() error {
				return host.restoreSelectionOnSTA(previousSelected, host.integratedViewsOnSTA(), trusted)
			},
			cancelled: func() error {
				host.mu.Lock()
				closing := host.closing || viewEntry.cancelled || viewEntry.closed
				host.mu.Unlock()
				if closing {
					return errors.New("WebView2 view move was cancelled")
				}
				return nil
			},
			cleanupTarget:   func() error { return host.destroyDetachedWindowHandle(viewEntry, target.window) },
			closeController: func() error { return host.closeMoveResourcesOnSTA(viewEntry, 0, target.window) },
		})
		parentAfterRollback, err = viewEntry.controller.GetParentWindow()
		if err != nil {
			return err
		}
		return moveErr
	})
	if err == nil || !strings.Contains(err.Error(), "move was cancelled") {
		t.Fatalf("move error during shutdown = %v, want cancellation after shutdown starts", err)
	}
	if parentAfterRollback != before.parent {
		t.Fatalf("controller ParentWindow after shutdown rollback = %#x, want %#x", parentAfterRollback, before.parent)
	}
	if targetWindow == 0 || testWindowExists(targetWindow) {
		t.Fatalf("cancelled move target HWND %#x was not retired", targetWindow)
	}
	select {
	case <-host.done:
	case <-ctx.Done():
		t.Fatalf("waiting for shutdown after controller move: %v", ctx.Err())
	}
	waitNativeViewEvent(t, ctx, events, "closed")
	if !viewEntry.controllerClosed || viewEntry.controller != nil || viewEntry.webview != nil || viewEntry.guard != nil {
		t.Fatalf("shutdown did not close controller resources exactly once: closed=%t controller=%p webview=%p guard=%p", viewEntry.controllerClosed, viewEntry.controller, viewEntry.webview, viewEntry.guard)
	}
	if testWindowExists(before.parent) {
		t.Fatalf("main HWND %#x remains after application shutdown", before.parent)
	}
}

type shutdownAfterReparent struct {
	moveController
	targetParent uintptr
	shutdown     func()
	requested    bool
}

func (operations shutdownAfterReparent) SetParent(parent uintptr) error {
	if err := operations.moveController.SetParent(parent); err != nil {
		return err
	}
	if parent == operations.targetParent && !operations.requested {
		operations.requested = true
		operations.shutdown()
	}
	return nil
}
