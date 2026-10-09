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
	opCtx, finish, err := r.beginOperation(ctx)
	if err != nil {
		return &Failure{Code: "lifecycle-conflict"}
	}
	defer finish()
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
	sshClient := r.ssh
	gate := r.environmentGateLocked(id)
	r.mu.Unlock()
	release, err := acquireGate(opCtx, gate)
	if err != nil {
		return classify(err)
	}
	defer release()
	if err := opCtx.Err(); err != nil {
		return classify(err)
	}
	if _, err := sshClient.Run(opCtx, host, []string{"true"}, probeTimeout); err != nil {
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
	client, err := jinushi.New(executor{client: sshClient, host: host}, jinushi.Options{SupervisorStartCommand: bootstrap, CommandTimeout: commandTimeout})
	if err != nil {
		return &Failure{Code: "invalid-request"}
	}
	if err := client.EnsureReady(opCtx); err != nil {
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
	var cancelAfterPublish context.CancelFunc
	if !r.closed && r.bindings[id].environment.SSHHost == host {
		r.jinushi[id] = "ready"
		delete(r.jinushiErrors, id)
		cancelAfterPublish = r.invalidatePollLocked()
	}
	r.mu.Unlock()
	if cancelAfterPublish != nil {
		cancelAfterPublish()
	}
	return nil
}

// AddService commits a validated service definition through the canonical registry.
func (r *Runtime) AddService(service registry.Service) error {
	opCtx, finish, err := r.beginOperation(context.Background())
	if err != nil {
		return &Failure{Code: "lifecycle-conflict"}
	}
	defer finish()
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	r.mu.Lock()
	if r.closed || r.store == nil || len(r.registry.Environments()) == 0 {
		r.mu.Unlock()
		return &Failure{Code: "lifecycle-conflict"}
	}
	if strings.TrimSpace(string(service.ID)) == "" {
		r.mu.Unlock()
		return &Failure{Code: "invalid-request"}
	}
	services := r.registry.Services()
	for _, existing := range services {
		if existing.ID == service.ID && existing.EnvironmentID == service.EnvironmentID {
			r.mu.Unlock()
			return &Failure{Code: "lifecycle-conflict"}
		}
	}
	baseVersion := r.configVersion
	environments := r.registry.Environments()
	store, sshClient, tunnels, httpClient := r.store, r.ssh, r.tunnels, r.httpClient
	services = append(services, service)
	r.mu.Unlock()
	snapshot, err := registry.NewSnapshot(environments, services)
	if err != nil {
		return &Failure{Code: "invalid-request"}
	}
	candidate := &Runtime{ssh: sshClient, tunnels: tunnels, httpClient: httpClient}
	if err := candidate.configure(snapshot); err != nil {
		return &Failure{Code: "invalid-request"}
	}
	if err := opCtx.Err(); err != nil {
		return classify(err)
	}
	if err := store.Save(snapshot); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.configVersion != baseVersion {
		return &Failure{Code: "lifecycle-conflict"}
	}
	return r.configure(snapshot)
}
