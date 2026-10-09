package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/config"
	"github.com/yohn-jp/matagi/internal/health"
	"github.com/yohn-jp/matagi/internal/jinushi"
	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/internal/ssh"
	"github.com/yohn-jp/matagi/internal/tunnel"
)

type fakeSSH struct {
	mu    sync.Mutex
	calls [][]string
	fn    func([]string) (ssh.Result, error)
}

func (f *fakeSSH) Run(_ context.Context, _ string, args []string, _ time.Duration) (ssh.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.mu.Unlock()
	return f.fn(args)
}
func fixture(t *testing.T) (*Runtime, *fakeSSH) {
	t.Helper()
	snapshot, err := registry.NewSnapshot([]registry.Environment{{ID: "env", SSHHost: "host", Jinushi: registry.JinushiDefinition{StateDir: "/state", SupervisorStartCommand: []string{"bootstrap", "arg"}}}}, []registry.Service{{ID: "svc", EnvironmentID: "env", DesiredState: registry.DesiredRunning, Execution: registry.ExecutionIntent{Argv: []string{"binary", "arg"}, CWD: "/work", Lifetime: registry.LifetimeDetached}, Health: registry.HealthDefinition{Type: registry.HealthHTTP, EndpointID: "health", Path: "/ready"}, Endpoints: []registry.Endpoint{{ID: "health", Label: "Health", RemoteAddress: "127.0.0.1", RemotePort: 1234}, {ID: "ui", Label: "UI", RemoteAddress: "127.0.0.1", RemotePort: 1235}}}})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSSH{fn: func(args []string) (ssh.Result, error) {
		switch args[1] {
		case "status":
			return ssh.Result{Stdout: []byte(`{"version":1,"error":{"code":"supervisor-unavailable","message":"offline"}}`)}, nil
		case "list":
			return ssh.Result{Stdout: []byte(`{"version":1,"runs":[],"nextCursor":""}`)}, nil
		default:
			return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
		}
	}}
	manager, _ := tunnel.NewManager(noLauncher{})
	r, err := Compose(snapshot, f, manager)
	if err != nil {
		t.Fatal(err)
	}
	return r, f
}

type noLauncher struct{}

func (noLauncher) Start(context.Context, tunnel.ForwardSpec) (tunnel.Process, error) {
	return nil, errors.New("not launched")
}
func TestCompositionAndObservation(t *testing.T) {
	r, f := fixture(t)
	s := r.services[key("env", "svc")]
	if s.CorrelationOwner() == "" || r.bindings["env"].environment.Jinushi.StateDir != "/state" {
		t.Fatal("mapping lost")
	}
	_, err := r.bindings["env"].observe.Status(context.Background(), s.CorrelationOwner())
	if err == nil {
		t.Fatal("observation should report unavailable")
	}
	for _, args := range f.calls {
		if args[0] != "jinushi" || args[2] != "--state-dir=/state" || args[1] == "bootstrap" {
			t.Fatalf("observation bootstrapped: %v", args)
		}
	}
	if got := r.Snapshot(); got.Environments[0].Services[0].Endpoints[1].TunnelState != tunnel.StateStopped {
		t.Fatal(got)
	}
	_ = r.Close(context.Background())
}
func TestExecutorTranslation(t *testing.T) {
	f := &fakeSSH{fn: func([]string) (ssh.Result, error) {
		return ssh.Result{ExitCode: 7}, &ssh.Error{Kind: ssh.FailureRemoteCommand, ExitCode: 7}
	}}
	result, err := (executor{client: f, host: "host"}).Run(context.Background(), []string{"jinushi"}, time.Second)
	if err != nil || result.ExitCode != 7 {
		t.Fatal(result, err)
	}
	f.fn = func([]string) (ssh.Result, error) {
		return ssh.Result{ExitCode: -1}, &ssh.Error{Kind: ssh.FailureTimeout}
	}
	_, err = (executor{client: f, host: "host"}).Run(context.Background(), []string{"jinushi"}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatal(err)
	}
}

