//go:build windows

package lifecycle

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/test/windows-e2e/harness"
)

func TestLifecycleAndTunnelOwnershipThroughProductionCandidate(t *testing.T) {
	fixtureDir := os.Getenv("MATAGI_E2E_SSH_FIXTURE_DIR")
	if fixtureDir == "" {
		t.Fatal("missing deterministic SSH fixture directory")
	}

	remotePort := startRemoteHTTP(t)
	snapshot, err := registry.NewSnapshot(
		[]registry.Environment{{
			ID:      "fixture-env",
			SSHHost: "fixture-host",
			Jinushi: registry.JinushiDefinition{SupervisorStartCommand: []string{"jinushi", "supervisor"}},
		}},
		[]registry.Service{{
			ID:            "fixture-service",
			EnvironmentID: "fixture-env",
			DesiredState:  registry.DesiredStopped,
			Execution: registry.ExecutionIntent{
				Argv:     []string{"fixture-service"},
				CWD:      "/fixture",
				Lifetime: registry.LifetimeDetached,
			},
			Health: registry.HealthDefinition{
				Type:       registry.HealthHTTP,
				EndpointID: "ui",
				Path:       "/healthz",
			},
			Endpoints: []registry.Endpoint{{
				ID:            "ui",
				Label:         "Fixture UI",
				RemoteAddress: registry.RemoteLoopbackAddress,
				RemotePort:    remotePort,
			}},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	fixtureRoot := t.TempDir()
	logPath := filepath.Join(fixtureRoot, "ssh-argv.jsonl")
	statePath := filepath.Join(fixtureRoot, "jinushi-state.json")
	apiURL, uiURL := harness.StartConfigured(t, harness.Options{
		Snapshot:   snapshot,
		PathPrefix: fixtureDir,
		Env: map[string]string{
			"MATAGI_E2E_SSH_LOG":   logPath,
			"MATAGI_E2E_SSH_STATE": statePath,
		},
	})

	postAction(t, uiURL, "start")
	localURL := openEndpoint(t, uiURL)
	if body := harness.Get(t, localURL+"/"); !strings.Contains(body, "fixture-ui") {
		t.Fatalf("forwarded endpoint returned unexpected body %q", body)
	}

	postAction(t, uiURL, "restart")
	if body := harness.Get(t, localURL+"/healthz"); !strings.Contains(body, "ready") {
		t.Fatalf("health endpoint through tunnel returned unexpected body %q", body)
	}

	stateBody := harness.Get(t, apiURL+"/v1/state")
	if !strings.Contains(stateBody, `"fixture-service"`) {
		t.Fatalf("registered service missing from API state: %s", stateBody)
	}

	postAction(t, uiURL, "stop")
	waitUnreachable(t, localURL)
	assertSSHBoundary(t, logPath)
}

func postAction(t *testing.T, uiURL, action string) {
	t.Helper()
	form := url.Values{
		"action":        {action},
		"environmentId": {"fixture-env"},
		"serviceId":     {"fixture-service"},
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.PostForm(uiURL+"/action", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.Fatalf("%s action: %s %s", action, resp.Status, body)
	}
}

func openEndpoint(t *testing.T, uiURL string) string {
	t.Helper()
	form := url.Values{
		"environmentId": {"fixture-env"},
		"serviceId":     {"fixture-service"},
		"endpointId":    {"ui"},
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.PostForm(uiURL+"/open", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.Fatalf("open endpoint: %s %s", resp.Status, body)
	}
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "http://127.0.0.1:") {
		t.Fatalf("non-loopback ensured endpoint %q", location)
	}
	return strings.TrimRight(location, "/")
}

func startRemoteHTTP(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_, _ = io.WriteString(w, "ready")
		default:
			_, _ = io.WriteString(w, "fixture-ui")
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().(*net.TCPAddr).Port
}

func waitUnreachable(t *testing.T, rawURL string) {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp4", parsed.Host, 200*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("Matagi-owned tunnel remained reachable after stop: %s", rawURL)
}

func assertSSHBoundary(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var runCount, cancelCount, awaitCount int
	var forwarding bool
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatalf("decode fixture argv %q: %v", line, err)
		}
		if len(args) >= 7 && args[0] == "-N" && args[1] == "-T" && args[2] == "-o" && args[3] == "ExitOnForwardFailure=yes" && args[4] == "-L" && strings.HasPrefix(args[5], "127.0.0.1:") && args[len(args)-1] == "fixture-host" {
			forwarding = true
		}
		if len(args) >= 3 && args[0] == "fixture-host" && args[1] == "jinushi" {
			switch args[2] {
			case "run":
				runCount++
			case "cancel":
				cancelCount++
			case "await":
				awaitCount++
			}
		}
	}
	if !forwarding {
		t.Fatal("production candidate never requested the required loopback OpenSSH forwarding argv")
	}
	if runCount < 2 || cancelCount < 2 || awaitCount < 2 {
		t.Fatalf("incomplete Jinushi lifecycle through SSH boundary: run=%d cancel=%d await=%d", runCount, cancelCount, awaitCount)
	}
}
