# CLAUDE.md — Silo

Guidance for AI agents working in this repo. Human docs: [README.md](README.md),
[docs/architecture.md](docs/architecture.md). The authoritative spec is
[silo-project-scaffold.md](silo-project-scaffold.md) — read it before implementing.

## What this is

Silo is a **Go** connector giving an agent fleet persistent, versioned,
multi-tenant memory with strict per-project isolation, plus an out-of-band
consolidation cycle (**Distilator**). Module path: `github.com/tooltropolis/silo`.

## Build & test commands

```bash
go build ./...              # compile everything
go vet ./...                # static checks
go test ./...               # unit tests
go test -race ./...         # what CI runs
gofmt -l .                  # must print nothing (CI fails otherwise); gofmt -w . to fix

docker compose -f deploy/docker-compose.yaml up -d   # local deps (SeaweedFS, rqlite x3, Vault)
```

Integration tests need the docker stack up; they're guarded so `go test ./...`
stays green without it.

## Layout

- `cmd/` — `silod` (daemon), `siloctl` (admin CLI), `silo-distil` (Distilator runner),
  `silo-admin` (operator console), `silo-mcp` (exposes a project's memory to an
  agent over MCP — this is how a repo actually connects to Silo).
- `internal/cache` — bbolt local fast path + offline write queue (`LocalCache`).
- `internal/backend` — durable storage (`DurableBackend`); SeaweedFS adapter is default.
- `internal/registry` — tenant registry on rqlite (`TenantRegistry`).
- `internal/kms` — per-project SSE keys on Vault (`KeyManager`).
- `internal/daemon` — CAS write path (`SafeWrite`), leader lock, sync worker.
- `internal/distilator` — consolidation orchestrator + `DistilatorProvider` (Claude-backed).
- `internal/admin` — onboarding (automated) + teardown (manual, per-layer).
- `pkg/client` — agent-facing SDK (`Read`/`Write`/`List`/`Search`).
- `web/dashboard` — v1 read/review web surface.
- `web/admin` — operator console: cache policy, onboarding wizard, teardown.
- `internal/mcpserver` — the MCP tool layer over `pkg/client`.

## Conventions & rules from the spec

- **Interfaces are the contract.** Implement against the signatures as given; if
  one must change, say so explicitly — don't diverge quietly.
- **Build in the spec's order** (Section 8); each step compiles and passes tests
  before the next. Package bodies currently return a not-implemented sentinel.
- **Isolation is the product.** Any change that could cross project boundaries
  needs an explicit isolation test, not just review.
- **Never commit secrets.** The only secrets in-tree are the dev-only throwaway
  values in `configs/example.yaml` and `deploy/docker-compose.yaml`. A gitleaks
  pre-commit hook enforces this (`git config core.hooksPath .githooks`).
- **Standard Go style:** keep it `gofmt`-clean; wrap errors with `%w`; use the
  package sentinel errors (`ErrPreconditionFailed`, `ErrNotFound`, …).

## Git

Don't commit, push, or tag on your own initiative — propose the commands. Use
Conventional Commits (`feat:`/`fix:`/`docs:`/`chore:`); they drive the changelog.

**Branches:** `main` is the **dev** branch (branch from it, merge into it).
`PRD` is **production** — it only ever receives a promotion PR from `main`,
never a direct push. A `.githooks/pre-push` hook refuses direct pushes to `PRD`
(`git config core.hooksPath .githooks` to enable).

- **No `Co-Authored-By` trailer in commits.**
- Reference the Linear issue: put `(NAV-xx)` in the subject and `Ref NAV-xx` in
  the footer (`Ref` links without auto-closing).
