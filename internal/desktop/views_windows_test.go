//go:build windows

package desktop

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type nativeViewEvent struct {
	kind string
	err  error
	at   time.Time
}

type nativeViewEvents chan nativeViewEvent

func callbacksFor(events nativeViewEvents) ViewCallbacks {
	return ViewCallbacks{
		Ready: func(ViewHandle) { events <- nativeViewEvent{kind: "ready", at: time.Now()} },
		Failed: func(_ ViewHandle, err error) {
			events <- nativeViewEvent{kind: "failed", err: err, at: time.Now()}
		},
		Closed: func(ViewHandle) { events <- nativeViewEvent{kind: "closed", at: time.Now()} },
	}
}

func waitNativeViewEvent(t *testing.T, ctx context.Context, events nativeViewEvents, kind string) nativeViewEvent {
	t.Helper()
	for {
		select {
		case event := <-events:
			if event.kind == "failed" {
				t.Fatalf("WebView2 controller failed: %v", event.err)
			}
			if event.kind == kind {
				return event
			}
		case <-ctx.Done():
			t.Fatalf("waiting for WebView2 %s event: %v", kind, ctx.Err())
		}
	}
}

func testViewServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, "<!doctype html><title>Matagi host test</title><main>ready</main>")
	}))
}

func TestWindowsViewsHostCreatesSelectsAndClosesRealControllers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	trustedServer := testViewServer()
	defer trustedServer.Close()
	serviceAServer := testViewServer()
	defer serviceAServer.Close()
	serviceBServer := testViewServer()
	defer serviceBServer.Close()
	profileHome, err := os.MkdirTemp("", "matagi-79-profile-*")
	if err != nil {
		t.Fatal(err)
	}
	// Keep the profile outside testing.TempDir because WebView2 may release
	// user-data handles asynchronously after the controllers have closed.
	t.Cleanup(func() { _ = os.RemoveAll(profileHome) })

	trustedEvents := make(nativeViewEvents, 8)
	trustedPolicy, err := NewTrustedViewPolicy(trustedServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, trusted, err := StartViewsHost(ctx, ViewConfig{
		URL:           trustedServer.URL,
		ProfileFolder: filepath.Join(profileHome, "trusted"),
		Policy:        trustedPolicy,
	}, callbacksFor(trustedEvents))
	if err != nil {
		t.Fatalf("starting actual WebView2 host: %v", err)
	}
	if trusted == "" {
		t.Fatal("WebView2 host returned an empty trusted view handle")
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := host.CloseAll(cleanupCtx); err != nil {
			t.Errorf("closing actual WebView2 host: %v", err)
		}
	})
	waitNativeViewEvent(t, ctx, trustedEvents, "ready")

	serviceAPolicy, err := NewServiceViewPolicy(serviceAServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	serviceAEvents := make(nativeViewEvents, 8)
	serviceA, err := host.Create(ctx, ViewConfig{
		URL:           serviceAServer.URL,
		ProfileFolder: filepath.Join(profileHome, "service-a"),
		Policy:        serviceAPolicy,
	}, callbacksFor(serviceAEvents))
	if err != nil {
		t.Fatalf("creating first actual service controller: %v", err)
	}
	waitNativeViewEvent(t, ctx, serviceAEvents, "ready")

	serviceBPolicy, err := NewServiceViewPolicy(serviceBServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	serviceBEvents := make(nativeViewEvents, 8)
	serviceB, err := host.Create(ctx, ViewConfig{
		URL:           serviceBServer.URL,
		ProfileFolder: filepath.Join(profileHome, "service-b"),
		Policy:        serviceBPolicy,
	}, callbacksFor(serviceBEvents))
	if err != nil {
		t.Fatalf("creating second actual service controller: %v", err)
	}
	waitNativeViewEvent(t, ctx, serviceBEvents, "ready")

	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"select first controller", func() error { return host.Select(ctx, serviceA) }},
		{"hide first controller", func() error { return host.Hide(ctx, serviceA) }},
		{"focus second controller", func() error { return host.Focus(ctx, serviceB) }},
	} {
		if err := step.run(); err != nil {
			t.Errorf("%s: %v", step.name, err)
		}
	}
	if err := host.Close(ctx, serviceA); err != nil {
		t.Fatalf("closing first actual service controller: %v", err)
	}
	waitNativeViewEvent(t, ctx, serviceAEvents, "closed")
	if err := host.Focus(ctx, serviceB); err != nil {
		t.Fatalf("focusing sibling after close: %v", err)
	}

	pendingPolicy, err := NewServiceViewPolicy(serviceAServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	pendingEvents := make(nativeViewEvents, 8)
	pending, err := host.Create(ctx, ViewConfig{
		URL:           serviceAServer.URL,
		ProfileFolder: filepath.Join(profileHome, "cancelled"),
		Policy:        pendingPolicy,
	}, callbacksFor(pendingEvents))
	if err != nil {
		t.Fatalf("starting controller for close-during-create: %v", err)
	}
	closeReturnedAt := time.Time{}
	if err := host.Close(ctx, pending); err != nil {
		t.Fatalf("closing controller during asynchronous creation: %v", err)
	}
	closeReturnedAt = time.Now()
	waitNativeViewEvent(t, ctx, pendingEvents, "closed")
	quiet := time.NewTimer(250 * time.Millisecond)
	defer quiet.Stop()
	for {
		select {
		case event := <-pendingEvents:
			if event.kind == "ready" && event.at.After(closeReturnedAt) {
				t.Fatal("cancelled controller published Ready after Close completed")
			}
			if event.kind == "failed" && event.at.After(closeReturnedAt) {
				t.Fatalf("cancelled controller published Failed after Close completed: %v", event.err)
			}
		case <-quiet.C:
			goto allClosed
		case <-ctx.Done():
			t.Fatalf("waiting for cancelled callback suppression: %v", ctx.Err())
		}
	}

allClosed:
	if err := host.CloseAll(ctx); err != nil {
		t.Fatalf("closing remaining actual WebView2 controllers: %v", err)
	}
	waitNativeViewEvent(t, ctx, trustedEvents, "closed")
	waitNativeViewEvent(t, ctx, serviceBEvents, "closed")
}
