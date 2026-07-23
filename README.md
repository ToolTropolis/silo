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

## Exercising the full cycle by hand

The end-to-end loop from the v1 definition of done. Everything below is
implemented and verified against the dev stack.

```bash
# S3 admin credentials, from deploy/bootstrap-dev.sh. Onboarding creates
# buckets, and anonymous access is disabled once any identity exists.
export SILO_S3_ACCESS_KEY=SILOADMIN SILO_S3_SECRET_KEY=SILOADMINSECRET

# 1. Provision an isolated project: bucket + per-project SSE key + registry
#    record + a credential scoped Read/Write to that bucket only.
#    --weed-binary is needed because credential issuance shells out to `weed`,
#    which lives in the SeaweedFS container rather than on the host.
siloctl onboard --project=proj-11 --vault-token=dev-only-token \
  --weed-binary=./deploy/weed-docker.sh

# 2. Agents read/write memory through the SDK against a running daemon.
silod --listen 127.0.0.1:8500 --tokens "agent-token=proj-11"

# 3. Propose consolidated changes from the project's captured sessions.
#    Writes to _distilations/<run-id>/ — the live store is NOT modified.
silo-distil run --project=proj-11 --since=24h

# 4. Review the proposals, then promote only the ones you approve. Promotion
#    goes through the daemon's CAS write path, tagged promoted_from:<run-id>.
silo-distil show    --project=proj-11 --run-id=<id>
silo-distil promote --project=proj-11 --run-id=<id> --paths=memory/conventions.md

# ...or review and approve in the browser instead:
silo-dashboard --listen 127.0.0.1:8600   # http://127.0.0.1:8600
```

### Dashboard

`silo-dashboard` serves the v1 read/review surface — the tenant registry, a
memory version browser, and Distilator proposal review with side-by-side diffs.

It is **read-only except for one action**: promoting an approved Distilator
proposal, which routes through the daemon's CAS write path. Teardown is never
exposed in the UI — that stays in `siloctl`'s confirmed per-layer CLI flow. The
registry view renders credential and key **references only**, never secrets.

The Distilator authenticates to Claude via the Go SDK's standard credential
chain. For subscription-based OAuth with **no API key**, run `ant auth login`
once ([Anthropic CLI](https://platform.claude.com/docs/en/api/sdks/cli)); the
zero-argument client picks the profile up automatically. A stale exported
`ANTHROPIC_API_KEY` silently takes precedence over the profile — `ant auth
status` shows which credential source is active.

See [`docs/architecture.md`](docs/architecture.md) for the full build sequence
and the v1 acceptance checklist.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md). Report security issues privately per
[`SECURITY.md`](SECURITY.md).

## License

[Apache License 2.0](LICENSE).
