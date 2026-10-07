package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"syscall"
	"time"

	"github.com/yohn-jp/matagi/internal/api"
	"github.com/yohn-jp/matagi/internal/config"
	"github.com/yohn-jp/matagi/internal/desktop"
	"github.com/yohn-jp/matagi/internal/i18n"
	"github.com/yohn-jp/matagi/internal/runtime"
	"github.com/yohn-jp/matagi/internal/settings"
	"github.com/yohn-jp/matagi/internal/ui"
)

type lifecycle interface {
	api.Runtime
	Run(context.Context) error
	Close(context.Context) error
}

func main() {
	if goruntime.GOOS == "windows" {
		desktop.HideOwnedConsole()
	}
	if err := runProduct(); err != nil {
		fmt.Fprintln(os.Stderr, "matagi:", err)
		if goruntime.GOOS == "windows" && !(len(os.Args) > 1 && os.Args[1] == "apply-update") {
			locale := i18n.Resolve("", i18n.HostLocales()...)
			if root, stateErr := config.UserStateRoot(); stateErr == nil {
				if prefs, settingsErr := settings.NewStore(root); settingsErr == nil {
					if saved, localeErr := prefs.Locale(); localeErr == nil {
						locale = i18n.Resolve(string(saved), i18n.HostLocales()...)
					}
				}
			}
			desktop.Native().ReportError(locale.T("Matagi could not start"), err.Error())
		}
		os.Exit(1)
	}
}

func runProduct() error {
	// The verified replacement helper must run without acquiring the desktop
	// instance: it waits for that instance to exit before replacing its file.
	if len(os.Args) > 1 {
		if os.Args[1] == "apply-update" {
			return cmdApplyUpdate(os.Args[2:])
		}
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
	if goruntime.GOOS == "windows" {
		platform := desktop.Native()
		_, release, err := desktop.Preflight(platform)
		if errors.Is(err, desktop.ErrAlreadyRunning) {
			if err := platform.Activate(); err != nil {
				return fmt.Errorf("activating running Matagi: %w", err)
			}
			return nil
		}
		if err != nil {
			return err
		}
		defer release()
		return runConfiguredDesktop(platform)
	}

	store, err := config.NewUserStore()
	if err != nil {
		return err
	}
	rt, err := runtime.New(store)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runHeadless(ctx, rt)
}

func runConfiguredDesktop(platform desktop.Platform) error {
	store, err := config.NewUserStore()
	if err != nil {
		return err
	}
	rt, err := runtime.New(store)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runDesktop(ctx, rt, platform)
}

func runHeadless(parent context.Context, rt lifecycle) error {
	ctx, stop := context.WithCancel(parent)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	serverDone := make(chan error, 1)
	go func() { serverDone <- api.Serve(ctx, rt) }()
	var result error
	select {
	case result = <-done:
		stop()
		result = errors.Join(result, <-serverDone)
	case result = <-serverDone:
		stop()
		result = errors.Join(result, <-done)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(result, rt.Close(shutdown))
}

// runDesktop owns all four components. Listeners are bound before the window
// opens so the UI client always receives the actual API port, not a guessed one.
func runDesktop(parent context.Context, rt lifecycle, platform desktop.Platform) error {
	ctx, stop := context.WithCancel(parent)
	defer stop()
	apiListener, err := api.Listen()
	if err != nil {
		return closeRuntime(rt, fmt.Errorf("binding API: %w", err))
	}
	defer apiListener.Close()
	uiListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return closeRuntime(rt, fmt.Errorf("binding UI: %w", err))
	}
	defer uiListener.Close()
	client, err := ui.NewClient("http://"+apiListener.Addr().String(), 30*time.Second)
	if err != nil {
		return closeRuntime(rt, err)
	}
	uiURL := "http://" + uiListener.Addr().String() + "/"
	policy, err := desktop.NewPolicy(uiURL)
	if err != nil {
		return closeRuntime(rt, err)
	}
	stateRoot, err := config.UserStateRoot()
	if err != nil {
		return closeRuntime(rt, err)
	}
	prefs, err := settings.NewStore(stateRoot)
	if err != nil {
		return closeRuntime(rt, err)
	}
	updates := newDesktopUpdates(stateRoot, prefs, stop)
	apiServer := &http.Server{Handler: api.New(rt), ReadHeaderTimeout: 5 * time.Second}
	uiServer := &http.Server{Handler: ui.NewHandlerWithOptions(client, policy, ui.Options{Settings: prefs, Updates: updates}), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 15 * time.Second}
	results := make(chan error, 3)
	go func() { results <- rt.Run(ctx) }()
	go func() {
		err := apiServer.Serve(apiListener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		results <- err
	}()
	go func() {
		err := uiServer.Serve(uiListener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		results <- err
	}()
	windowDone := make(chan error, 1)
	go func() {
		cache, err := os.UserCacheDir()
		if err != nil {
			windowDone <- fmt.Errorf("locating WebView2 profile: %w", err)
			return
		}
		windowDone <- desktop.Open(ctx, platform, desktop.Window{Title: "Matagi", URL: uiURL, DataDir: filepath.Join(cache, "Matagi", "WebView2"), Policy: policy})
	}()
	var result error
	remainingServers, remainingWindow := 3, true
	select {
	case result = <-results:
		remainingServers--
	case result = <-windowDone:
		remainingWindow = false
	case <-parent.Done():
	case <-ctx.Done():
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Stop accepting requests before closing the runtime and its owned tunnels.
	result = errors.Join(result, uiServer.Shutdown(shutdown), apiServer.Shutdown(shutdown))
	for i := 0; i < remainingServers; i++ {
		select {
		case err := <-results:
			result = errors.Join(result, err)
		case <-shutdown.Done():
			result = errors.Join(result, shutdown.Err())
		}
	}
	if remainingWindow {
		select {
		case err := <-windowDone:
			result = errors.Join(result, err)
		case <-shutdown.Done():
			result = errors.Join(result, shutdown.Err())
		}
	}
	result = errors.Join(result, rt.Close(shutdown))
	return result
}

func closeRuntime(rt lifecycle, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(cause, rt.Close(ctx))
}
