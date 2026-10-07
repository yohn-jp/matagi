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
	"sync"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/test/windows-e2e/harness"
)

func TestLifecycleAndTunnelOwnershipThroughProductionCandidate(t *testing.T) {
	fixtureDir := harness.RequireFixture(t)

	remotePort, markReady := startRemoteHTTP(t)
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
	manualRun := map[string]any{
		"next": 1,
		"run": map[string]any{
			"runId":      "manual-unowned-run",
			"state":      "running",
			"generation": 1,
			"createdAt":  "2026-01-01T00:00:00Z",
			"spec":       map[string]any{"correlation": map[string]string{"owner": "external-manual-owner"}},
		},
	}
	data, err := json.Marshal(manualRun)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	apiURL, uiURL := harness.StartConfigured(t, harness.Options{
		Snapshot:   snapshot,
		PathPrefix: fixtureDir,
		Env: map[string]string{
			"MATAGI_E2E_SSH_LOG":   logPath,
			"MATAGI_E2E_SSH_STATE": statePath,
		},
	})

	initial := readServiceProjection(t, apiURL)
	assertNoLaunchInProgress(t, initial)
	if initial.Process != "unknown" {
		t.Fatalf("unowned Jinushi Run was adopted as process state %q", initial.Process)
	}
	if initial.ProcessError != "" {
		t.Fatalf("Jinushi process observation failed instead of classifying the unowned Run: %q", initial.ProcessError)
	}

	localURL := openEndpoint(t, uiURL)
	waiting := waitServiceProjection(t, apiURL, "unknown", "not-ready")
	assertNoLaunchInProgress(t, waiting)
	if waiting.ProcessError != "" {
		t.Fatalf("Jinushi process observation failed instead of classifying the unowned Run: %q", waiting.ProcessError)
	}
	if waiting.DesiredState != "stopped" {
		t.Fatalf("registered service desired state = %q, want stopped", waiting.DesiredState)
	}

	postAction(t, uiURL, "start")
	markReady()
	started := waitServiceProjection(t, apiURL, "running", "ready")
	if started.State != "ready" {
		t.Fatalf("explicit Start did not converge to ready: %#v", started)
	}
	if body := harness.Get(t, localURL+"/"); !strings.Contains(body, "fixture-ui") {
		t.Fatalf("forwarded endpoint returned unexpected body %q", body)
	}

	postAction(t, uiURL, "restart")
	if body := harness.Get(t, localURL+"/healthz"); !strings.Contains(body, "ready") {
		t.Fatalf("health endpoint through tunnel returned unexpected body %q", body)
	}
	restarted := waitServiceProjection(t, apiURL, "running", "ready")
	if restarted.State != "ready" {
		t.Fatalf("explicit Restart did not converge to ready: %#v", restarted)
	}

	stateBody := harness.Get(t, apiURL+"/v1/state")
	if !strings.Contains(stateBody, `"fixture-service"`) {
		t.Fatalf("registered service missing from API state: %s", stateBody)
	}

	postAction(t, uiURL, "stop")
	waitUnreachable(t, localURL)
	stopped := waitServiceProjection(t, apiURL, "stopped", "unknown")
	if stopped.State != "stopped" {
		t.Fatalf("explicit Stop did not converge to stopped: %#v", stopped)
	}
	assertSSHBoundary(t, logPath, snapshot.Services()[0].CorrelationOwner())
}

func postAction(t *testing.T, uiURL, action string) {
	t.Helper()
	form := url.Values{
		"token":         {formToken(t, uiURL)},
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
		"token":         {formToken(t, uiURL)},
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

func formToken(t *testing.T, uiURL string) string {
	t.Helper()
	body := harness.Get(t, uiURL+"/")
	const marker = `name="token" value="`
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatal("candidate page did not contain form token")
	}
	start += len(marker)
	end := strings.IndexByte(body[start:], '"')
	if end < 0 {
		t.Fatal("candidate page contained malformed form token")
	}
	token := body[start : start+end]
	if len(token) != 32 {
		t.Fatalf("unexpected form token length %d", len(token))
	}
	return token
}

func startRemoteHTTP(t *testing.T) (int, func()) {
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
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/healthz":
			_, _ = io.WriteString(w, "ready")
		default:
			_, _ = io.WriteString(w, "fixture-ui")
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().(*net.TCPAddr).Port, func() { once.Do(func() { close(ready) }) }
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

func assertSSHBoundary(t *testing.T, path, expectedOwner string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var runCount, cancelCount, awaitCount int
	var forwarding bool
	submissions := map[string]struct{}{}
	ownerFound := false
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
				for _, arg := range args {
					if strings.HasPrefix(arg, "--submission-id=") && len(strings.TrimPrefix(arg, "--submission-id=")) > 0 {
						submissions[strings.TrimPrefix(arg, "--submission-id=")] = struct{}{}
					}
					if arg == "--correlation=owner="+expectedOwner {
						ownerFound = true
					}
				}
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
	if !ownerFound {
		t.Fatalf("Jinushi Start omitted Matagi service correlation owner %q", expectedOwner)
	}
	if len(submissions) < 2 {
		t.Fatalf("Start and Restart did not use distinct non-empty Jinushi submission IDs: %v", submissions)
	}
}

type serviceProjection struct {
	DesiredState string `json:"desiredState"`
	State        string `json:"state"`
	Process      string `json:"process"`
	Readiness    string `json:"readiness"`
	ProcessError string `json:"processError"`
}

func readServiceProjection(t *testing.T, apiURL string) serviceProjection {
	t.Helper()
	var state struct {
		Environments []struct {
			Services []serviceProjection `json:"services"`
		} `json:"environments"`
	}
	if err := json.Unmarshal([]byte(harness.Get(t, apiURL+"/v1/state")), &state); err != nil {
		t.Fatal(err)
	}
	for _, environment := range state.Environments {
		if len(environment.Services) != 0 {
			return environment.Services[0]
		}
	}
	t.Fatal("candidate state omitted the registered service")
	return serviceProjection{}
}

func waitServiceProjection(t *testing.T, apiURL, process, readiness string) serviceProjection {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		projection := readServiceProjection(t, apiURL)
		if projection.Process == process && projection.Readiness == readiness {
			return projection
		}
		time.Sleep(100 * time.Millisecond)
	}
	projection := readServiceProjection(t, apiURL)
	t.Fatalf("candidate service projection did not converge: got %#v, want process=%q readiness=%q", projection, process, readiness)
	return serviceProjection{}
}

func assertNoLaunchInProgress(t *testing.T, projection serviceProjection) {
	t.Helper()
	if projection.State == "starting" {
		t.Fatalf("service without correlated lifecycle evidence rendered STARTING: %#v", projection)
	}
}
