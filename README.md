# Matagi

Matagi is a Windows-side control plane for resident services running in connected development environments.

It gives Windows one place to see registered service health, start or restart remote services, and open their web UIs without manually managing SSH tunnels or remote ports.

Matagi uses system OpenSSH as its transport primitive and delegates remote process lifecycle to Jinushi. The Windows application is implemented in Go with a WebView2 desktop surface.

The accepted architecture is documented in [docs/architecture.md](docs/architecture.md).

## Status

Matagi is pre-implementation. Issue #1 establishes the initial architecture and repository baseline.

## Development

Read [AGENTS.md](AGENTS.md) and [CONTRIBUTING.md](CONTRIBUTING.md) before making changes.

## Security

Report vulnerabilities through the process in [SECURITY.md](SECURITY.md).

## License

MIT. See [LICENSE](LICENSE).
