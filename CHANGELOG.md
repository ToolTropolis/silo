# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Project scaffold to the OpenSSF OSPS Baseline (Level 2): Go module layout,
  core interfaces (`DurableBackend`, `LocalCache`, `TenantRegistry`,
  `KeyManager`, `DistilatorProvider`, `client.Client`), the daemon `SafeWrite`
  CAS write path, and the three command binaries (`silod`, `siloctl`,
  `silo-distil`).
- Local development stack via `deploy/docker-compose.yaml` (SeaweedFS, a 3-node
  rqlite cluster, and Vault in dev mode) with dev-only `configs/example.yaml`.
- Governance & CI: `LICENSE` (Apache-2.0), `SECURITY.md` with a coordinated
  disclosure policy, `CONTRIBUTING.md`, secret-scanning pre-commit hook, and a
  CI workflow running `go build` / `go vet` / `go test` / `gofmt`.

[Unreleased]: https://github.com/tooltropolis/silo/commits/main
