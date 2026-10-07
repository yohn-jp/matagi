package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/health"
	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/internal/ssh"
	"github.com/yohn-jp/matagi/internal/tunnel"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type trackingProcess struct {
	once    sync.Once
	stopped chan struct{}
}

func newTrackingProcess() *trackingProcess {
	return &trackingProcess{stopped: make(chan struct{})}
}

func (p *trackingProcess) Wait() error {
	<-p.stopped
	return nil
}

func (p *trackingProcess) Stop() error {
	p.once.Do(func() { close(p.stopped) })
	return nil
}

type trackingLauncher struct {
	mu        sync.Mutex
	specs     []tunnel.ForwardSpec
	processes []*trackingProcess
}

func (l *trackingLauncher) Start(_ context.Context, spec tunnel.ForwardSpec) (tunnel.Process, error) {
	process := newTrackingProcess()
	l.mu.Lock()
	l.specs = append(l.specs, spec)
	l.processes = append(l.processes, process)
	l.mu.Unlock()
	return process, nil
}

func runtimeWithTrackingTunnels(t *testing.T, timeoutMS int) (*Runtime, *fakeSSH, *trackingLauncher) {
	t.Helper()
	snapshot, err := registry.NewSnapshot(
		[]registry.Environment{{
			ID:      "env",
			SSHHost: "host",
			Jinushi: registry.JinushiDefinition{
				StateDir:               "/state",
				SupervisorStartCommand: []string{"bootstrap"},
			},
		}},
		[]registry.Service{{
			ID:            "svc",
			EnvironmentID: "env",
			DesiredState:  registry.DesiredRunning,
			Execution: registry.ExecutionIntent{
				Argv:     []string{"binary"},
				CWD:      "/work",
				Lifetime: registry.LifetimeDetached,
			},
			Health: registry.HealthDefinition{
				Type:       registry.HealthHTTP,
				EndpointID: "health",
				Path:       "/ready",
				TimeoutMS:  &timeoutMS,
			},
			Endpoints: []registry.Endpoint{
				{ID: "health", Label: "Health", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 1234},
				{ID: "ui", Label: "UI", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 1235},
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	sshClient := &fakeSSH{fn: func(args []string) (ssh.Result, error) {
		if len(args) == 1 && args[0] == "true" {
			return ssh.Result{ExitCode: 0}, nil
		}
		if len(args) < 2 {
			return ssh.Result{}, errors.New("unexpected SSH command")
		}
		switch args[1] {
		case "status":
			return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`), ExitCode: 0}, nil
		case "list":
			return ssh.Result{Stdout: []byte(`{"version":1,"runs":[],"nextCursor":""}`), ExitCode: 0}, nil
		default:
			return ssh.Result{}, errors.New("unexpected Jinushi command")
		}
	}}
	launcher := &trackingLauncher{}
	manager, err := tunnel.NewManager(launcher)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Compose(snapshot, sshClient, manager)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, sshClient, launcher
}

func TestPollPreservesConfiguredReadinessTimeoutAndKeepsUIEndpointLazy(t *testing.T) {
	runtime, _, _ := runtimeWithTrackingTunnels(t, 10_000)
	defer runtime.Close(context.Background())

	var remaining time.Duration
	runtime.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok {
			t.Fatal("readiness request has no deadline")
		}
		remaining = time.Until(deadline)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})

	if err := runtime.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if remaining <= 7*time.Second {
		t.Fatalf("readiness deadline remaining = %s, want configured timeout to remain above former 3s limit", remaining)
	}

	healthIdentity := tunnel.Identity{Environment: "env", Service: "svc", Endpoint: "health"}
	healthTunnel, err := runtime.tunnels.Get(healthIdentity)
	if err != nil || healthTunnel.State != tunnel.StateReady {
		t.Fatalf("health tunnel = %#v, err = %v", healthTunnel, err)
	}
	uiIdentity := tunnel.Identity{Environment: "env", Service: "svc", Endpoint: "ui"}
	if uiTunnel, err := runtime.tunnels.Get(uiIdentity); err == nil {
		t.Fatalf("non-health UI tunnel was eagerly created: %#v", uiTunnel)
	}
}

func TestUncorrelatedServiceReadinessFailureDoesNotProjectStarting(t *testing.T) {
	runtime, _, _ := runtimeWithTrackingTunnels(t, 2_000)
	defer runtime.Close(context.Background())
	runtime.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New("service is not ready")
	})

	if _, err := runtime.Ensure(context.Background(), "env", "svc", "health"); err != nil {
		t.Fatalf("ensure health endpoint: %v", err)
	}
	if err := runtime.Poll(context.Background()); err != nil {
		t.Fatalf("Poll(): %v", err)
	}

	service := runtime.Snapshot().Environments[0].Services[0]
	if service.Process != health.ProcessUnknown || service.Readiness != health.ReadinessNotReady || service.State != health.ServiceUnknown {
		t.Fatalf("service with no correlated Jinushi Run = %#v; want unknown process/readiness failure without STARTING", service)
	}
	if endpoint := service.Endpoints[0]; endpoint.TunnelState != tunnel.StateReady {
		t.Fatalf("health endpoint tunnel = %#v; want ready tunnel as separate evidence", endpoint)
	}
}

func TestStartPublishesCorrelatedRunAndReadinessImmediately(t *testing.T) {
	runtime, sshClient, _ := runtimeWithTrackingTunnels(t, 2_000)
	defer runtime.Close(context.Background())
	owner := runtime.services[key("env", "svc")].CorrelationOwner()
	sshClient.fn = func(args []string) (ssh.Result, error) {
		switch args[1] {
		case "status":
			return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
		case "list":
			return ssh.Result{Stdout: []byte(`{"version":1,"runs":[],"nextCursor":""}`)}, nil
		case "run":
			return ssh.Result{Stdout: []byte(`{"version":1,"run":{"runId":"managed-run","state":"starting","spec":{"correlation":{"owner":"` + owner + `"}}}}`)}, nil
		default:
			return ssh.Result{}, errors.New("unexpected Jinushi command")
		}
	}
	runtime.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New("service is not ready")
	})

	service, err := runtime.Start(context.Background(), "env", "svc")
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if service.Process != health.ProcessStarting || service.Readiness != health.ReadinessNotReady || service.State != health.ServiceStarting {
		t.Fatalf("Start() projection = %#v; want correlated launch plus not-ready evidence", service)
	}
	observed := runtime.Snapshot().Environments[0].Services[0]
	if observed.Process != health.ProcessStarting || observed.Readiness != health.ReadinessNotReady || observed.State != health.ServiceStarting {
		t.Fatalf("published state = %#v; want immediate correlated lifecycle/readiness evidence", observed)
	}
}

func TestConnectivityPreservesTransportAndProbeFailureSemantics(t *testing.T) {
	runtime, sshClient := fixture(t)
	defer runtime.Close(context.Background())

	sshClient.fn = func(args []string) (ssh.Result, error) {
		return ssh.Result{ExitCode: 0}, nil
	}
	state, err := runtime.connectivity(context.Background(), "env")
	if err != nil || state != health.ConnectivityConnected {
		t.Fatalf("connected probe = %q, %v", state, err)
	}

	sshClient.fn = func(args []string) (ssh.Result, error) {
		return ssh.Result{ExitCode: 255}, &ssh.Error{Kind: ssh.FailureTransport, ExitCode: 255, Err: errors.New("transport unavailable")}
	}
	state, err = runtime.connectivity(context.Background(), "env")
	if err != nil || state != health.ConnectivityUnreachable {
		t.Fatalf("transport failure = %q, %v", state, err)
	}

	for _, kind := range []ssh.FailureKind{ssh.FailureRemoteCommand, ssh.FailureProcess, ssh.FailureTimeout, ssh.FailureCanceled} {
		t.Run(string(kind), func(t *testing.T) {
			sshClient.fn = func(args []string) (ssh.Result, error) {
				return ssh.Result{ExitCode: -1}, &ssh.Error{Kind: kind, ExitCode: -1, Err: errors.New("probe failed")}
			}
			state, err := runtime.connectivity(context.Background(), "env")
			if err == nil || state != health.ConnectivityError {
				t.Fatalf("probe failure %s = %q, %v", kind, state, err)
			}
		})
	}
}

func TestCloseStopsOwnedTunnelProcesses(t *testing.T) {
	runtime, _, launcher := runtimeWithTrackingTunnels(t, 2_000)
	if _, err := runtime.Ensure(context.Background(), "env", "svc", "ui"); err != nil {
		t.Fatal(err)
	}

	launcher.mu.Lock()
	if len(launcher.processes) != 1 {
		launcher.mu.Unlock()
		t.Fatalf("owned processes = %d, want 1", len(launcher.processes))
	}
	process := launcher.processes[0]
	launcher.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.stopped:
	default:
		t.Fatal("runtime Close did not stop its owned tunnel process")
	}
}
