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
