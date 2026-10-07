package runtime

import (
	"context"
	"errors"
	"strings"

	"github.com/yohn-jp/matagi/internal/jinushi"
	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/internal/ssh"
)

// Connect verifies the transport and supervisor before committing any configuration.
// Bootstrap is only attempted when the operator explicitly supplies an argv.
func (r *Runtime) Connect(ctx context.Context, id, host string, bootstrap []string) error {
	env := registry.Environment{ID: registry.EnvironmentID(id), SSHHost: host, Jinushi: registry.JinushiDefinition{SupervisorStartCommand: bootstrap}}
	snap, err := registry.NewSnapshot([]registry.Environment{env}, nil)
	if err != nil {
		return &Failure{Code: "invalid-request"}
	}
	r.mu.Lock()
	if r.closed || len(r.registry.Environments()) != 0 {
		r.mu.Unlock()
		return &Failure{Code: "lifecycle-conflict"}
	}
	r.mu.Unlock()
	if _, err := r.ssh.Run(ctx, host, []string{"true"}, probeTimeout); err != nil {
		var se *ssh.Error
		if errors.As(err, &se) {
			if se.Kind == ssh.FailureTransport {
				return &Failure{Code: "ssh-transport-failed"}
			}
			if se.Kind == ssh.FailureTimeout {
				return &Failure{Code: "ssh-timeout"}
			}
		}
		return &Failure{Code: "ssh-connectivity-failed"}
	}
	client, err := jinushi.New(executor{client: r.ssh, host: host}, jinushi.Options{SupervisorStartCommand: bootstrap, CommandTimeout: commandTimeout})
	if err != nil {
		return &Failure{Code: "invalid-request"}
	}
	if err := client.EnsureReady(ctx); err != nil {
		var f *jinushi.Failure
		if errors.As(err, &f) {
			switch f.Kind {
			case jinushi.KindBootstrapFailed:
				return &Failure{Code: "jinushi-bootstrap-failed"}
			case jinushi.KindBootstrapNotConfigured, jinushi.KindSupervisorUnavailable:
				return &Failure{Code: "jinushi-unavailable"}
			case jinushi.KindTimeout:
				return &Failure{Code: "jinushi-readiness-timeout"}
			}
		}
		return &Failure{Code: "jinushi-readiness-failed"}
	}
	if err := r.Register(snap); err != nil {
		return err
	}
	r.mu.Lock()
	r.jinushi[id] = "ready"
	r.mu.Unlock()
	return nil
}

// AddService commits a validated service definition through the canonical registry.
func (r *Runtime) AddService(service registry.Service) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.store == nil || len(r.registry.Environments()) == 0 {
		return &Failure{Code: "lifecycle-conflict"}
	}
	if strings.TrimSpace(string(service.ID)) == "" {
		return &Failure{Code: "invalid-request"}
	}
	services := r.registry.Services()
	for _, existing := range services {
		if existing.ID == service.ID && existing.EnvironmentID == service.EnvironmentID {
			return &Failure{Code: "lifecycle-conflict"}
		}
	}
	services = append(services, service)
	snapshot, err := registry.NewSnapshot(r.registry.Environments(), services)
	if err != nil {
		return &Failure{Code: "invalid-request"}
	}
	candidate := &Runtime{ssh: r.ssh, tunnels: r.tunnels, httpClient: r.httpClient}
	if err := candidate.configure(snapshot); err != nil {
		return &Failure{Code: "invalid-request"}
	}
	if err := r.store.Save(snapshot); err != nil {
		return err
	}
	return r.configure(snapshot)
}