func TestClassifyLifecycleFailuresKeepsAuthorityBoundaries(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		want         string
		wantEvidence string
	}{
		{
			name: "SSH transport",
			err:  &jinushi.Failure{Kind: jinushi.KindTransport, Cause: &jinushi.ExecutionError{Kind: jinushi.ExecutionTransportFailure, Err: &ssh.Error{Kind: ssh.FailureTransport}}},
			want: "host-unreachable",
		},
		{
			name: "SSH command timeout",
			err:  &jinushi.Failure{Kind: jinushi.KindTimeout, Cause: &jinushi.ExecutionError{Kind: jinushi.ExecutionTimeout, Err: &ssh.Error{Kind: ssh.FailureTimeout}}},
			want: "ssh-timeout",
		},
		{
			name: "local SSH client failure",
			err:  &jinushi.Failure{Kind: jinushi.KindTransport, Cause: &jinushi.ExecutionError{Kind: jinushi.ExecutionTransportFailure, Err: &ssh.Error{Kind: ssh.FailureExecutableLookup}}},
			want: "ssh-client-failed",
		},
		{name: "Jinushi unavailable", err: &jinushi.Failure{Kind: jinushi.KindSupervisorUnavailable}, want: "jinushi-unavailable"},
		{name: "Jinushi command rejection", err: &jinushi.Failure{Kind: jinushi.KindCommand, Code: "invalid-run"}, want: "jinushi-command-failed", wantEvidence: "invalid-run"},
		{name: "token-like Jinushi code is not evidence", err: &jinushi.Failure{Kind: jinushi.KindCommand, Code: "bearer-secret-abc123"}, want: "jinushi-command-failed"},
		{name: "unsafe Jinushi code is not evidence", err: &jinushi.Failure{Kind: jinushi.KindCommand, Code: "path /srv/private"}, want: "jinushi-command-failed"},
		{name: "Jinushi protocol failure", err: &jinushi.Failure{Kind: jinushi.KindProtocol}, want: "jinushi-protocol-failed"},
		{name: "ambiguous lifecycle evidence", err: &jinushi.Failure{Kind: jinushi.KindAmbiguous}, want: "lifecycle-conflict"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var failure *Failure
			if err := classify(test.err); !errors.As(err, &failure) || failure.Code != test.want || failure.Evidence != test.wantEvidence {
				t.Fatalf("classify() = %v, want code %q", err, test.want)
			}
		})
	}
}

func TestRuntimeStateProjectsOnlyStableFailureDiagnostics(t *testing.T) {
	r, _ := fixture(t)
	defer r.Close(context.Background())

	s := r.services[key("env", "svc")]
	view := r.serviceView(s, health.ServiceObservation{
		ID:             string(s.ID),
		Process:        health.ProcessUnknown,
		ProcessError:   "host-unreachable",
		ReadinessError: "bearer-secret-abc123",
		Endpoints: []health.EndpointObservation{{
			ID:    "ui",
			State: health.EndpointError,
			Error: endpointEvidenceMissing,
		}},
	})
	if view.ProcessError != "host-unreachable" {
		t.Fatalf("process diagnostic = %q; want stable SSH classification", view.ProcessError)
	}
	if view.ReadinessError != "remote-failure" {
		t.Fatalf("untrusted readiness diagnostic = %q; want safe fallback", view.ReadinessError)
	}
	if view.Endpoints[1].Failure != "endpoint-evidence-missing" {
		t.Fatalf("endpoint diagnostic = %q; want stable endpoint classification", view.Endpoints[1].Failure)
	}
}

