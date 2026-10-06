package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/desktop"
	"github.com/yohn-jp/matagi/internal/runtime"
)

type fakeRuntime struct {
	closed  atomic.Bool
	running atomic.Bool
	fail    error
}

func (r *fakeRuntime) Snapshot() runtime.State { return runtime.State{Version: 1} }
func (r *fakeRuntime) Start(context.Context, string, string) (runtime.Service, error) {
	return runtime.Service{}, nil
}
func (r *fakeRuntime) Stop(context.Context, string, string) (runtime.Service, error) {
	return runtime.Service{}, nil
}
func (r *fakeRuntime) Restart(context.Context, string, string) (runtime.Service, error) {
	return runtime.Service{}, nil
}
func (r *fakeRuntime) Ensure(context.Context, string, string, string) (runtime.Endpoint, error) {
	return runtime.Endpoint{}, nil
}
func (r *fakeRuntime) Run(ctx context.Context) error {
	r.running.Store(true)
	if r.fail != nil {
		return r.fail
	}
	<-ctx.Done()
	return nil
}
func (r *fakeRuntime) Close(context.Context) error { r.closed.Store(true); return nil }

type fakePlatform struct {
	open func(context.Context, desktop.Window) error
}

func (fakePlatform) RuntimeVersion() (string, error)                    { return "test", nil }
func (fakePlatform) AcquireInstance() (func(), error)                   { return func() {}, nil }
func (fakePlatform) Activate() error                                    { return nil }
func (p fakePlatform) Open(ctx context.Context, w desktop.Window) error { return p.open(ctx, w) }
func (fakePlatform) ReportError(string, string)                         {}

func TestProductionComposition(t *testing.T) {
	rt := &fakeRuntime{}
	p := fakePlatform{open: func(ctx context.Context, w desktop.Window) error {
		if !w.Policy.AllowNavigation(w.URL) || w.Policy.AllowNavigation("https://example.com") || w.Policy.AllowNavigation("http://127.0.0.1:9/") {
			t.Error("navigation policy")
		}
		c := &http.Client{Timeout: time.Second}
		deadline := time.Now().Add(2 * time.Second)
		for {
			resp, err := c.Get(w.URL)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Errorf("UI response: %d", resp.StatusCode)
				}
				break
			}
			if time.Now().After(deadline) {
				return err
			}
			time.Sleep(10 * time.Millisecond)
		}
		<-ctx.Done()
		return nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runDesktop(ctx, rt, p); err != nil {
		t.Fatal(err)
	}
	if !rt.closed.Load() || !rt.running.Load() {
		t.Fatal("runtime was not run and closed")
	}
}

func TestProductionFailureClosesRuntime(t *testing.T) {
	rt := &fakeRuntime{fail: errors.New("runtime failed")}
	p := fakePlatform{open: func(ctx context.Context, w desktop.Window) error { <-ctx.Done(); return nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runDesktop(ctx, rt, p); err == nil || !strings.Contains(err.Error(), "runtime failed") {
		t.Fatalf("failure: %v", err)
	}
	if !rt.closed.Load() {
		t.Fatal("runtime not closed")
	}
}
