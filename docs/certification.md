# Issue #10 production certification

Status: **REAL_HOST_CERTIFICATION_BLOCKED**. Issue #10 remains open. The implementation environment is Linux and does not have the user's Windows/WebView2 session or access to the development host. Portable tests are not Windows-to-host evidence. This document does not claim a successful run.

A future candidate can be accepted only after recording all of the following from the **same exact production binary revision**:

- Matagi candidate commit SHA and production `matagi.exe` build invocation/hash;
- Windows version, system OpenSSH executable path and `ssh -V` output;
- non-secret SSH host alias and remote identity; user-managed host-key verification intact;
- Jinushi version/revision and explicit detection or bootstrap readiness result;
- actual Inari and Yokodori registry definitions used (redact secrets, never persist keys);
- start, status, restart, stop evidence for both services through the production Matagi/Jinushi path;
- each Matagi-owned Windows IPv4 loopback tunnel URL/port and HTTP result for both product UIs;
- independent connectivity, Jinushi, readiness, and tunnel failure observations;
- shutdown and restart evidence showing no Matagi tunnel leaks and no unrelated process termination;
- CI/governance/security check links and exact SHA on which they passed.

Do not substitute the repository's deterministic fixture SSH server or package tests for this real-host run. Do not mark Issue #10 complete without a successful candidate run.
