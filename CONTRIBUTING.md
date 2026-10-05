# Contributing to Matagi

## Start with the contract

Read `AGENTS.md`, the organization governance contract, and the applicable purpose-specific Skill before making changes. `docs/architecture.md` defines the accepted product architecture; the governing Issue or latest explicit owner instruction defines the requested work.

Matagi owns the Windows-side control plane for registered resident development services. Do not move remote process supervision, workspace authority, orchestration policy, or product-specific UI implementation into this repository.

## Environment

Matagi is a Go module. Use the Go version declared in `go.mod`.

Use the system OpenSSH client for SSH transport behavior. Do not add an independent SSH configuration/authentication stack unless an accepted architecture change explicitly requires it.

## Isolated work

Do not implement directly on `main`. Use a governed branch/worktree and preserve unrelated work. Do not reset, force-push, merge, close, or alter live repository settings without explicit authority.

## Implementation boundaries

Reuse the accepted service registry, health observation, tunnel, endpoint, Jinushi integration, and Windows/WebView2 boundaries before adding new machinery.

The Windows runtime is the authority for Matagi's registered environments, observed service state, tunnels, and presented endpoints. Jinushi remains the authority for managed remote process lifecycle. The WebView2 shell must stay a thin presentation surface over the same runtime state.

Do not introduce a Matagi remote agent in the initial architecture. SSH is the bootstrap/control transport.

## Validation

Run focused tests while editing. Before delivery run:

```sh
gofmt -l .
go vet ./...
go test -race ./...
```

A clean portable test run does not prove Windows WebView2 behavior, real OpenSSH integration, live SSH tunneling, or remote Jinushi/service lifecycle. Report the exact environment and checks actually executed.

## Pull requests

Use the repository's current branch, title, template, and governance contracts. Keep PRs bounded to the accepted change and describe actual validation and limitations.

A PR being open or mergeable is not approval or merge authority.

## Security and disclosure

Follow [SECURITY.md](SECURITY.md). Never publish credentials, SSH private keys, tokens, sensitive runtime data, or unsafe reproduction material in Issues, PRs, logs, or retained evidence.
