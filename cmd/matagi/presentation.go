package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/yohn-jp/matagi/internal/desktop"
	"github.com/yohn-jp/matagi/internal/i18n"
	"github.com/yohn-jp/matagi/internal/presentation"
	"github.com/yohn-jp/matagi/internal/settings"
	"github.com/yohn-jp/matagi/internal/ui"
)

var errPresentationOperationStale = errors.New("presentation operation is no longer current")

type desktopPresentation struct {
	mu sync.RWMutex

	ctx          context.Context
	client       *ui.Client
	settings     *settings.Store
	store        *presentation.Store
	model        *presentation.Model
	cacheRoot    string
	host         desktop.ViewsHost
	trusted      desktop.ViewHandle
	trustedReady bool
	stopped      bool
	shutdownCtx  context.Context
	entries      map[presentation.ViewID]*presentationBinding

	runStarted bool
	startDone  chan struct{}
	rootEvents chan presentationHostEvent
	reconcile  sync.Mutex
}

type presentationBinding struct {
	id        presentation.ViewID
	key       presentation.EndpointKey
	operation presentation.Operation
	location  presentation.Location
	origin    string

	mu          sync.Mutex
	handle      desktop.ViewHandle
	ready       bool
	closing     bool
	failure     error
	completion  chan struct{}
	completeErr error
	complete    sync.Once
}

type presentationHostEvent struct {
	handle desktop.ViewHandle
	kind   uint8
	err    error
}

const (
	presentationHostReady uint8 = iota + 1
	presentationHostFailed
	presentationHostClosed
)

func newDesktopPresentation(ctx context.Context, client *ui.Client, prefs *settings.Store, stateRoot, cacheRoot string) (*desktopPresentation, error) {
	if ctx == nil {
		return nil, errors.New("desktop presentation context is required")
	}
	if client == nil {
		return nil, errors.New("desktop presentation HTTP client is required")
	}
	store, err := presentation.NewStore(stateRoot)
	if err != nil {
		return nil, err
	}
	if cacheRoot == "" {
		return nil, errors.New("WebView2 cache root is required")
	}
	return &desktopPresentation{
		ctx:        ctx,
		client:     client,
		settings:   prefs,
		store:      store,
		model:      presentation.NewModel(),
		cacheRoot:  cacheRoot,
		entries:    make(map[presentation.ViewID]*presentationBinding),
		rootEvents: make(chan presentationHostEvent, 8),
	}, nil
}