func TestPollPreservesUnreachableConnectivityAndSafeEnvironmentReason(t *testing.T) {
	r, f := fixture(t)
	defer r.Close(context.Background())
	const secret = "ssh transport detail bearer-secret-abc123"
	f.fn = func(args []string) (ssh.Result, error) {
		if len(args) == 1 && args[0] == "true" {
			return ssh.Result{}, &ssh.Error{Kind: ssh.FailureTransport, Err: errors.New(secret)}
		}
		return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
	}
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("Poll(): %v", err)
	}
	environment := r.Snapshot().Environments[0]
	if environment.Connectivity != health.ConnectivityUnreachable || environment.Error != "host-unreachable" || strings.Contains(environment.Error, secret) {
		t.Fatalf("transport failure state = %#v; want unreachable with safe host reason", environment)
	}
}

func TestPollClassifiesSSHRemoteCommandConnectivityFailure(t *testing.T) {
	r, f := fixture(t)
	defer r.Close(context.Background())
	f.fn = func(args []string) (ssh.Result, error) {
		if len(args) == 1 && args[0] == "true" {
			return ssh.Result{ExitCode: 1}, &ssh.Error{Kind: ssh.FailureRemoteCommand, Err: errors.New("remote stderr bearer-secret-abc123")}
		}
		return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
	}
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("Poll(): %v", err)
	}
	environment := r.Snapshot().Environments[0]
	if environment.Connectivity != health.ConnectivityError || environment.Error != "ssh-connectivity-failed" {
		t.Fatalf("remote command failure = %#v; want same error state with SSH connectivity classification", environment)
	}
}

func TestPollProjectsJinushiFailureWithoutChangingConnectedStateAndClearsOnRecovery(t *testing.T) {
	r, f := fixture(t)
	defer r.Close(context.Background())
	failed := true
	f.fn = func(args []string) (ssh.Result, error) {
		if len(args) == 1 && args[0] == "true" {
			return ssh.Result{ExitCode: 0}, nil
		}
		if len(args) > 1 && args[1] == "status" && failed {
			return ssh.Result{Stdout: []byte(`{"version":1,"error":{"code":"supervisor-unavailable","message":"private stderr bearer-secret-abc123"}}`)}, nil
		}
		if len(args) > 1 && args[1] == "list" {
			return ssh.Result{Stdout: []byte(`{"version":1,"runs":[],"nextCursor":""}`)}, nil
		}
		return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
	}
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("Poll() with Jinushi failure: %v", err)
	}
	environment := r.Snapshot().Environments[0]
	if environment.Connectivity != health.ConnectivityConnected || environment.Jinushi != "unavailable" || environment.Error != "jinushi-unavailable" {
		t.Fatalf("connected Jinushi failure state = %#v", environment)
	}
	failed = false
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("Poll() after Jinushi recovery: %v", err)
	}
	environment = r.Snapshot().Environments[0]
	if environment.Connectivity != health.ConnectivityConnected || environment.Jinushi != "ready" || environment.Error != "" {
		t.Fatalf("recovered state retained an error: %#v", environment)
	}
}

func TestStateDeterministic(t *testing.T) {
	r, _ := fixture(t)
	a, b := r.Snapshot(), r.Snapshot()
	if !reflect.DeepEqual(a, b) {
		t.Fatal(a, b)
	}
	if a.Environments[0].Services[0].DesiredState != registry.DesiredRunning {
		t.Fatal(a)
	}
	if _, err := r.Ensure(context.Background(), "env", "missing", "ui"); err == nil {
		t.Fatal("unknown service accepted")
	}
	_ = r.Close(context.Background())
}

func TestLoadSnapshotTreatsOnlyMissingRegistryAsEmpty(t *testing.T) {
	store, err := config.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := loadSnapshot(store)
	if err != nil {
		t.Fatalf("missing registry should be empty: %v", err)
	}
	if len(snapshot.Environments()) != 0 || len(snapshot.Services()) != 0 {
		t.Fatalf("missing registry was not empty: %#v", snapshot)
	}

	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSnapshot(store); err == nil {
		t.Fatal("malformed existing registry was silently treated as empty")
	}
}
