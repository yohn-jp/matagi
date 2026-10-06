# Portable Windows candidate certification

The main-push workflow builds one Windows amd64 `matagi.exe`, records its source commit and SHA-256, and distributes that artifact to hosted Windows test shards. The aggregate rejects missing candidate identity, failed jobs, missing shard evidence, and evidence for different bytes. Only a passing main-push aggregate may publish the downloaded candidate bytes and matching SHA-256 under `0.1.N-dev`. Pull requests build and test without publication.

Hosted Windows evidence is **not** physical or real-host certification. The tests must not depend on a NixOS/development host or credentials. Issue #19 alone owns Windows → real system OpenSSH → development host → Jinushi → Inari/Yokodori real-host certification.

The current portable shards cover executable startup/shutdown and local API/UI presentation, a bounded unknown-service failure path, system OpenSSH configuration lookup, and the navigation policy. They do **not** yet certify a successful UI-to-API-to-OpenSSH-to-fixture lifecycle action or loopback tunnel ownership and cleanup. Until those deterministic fixture scenarios are added and validated on hosted Windows, this work is not complete and must not be merged or treated as release-ready.
