// Package registry defines Matagi's declarative environment and service model.
// It contains desired configuration only; runtime observations belong to the
// Matagi runtime.
package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const RemoteLoopbackAddress = "127.0.0.1"

type EnvironmentID string
type ServiceID string
type EndpointID string

type Environment struct {
	ID      EnvironmentID     `json:"id"`
	SSHHost string            `json:"sshHost"`
	Jinushi JinushiDefinition `json:"jinushi"`
}

// JinushiDefinition contains the environment-specific configuration required
// to construct Jinushi client options. SupervisorStartCommand preserves the
// configured argv boundaries used to start the remote supervisor.
type JinushiDefinition struct {
	StateDir               string   `json:"stateDir,omitempty"`
	SupervisorStartCommand []string `json:"supervisorStartCommand"`
}

type DesiredState string

const (
	DesiredRunning DesiredState = "running"
	DesiredStopped DesiredState = "stopped"
)

type ExecutionLifetime string

const LifetimeDetached ExecutionLifetime = "detached"

// ExecutionIntent preserves argv boundaries and the explicit working
// directory required by Jinushi. Runtime run and submission identities are
// deliberately not stored.
type ExecutionIntent struct {
	Argv     []string          `json:"argv"`
	CWD      string            `json:"cwd"`
	Lifetime ExecutionLifetime `json:"lifetime"`
}

type HealthType string

const HealthHTTP HealthType = "http"

// HealthDefinition is the persisted HTTP readiness probe configuration.
// Optional pointers distinguish omitted defaults from explicitly invalid zero
// values; snapshots materialize defaults before publication.
type HealthDefinition struct {
	Type           HealthType `json:"type"`
	EndpointID     EndpointID `json:"endpointId"`
	Path           string     `json:"path"`
	Method         *string    `json:"method,omitempty"`
	ExpectedStatus *[]int     `json:"expectedStatus,omitempty"`
	TimeoutMS      *int       `json:"timeoutMs,omitempty"`
}

func (h *HealthDefinition) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"method", "expectedStatus", "timeoutMs"} {
		if value, exists := fields[name]; exists && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("health field %q must be omitted or have a value", name)
		}
	}
	type healthDefinition HealthDefinition
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded healthDefinition
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return errors.New("health definition contains trailing JSON")
		}
		return err
	}
	*h = HealthDefinition(decoded)
	return nil
}

// Endpoint describes a registered UI endpoint on the remote development
// host. RemoteAddress is restricted to the loopback target used by Matagi's
// tunnel contract; local allocated ports are runtime state.
type Endpoint struct {
	ID            EndpointID `json:"id"`
	Label         string     `json:"label"`
	RemoteAddress string     `json:"remoteAddress"`
	RemotePort    int        `json:"remotePort"`
}

type Service struct {
	ID            ServiceID        `json:"id"`
	EnvironmentID EnvironmentID    `json:"environmentId"`
	DesiredState  DesiredState     `json:"desiredState"`
	Execution     ExecutionIntent  `json:"execution"`
	Health        HealthDefinition `json:"health"`
	Endpoints     []Endpoint       `json:"endpoints"`
}

// CorrelationOwner returns a bounded opaque owner derived from the stable
// Matagi service identity. It is intentionally not a Jinushi run or submission
// identity and is not stored separately in the registry document.
func (s Service) CorrelationOwner() string {
	identity := string(s.EnvironmentID) + "\x00" + string(s.ID)
	sum := sha256.Sum256([]byte(identity))
	return "matagi:" + hex.EncodeToString(sum[:])
}

// Snapshot is a validated immutable registry view. Its accessors return
// defensive copies so callers cannot mutate published state.
type Snapshot struct {
	environments []Environment
	services     []Service
}

