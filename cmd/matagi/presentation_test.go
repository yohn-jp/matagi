package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/desktop"
	"github.com/yohn-jp/matagi/internal/presentation"
	"github.com/yohn-jp/matagi/internal/settings"
	"github.com/yohn-jp/matagi/internal/ui"
)

type createdPresentationView struct {
	handle    desktop.ViewHandle
	config    desktop.ViewConfig
	callbacks desktop.ViewCallbacks
}

type recordingViewsHost struct {
	mu        sync.Mutex
	next      int
	created   chan createdPresentationView
	callbacks map[desktop.ViewHandle]desktop.ViewCallbacks
	closed    []desktop.ViewHandle
	selected  []desktop.ViewHandle
	moves     []struct {
		handle   desktop.ViewHandle
		location desktop.ViewLocation
		bounds   desktop.DIPBounds
	}
}

func newRecordingViewsHost() *recordingViewsHost {
	return &recordingViewsHost{
		created:   make(chan createdPresentationView, 4),
		callbacks: make(map[desktop.ViewHandle]desktop.ViewCallbacks),
	}
}

func (h *recordingViewsHost) Create(_ context.Context, config desktop.ViewConfig, callbacks desktop.ViewCallbacks) (desktop.ViewHandle, error) {
	h.mu.Lock()
	h.next++
	handle := desktop.ViewHandle(fmt.Sprintf("view-%d", h.next))
	h.callbacks[handle] = callbacks
	h.mu.Unlock()
	h.created <- createdPresentationView{handle: handle, config: config, callbacks: callbacks}
	return handle, nil
}

func (h *recordingViewsHost) Select(_ context.Context, handle desktop.ViewHandle) error {
	h.mu.Lock()
	h.selected = append(h.selected, handle)
	h.mu.Unlock()
	return nil
}

func (*recordingViewsHost) Hide(context.Context, desktop.ViewHandle) error  { return nil }
func (*recordingViewsHost) Focus(context.Context, desktop.ViewHandle) error { return nil }

func (h *recordingViewsHost) Move(_ context.Context, handle desktop.ViewHandle, location desktop.ViewLocation, bounds desktop.DIPBounds) error {
	h.mu.Lock()
	h.moves = append(h.moves, struct {
		handle   desktop.ViewHandle
		location desktop.ViewLocation
		bounds   desktop.DIPBounds
	}{handle: handle, location: location, bounds: bounds})
	h.mu.Unlock()
	return nil
}

func (h *recordingViewsHost) Close(_ context.Context, handle desktop.ViewHandle) error {
	h.mu.Lock()
	h.closed = append(h.closed, handle)
	callbacks := h.callbacks[handle]
	h.mu.Unlock()
	if callbacks.Closed != nil {
		go callbacks.Closed(handle)
	}
	return nil
}

func (h *recordingViewsHost) CloseAll(ctx context.Context) error {
	h.mu.Lock()
	handles := make([]desktop.ViewHandle, 0, len(h.callbacks))
	for handle := range h.callbacks {
		handles = append(handles, handle)
	}
	h.mu.Unlock()
	for _, handle := range handles {
		if err := h.Close(ctx, handle); err != nil {
			return err
		}
	}
	return nil
}