// Run starts the one native multiview host and waits for its main window to
// close. The host is started independently of ctx so shutdown can always obtain
// its handle and drain it through the application's existing bounded context.
func (p *desktopPresentation) Run(ctx context.Context, uiURL string) error {
	if ctx == nil {
		return errors.New("desktop presentation context is required")
	}
	p.mu.Lock()
	if p.runStarted {
		p.mu.Unlock()
		return errors.New("desktop presentation host was already started")
	}
	p.runStarted = true
	p.startDone = make(chan struct{})
	stopped := p.stopped
	p.mu.Unlock()
	if stopped {
		p.finishHostStart(nil, "")
		return nil
	}

	policy, err := desktop.NewTrustedViewPolicy(uiURL)
	if err != nil {
		p.finishHostStart(nil, "")
		return err
	}
	callbacks := desktop.ViewCallbacks{
		Ready: func(handle desktop.ViewHandle) {
			p.rootEvents <- presentationHostEvent{handle: handle, kind: presentationHostReady}
		},
		Failed: func(handle desktop.ViewHandle, err error) {
			p.rootEvents <- presentationHostEvent{handle: handle, kind: presentationHostFailed, err: err}
		},
		Closed: func(handle desktop.ViewHandle) {
			p.rootEvents <- presentationHostEvent{handle: handle, kind: presentationHostClosed}
		},
	}
	host, trusted, err := desktop.StartViewsHost(context.Background(), desktop.ViewConfig{
		URL:           uiURL,
		ProfileFolder: filepath.Join(p.cacheRoot, "Matagi", "WebView2"),
		Policy:        policy,
	}, callbacks)
	p.finishHostStart(host, trusted)
	if err != nil {
		return err
	}

	p.mu.RLock()
	stopped = p.stopped
	shutdownCtx := p.shutdownCtx
	p.mu.RUnlock()
	if stopped {
		if shutdownCtx != nil {
			return host.CloseAll(shutdownCtx)
		}
		return nil
	}
	for {
		select {
		case event := <-p.rootEvents:
			if event.handle != trusted {
				continue
			}
			switch event.kind {
			case presentationHostReady:
				p.mu.Lock()
				p.trustedReady = true
				p.mu.Unlock()
			case presentationHostFailed:
				return fmt.Errorf("trusted Matagi WebView2 controller failed: %w", event.err)
			case presentationHostClosed:
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (p *desktopPresentation) finishHostStart(host desktop.ViewsHost, trusted desktop.ViewHandle) {
	p.mu.Lock()
	p.host = host
	p.trusted = trusted
	if p.startDone != nil {
		close(p.startDone)
		p.startDone = nil
	}
	p.mu.Unlock()
}

func (p *desktopPresentation) ReserveOpen(key presentation.EndpointKey, location presentation.Location) (ui.OpenReservation, error) {
	p.reconcileEndpoints()
	p.mu.RLock()
	stopped, hostAvailable := p.stopped, p.host != nil
	p.mu.RUnlock()
	if stopped || p.ctx.Err() != nil {
		return ui.OpenReservation{}, errors.New("desktop presentation is closing")
	}
	if !hostAvailable {
		return ui.OpenReservation{}, errors.New("desktop presentation host is unavailable")
	}
	reserved, err := p.model.ReserveOpen(key, location)
	if err != nil {
		return ui.OpenReservation{}, err
	}
	if p.isStopped() || p.ctx.Err() != nil {
		p.model.Close(reserved.View.ID)
		return ui.OpenReservation{}, errors.New("desktop presentation is closing")
	}
	return ui.OpenReservation{View: reserved.View, Operation: reserved.Operation, Ensure: reserved.Ensure}, nil
}

func (p *desktopPresentation) CompleteOpen(reservation ui.OpenReservation, localURL string) error {
	if !reservation.Ensure || reservation.View.ID == 0 {
		return errors.New("desktop presentation open was not reserved for this caller")
	}
	if !p.model.IsCurrent(reservation.Operation) {
		return errPresentationOperationStale
	}
	if err := p.ctx.Err(); err != nil {
		return err
	}
	origin, err := ui.LoopbackHTTPOrigin(localURL)
	if err != nil {
		return errors.New("ensured endpoint URL is not an admitted loopback origin")
	}
	profile, err := endpointProfileFolder(p.cacheRoot, reservation.View.Key)
	if err != nil {
		return err
	}
	policy, err := desktop.NewServiceViewPolicy(localURL)
	if err != nil {
		return err
	}
	p.mu.RLock()
	host, stopped := p.host, p.stopped
	p.mu.RUnlock()
	if stopped || host == nil {
		return errors.New("desktop presentation host is unavailable")
	}
	entry := &presentationBinding{
		id:         reservation.View.ID,
		key:        reservation.View.Key,
		operation:  reservation.Operation,
		location:   reservation.View.Location,
		origin:     origin,
		completion: make(chan struct{}),
	}
	p.mu.Lock()
	if p.stopped || p.entries[entry.id] != nil {
		p.mu.Unlock()
		return errors.New("desktop presentation view is already being created")
	}
	p.entries[entry.id] = entry
	p.mu.Unlock()
	if !p.model.IsCurrent(reservation.Operation) {
		p.removeBinding(entry)
		return errPresentationOperationStale
	}

	locale := p.locale()
	callbacks := desktop.ViewCallbacks{
		Ready: func(handle desktop.ViewHandle) { p.viewReady(entry, handle) },
		Failed: func(handle desktop.ViewHandle, failure error) {
			p.viewFailed(entry, handle, failure)
		},
		Closed: func(handle desktop.ViewHandle) { p.viewClosed(entry, handle) },
		ReturnToTabs: func(handle desktop.ViewHandle) {
			go p.moveByHandle(entry, handle, presentation.LocationTab)
		},
		CloseRequested: func(handle desktop.ViewHandle) {
			go p.closeByHandle(entry, handle)
		},
	}
	handle, err := host.Create(p.ctx, desktop.ViewConfig{
		URL:               localURL,
		ProfileFolder:     profile,
		Policy:            policy,
		WindowTitle:       fmt.Sprintf("%s · %s · %s", entry.key.EnvironmentID, entry.key.ServiceID, entry.key.EndpointID),
		ReturnToTabsLabel: locale.T("Return to tabs"),
		CloseViewLabel:    locale.T("Close view"),
	}, callbacks)
	if err != nil {
		p.model.FailOpen(reservation.Operation)
		entry.signal(err)
		p.removeBinding(entry)
		return err
	}
	p.bindHandle(entry, handle)
	if !p.model.IsCurrent(reservation.Operation) || entry.isClosing() || p.isStopped() {
		entry.signal(errPresentationOperationStale)
		_ = p.closeController(entry, handle)
		return errPresentationOperationStale
	}
	select {
	case <-entry.completion:
		return entry.completionError()
	case <-p.ctx.Done():
		p.model.FailOpen(reservation.Operation)
		entry.signal(p.ctx.Err())
		go p.closeController(entry, handle)
		return p.ctx.Err()
	}
}

func (p *desktopPresentation) FailOpen(reservation ui.OpenReservation) {
	if !p.model.FailOpen(reservation.Operation) {
		return
	}
	entry := p.binding(reservation.View.ID)
	if entry == nil {
		return
	}
	entry.markClosing()
	entry.signal(errPresentationOperationStale)
	if handle := entry.viewHandle(); handle != "" {
		go p.closeController(entry, handle)
	}
}

func (p *desktopPresentation) Select(id presentation.ViewID) error {
	p.reconcileEndpoints()
	if p.isStopped() {
		return errors.New("desktop presentation is closing")
	}
	if !p.model.Select(id) {
		return errors.New("desktop presentation view was not found")
	}
	view, found := p.view(id)
	if !found || (view.State != presentation.ViewCreated && view.State != presentation.ViewMoving) {
		return nil
	}
	entry := p.binding(id)
	if entry == nil || entry.viewHandle() == "" {
		return errors.New("desktop presentation controller is unavailable")
	}
	host := p.currentHost()
	if host == nil {
		return errors.New("desktop presentation host is unavailable")
	}
	return host.Select(p.ctx, entry.viewHandle())
}

func (p *desktopPresentation) SelectTrusted(destination ui.TrustedDestination) error {
	switch destination {
	case ui.TrustedWorkspace, ui.TrustedUpdates, ui.TrustedSettings:
	default:
		return errors.New("trusted Matagi destination is invalid")
	}
	p.mu.RLock()
	host, trusted, ready, stopped := p.host, p.trusted, p.trustedReady, p.stopped
	p.mu.RUnlock()
	if stopped {
		return errors.New("desktop presentation is closing")
	}
	if host == nil || trusted == "" || !ready {
		return nil
	}
	return host.Select(p.ctx, trusted)
}

func (p *desktopPresentation) Move(id presentation.ViewID, target presentation.Location) error {
	p.reconcileEndpoints()
	view, found := p.view(id)
	if !found {
		return errors.New("desktop presentation view was not found")
	}
	var targetBounds *presentation.DIPBounds
	if target == presentation.LocationWindow {
		targetBounds = view.Bounds
	}
	operation, err := p.model.BeginMove(id, target, targetBounds)
	if err != nil {
		return err
	}
	entry := p.binding(id)
	if entry == nil || entry.viewHandle() == "" {
		p.model.FailMove(operation)
		return errors.New("desktop presentation controller is unavailable")
	}
	host := p.currentHost()
	if host == nil {
		p.model.FailMove(operation)
		return errors.New("desktop presentation host is unavailable")
	}
	location := desktop.ViewIntegrated
	bounds := desktop.DIPBounds{}
	if target == presentation.LocationWindow {
		location = desktop.ViewDetached
		bounds, err = desktopBounds(view.Bounds)
		if err != nil {
			p.model.FailMove(operation)
			return err
		}
	}
	if err := host.Move(p.ctx, entry.viewHandle(), location, bounds); err != nil {
		if errors.Is(err, desktop.ErrViewMoveRollbackFailed) {
			p.model.MarkUnavailable(id)
			entry.markClosing()
			_ = p.closeController(entry, entry.viewHandle())
		} else {
			p.model.FailMove(operation)
		}
		return err
	}
	if _, ok := p.model.CommitMove(operation); !ok {
		return errPresentationOperationStale
	}
	return nil
}

func (p *desktopPresentation) Close(id presentation.ViewID) error {
	if !p.model.Close(id) {
		return errors.New("desktop presentation view was not found")
	}
	entry := p.binding(id)
	if entry == nil {
		return nil
	}
	entry.markClosing()
	entry.signal(errPresentationOperationStale)
	handle := entry.viewHandle()
	if handle == "" {
		return nil
	}
	return p.closeController(entry, handle)
}

func (p *desktopPresentation) Snapshot() presentation.Snapshot {
	p.reconcileEndpoints()
	return p.model.Snapshot()
}

func (p *desktopPresentation) ResetSavedLayout() error {
	if p.isStopped() {
		return errors.New("desktop presentation is closing")
	}
	return p.store.ResetSavedLayout()
}

func (p *desktopPresentation) Stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	entries := make([]*presentationBinding, 0, len(p.entries))
	for _, entry := range p.entries {
		entries = append(entries, entry)
	}
	p.mu.Unlock()
	for _, entry := range entries {
		entry.markClosing()
		entry.signal(errors.New("desktop presentation stopped"))
	}
	for _, view := range p.model.Snapshot().Views {
		p.model.Close(view.ID)
	}
}

func (p *desktopPresentation) CloseAll(ctx context.Context) error {
	if ctx == nil {
		return errors.New("desktop presentation shutdown context is required")
	}
	p.mu.Lock()
	p.shutdownCtx = ctx
	p.mu.Unlock()
	p.Stop()

	p.mu.RLock()
	host := p.host
	startDone := p.startDone
	runStarted := p.runStarted
	p.mu.RUnlock()
	if host == nil && runStarted && startDone != nil {
		select {
		case <-startDone:
		case <-ctx.Done():
			return ctx.Err()
		}
		p.mu.RLock()
		host = p.host
		p.mu.RUnlock()
	}
	if host == nil {
		return nil
	}
	return host.CloseAll(ctx)
}

func (p *desktopPresentation) viewReady(entry *presentationBinding, handle desktop.ViewHandle) {
	p.bindHandle(entry, handle)
	if entry.isClosing() || p.isStopped() || !p.model.IsCurrent(entry.operation) {
		entry.signal(errPresentationOperationStale)
		go p.closeController(entry, handle)
		return
	}
	if err := p.verifyCurrentEndpoint(entry); err != nil {
		p.viewFailed(entry, handle, err)
		return
	}
	if entry.isClosing() || p.isStopped() || !p.model.IsCurrent(entry.operation) {
		entry.signal(errPresentationOperationStale)
		go p.closeController(entry, handle)
		return
	}
	if entry.location == presentation.LocationWindow {
		host := p.currentHost()
		if host == nil {
			p.viewFailed(entry, handle, errors.New("desktop presentation host is unavailable"))
			return
		}
		if err := host.Move(p.ctx, handle, desktop.ViewDetached, desktop.DIPBounds{}); err != nil {
			p.viewFailed(entry, handle, err)
			return
		}
	}
	if p.isStopped() {
		entry.signal(errPresentationOperationStale)
		go p.closeController(entry, handle)
		return
	}
	if _, ok := p.model.CompleteOpen(entry.operation); !ok {
		entry.signal(errPresentationOperationStale)
		go p.closeController(entry, handle)
		return
	}
	entry.mu.Lock()
	entry.ready = true
	entry.mu.Unlock()
	entry.signal(nil)
}

func (p *desktopPresentation) viewFailed(entry *presentationBinding, handle desktop.ViewHandle, failure error) {
	p.bindHandle(entry, handle)
	if failure == nil {
		failure = errors.New("WebView2 controller failed")
	}
	if !p.model.FailOpen(entry.operation) {
		p.model.MarkUnavailable(entry.id)
	}
	entry.mu.Lock()
	entry.failure = failure
	entry.mu.Unlock()
	entry.markClosing()
	go func() {
		closeErr := p.closeController(entry, handle)
		entry.signal(errors.Join(failure, closeErr))
	}()
}

func (p *desktopPresentation) viewClosed(entry *presentationBinding, handle desktop.ViewHandle) {
	p.bindHandle(entry, handle)
	if !entry.isClosing() && !p.isStopped() {
		if !p.model.FailOpen(entry.operation) {
			p.model.MarkUnavailable(entry.id)
		}
	}
	entry.mu.Lock()
	failure := entry.failure
	entry.ready = false
	entry.mu.Unlock()
	if failure == nil {
		failure = errors.New("WebView2 controller closed before the operation completed")
	}
	entry.signal(failure)
	p.removeBinding(entry)
}

func (p *desktopPresentation) moveByHandle(entry *presentationBinding, handle desktop.ViewHandle, target presentation.Location) {
	if entry.viewHandle() != handle || p.isStopped() {
		return
	}
	if err := p.Move(entry.id, target); err != nil {
		fmt.Fprintf(os.Stderr, "Matagi: moving WebView2 view failed: %v\n", err)
	}
}

func (p *desktopPresentation) closeByHandle(entry *presentationBinding, handle desktop.ViewHandle) {
	if entry.viewHandle() != handle || p.isStopped() {
		return
	}
	if err := p.Close(entry.id); err != nil {
		fmt.Fprintf(os.Stderr, "Matagi: closing WebView2 view failed: %v\n", err)
	}
}

func (p *desktopPresentation) reconcileEndpoints() {
	p.reconcile.Lock()
	defer p.reconcile.Unlock()
	if p.isStopped() {
		return
	}
	state, err := p.client.GetState(p.ctx)
	origins := make(map[presentation.EndpointKey]string)
	if err == nil {
		origins = p.endpointOrigins(state)
	}
	for _, view := range p.model.Snapshot().Views {
		if view.State != presentation.ViewCreated && view.State != presentation.ViewMoving {
			continue
		}
		entry := p.binding(view.ID)
		if entry == nil || !entry.isReady() {
			continue
		}
		if origin, ok := origins[view.Key]; ok && origin == entry.origin {
			continue
		}
		if !p.model.MarkUnavailable(view.ID) {
			continue
		}
		entry.markClosing()
		if handle := entry.viewHandle(); handle != "" {
			_ = p.closeController(entry, handle)
		}
	}
}

func (p *desktopPresentation) locale() i18n.Locale {
	if p.settings == nil {
		return i18n.Resolve("", i18n.HostLocales()...)
	}
	saved, err := p.settings.Locale()
	if err != nil {
		saved = ""
	}
	return i18n.Resolve(string(saved), i18n.HostLocales()...)
}

func (p *desktopPresentation) bindHandle(entry *presentationBinding, handle desktop.ViewHandle) {
	if handle == "" {
		return
	}
	entry.mu.Lock()
	if entry.handle == "" {
		entry.handle = handle
	}
	entry.mu.Unlock()
}

func (p *desktopPresentation) closeController(entry *presentationBinding, handle desktop.ViewHandle) error {
	if handle == "" {
		return nil
	}
	entry.markClosing()
	host := p.currentHost()
	if host == nil {
		p.removeBinding(entry)
		return errors.New("desktop presentation host is unavailable")
	}
	err := host.Close(p.ctx, handle)
	p.removeBinding(entry)
	return err
}

func (p *desktopPresentation) removeBinding(entry *presentationBinding) {
	p.mu.Lock()
	if p.entries[entry.id] == entry {
		delete(p.entries, entry.id)
	}
	p.mu.Unlock()
}

func (p *desktopPresentation) binding(id presentation.ViewID) *presentationBinding {
	p.mu.RLock()
	entry := p.entries[id]
	p.mu.RUnlock()
	return entry
}

func (p *desktopPresentation) currentHost() desktop.ViewsHost {
	p.mu.RLock()
	host := p.host
	p.mu.RUnlock()
	return host
}

func (p *desktopPresentation) isStopped() bool {
	p.mu.RLock()
	stopped := p.stopped
	p.mu.RUnlock()
	return stopped
}

func (p *desktopPresentation) view(id presentation.ViewID) (presentation.ViewSnapshot, bool) {
	for _, view := range p.model.Snapshot().Views {
		if view.ID == id {
			return view, true
		}
	}
	return presentation.ViewSnapshot{}, false
}

func (p *desktopPresentation) verifyCurrentEndpoint(entry *presentationBinding) error {
	state, err := p.client.GetState(p.ctx)
	if err != nil {
		return fmt.Errorf("confirming ensured endpoint state: %w", err)
	}
	origin, available := p.endpointOrigins(state)[entry.key]
	if !available || origin != entry.origin {
		return errors.New("ensured endpoint origin changed before the view became ready")
	}
	return nil
}

func (p *desktopPresentation) endpointOrigins(state ui.State) map[presentation.EndpointKey]string {
	origins := make(map[presentation.EndpointKey]string)
	for _, environment := range state.Environments {
		for _, service := range environment.Services {
			for _, endpoint := range service.Endpoints {
				if endpoint.EndpointState != "available" || endpoint.TunnelState != "ready" {
					continue
				}
				origin, err := ui.LoopbackHTTPOrigin(endpoint.LocalURL)
				if err == nil {
					origins[presentation.EndpointKey{EnvironmentID: environment.ID, ServiceID: service.ID, EndpointID: endpoint.ID}] = origin
				}
			}
		}
	}
	return origins
}

func (entry *presentationBinding) viewHandle() desktop.ViewHandle {
	entry.mu.Lock()
	handle := entry.handle
	entry.mu.Unlock()
	return handle
}

func (entry *presentationBinding) isClosing() bool {
	entry.mu.Lock()
	closing := entry.closing
	entry.mu.Unlock()
	return closing
}

func (entry *presentationBinding) isReady() bool {
	entry.mu.Lock()
	ready := entry.ready
	entry.mu.Unlock()
	return ready
}

func (entry *presentationBinding) markClosing() {
	entry.mu.Lock()
	entry.closing = true
	entry.mu.Unlock()
}

func (entry *presentationBinding) signal(err error) {
	entry.complete.Do(func() {
		entry.mu.Lock()
		entry.completeErr = err
		close(entry.completion)
		entry.mu.Unlock()
	})
}

func (entry *presentationBinding) completionError() error {
	entry.mu.Lock()
	err := entry.completeErr
	entry.mu.Unlock()
	return err
}

func endpointProfileFolder(cacheRoot string, key presentation.EndpointKey) (string, error) {
	if !key.Valid() {
		return "", errors.New("endpoint profile identity is invalid")
	}
	identity, err := json.Marshal(key)
	if err != nil {
		return "", fmt.Errorf("encoding endpoint profile identity: %w", err)
	}
	digest := sha256.Sum256(identity)
	return filepath.Join(cacheRoot, "Matagi", "WebView2-services", hex.EncodeToString(digest[:])), nil
}

func desktopBounds(bounds *presentation.DIPBounds) (desktop.DIPBounds, error) {
	if bounds == nil {
		return desktop.DIPBounds{}, nil
	}
	values := []float64{bounds.X, bounds.Y, bounds.Width, bounds.Height}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < math.MinInt32 || value > math.MaxInt32 {
			return desktop.DIPBounds{}, errors.New("detached-window bounds are outside the supported range")
		}
	}
	return desktop.DIPBounds{
		X:      int32(math.Round(bounds.X)),
		Y:      int32(math.Round(bounds.Y)),
		Width:  int32(math.Round(bounds.Width)),
		Height: int32(math.Round(bounds.Height)),
	}, nil
}
