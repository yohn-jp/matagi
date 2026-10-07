package runtime

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/yohn-jp/matagi/internal/config"
	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/internal/ssh"
	"github.com/yohn-jp/matagi/internal/tunnel"
)

func onboardingRuntime(t *testing.T, f *fakeSSH) (*Runtime, *config.Store) {
	t.Helper()
	empty, _ := registry.NewSnapshot(nil, nil)
	manager, _ := tunnel.NewManager(noLauncher{})
	r, err := Compose(empty, f, manager)
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.store = store
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r, store
}
func TestConnectValidatesAndDoesNotPersistFailures(t *testing.T) {
	f := &fakeSSH{fn: func(args []string) (ssh.Result, error) { return ssh.Result{}, &ssh.Error{Kind: ssh.FailureTransport} }}
	r, store := onboardingRuntime(t, f)
	if err := r.Connect(context.Background(), "", "host", nil); err == nil {
		t.Fatal("invalid identity accepted")
	}
	if len(f.calls) != 0 {
		t.Fatal("validation performed network activity")
	}
	if err := r.Connect(context.Background(), "dev", "alias", nil); err == nil || err.Error() != "ssh-transport-failed" {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	f.fn = func(args []string) (ssh.Result, error) {
		if args[0] == "true" {
			return ssh.Result{}, nil
		}
		return ssh.Result{Stdout: []byte(`{"version":1,"error":{"code":"supervisor-unavailable","message":"offline"}}`)}, nil
	}
	if err := r.Connect(context.Background(), "dev", "alias", nil); err == nil || err.Error() != "jinushi-unavailable" {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
func TestConnectPersistsAndReloadsWithoutBootstrap(t *testing.T) {
	f := &fakeSSH{fn: func(args []string) (ssh.Result, error) {
		if args[0] == "true" {
			return ssh.Result{}, nil
		}
		return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
	}}
	r, store := onboardingRuntime(t, f)
	if err := r.Connect(context.Background(), "dev", "alias", nil); err != nil {
		t.Fatal(err)
	}
	if got := r.Snapshot().Environments[0]; got.ID != "dev" || got.SSHHost != "alias" || got.Jinushi != "ready" {
		t.Fatal(got)
	}
	if err := r.Connect(context.Background(), "dev", "alias", nil); err == nil {
		t.Fatal("duplicate accepted")
	}
	snap, err := store.Load()
	if err != nil || len(snap.Environments()) != 1 || len(snap.Environments()[0].Jinushi.SupervisorStartCommand) != 0 {
		t.Fatal(snap, err)
	}
	manager, _ := tunnel.NewManager(noLauncher{})
	reloaded, err := Compose(snap, f, manager)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close(context.Background())
	if reloaded.Snapshot().Environments[0].ID != "dev" {
		t.Fatal("relaunch lost environment")
	}
}
