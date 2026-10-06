# Portable Windows candidate certification

The main-push workflow builds one Windows amd64 `matagi.exe`, records its source commit and SHA-256, and distributes that artifact to hosted Windows test shards. The aggregate rejects missing candidate identity, failed jobs, missing shard evidence, and evidence for different bytes. Only a passing main-push aggregate may publish the downloaded candidate bytes and matching SHA-256 under `0.1.N-dev`. Pull requests build and test without publication.

Hosted Windows evidence is **not** physical or real-host certification. The tests do not depend on a NixOS/development host or credentials. Issue #19 alone owns Windows → real system OpenSSH → development host → Jinushi → Inari/Yokodori real-host certification.

The portable shards cover production executable startup/shutdown, local API/UI presentation, bounded failure presentation, system OpenSSH configuration lookup, navigation policy, and a successful lifecycle/tunnel scenario. The lifecycle shard starts the exact candidate with a validated fixture registry and an executable-compatible deterministic SSH boundary fixture. It proves Matagi emits the expected Jinushi and `ssh -N -T -o ExitOnForwardFailure=yes -L ...` operations, establishes a loopback-only tunnel to a local fixture endpoint, exercises start/restart/stop through the UI/API surface, and proves Matagi-owned tunnel cleanup after stop.

The deterministic fixture is hosted-runner evidence for Matagi's Windows orchestration boundary only. It is not evidence that a real OpenSSH server, Jinushi installation, development host, Inari, or Yokodori behaves correctly; that evidence remains exclusively in #19.
