package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/health"
	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/internal/ssh"
	"github.com/yohn-jp/matagi/internal/tunnel"
)

type dynamicEvidence struct {
	runID                 string
	runState              string
	stdout                []byte
	descriptor            []byte
	descriptorExit        int
	noRun                 bool
	otherRuns             int
	changeRunOnDescriptor bool
}

func newDynamicRuntime(t *testing.T, evidence *dynamicEvidence, launcher tunnel.Launcher, endpoints []registry.Endpoint, healthEndpoint string) (*Runtime, *fakeSSH) {
	return newDynamicRuntimeWithDesired(t, evidence, launcher, endpoints, healthEndpoint, registry.DesiredRunning)
}

func newDynamicRuntimeWithDesired(t *testing.T, evidence *dynamicEvidence, launcher tunnel.Launcher, endpoints []registry.Endpoint, healthEndpoint string, desired registry.DesiredState) (*Runtime, *fakeSSH) {
	t.Helper()
	if evidence.runID == "" {
		evidence.runID = "run-managed"
	}
	if evidence.runState == "" {
		evidence.runState = "running"
	}
	snapshot, err := registry.NewSnapshot([]registry.Environment{{
		ID: "env", SSHHost: "host", Jinushi: registry.JinushiDefinition{StateDir: "/state"},
	}}, []registry.Service{{
		ID: "svc", EnvironmentID: "env", DesiredState: desired,
		Execution: registry.ExecutionIntent{Argv: []string{"service"}, CWD: "/work", Lifetime: registry.LifetimeDetached},
		Health:    registry.HealthDefinition{Type: registry.HealthHTTP, EndpointID: registry.EndpointID(healthEndpoint), Path: "/ready"},
		Endpoints: endpoints,
	}})
	if err != nil {
		t.Fatal(err)
	}
	owner := snapshot.Services()[0].CorrelationOwner()
	fake := &fakeSSH{fn: func(args []string) (ssh.Result, error) {
		if len(args) == 1 && args[0] == "true" {
			return ssh.Result{ExitCode: 0}, nil
		}
		if len(args) == 0 {
			return ssh.Result{}, errors.New("empty remote command")
		}
		if args[0] == "head" {
			result := ssh.Result{Stdout: append([]byte(nil), evidence.descriptor...), ExitCode: evidence.descriptorExit}
			if evidence.changeRunOnDescriptor {
				evidence.changeRunOnDescriptor = false
				evidence.runID = "run-changed-during-resolution"
			}
			return result, nil
		}
		if len(args) < 2 || args[0] != "jinushi" {
			return ssh.Result{}, fmt.Errorf("unexpected remote command %v", args)
		}
		switch args[1] {
		case "status":
			return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`), ExitCode: 0}, nil
		case "list":
			runs := []any{}
			if !evidence.noRun {
				runs = append(runs, dynamicRun(evidence.runID, evidence.runState, owner))
				for i := 0; i < evidence.otherRuns; i++ {
					runs = append(runs, dynamicRun(fmt.Sprintf("run-other-%d", i), "running", owner))
				}
			}
			return jsonSSHResult(t, map[string]any{"version": 1, "runs": runs, "nextCursor": ""}), nil
		case "inspect":
			return jsonSSHResult(t, map[string]any{"version": 1, "run": dynamicRun(evidence.runID, evidence.runState, owner)}), nil
		case "output":
			if args[len(args)-1] != evidence.runID {
				return ssh.Result{}, fmt.Errorf("output requested for unexpected Run %q", args[len(args)-1])
			}
			return jsonSSHResult(t, map[string]any{"version": 1, "data": base64.StdEncoding.EncodeToString(evidence.stdout)}), nil
		default:
			return ssh.Result{}, fmt.Errorf("unexpected Jinushi command %q", args[1])
		}
	}}
	manager, err := tunnel.NewManager(launcher)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Compose(snapshot, fake, manager)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, fake
}

func dynamicRun(id, state, owner string) map[string]any {
	return map[string]any{
		"runId": id, "state": state, "generation": 1, "createdAt": time.Now().UTC().Format(time.RFC3339Nano),
		"spec": map[string]any{"correlation": map[string]string{"owner": owner}},
	}
}

func jsonSSHResult(t *testing.T, value any) ssh.Result {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return ssh.Result{Stdout: data, ExitCode: 0}
}

func dynamicEndpoint(path string, stalePort int) registry.Endpoint {
	return registry.Endpoint{
		ID: "ui", Label: "Dashboard", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: stalePort,
		Resolution: &registry.EndpointResolution{Type: registry.EndpointResolutionJSONURLFile, Path: path},
	}
}

func dynamicURL(port int) []byte {
	return []byte(fmt.Sprintf("dashboard: http://127.0.0.1:%d/\n", port))
}

func dynamicDescriptor(port int) []byte {
	return []byte(fmt.Sprintf(`{"url":"http://127.0.0.1:%d"}`, port))
}

func TestEnsureUsesCorrelatedDynamicEndpointForTunnelAndReadiness(t *testing.T) {
	evidence := &dynamicEvidence{stdout: dynamicURL(34299), descriptor: dynamicDescriptor(34299)}
	launcher := &trackingLauncher{}
	runtime, _ := newDynamicRuntime(t, evidence, launcher, []registry.Endpoint{dynamicEndpoint("/home/dev/.cache/service/endpoint.json", 33031)}, "ui")
	defer runtime.Close(context.Background())

	var requestMu sync.Mutex
	var requests []*http.Request
	runtime.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestMu.Lock()
		requests = append(requests, request.Clone(request.Context()))
		requestMu.Unlock()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
	})
	endpoint, err := runtime.Ensure(context.Background(), "env", "svc", "ui")
	if err != nil {
		t.Fatalf("Ensure(): %v", err)
	}
	if endpoint.EndpointState != health.EndpointAvailable || endpoint.TunnelState != tunnel.StateReady {
		t.Fatalf("ensured endpoint = %#v; want application and forwarding ready", endpoint)
	}
	launcher.mu.Lock()
	if len(launcher.specs) != 1 || launcher.specs[0].RemotePort != 34299 {
		launcher.mu.Unlock()
		t.Fatalf("forward specs = %#v; stale static port overrode live descriptor", launcher.specs)
	}
	launcher.mu.Unlock()
	ready, err := runtime.readiness(context.Background(), "env", "svc")
	if err != nil || ready != health.ReadinessReady {
		t.Fatalf("readiness() = %q, %v", ready, err)
	}
	local, err := url.Parse(endpoint.LocalURL)
	if err != nil {
		t.Fatal(err)
	}
	requestMu.Lock()
	defer requestMu.Unlock()
	if len(requests) != 2 || requests[0].URL.Path != "" || requests[1].URL.Path != "/ready" || requests[0].URL.Host != local.Host || requests[1].URL.Host != local.Host {
		t.Fatalf("application/readiness requests = %#v; want the same resolved forward", requests)
	}
}

func TestDynamicEndpointResolutionFailsClosedAndRetiresExistingTunnel(t *testing.T) {
	evidence := &dynamicEvidence{stdout: dynamicURL(34299), descriptor: dynamicDescriptor(34299)}
	launcher := &trackingLauncher{}
	runtime, _ := newDynamicRuntime(t, evidence, launcher, []registry.Endpoint{dynamicEndpoint("/run/endpoint.json", 33031)}, "ui")
	defer runtime.Close(context.Background())
	runtime.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
	})
	if _, err := runtime.Ensure(context.Background(), "env", "svc", "ui"); err != nil {
		t.Fatalf("initial Ensure(): %v", err)
	}

	tests := []struct {
		name     string
		mutate   func()
		contains string
	}{
		{name: "stale descriptor", mutate: func() { evidence.descriptor = dynamicDescriptor(45789) }, contains: "stale or conflicting"},
		{name: "missing descriptor", mutate: func() { evidence.descriptorExit = 1 }, contains: "evidence is missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.mutate()
			_, err := runtime.Ensure(context.Background(), "env", "svc", "ui")
			var failure *Failure
			if !errors.As(err, &failure) || failure.Code != "endpoint-unavailable" || !strings.Contains(failure.Evidence, test.contains) {
				t.Fatalf("Ensure() error = %#v; want bounded %q diagnostic", err, test.contains)
			}
			snapshot, getErr := runtime.tunnels.Get(tunnel.Identity{Environment: "env", Service: "svc", Endpoint: "ui"})
			if getErr != nil || snapshot.State != tunnel.StateStopped {
				t.Fatalf("rejected endpoint tunnel = %#v, %v; stale forward must be stopped", snapshot, getErr)
			}
		})
	}
}

func TestDynamicEndpointMissingRunAndAmbiguousRunsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name     string
		evidence *dynamicEvidence
		want     string
	}{
		{name: "missing Run", evidence: &dynamicEvidence{noRun: true, stdout: dynamicURL(34299), descriptor: dynamicDescriptor(34299)}, want: "evidence is missing"},
		{name: "multiple live Runs", evidence: &dynamicEvidence{stdout: dynamicURL(34299), descriptor: dynamicDescriptor(34299), otherRuns: 1}, want: "evidence is ambiguous"},
		{name: "multiple endpoint URLs", evidence: &dynamicEvidence{stdout: []byte("http://127.0.0.1:34299/ http://127.0.0.1:45789/"), descriptor: dynamicDescriptor(34299)}, want: "evidence is ambiguous"},
		{name: "missing URL output", evidence: &dynamicEvidence{stdout: []byte("service started\n"), descriptor: dynamicDescriptor(34299)}, want: "evidence is missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher := &trackingLauncher{}
			runtime, _ := newDynamicRuntime(t, test.evidence, launcher, []registry.Endpoint{dynamicEndpoint("/run/endpoint.json", 33031)}, "ui")
			defer runtime.Close(context.Background())
			_, err := runtime.Ensure(context.Background(), "env", "svc", "ui")
			var failure *Failure
			if !errors.As(err, &failure) || !strings.Contains(failure.Evidence, test.want) {
				t.Fatalf("Ensure() error = %#v; want %q", err, test.want)
			}
			launcher.mu.Lock()
			defer launcher.mu.Unlock()
			if len(launcher.specs) != 0 {
				t.Fatalf("unresolved endpoint started a tunnel: %#v", launcher.specs)
			}
		})
	}
}

func TestDynamicEndpointRechecksTheSameManagedRunAfterReadingEvidence(t *testing.T) {
	evidence := &dynamicEvidence{stdout: dynamicURL(34299), descriptor: dynamicDescriptor(34299), changeRunOnDescriptor: true}
	launcher := &trackingLauncher{}
	runtime, _ := newDynamicRuntime(t, evidence, launcher, []registry.Endpoint{dynamicEndpoint("/run/endpoint.json", 33031)}, "ui")
	defer runtime.Close(context.Background())
	_, err := runtime.Ensure(context.Background(), "env", "svc", "ui")
	var failure *Failure
	if !errors.As(err, &failure) || !strings.Contains(failure.Evidence, "stale or conflicting") {
		t.Fatalf("Ensure() after Run identity changed = %#v; want stale evidence failure", err)
	}
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	if len(launcher.specs) != 0 {
		t.Fatalf("changed Run identity created a tunnel: %#v", launcher.specs)
	}
}

func TestDynamicTunnelFailureAndApplicationReadinessRemainSeparate(t *testing.T) {
	evidence := &dynamicEvidence{stdout: dynamicURL(34299), descriptor: dynamicDescriptor(34299)}
	runtime, _ := newDynamicRuntime(t, evidence, noLauncher{}, []registry.Endpoint{dynamicEndpoint("/run/endpoint.json", 33031)}, "ui")
	defer runtime.Close(context.Background())
	_, err := runtime.Ensure(context.Background(), "env", "svc", "ui")
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "endpoint-unavailable" || !strings.Contains(failure.Evidence, "SSH tunnel") {
		t.Fatalf("tunnel failure = %#v; want bounded transport diagnostic", err)
	}
	if state, err := runtime.readiness(context.Background(), "env", "svc"); err != nil || state != health.ReadinessUnknown {
		t.Fatalf("readiness after tunnel failure = %q, %v; forwarding failure is not an application probe result", state, err)
	}

	launcher := &trackingLauncher{}
	runtime, _ = newDynamicRuntime(t, evidence, launcher, []registry.Endpoint{dynamicEndpoint("/run/endpoint.json", 33031)}, "ui")
	defer runtime.Close(context.Background())
	runtime.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/ready" {
			return nil, errors.New("connection refused")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ui")), Request: request}, nil
	})
	endpoint, err := runtime.Ensure(context.Background(), "env", "svc", "ui")
	if err != nil || endpoint.EndpointState != health.EndpointAvailable || endpoint.TunnelState != tunnel.StateReady {
		t.Fatalf("application endpoint = %#v, %v", endpoint, err)
	}
	readiness, err := runtime.readiness(context.Background(), "env", "svc")
	if err != nil || readiness != health.ReadinessNotReady {
		t.Fatalf("unavailable readiness probe = %q, %v", readiness, err)
	}
}

func TestStaticForwardReadinessDoesNotClaimUnavailableApplication(t *testing.T) {
	launcher := &trackingLauncher{}
	endpoint := registry.Endpoint{ID: "ui", Label: "UI", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 33031}
	runtime, _ := newDynamicRuntime(t, &dynamicEvidence{}, launcher, []registry.Endpoint{endpoint}, "ui")
	defer runtime.Close(context.Background())
	runtime.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("remote application is not listening")
	})
	ensured, err := runtime.Ensure(context.Background(), "env", "svc", "ui")
	if err != nil {
		t.Fatalf("Ensure(): %v", err)
	}
	if ensured.TunnelState != tunnel.StateReady || ensured.EndpointState != health.EndpointUnavailable || ensured.Failure != "endpoint-application-unavailable" {
		t.Fatalf("forward-ready endpoint = %#v; application must remain unavailable", ensured)
	}
}

func TestPollProjectsUnhealthyReadinessWithoutChangingProcessOrEndpointAxes(t *testing.T) {
	launcher := &trackingLauncher{}
	endpoints := []registry.Endpoint{
		{ID: "health", Label: "Health", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 9090},
		{ID: "ui", Label: "UI", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 33031},
	}
	runtime, _ := newDynamicRuntime(t, &dynamicEvidence{}, launcher, endpoints, "health")
	defer runtime.Close(context.Background())
	runtime.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		status := http.StatusOK
		if request.URL.Path == "/ready" {
			status = http.StatusServiceUnavailable
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
	})
	if _, err := runtime.Ensure(context.Background(), "env", "svc", "ui"); err != nil {
		t.Fatalf("Ensure(ui): %v", err)
	}
	if err := runtime.Poll(context.Background()); err != nil {
		t.Fatalf("Poll(): %v", err)
	}
	service := runtime.Snapshot().Environments[0].Services[0]
	if service.Process != health.ProcessRunning || service.Readiness != health.ReadinessUnhealthy || service.ReadinessError != "readiness-check-failed" {
		t.Fatalf("process/readiness projection = %#v; want running process plus safe readiness failure", service)
	}
	if len(service.Endpoints) != 2 || service.Endpoints[1].EndpointState != health.EndpointAvailable || service.Endpoints[1].Failure != "" {
		t.Fatalf("readiness failure changed independent endpoint state: %#v", service.Endpoints)
	}
}

func TestStoppedProcessSuppressesDerivedReadinessErrorButKeepsExplicitError(t *testing.T) {
	runtime, _ := newDynamicRuntime(t, &dynamicEvidence{}, noLauncher{}, []registry.Endpoint{{ID: "health", Label: "Health", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 9090}}, "health")
	defer runtime.Close(context.Background())
	service := runtime.services[key("env", "svc")]
	view := runtime.serviceView(service, health.ServiceObservation{Process: health.ProcessStopped, Readiness: health.ReadinessNotReady})
	if view.ReadinessError != "" {
		t.Fatalf("stopped process acquired derived readiness error %q", view.ReadinessError)
	}
	view = runtime.serviceView(service, health.ServiceObservation{Process: health.ProcessStopped, Readiness: health.ReadinessError, ReadinessError: "jinushi-protocol-failed"})
	if view.ReadinessError != "jinushi-protocol-failed" {
		t.Fatalf("explicit readiness error was suppressed for stopped process: %q", view.ReadinessError)
	}
}

func TestPollRevalidatesOwnedDynamicNonHealthEndpoint(t *testing.T) {
	evidence := &dynamicEvidence{stdout: dynamicURL(34299), descriptor: dynamicDescriptor(34299)}
	launcher := &trackingLauncher{}
	endpoints := []registry.Endpoint{
		{ID: "health", Label: "Health", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 9090},
		dynamicEndpoint("/run/ui.json", 33031),
	}
	runtime, _ := newDynamicRuntimeWithDesired(t, evidence, launcher, endpoints, "health", registry.DesiredStopped)
	defer runtime.Close(context.Background())
	runtime.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
	})
	if _, err := runtime.Ensure(context.Background(), "env", "svc", "ui"); err != nil {
		t.Fatalf("initial UI Ensure(): %v", err)
	}
	evidence.runID = "run-restarted"
	evidence.stdout = dynamicURL(45789)
	evidence.descriptor = dynamicDescriptor(45789)
	if err := runtime.Poll(context.Background()); err != nil {
		t.Fatalf("Poll() after managed Run changed endpoint: %v", err)
	}
	launcher.mu.Lock()
	if len(launcher.specs) != 2 || launcher.specs[0].RemotePort != 34299 || launcher.specs[1].RemotePort != 45789 {
		launcher.mu.Unlock()
		t.Fatalf("forward specs after correlated Run change = %#v", launcher.specs)
	}
	launcher.mu.Unlock()

	evidence.noRun = true
	if err := runtime.Poll(context.Background()); err != nil {
		t.Fatalf("Poll() after Run evidence disappeared: %v", err)
	}
	var state State = runtime.Snapshot()
	service := state.Environments[0].Services[0]
	var ui Endpoint
	for _, endpoint := range service.Endpoints {
		if endpoint.ID == "ui" {
			ui = endpoint
		}
	}
	if ui.EndpointState != health.EndpointError || ui.TunnelState != tunnel.StateStopped || ui.Failure != "endpoint-evidence-missing" {
		t.Fatalf("dynamic endpoint projection after evidence loss = %#v", ui)
	}
}
