package registry

import (
	"strings"
	"testing"
)

func registryFixture() ([]Environment, []Service) {
	return []Environment{{ID: "dev", SSHHost: "dev-host"}}, []Service{{
		ID:            "dashboard",
		EnvironmentID: "dev",
		DesiredState:  DesiredRunning,
		Execution: ExecutionIntent{
			Argv:     []string{"/opt/dashboard", "--port", "8080"},
			CWD:      "/srv/dashboard",
			Lifetime: LifetimeDetached,
		},
		Health:    HealthDefinition{Type: HealthHTTP, EndpointID: "web", Path: "/ready"},
		Endpoints: []Endpoint{{ID: "web", Label: "Dashboard", RemoteAddress: RemoteLoopbackAddress, RemotePort: 8080}},
	}}
}

func TestNewSnapshotRejectsInvalidDefinitions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]Environment, []Service) ([]Environment, []Service)
	}{
		{name: "duplicate environment", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			return append(envs, envs[0]), services
		}},
		{name: "invalid ssh target", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			envs[0].SSHHost = "-oProxyCommand=unsafe"
			return envs, services
		}},
		{name: "unknown environment", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			services[0].EnvironmentID = "missing"
			return envs, services
		}},
		{name: "duplicate service", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			return envs, append(services, services[0])
		}},
		{name: "invalid desired state", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			services[0].DesiredState = "starting"
			return envs, services
		}},
		{name: "empty argv", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			services[0].Execution.Argv = nil
			return envs, services
		}},
		{name: "empty argv element", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			services[0].Execution.Argv[1] = ""
			return envs, services
		}},
		{name: "non-detached execution", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			services[0].Execution.Lifetime = "foreground"
			return envs, services
		}},
		{name: "unknown health type", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			services[0].Health.Type = "tcp"
			return envs, services
		}},
		{name: "health path without slash", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			services[0].Health.Path = "ready"
			return envs, services
		}},
		{name: "health endpoint does not belong to service", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			services[0].Health.EndpointID = "other"
			return envs, services
		}},
		{name: "health method", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			method := "POST"
			services[0].Health.Method = &method
			return envs, services
		}},
		{name: "empty expected statuses", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			statuses := []int{}
			services[0].Health.ExpectedStatus = &statuses
			return envs, services
		}},
		{name: "invalid expected status", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			statuses := []int{200, 600}
			services[0].Health.ExpectedStatus = &statuses
			return envs, services
		}},
		{name: "nonpositive timeout", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			timeout := 0
			services[0].Health.TimeoutMS = &timeout
			return envs, services
		}},
		{name: "non-loopback endpoint", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			services[0].Endpoints[0].RemoteAddress = "0.0.0.0"
			return envs, services
		}},
		{name: "invalid endpoint port", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			services[0].Endpoints[0].RemotePort = 65536
			return envs, services
		}},
		{name: "duplicate endpoint", mutate: func(envs []Environment, services []Service) ([]Environment, []Service) {
			services[0].Endpoints = append(services[0].Endpoints, services[0].Endpoints[0])
			return envs, services
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environments, services := registryFixture()
			environments, services = test.mutate(environments, services)
			if _, err := NewSnapshot(environments, services); err == nil {
				t.Fatal("NewSnapshot() succeeded for invalid definition")
			}
		})
	}
}

func TestNewSnapshotDefaultsCanonicalOrderAndDefensiveCopies(t *testing.T) {
	environments, services := registryFixture()
	services[0].Endpoints = append(services[0].Endpoints, Endpoint{ID: "logs", Label: "Logs", RemoteAddress: RemoteLoopbackAddress, RemotePort: 9090})
	statuses := []int{204, 200}
	services[0].Health.ExpectedStatus = &statuses
	snapshot, err := NewSnapshot(environments, services)
	if err != nil {
		t.Fatalf("NewSnapshot() error = %v", err)
	}

	got := snapshot.Services()[0]
	if got.Health.Method == nil || *got.Health.Method != "GET" {
		t.Fatalf("default method = %v, want GET", got.Health.Method)
	}
	if got.Health.ExpectedStatus == nil || len(*got.Health.ExpectedStatus) != 2 || (*got.Health.ExpectedStatus)[0] != 200 || (*got.Health.ExpectedStatus)[1] != 204 {
		t.Fatalf("canonical statuses = %v, want [200 204]", got.Health.ExpectedStatus)
	}
	if got.Health.TimeoutMS == nil || *got.Health.TimeoutMS != 2000 {
		t.Fatalf("default timeout = %v, want 2000", got.Health.TimeoutMS)
	}
	if got.Endpoints[0].ID != "logs" || got.Endpoints[1].ID != "web" {
		t.Fatalf("endpoint order = [%s %s], want [logs web]", got.Endpoints[0].ID, got.Endpoints[1].ID)
	}

	got.Execution.Argv[0] = "changed"
	got.Endpoints[0].Label = "changed"
	*got.Health.Method = "changed"
	if again := snapshot.Services()[0]; again.Execution.Argv[0] != "/opt/dashboard" || again.Endpoints[0].Label != "Logs" || *again.Health.Method != "GET" {
		t.Fatalf("snapshot was mutated through accessor: %#v", again)
	}
}

func TestCorrelationOwnerIsStableBoundedAndOpaque(t *testing.T) {
	service := Service{ID: "dashboard", EnvironmentID: "dev"}
	owner := service.CorrelationOwner()
	if owner != service.CorrelationOwner() {
		t.Fatal("correlation owner is not stable")
	}
	if len(owner) > 80 || strings.Contains(owner, string(service.ID)) || strings.Contains(owner, string(service.EnvironmentID)) {
		t.Fatalf("correlation owner is not bounded and opaque: %q", owner)
	}
	if owner == (Service{ID: "dashboard", EnvironmentID: "other"}).CorrelationOwner() {
		t.Fatal("different service identity produced the same owner")
	}
}