// NewSnapshot validates and canonicalizes one complete registry document.
func NewSnapshot(environments []Environment, services []Service) (*Snapshot, error) {
	environments = cloneEnvironments(environments)
	services = cloneServices(services)

	seenEnvironments := make(map[EnvironmentID]struct{}, len(environments))
	for _, environment := range environments {
		if !validIdentity(string(environment.ID)) {
			return nil, errors.New("environment ID is required and must not have surrounding whitespace")
		}
		if _, exists := seenEnvironments[environment.ID]; exists {
			return nil, fmt.Errorf("duplicate environment ID %q", environment.ID)
		}
		seenEnvironments[environment.ID] = struct{}{}
		if strings.TrimSpace(environment.SSHHost) == "" || environment.SSHHost != strings.TrimSpace(environment.SSHHost) || strings.HasPrefix(environment.SSHHost, "-") || strings.ContainsAny(environment.SSHHost, " \t\r\n\x00") {
			return nil, fmt.Errorf("environment %q has an invalid OpenSSH host target", environment.ID)
		}
		if err := validateJinushiDefinition(environment.ID, environment.Jinushi); err != nil {
			return nil, err
		}
	}

	seenServices := make(map[string]struct{}, len(services))
	for i := range services {
		service := &services[i]
		if !validIdentity(string(service.ID)) {
			return nil, errors.New("service ID is required and must not have surrounding whitespace")
		}
		if !validIdentity(string(service.EnvironmentID)) {
			return nil, fmt.Errorf("service %q must reference an environment", service.ID)
		}
		if _, exists := seenEnvironments[service.EnvironmentID]; !exists {
			return nil, fmt.Errorf("service %q references unknown environment %q", service.ID, service.EnvironmentID)
		}
		identity := string(service.EnvironmentID) + "\x00" + string(service.ID)
		if _, exists := seenServices[identity]; exists {
			return nil, fmt.Errorf("duplicate service ID %q in environment %q", service.ID, service.EnvironmentID)
		}
		seenServices[identity] = struct{}{}
		if service.DesiredState != DesiredRunning && service.DesiredState != DesiredStopped {
			return nil, fmt.Errorf("service %q has invalid desired state %q", service.ID, service.DesiredState)
		}
		if err := validateExecution(service.ID, service.Execution); err != nil {
			return nil, err
		}
		service.Execution.Argv = append([]string(nil), service.Execution.Argv...)
		if err := normalizeAndValidateHealth(service.ID, service.Health, &service.Health); err != nil {
			return nil, err
		}

		seenEndpoints := make(map[EndpointID]struct{}, len(service.Endpoints))
		for _, endpoint := range service.Endpoints {
			if !validIdentity(string(endpoint.ID)) {
				return nil, fmt.Errorf("service %q has an endpoint with an invalid ID", service.ID)
			}
			if _, exists := seenEndpoints[endpoint.ID]; exists {
				return nil, fmt.Errorf("duplicate endpoint ID %q in service %q", endpoint.ID, service.ID)
			}
			seenEndpoints[endpoint.ID] = struct{}{}
			if strings.TrimSpace(endpoint.Label) == "" || strings.ContainsRune(endpoint.Label, '\x00') {
				return nil, fmt.Errorf("endpoint %q in service %q requires a presentation label", endpoint.ID, service.ID)
			}
			if endpoint.RemoteAddress != RemoteLoopbackAddress {
				return nil, fmt.Errorf("endpoint %q in service %q must target remote loopback %s", endpoint.ID, service.ID, RemoteLoopbackAddress)
			}
			if endpoint.RemotePort < 1 || endpoint.RemotePort > 65535 {
				return nil, fmt.Errorf("endpoint %q in service %q remote port must be between 1 and 65535", endpoint.ID, service.ID)
			}
		}
		if _, exists := seenEndpoints[service.Health.EndpointID]; !exists {
			return nil, fmt.Errorf("service %q health definition references unknown endpoint %q", service.ID, service.Health.EndpointID)
		}
		sort.Slice(service.Endpoints, func(a, b int) bool { return service.Endpoints[a].ID < service.Endpoints[b].ID })
	}

	sort.Slice(environments, func(a, b int) bool { return environments[a].ID < environments[b].ID })
	sort.Slice(services, func(a, b int) bool {
		if services[a].EnvironmentID != services[b].EnvironmentID {
			return services[a].EnvironmentID < services[b].EnvironmentID
		}
		return services[a].ID < services[b].ID
	})
	return &Snapshot{environments: environments, services: services}, nil
}

