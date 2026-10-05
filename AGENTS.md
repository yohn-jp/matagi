# Matagi

Read `.github/agent-governance/AGENTS.md` and the applicable organization Skill before work. The organization execution contract remains in force.

Matagi is the Windows-side control plane for registered resident services in connected development environments. It owns Windows-side environment/service registration, liveness observation, SSH tunnel lifecycle, and presentation of registered service UI endpoints.

## Authority

- `docs/architecture.md` is the product architecture authority.
- The governing Issue or latest explicit product-owner instruction defines requested work.
- Architecture changes require explicit product-owner/design-review approval.
- Matagi owns Windows-side service discovery, health, transport, and UI access. It does not own remote process supervision, workspace authority, orchestration policy, or product-specific web UIs.
- Jinushi remains the remote process lifecycle authority for managed services.
- System OpenSSH remains the transport primitive. Do not create a second SSH configuration or authentication authority.
- The WebView2 shell is a presentation surface over the same Matagi runtime state. Do not create a second lifecycle or policy authority in the GUI.

## Validation

Canonical portable verification:

```sh
gofmt -l .
go vet ./...
go test -race ./...
```

Run focused Go tests while editing, then canonical verification before delivery. Windows/WebView2 behavior, real SSH connectivity, tunnel behavior, and live remote-service lifecycle are separate evidence boundaries; do not claim them from portable unit tests alone.
