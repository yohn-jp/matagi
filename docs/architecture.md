# Matagi Architecture

## Purpose

Matagi makes resident services running in development environments feel local from Windows.

The primary user experience is:

1. Register a development environment and its resident services once.
2. See whether those services are reachable and running from Windows.
3. Start, stop, or restart managed services without manually entering the remote host.
4. Open each registered web UI from Windows without manually creating SSH tunnels or tracking remote ports.

Matagi is therefore both a resident-service manager from the Windows user's perspective and a Windows primitive for reaching development-environment service UIs.

## System context

```text
Windows
┌────────────────────────────────────────────────────┐
│ Matagi                                             │
│                                                    │
│ Environment / Service Registry                     │
│ Health Observation                                 │
│ SSH Connection + Tunnel Lifecycle                  │
│ UI Endpoint Presentation                           │
│ WebView2 Desktop Surface                           │
└──────────────────────┬─────────────────────────────┘
                       │ system OpenSSH
                       │ control + local forwarding
                       ▼
Development environment
┌────────────────────────────────────────────────────┐
│ sshd                                               │
│                                                    │
│ Jinushi                                            │
│ ├─ Inari resident runtime                          │
│ ├─ Yokodori daemon                                 │
│ └─ other registered resident services             │
└────────────────────────────────────────────────────┘
```

Matagi does not require a Matagi-specific remote agent in the initial architecture.

## Authority boundaries

### Matagi owns

On Windows, Matagi owns:

- registered development environments;
- registered resident-service definitions;
- observed connection and service health;
- SSH connection/tunnel lifecycle initiated by Matagi;
- mapping remote service endpoints to Windows-local endpoints;
- presentation and opening of registered service web UIs;
- the desired Windows-visible state of registered services.

Matagi's runtime state is the single authority behind both its programmatic behavior and the WebView2 presentation surface.

### Development environment owns

The remote development environment owns actual process state.

For services managed by Jinushi, Jinushi remains the process lifecycle authority:

- process start;
- process stop;
- process supervision;
- exit state;
- stdout/stderr and process-level evidence exposed by Jinushi.

Matagi may request those operations, but it must not implement a competing remote supervisor.

### Other products retain their authority

- Mottainai owns higher-level orchestration policy, not Matagi.
- Nawabari owns workspace/filesystem authority; it is not a resident-service manager.
- Each product continues to own its own web UI and runtime semantics.
- Matagi transports and presents those UIs; it does not reimplement them.

## SSH is the transport primitive

Matagi uses the system OpenSSH client rather than defining a proprietary remote-control protocol.

The architecture should preserve normal OpenSSH semantics, including the user's existing configuration such as:

- `Host` aliases;
- `IdentityFile`;
- `ProxyJump`;
- host-key verification and `known_hosts`;
- other standard client options that are already part of the user's SSH environment.

Matagi must not maintain a second independent SSH identity/configuration authority.

SSH serves two purposes:

1. bootstrap/control operations needed to reach the development environment;
2. local port forwarding used to expose registered remote UI endpoints to Windows.

## Remote service lifecycle

The normal lifecycle is:

```text
Matagi
  │
  ├─ SSH connect
  │
  ├─ Is Jinushi reachable?
  │      └─ no → bootstrap/start Jinushi through SSH
  │
  └─ Jinushi ready
         ├─ ensure Inari state
         ├─ ensure Yokodori state
         └─ ensure other registered service state
```

Jinushi is special only because it is the lifecycle authority Matagi depends on. SSH is the bootstrap primitive used before Jinushi is available.

The initial architecture does not define a second remote daemon for Matagi.

## Service registration model

A registered service belongs to a registered development environment and has enough information for Matagi to:

- identify the service;
- determine how its lifecycle is delegated to Jinushi;
- determine health/readiness;
- discover one or more UI endpoints;
- determine whether a UI endpoint should be exposed to Windows.

The exact serialization format is an implementation contract to be defined by a governing implementation Issue. This architecture does not require a particular YAML/JSON schema.

## Health model

Matagi distinguishes transport reachability from service health.

At minimum, the UI must be able to represent:

- development environment disconnected/unreachable;
- service stopped;
- service starting/not ready;
- service running/ready;
- service unhealthy or otherwise failing observation.

A successful SSH connection is not evidence that a service is ready. A running remote process is not necessarily evidence that its UI endpoint is healthy.

## Endpoint and tunnel model

Remote services may bind only to the development host's loopback interface.

Matagi maps a logical registered UI endpoint to a Windows-local endpoint through SSH local forwarding:

```text
registered UI: yokodori.dashboard
            │
            ▼
Windows 127.0.0.1:<allocated-port>
            │
            │ managed SSH local forward
            ▼
Remote  127.0.0.1:33031
            │
            ▼
        Yokodori UI
```

The Windows-local port does not have to equal the remote port. Matagi may allocate a free local port and retain the logical service/endpoint identity.

The user-facing contract is the logical endpoint and the Open action, not manual port management.

Tunnel state is part of Matagi's Windows-side runtime state. A service can therefore be healthy remotely while its Windows presentation is unavailable because the tunnel is not established; those states must not be conflated.

## Windows application

The initial implementation is Go.

The Windows desktop surface uses WebView2. WebView2 is a presentation layer over Matagi's runtime and endpoint registry; it must not become an independent lifecycle authority.

The primary UI presents registered development environments and services, including:

- connection state;
- service liveness/readiness;
- lifecycle actions allowed by the service contract;
- registered UI endpoints;
- an Open action for each UI endpoint.

Opening a registered UI should require no manual SSH command, tunnel setup, or remote-port lookup.

## Security posture

The architecture crosses three material trust boundaries:

1. Windows ↔ SSH client configuration and credentials;
2. Windows ↔ remote development environment;
3. WebView2 ↔ tunneled product UIs.

Consequently:

- host-key verification must remain intact;
- SSH private material must not be copied into Matagi-specific storage merely for convenience;
- automatically exposed endpoints should remain loopback-local on Windows;
- arbitrary unregistered remote endpoints must not become silently browsable;
- WebView2 navigation/permissions must be constrained to the intended registered presentation surface;
- service lifecycle requests must preserve Jinushi's authority rather than bypassing it with an ad hoc process manager.

## Initial technology decisions

The accepted initial stack is:

- Go for the Matagi runtime;
- WebView2 for the Windows desktop presentation surface;
- system OpenSSH for SSH connectivity and forwarding;
- Jinushi for managed remote process lifecycle;
- product-owned HTTP/Web UIs presented through Matagi-managed local forwarding.

These are architecture decisions, not implementation completion claims.

## Non-goals

The initial architecture does not make Matagi:

- a general orchestration engine;
- a replacement for Mottainai;
- a replacement for Jinushi;
- a workspace/filesystem authority;
- a replacement for Nawabari;
- a remote desktop product;
- a generic SSH client;
- a reverse proxy for arbitrary internet-facing services;
- an implementation host for Inari, Yokodori, or other product-specific UIs.

Future changes to these boundaries require explicit architecture approval.
