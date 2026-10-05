# Security Policy

## Supported versions

Matagi is pre-1.0. Security fixes target `main` and the latest 0.x release; there is no long-term-support branch.

## Reporting a vulnerability

Report suspected vulnerabilities privately through [GitHub Security Advisories](../../security/advisories/new), not a public Issue. If that channel is unavailable, open an Issue containing only minimal nonsensitive detail and ask a maintainer to establish a private channel.

Include the affected version or commit, impact, and a minimal safe reproduction. Do not publish credentials, SSH keys, tokens, private data, host-specific secrets, or sensitive development-environment details.

The project aims to acknowledge reports within five business days. Response is best-effort for an independently maintained project without a dedicated security team.

## Security boundaries

Matagi is a Windows-side control plane that reaches development environments over SSH.

- Use the system OpenSSH client and the user's existing SSH configuration and host-key verification. Matagi must not silently weaken `known_hosts` or host verification.
- UI endpoints presented on Windows are transported through managed SSH forwarding and should bind to loopback unless an explicitly governed architecture change authorizes broader exposure.
- Registered remote services may expose privileged development controls. Endpoint discovery and WebView2 navigation must not turn a remote service into an unintended network-facing surface.
- Jinushi remains the process lifecycle authority on managed development hosts. Matagi must not grow an independent remote supervisor with conflicting process state.
- The WebView2 shell is a presentation surface. Preserve navigation and permission restrictions appropriate to displaying trusted registered local/tunneled endpoints.
- SSH command construction, service registration, tunnel allocation, endpoint opening, and remote bootstrap cross trust boundaries and require focused security review.

## Evidence

Portable Go tests, Windows desktop tests, OpenSSH integration, SSH forwarding, and live Jinushi/service behavior prove different boundaries. Report the exact environment and checks actually executed.
