# Silo

**Silo** is a pluggable Go connector that gives any repo's agent fleet
**persistent, versioned, multi-tenant memory** with an out-of-band consolidation
cycle called **Distilator**.

The two names tell one story:

- **Silo** — strict per-project isolation. Bucket-per-project, per-project scoped
  credentials, per-project encryption keys. Each project's memory lives in its
  own silo; nothing leaks between them.
- **Distilator** — the out-of-band consolidation job that runs *inside* each
  Silo. It reads a project's own sessions and its own memory store and refines
  them, never touching another project's data because it never has access to it.

> **Status: v1 in progress.** This repository is scaffolded to its target
> architecture — every package and interface from the spec is in place, with the
> implementation being filled in per the build sequence in
> [`docs/architecture.md`](docs/architecture.md). Package bodies currently return
> a not-implemented sentinel; the module builds, vets, and tests green.

## Architecture at a glance

Three zones (full diagram and narrative in
[`docs/architecture.md`](docs/architecture.md)):

- **Runtime read/write path** — agents call the `pkg/client` SDK → local daemon
  (bbolt cache + leader lock + write queue) → tenant registry (rqlite) / KMS
  (Vault) / durable backend (SeaweedFS, bucket-per-project, versioned, ETag CAS).
- **Async Distilator pipeline** — SeaweedFS → Distilator job → separate output
  store → human review & promote.
- **Ops / admin** — web dashboard (browse versions, audit, review proposals) and
  a deliberately manual, per-layer teardown flow.

| Concern | Choice |
|---|---|
| Local cache | bbolt, one instance per project |
| Durable backend | SeaweedFS (S3-compatible), bucket-per-project, versioned |
| Concurrency | ETag conditional writes (`If-Match`) with retry |
| Isolation | bucket + scoped IAM credential + per-project SSE key |
| Key management | Vault (self-hosted KMS) |
| Tenant registry | rqlite (SQLite semantics + Raft HA) |
| Agent access | Go SDK (`pkg/client`) in v1; FUSE mount tracked for v2 |

## Repository layout

```
cmd/          silod (daemon), siloctl (admin CLI), silo-distil (Distilator runner)
internal/     cache, backend, registry, kms, daemon, transcript, distilator, admin
pkg/client/   agent-facing SDK (Read/Write/List/Search) + framework adapters
web/dashboard v1 read/review web surface
configs/      example.yaml (dev-only defaults)
deploy/       docker-compose.yaml (SeaweedFS + 3-node rqlite + Vault) + migrations
docs/         architecture.md
```

## Requirements

- **Go 1.26+**
- **Docker** (for the local dependency stack)

## Running the stack locally

```bash
# 1. Stand up local dependencies: SeaweedFS, a 3-node rqlite cluster, Vault (dev).
docker compose -f deploy/docker-compose.yaml up -d

# 2. Provision the silo-admin S3 identity the backend authenticates as.
deploy/bootstrap-dev.sh

# 3. Build everything.
go build ./...

# 4. Run the tests (integration tests skip cleanly if the stack isn't up).
go test ./...
```

The local defaults in [`configs/example.yaml`](configs/example.yaml) point at
the compose services. **These are dev-only values** — production config comes
from environment variables or a secrets manager, never a checked-in file.

> **Isolation note.** Silo's guarantee is per-project isolation: onboarding
> issues each project a SeaweedFS identity scoped Read/Write to *only* its own
> bucket, so one project's credential is denied (403) on another's bucket. A
> consequence is that **once any identity exists, SeaweedFS disables anonymous
> access cluster-wide** — so Silo's own components authenticate as the
> `silo-admin` identity (`deploy/bootstrap-dev.sh`). The isolation guarantee is
> covered by an integration test that drives one project's credential against
> another's bucket and asserts it's refused.

Once the daemon and Distilator are implemented, the end-to-end loop to exercise
by hand (per the v1 definition of done) is:

```bash
siloctl onboard --project=proj-11       # provision bucket + key + registry entry
# ... agents Read/Write via pkg/client against the daemon ...
silo-distil run --project=proj-11 --since=24h   # propose consolidated changes
# ... review & promote in the dashboard ...
```

See [`docs/architecture.md`](docs/architecture.md) for the full build sequence
and the v1 acceptance checklist.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md). Report security issues privately per
[`SECURITY.md`](SECURITY.md).

## License

[Apache License 2.0](LICENSE).
