package lifecycle

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

func startRemoteHTTP(t *testing.T) (int, func()) {
	return startRemoteHTTPWithBody(t, "fixture-ui")
}

func startRemoteHTTPWithBody(t *testing.T, uiBody string) (int, func()) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	var once sync.Once
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-ready:
		default:
			// A failed HTTP exchange is not-ready in the production runtime.
			// A completed 503 response is unhealthy and would exercise a
			// different projection than the absent-Run regression requires.
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("disconnect readiness fixture: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		switch r.URL.Path {
		case "/healthz":
			_, _ = io.WriteString(w, "ready")
		default:
			_, _ = io.WriteString(w, uiBody)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().(*net.TCPAddr).Port, func() { once.Do(func() { close(ready) }) }
}

func TestRemoteReadinessFixtureRefusesHTTPUntilReady(t *testing.T) {
	port, markReady := startRemoteHTTP(t)
	client := &http.Client{Timeout: time.Second}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	if response, err := client.Get(endpoint); err == nil {
		response.Body.Close()
		t.Fatalf("pending fixture completed HTTP with status %d; production would classify it as ready or unhealthy, not not-ready", response.StatusCode)
	}
	markReady()
	response, err := client.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "ready" {
		t.Fatalf("ready fixture: status=%d body=%q err=%v", response.StatusCode, body, err)
	}
}