func validateExecution(serviceID ServiceID, execution ExecutionIntent) error {
	if len(execution.Argv) == 0 {
		return fmt.Errorf("service %q execution argv must not be empty", serviceID)
	}
	for i, arg := range execution.Argv {
		if arg == "" || (i == 0 && strings.TrimSpace(arg) == "") || strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("service %q execution argv[%d] is invalid", serviceID, i)
		}
	}
	if strings.TrimSpace(execution.CWD) == "" || execution.CWD != strings.TrimSpace(execution.CWD) || strings.ContainsRune(execution.CWD, '\x00') {
		return fmt.Errorf("service %q execution cwd must be non-empty, trimmed, and NUL-free", serviceID)
	}
	if execution.Lifetime != LifetimeDetached {
		return fmt.Errorf("service %q execution lifetime must be %q", serviceID, LifetimeDetached)
	}
	return nil
}

func validateJinushiDefinition(environmentID EnvironmentID, definition JinushiDefinition) error {
	if strings.ContainsRune(definition.StateDir, '\x00') {
		return fmt.Errorf("environment %q Jinushi state directory cannot contain NUL", environmentID)
	}
	for i, arg := range definition.SupervisorStartCommand {
		if arg == "" || (i == 0 && strings.TrimSpace(arg) == "") || strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("environment %q Jinushi supervisor start command argv[%d] is invalid", environmentID, i)
		}
	}
	return nil
}

func normalizeAndValidateHealth(serviceID ServiceID, health HealthDefinition, destination *HealthDefinition) error {
	if health.Type != HealthHTTP {
		return fmt.Errorf("service %q health type must be %q", serviceID, HealthHTTP)
	}
	if !validIdentity(string(health.EndpointID)) {
		return fmt.Errorf("service %q health endpoint ID is required", serviceID)
	}
	if health.Path == "" || !strings.HasPrefix(health.Path, "/") || strings.ContainsAny(health.Path, "\x00\r\n") {
		return fmt.Errorf("service %q health path must begin with /", serviceID)
	}
	if health.Method == nil {
		method := "GET"
		health.Method = &method
	} else if *health.Method != "GET" {
		return fmt.Errorf("service %q health method must be GET", serviceID)
	}
	if health.ExpectedStatus == nil {
		statuses := []int{200}
		health.ExpectedStatus = &statuses
	} else {
		if len(*health.ExpectedStatus) == 0 {
			return fmt.Errorf("service %q health expectedStatus must not be empty", serviceID)
		}
		statuses := append([]int(nil), (*health.ExpectedStatus)...)
		for _, status := range statuses {
			if status < 100 || status > 599 {
				return fmt.Errorf("service %q health status %d is invalid", serviceID, status)
			}
		}
		sort.Ints(statuses)
		health.ExpectedStatus = &statuses
	}
	if health.TimeoutMS == nil {
		timeout := 2000
		health.TimeoutMS = &timeout
	} else if *health.TimeoutMS <= 0 {
		return fmt.Errorf("service %q health timeoutMs must be positive", serviceID)
	}
	*destination = cloneHealth(health)
	return nil
}

func validIdentity(value string) bool {
	return strings.TrimSpace(value) != "" && value == strings.TrimSpace(value) && !strings.ContainsRune(value, '\x00')
}

func (s *Snapshot) Environments() []Environment {
	if s == nil {
		return nil
	}
	return cloneEnvironments(s.environments)
}

func (s *Snapshot) Services() []Service {
	if s == nil {
		return nil
	}
	return cloneServices(s.services)
}

func cloneServices(services []Service) []Service {
	cloned := make([]Service, len(services))
	for i, service := range services {
		cloned[i] = service
		cloned[i].Execution.Argv = append([]string{}, service.Execution.Argv...)
		cloned[i].Health = cloneHealth(service.Health)
		cloned[i].Endpoints = append([]Endpoint{}, service.Endpoints...)
	}
	return cloned
}

func cloneEnvironments(environments []Environment) []Environment {
	cloned := make([]Environment, len(environments))
	copy(cloned, environments)
	for i := range cloned {
		cloned[i].Jinushi.SupervisorStartCommand = append([]string{}, environments[i].Jinushi.SupervisorStartCommand...)
	}
	return cloned
}

func cloneHealth(health HealthDefinition) HealthDefinition {
	cloned := health
	if health.Method != nil {
		method := *health.Method
		cloned.Method = &method
	}
	if health.ExpectedStatus != nil {
		statuses := append([]int(nil), (*health.ExpectedStatus)...)
		cloned.ExpectedStatus = &statuses
	}
	if health.TimeoutMS != nil {
		timeout := *health.TimeoutMS
		cloned.TimeoutMS = &timeout
	}
	return cloned
}
