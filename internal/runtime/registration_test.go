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

func TestRegistrationPersistsAndRebindsWithoutSecondRuntime(t *testing.T) {
	store, err := config.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	empty, _ := registry.NewSnapshot(nil, nil)
	manager, _ := tunnel.NewManager(noLauncher{})
	client := &fakeSSH{fn: func([]string) (ssh.Result, error) { return ssh.Result{}, nil }}
	r, err := Compose(empty, client, manager)
	if err != nil {
		t.Fatal(err)
	}
	r.store = store
	registered, err := registry.NewSnapshot([]registry.Environment{{ID: "dev", SSHHost: "alias", Jinushi: registry.JinushiDefinition{SupervisorStartCommand: []string{"systemctl", "--user", "start", "jinushi"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(registered); err != nil {
		t.Fatal(err)
	}
	if r.Snapshot().Environments[0].ID != "dev" {
		t.Fatal("runtime did not rebind")
	}
	loaded, err := store.Load()
	if err != nil || loaded.Environments()[0].SSHHost != "alias" {
		t.Fatal(loaded, err)
	}
	if err := r.Register(registered); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	fixtureRuntime, _ := fixture(t)
	services := fixtureRuntime.registry.Services()
	services[0].EnvironmentID = "dev"
	complete, err := registry.NewSnapshot(registered.Environments(), services)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(complete); err != nil {
		t.Fatal("completing draft:", err)
	}
	if len(r.Snapshot().Environments[0].Services) != 1 {
		t.Fatal("services not rebound")
	}
	if err := r.Register(complete); err == nil {
		t.Fatal("existing services overwritten")
	}
	_ = fixtureRuntime.Close(context.Background())
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(registered); err == nil {
		t.Fatal("closed runtime accepted registration")
	}
	reloaded, err := loadSnapshot(store)
	if err != nil || len(reloaded.Environments()) != 1 || len(reloaded.Services()) != 1 {
		t.Fatal(reloaded, err)
	}
}

func TestExplicitJinushiBootstrapIsSeparateFromConnectivity(t *testing.T) {
	r, f := fixture(t)
	original := f.fn
	f.fn = func(args []string) (ssh.Result, error) {
		if len(args) == 1 && args[0] == "true" {
			return ssh.Result{}, nil
		}
		return original(args)
	}
	if err := r.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.Snapshot().Environments[0]; got.Connectivity != "connected" || got.Jinushi != "unavailable" {
		t.Fatal(got)
	}
	for _, args := range f.calls {
		if len(args) > 0 && args[0] == "bootstrap" {
			t.Fatal("observation bootstrapped")
		}
	}
	if err := r.EnsureJinushi(context.Background(), "env"); err == nil {
		t.Fatal("unready Jinushi reported ready")
	}
	bootstrapped := false
	for _, args := range f.calls {
		if len(args) > 0 && args[0] == "bootstrap" {
			bootstrapped = true
		}
	}
	if !bootstrapped {
		t.Fatal("explicit action did not attempt configured bootstrap")
	}
	_ = r.Close(context.Background())
}

func TestRegistrationRequiresStoreAndPreservesMissingFile(t *testing.T) {
	empty, _ := registry.NewSnapshot(nil, nil)
	manager, _ := tunnel.NewManager(noLauncher{})
	r, _ := Compose(empty, &fakeSSH{fn: func([]string) (ssh.Result, error) { return ssh.Result{}, nil }}, manager)
	if err := r.Register(empty); err == nil {
		t.Fatal("storeless runtime accepted registration")
	}
	store, _ := config.NewStore(t.TempDir())
	r.store = store
	if err := r.Register(nil); err == nil {
		t.Fatal("nil snapshot accepted")
	}
	if _, err := os.Stat(store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	_ = r.Close(context.Background())
}
