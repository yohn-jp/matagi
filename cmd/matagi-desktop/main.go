package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/yohn-jp/matagi/internal/desktop"
	"github.com/yohn-jp/matagi/internal/ui"
)

func main() {
	apiURL := flag.String("api-url", os.Getenv("MATAGI_API_URL"), "loopback URL of the Matagi HTTP v1 API")
	apiTimeout := flag.Duration("api-timeout", 5*time.Second, "maximum duration of each API request")
	flag.Parse()

	platform := desktop.Native()
	if err := run(*apiURL, *apiTimeout, platform); err != nil {
		fmt.Fprintln(os.Stderr, "matagi-desktop:", err)
		platform.ReportError("Matagi could not start", err.Error())
		os.Exit(1)
	}
}

func run(apiURL string, timeout time.Duration, platform desktop.Platform) error {
	if apiURL == "" {
		return errors.New("set -api-url or MATAGI_API_URL to the local Matagi HTTP v1 API")
	}
	client, err := ui.NewClient(apiURL, timeout)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("starting the local UI listener: %w", err)
	}

	uiURL := "http://" + listener.Addr().String() + "/"
	policy, err := desktop.NewPolicy(uiURL)
	if err != nil {
		_ = listener.Close()
		return err
	}
	server := &http.Server{
		Handler:           ui.NewHandler(client, policy),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       15 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	window := desktop.Window{
		Title:   "Matagi",
		URL:     uiURL,
		DataDir: webViewDataDir(),
		Policy:  policy,
	}
	openErr := make(chan error, 1)
	go func() {
		openErr <- desktop.Open(ctx, platform, window)
	}()

	var result error
	select {
	case result = <-openErr:
	case err := <-serveErr:
		stop()
		<-openErr
		result = fmt.Errorf("local UI server stopped: %w", err)
	case <-ctx.Done():
		result = <-openErr
	}
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && result == nil {
		result = fmt.Errorf("stopping the local UI server: %w", err)
	}
	return result
}

func webViewDataDir() string {
	cache, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(cache, "Matagi", "WebView2")
}
