<img src="docs/brand/logo-wordmark.svg" alt="Silo" height="56">

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

> **Status: v1 complete.** All seven build-sequence steps are implemented and
> verified against a live stack, and the v1 definition-of-done checklist
> (spec §10) passes 14/14 — including leader-failover, a real backend outage
> with no data loss, and a proven cross-project isolation boundary. See
> [`docs/architecture.md`](docs/architecture.md).

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
cmd/          silod (daemon), siloctl (admin CLI), silo-distil, silo-dashboard
internal/     cache, backend, registry, kms, daemon, transcript, distilator, admin
pkg/client/   agent-facing SDK (Read/Write/List/Search) + framework adapters
web/dashboard v1 read/review web surface
configs/      example.yaml (dev-only defaults)
deploy/       docker-compose.yaml (SeaweedFS + 3-node rqlite + Vault) + migrations
docs/         architecture.md
```

## Quickstart

```bash
git clone https://github.com/ToolTropolis/silo.git && cd silo
deploy/demo.sh
```

Starts the stack, creates an isolated project, runs the daemon, and writes and
reads back a memory file — printing every command as it goes. See
[**docs/QUICKSTART.md**](docs/QUICKSTART.md) for the same thing by hand.

Already have a Silo deployment and just need the client binaries?
[**docs/installing.md**](docs/installing.md) covers the installer, `go install`,
and building from source.

## Requirements

- **Go 1.26+**
- **Docker** (for the local dependency stack)
- **`weed`** (SeaweedFS CLI) — *only* to onboard a project, which issues its
  bucket-scoped S3 credential via `weed shell`. `brew install seaweedfs`, or run it
  inside the dev-stack container (see [docs/QUICKSTART.md](docs/QUICKSTART.md#prerequisites)).
  Serving memory never needs it.

## Running the stack locally

```bash
# 1. Stand up local dependencies: SeaweedFS, a 3-node rqlite cluster, Vault (dev).
docker compose -f deploy/docker-compose.yaml up -d

# 2. Initialize/unseal Vault and provision the silo-admin S3 identity.
#    Idempotent — re-run after every `up` (Vault seals on restart).
deploy/bootstrap-dev.sh

# 3. Build everything.
go build ./...

# 4. Run the tests (integration tests skip cleanly if the stack isn't up).
go test ./...
```

The local defaults in [`configs/example.yaml`](configs/example.yaml) point at
the compose services. **These are dev-only values** — production config comes
from environment variables or a secrets manager, never a checked-in file.

> **Data persists.** Every service writes to a named Docker volume, so the
> registry, memory objects, and per-project encryption keys survive
> `docker compose down` and container restarts. To wipe the stack and start
> clean, use `docker compose -f deploy/docker-compose.yaml down -v`.
>
> Vault runs in **server mode with file storage**, not `-dev`. Dev mode keeps
> everything in memory, so a plain `docker restart` silently destroyed every
> per-project SSE key while the registry still referenced it. The tradeoff is
> that Vault seals on restart — `deploy/bootstrap-dev.sh` unseals it and is
> safe to re-run.

> **Isolation note.** Silo's guarantee is per-project isolation: onboarding
> issues each project a SeaweedFS identity scoped Read/Write to *only* its own
> bucket, so one project's credential is denied (403) on another's bucket. A
> consequence is that **once any identity exists, SeaweedFS disables anonymous
> access cluster-wide** — so Silo's own components authenticate. Two identities
> are provisioned, least privilege: **`silo-admin`** (bucket lifecycle, used
> *only* by `siloctl onboard`/`teardown`) and **`silo-runtime`** (object CRUD
> only, used by `silod`/`silo-dashboard`/`silo-distil`). SeaweedFS denies
> `silo-runtime` bucket creation and deletion, so a compromised daemon or
> dashboard cannot destroy a project's bucket. The isolation guarantee is
> covered by an integration test that drives one project's credential against
> another's bucket and asserts it's refused.

## Onboarding your own repo

To give one of your repositories its own isolated memory silo — provisioning,
running the daemon, and reading/writing its memory — follow
[`docs/onboarding-a-repo.md`](docs/onboarding-a-repo.md).

Note that there is **no agent-framework adapter yet**: the SDK exists, but
nothing in Claude Code (or any other framework) calls it automatically. The
guide covers what works today and sketches the integration options.

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

### Decommissioning a project

Teardown is deliberately **manual, one confirmed layer at a time** — there is no
flag that runs all four steps:

```bash
siloctl teardown --project=proj-11 --step=revoke-credential  # 1. revoke S3 access
siloctl teardown --project=proj-11 --step=revoke-key         # 2. destroy the SSE key
siloctl teardown --project=proj-11 --step=delete-bucket      # 3. IRREVERSIBLE
siloctl teardown --project=proj-11 --step=deregister         # 4. remove the record
```

Ordering is enforced against the registry record, so a step can't be skipped or
replayed — access is revoked before data is destroyed. The reversible steps
prompt `[y/N]`; **`delete-bucket` requires typing the project ID**, because a
reflexive "y" shouldn't be able to destroy every version of a project's memory.
`--yes` skips the prompt for scripted use but never bypasses the one-step-per-
invocation rule. The record moves to `decommissioning` after step 1 and
`decommissioned` after step 4, so an interrupted teardown is visible in the
registry and the dashboard.

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

## Verifying a release

Release assets ship with a SHA-256 manifest (`SHA256SUMS.txt`) signed by
[cosign](https://docs.sigstore.dev/) keyless signing. There is no public key to
distribute: the signature is tied to the workflow that produced it, and is
recorded in the Rekor transparency log.

```bash
# 1. Verify the manifest signature came from this repo's release workflow.
cosign verify-blob \
  --signature SHA256SUMS.txt.sig \
  --certificate SHA256SUMS.txt.pem \
  --certificate-identity-regexp '^https://github.com/ToolTropolis/silo/\.github/workflows/release\.yaml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS.txt

# 2. Verify the binaries match the (now-trusted) manifest.
sha256sum -c SHA256SUMS.txt
```

Step 1 without step 2 proves the manifest is authentic but says nothing about
the binary you downloaded; step 2 without step 1 proves the binary matches a
manifest that anyone could have written. Run both.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md). Report security issues privately per
[`SECURITY.md`](SECURITY.md).

## License

[Apache License 2.0](LICENSE).