func newPresentationTest(t *testing.T, stateHandler http.Handler) (*desktopPresentation, *recordingViewsHost) {
	t.Helper()
	server := httptest.NewServer(stateHandler)
	t.Cleanup(server.Close)
	client, err := ui.NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	prefs, err := settings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := newRecordingViewsHost()
	presenter, err := newDesktopPresentation(context.Background(), client, prefs, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	presenter.finishHostStart(host, "trusted")
	return presenter, host
}

func TestEndpointProfileFolderHashesTheCanonicalEndpointTuple(t *testing.T) {
	root := t.TempDir()
	key := presentation.EndpointKey{EnvironmentID: "env-a", ServiceID: "same-service", EndpointID: "web"}
	profile, err := endpointProfileFolder(root, key)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	want := filepath.Join(root, "Matagi", "WebView2-services", fmt.Sprintf("%x", digest))
	if profile != want {
		t.Fatalf("endpoint profile = %q, want %q", profile, want)
	}
	legacy := filepath.Join(root, "Matagi", "WebView2")
	if profile == legacy || strings.Contains(profile, key.ServiceID) {
		t.Fatalf("endpoint profile is not an isolated hashed sibling: %q", profile)
	}
	otherEnvironment := key
	otherEnvironment.EnvironmentID = "env-b"
	otherEndpoint := key
	otherEndpoint.EndpointID = "admin"
	for _, other := range []presentation.EndpointKey{otherEnvironment, otherEndpoint} {
		otherProfile, err := endpointProfileFolder(root, other)
		if err != nil {
			t.Fatal(err)
		}
		if otherProfile == profile {
			t.Fatalf("distinct endpoint identity reused profile %q", profile)
		}
	}
	if _, err := endpointProfileFolder(root, presentation.EndpointKey{EnvironmentID: "env", ServiceID: "svc"}); err == nil {
		t.Fatal("invalid endpoint identity received a profile")
	}
}

func TestDesktopPresentationPublishesOnlyAfterAsyncHostReady(t *testing.T) {
	key := presentation.EndpointKey{EnvironmentID: "dev", ServiceID: "dashboard", EndpointID: "web"}
	presenter, host := newPresentationTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"version":1,"environments":[{"id":%q,"services":[{"id":%q,"endpoints":[{"id":%q,"endpointState":"available","tunnelState":"ready","localUrl":"http://127.0.0.1:43123/"}]}]}]}`, key.EnvironmentID, key.ServiceID, key.EndpointID)
	}))
	reservation, err := presenter.ReserveOpen(key, presentation.LocationTab)
	if err != nil || !reservation.Ensure {
		t.Fatalf("ReserveOpen() = %#v, %v", reservation, err)
	}
	complete := make(chan error, 1)
	go func() { complete <- presenter.CompleteOpen(reservation, "http://127.0.0.1:43123/dashboard") }()
	created := <-host.created
	if created.config.URL != "http://127.0.0.1:43123/dashboard" || !created.config.Policy.AllowNavigation(created.config.URL) || created.config.Policy.AllowNavigation("http://127.0.0.1:43124/") {
		t.Fatalf("service controller config admitted the wrong origin: %#v", created.config)
	}
	wantProfile, err := endpointProfileFolder(presenter.cacheRoot, key)
	if err != nil || created.config.ProfileFolder != wantProfile {
		t.Fatalf("service UDF = %q, want %q (%v)", created.config.ProfileFolder, wantProfile, err)
	}
	if views := presenter.model.Snapshot().Views; len(views) != 1 || views[0].State != presentation.ViewPending {
		t.Fatalf("view published before host Ready: %#v", views)
	}
	created.callbacks.Ready(created.handle)
	if err := <-complete; err != nil {
		t.Fatalf("CompleteOpen() = %v", err)
	}
	if views := presenter.model.Snapshot().Views; len(views) != 1 || views[0].State != presentation.ViewCreated {
		t.Fatalf("view not published after host Ready: %#v", views)
	}
	if err := presenter.Select(reservation.View.ID); err != nil {
		t.Fatalf("Select() = %v", err)
	}
	host.mu.Lock()
	selected := append([]desktop.ViewHandle(nil), host.selected...)
	host.mu.Unlock()
	if len(selected) != 1 || selected[0] != created.handle {
		t.Fatalf("host selection calls = %v", selected)
	}
	if err := presenter.Move(reservation.View.ID, presentation.LocationWindow); err != nil {
		t.Fatalf("Move() = %v", err)
	}
	if views := presenter.model.Snapshot().Views; views[0].Location != presentation.LocationWindow {
		t.Fatalf("move was not committed: %#v", views[0])
	}
	if err := presenter.Close(reservation.View.ID); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if views := presenter.model.Snapshot().Views; len(views) != 0 {
		t.Fatalf("close retained logical view: %#v", views)
	}
}

func TestDesktopPresentationRejectsLateReadyAfterClose(t *testing.T) {
	presenter, host := newPresentationTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":1,"environments":[]}`))
	}))
	key := presentation.EndpointKey{EnvironmentID: "dev", ServiceID: "dashboard", EndpointID: "web"}
	reservation, err := presenter.ReserveOpen(key, presentation.LocationTab)
	if err != nil {
		t.Fatal(err)
	}
	complete := make(chan error, 1)
	go func() { complete <- presenter.CompleteOpen(reservation, "http://127.0.0.1:43123/") }()
	created := <-host.created
	if err := presenter.Close(reservation.View.ID); err != nil {
		t.Fatalf("Close() during create = %v", err)
	}
	created.callbacks.Ready(created.handle)
	select {
	case err := <-complete:
		if !errors.Is(err, errPresentationOperationStale) {
			t.Fatalf("late completion error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending CompleteOpen was not released by Close")
	}
	deadline := time.After(time.Second)
	for {
		host.mu.Lock()
		closed := len(host.closed)
		host.mu.Unlock()
		if closed > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("late controller was not closed")
		case <-time.After(time.Millisecond):
		}
	}
	if views := presenter.model.Snapshot().Views; len(views) != 0 {
		t.Fatalf("stale completion resurrected a closed view: %#v", views)
	}
}

func TestDesktopPresentationInvalidatesChangedEndpointOrigin(t *testing.T) {
	key := presentation.EndpointKey{EnvironmentID: "dev", ServiceID: "dashboard", EndpointID: "web"}
	state := `{"version":1,"environments":[{"id":"dev","services":[{"id":"dashboard","endpoints":[{"id":"web","endpointState":"available","tunnelState":"ready","localUrl":"http://127.0.0.1:43123/"}]}]}]}`
	var stateMu sync.Mutex
	presenter, host := newPresentationTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		stateMu.Lock()
		defer stateMu.Unlock()
		_, _ = w.Write([]byte(state))
	}))
	reservation, err := presenter.ReserveOpen(key, presentation.LocationTab)
	if err != nil {
		t.Fatal(err)
	}
	complete := make(chan error, 1)
	go func() { complete <- presenter.CompleteOpen(reservation, "http://127.0.0.1:43123/") }()
	created := <-host.created
	created.callbacks.Ready(created.handle)
	if err := <-complete; err != nil {
		t.Fatal(err)
	}
	stateMu.Lock()
	state = `{"version":1,"environments":[{"id":"dev","services":[{"id":"dashboard","endpoints":[{"id":"web","endpointState":"available","tunnelState":"ready","localUrl":"http://127.0.0.1:43124/"}]}]}]}`
	stateMu.Unlock()
	snapshot := presenter.Snapshot()
	if len(snapshot.Views) != 1 || snapshot.Views[0].State != presentation.ViewUnavailable {
		t.Fatalf("changed endpoint origin was not invalidated: %#v", snapshot)
	}
	host.mu.Lock()
	closed := append([]desktop.ViewHandle(nil), host.closed...)
	host.mu.Unlock()
	if len(closed) != 1 || closed[0] != created.handle {
		t.Fatalf("invalidated controller close calls = %v", closed)
	}
	resumed, err := presenter.ReserveOpen(key, presentation.LocationTab)
	if err != nil || !resumed.Ensure {
		t.Fatalf("changed origin did not require a fresh Ensure: %#v, %v", resumed, err)
	}
}
