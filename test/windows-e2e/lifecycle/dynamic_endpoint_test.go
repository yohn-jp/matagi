//go:build windows

package lifecycle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/test/windows-e2e/harness"
)

func TestDynamicEndpointOpensTheExactManagedRunsEphemeralPort(t *testing.T) {
	fixtureDir := harness.RequireFixture(t)
	managedPort, markManagedReady := startRemoteHTTPWithBody(t, "managed-instance")
	unrelatedPort, markUnrelatedReady := startRemoteHTTPWithBody(t, "unrelated-instance")
	markUnrelatedReady()
	endpointURL := fmt.Sprintf("http://127.0.0.1:%d/", managedPort)
	const descriptorPath = "/fixture/endpoint.json"
	snapshot, err := registry.NewSnapshot(
		[]registry.Environment{{
			ID:      "fixture-env",
			SSHHost: "fixture-host",
			Jinushi: registry.JinushiDefinition{SupervisorStartCommand: []string{"jinushi", "supervisor"}},
		}},
		[]registry.Service{{
			ID: "fixture-service", EnvironmentID: "fixture-env", DesiredState: registry.DesiredStopped,
			Execution: registry.ExecutionIntent{Argv: []string{"fixture-service"}, CWD: "/fixture", Lifetime: registry.LifetimeDetached},
			Health:    registry.HealthDefinition{Type: registry.HealthHTTP, EndpointID: "ui", Path: "/healthz"},
			Endpoints: []registry.Endpoint{{
				ID: "ui", Label: "Fixture UI", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 33031,
				Resolution: &registry.EndpointResolution{Type: registry.EndpointResolutionJSONURLFile, Path: descriptorPath},
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
		Snapshot: snapshot, PathPrefix: fixtureDir,
		Env: map[string]string{
			"MATAGI_E2E_SSH_LOG": logPath, "MATAGI_E2E_SSH_STATE": statePath,
			"MATAGI_E2E_ENDPOINT_URL": endpointURL, "MATAGI_E2E_ENDPOINT_PATH": descriptorPath,
		},
	})
	if projection := readServiceProjection(t, apiURL); projection.DesiredState != "stopped" {
		t.Fatalf("dynamic service desired state = %q; want stopped before explicit Start", projection.DesiredState)
	}

	postAction(t, uiURL, "start")
	assertOpenUnavailable(t, uiURL)
	markManagedReady()
	started := waitServiceProjection(t, apiURL, "running", "ready")
	if started.State != "ready" {
		t.Fatalf("dynamic service did not become ready against its managed endpoint: %#v", started)
	}
	localURL := openEndpoint(t, apiURL, uiURL)
	if body := harness.Get(t, localURL+"/"); body != "managed-instance" {
		t.Fatalf("Open reached %q; want exact managed Run endpoint (unrelated listener was %d)", body, unrelatedPort)
	}
	assertDynamicEndpointSSHBoundary(t, logPath, managedPort, descriptorPath)
}

func assertDynamicEndpointSSHBoundary(t *testing.T, path string, managedPort int, descriptorPath string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var forwardedPorts []string
	var runStarted bool
	var outputRead, descriptorRead bool
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatalf("decode fixture argv %q: %v", line, err)
		}
		if len(args) >= 7 && args[0] == "-N" && args[1] == "-T" && args[4] == "-L" {
			parts := strings.Split(args[5], ":")
			if len(parts) != 4 || parts[0] != "127.0.0.1" || parts[2] != "127.0.0.1" {
				t.Fatalf("forwarding target is not loopback-only: %v", args)
			}
			forwardedPorts = append(forwardedPorts, parts[3])
		}
		if len(args) < 3 || args[0] != "fixture-host" {
			continue
		}
		if args[1] == "jinushi" && args[2] == "run" {
			for _, arg := range args {
				if strings.HasPrefix(arg, "--submission-id=") {
					runStarted = true
				}
			}
		}
		if args[1] == "jinushi" && args[2] == "output" {
			outputRead = true
			if args[len(args)-1] != "fixture-run-1" {
				t.Fatalf("stdout was read for Run %q, want exact managed Run fixture-run-1", args[len(args)-1])
			}
		}
		if args[1] == "head" {
			descriptorRead = true
			if args[len(args)-1] != descriptorPath {
				t.Fatalf("descriptor path = %q, want %q", args[len(args)-1], descriptorPath)
			}
		}
	}
	if !runStarted || !outputRead || !descriptorRead || len(forwardedPorts) == 0 {
		t.Fatalf("missing managed endpoint evidence: Run=%t output=%t descriptor=%t forwards=%v", runStarted, outputRead, descriptorRead, forwardedPorts)
	}
	for _, port := range forwardedPorts {
		if port != fmt.Sprint(managedPort) || port == fmt.Sprint(33031) {
			t.Fatalf("forwarded remote port %q; want ephemeral managed port %d and never stale 33031", port, managedPort)
		}
	}
}
