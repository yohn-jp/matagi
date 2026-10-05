package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yohn-jp/matagi/internal/registry"
)

func configFixture(reverse bool) *registry.Snapshot {
	environments := []registry.Environment{
		{ID: "dev-b", SSHHost: "dev-b"},
		{ID: "dev-a", SSHHost: "dev-a"},
	}
	services := []registry.Service{
		{
			ID:            "worker",
			EnvironmentID: "dev-b",
			DesiredState:  registry.DesiredStopped,
			Execution: registry.ExecutionIntent{
				Argv:     []string{"/usr/bin/worker", "--label", "two words"},
				Lifetime: registry.LifetimeDetached,
			},
			Health:    registry.HealthDefinition{Type: registry.HealthHTTP, EndpointID: "metrics", Path: "/ready"},
			Endpoints: []registry.Endpoint{{ID: "metrics", Label: "Metrics", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 9200}},
		},
		{
			ID:            "dashboard",
			EnvironmentID: "dev-a",
			DesiredState:  registry.DesiredRunning,
			Execution: registry.ExecutionIntent{
				Argv:     []string{"/opt/dashboard", "--flag"},
				CWD:      "/srv/dashboard",
				Lifetime: registry.LifetimeDetached,
			},
			Health:    registry.HealthDefinition{Type: registry.HealthHTTP, EndpointID: "web", Path: "/health"},
			Endpoints: []registry.Endpoint{{ID: "web", Label: "Dashboard", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 8080}},
		},
	}
	if reverse {
		for left, right := 0, len(environments)-1; left < right; left, right = left+1, right-1 {
			environments[left], environments[right] = environments[right], environments[left]
		}
		for left, right := 0, len(services)-1; left < right; left, right = left+1, right-1 {
			services[left], services[right] = services[right], services[left]
		}
	}
	snapshot, err := registry.NewSnapshot(environments, services)
	if err != nil {
		panic(err)
	}
	return snapshot
}

func TestStateRootFor(t *testing.T) {
	root, err := StateRootFor(filepath.Join("user", "config"))
	if err != nil {
		t.Fatalf("StateRootFor() error = %v", err)
	}
	if want := filepath.Join("user", "config", "Matagi"); root != want {
		t.Fatalf("StateRootFor() = %q, want %q", root, want)
	}
	if _, err := StateRootFor("  "); err == nil {
		t.Fatal("StateRootFor() accepted an empty base directory")
	}
}

func TestStoreRoundTripsDeterministically(t *testing.T) {
	first, err := NewStore(filepath.Join(t.TempDir(), "Matagi"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewStore(filepath.Join(t.TempDir(), "Matagi"))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Save(configFixture(false)); err != nil {
		t.Fatalf("first Save() error = %v", err)
	}
	if err := second.Save(configFixture(true)); err != nil {
		t.Fatalf("second Save() error = %v", err)
	}
	firstBytes, err := os.ReadFile(first.Path())
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := os.ReadFile(second.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBytes) != string(secondBytes) {
		t.Fatalf("registry encoding is not deterministic:\n%s\n---\n%s", firstBytes, secondBytes)
	}
	for _, forbidden := range []string{"privateKey", "password", "runId", "generation", "pid", "localPort", "healthResult"} {
		if strings.Contains(string(firstBytes), forbidden) {
			t.Fatalf("registry document contains forbidden field %q", forbidden)
		}
	}

	loaded, err := first.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(loaded.Environments(), configFixture(false).Environments()) || !reflect.DeepEqual(loaded.Services(), configFixture(false).Services()) {
		t.Fatalf("loaded snapshot differs from saved snapshot:\nenvironments: %#v\nservices: %#v", loaded.Environments(), loaded.Services())
	}
	service := loaded.Services()[0]
	if service.Health.Method == nil || *service.Health.Method != "GET" || service.Health.ExpectedStatus == nil || !reflect.DeepEqual(*service.Health.ExpectedStatus, []int{200}) || service.Health.TimeoutMS == nil || *service.Health.TimeoutMS != 2000 {
		t.Fatalf("omitted health options did not receive their defaults: %#v", service.Health)
	}
}

func TestLoadRejectsCorruptUnsupportedAndSecretFields(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "corrupt JSON", data: `{"version":`},
		{name: "unsupported version", data: `{"version":2,"environments":[],"services":[]}`},
		{name: "trailing value", data: `{"version":1,"environments":[],"services":[]} {}`},
		{name: "unknown authentication field", data: `{"version":1,"sshPrivateKey":"secret","environments":[],"services":[]}`},
		{name: "null optional health field", data: `{"version":1,"environments":[{"id":"dev","sshHost":"dev"}],"services":[{"id":"app","environmentId":"dev","desiredState":"running","execution":{"argv":["app"],"lifetime":"detached"},"health":{"type":"http","endpointId":"web","path":"/ready","method":null},"endpoints":[{"id":"web","label":"Web","remoteAddress":"127.0.0.1","remotePort":8080}]}]}`},
		{name: "invalid registry", data: `{"version":1,"environments":[{"id":"dev","sshHost":"dev"}],"services":[{"id":"app","environmentId":"dev","desiredState":"running","execution":{"argv":[],"lifetime":"detached"},"health":{"type":"http","endpointId":"web","path":"/ready"},"endpoints":[{"id":"web","label":"Web","remoteAddress":"127.0.0.1","remotePort":8080}]}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.Path(), []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := store.Load()
			if err == nil {
				t.Fatalf("Load() accepted invalid document: %#v", got)
			}
			if got != nil {
				t.Fatalf("Load() returned a partial snapshot on error: %#v", got)
			}
		})
	}
}

func TestSaveRequiresSnapshot(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(nil); err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("Save(nil) error = %v, want snapshot error", err)
	}
}
