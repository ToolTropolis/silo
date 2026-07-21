# Contributing to Silo

Thanks for your interest in Silo — a pluggable Go connector for persistent,
versioned, multi-tenant agent memory. This guide covers how to propose a change.

## Ground rules from the spec

The design is captured in [`silo-project-scaffold.md`](silo-project-scaffold.md)
and mirrored in [`docs/architecture.md`](docs/architecture.md). A few things the
spec locks in that contributors should respect:

- **The interfaces in the spec are the contract.** Implement against the
  signatures as given (`DurableBackend`, `LocalCache`, `TenantRegistry`,
  `KeyManager`, `DistilatorProvider`, `client.Client`). If a signature needs to
  change, say so explicitly in the PR rather than diverging quietly.
- **Build in the order the spec gives.** Each step should be a working, tested
  increment.
- **Isolation is the product.** Any change that could let one project reach
  another project's data needs an explicit isolation test, not just review.

## Development setup

Prerequisites: **Go 1.26+**, Docker (for the local dependency stack).

```bash
# Clone and enter the repo, then:
go mod download

# Stand up the local dependencies (SeaweedFS, 3-node rqlite, Vault dev mode):
docker compose -f deploy/docker-compose.yaml up -d
```

## Build, test, and lint

These are the exact commands CI runs — run them before opening a PR:

```bash
go build ./...     # compile every package
go vet ./...       # static checks
go test ./...      # unit tests (integration tests need the docker stack up)
gofmt -l .         # must print nothing; run `gofmt -w .` to fix
```

Integration tests that need the local stack are guarded so `go test ./...` stays
green without Docker; run them with the stack up per the spec's build sequence.

## Proposing a change

1. **Open an issue first** for anything non-trivial (see the
   [issue templates](.github/ISSUE_TEMPLATE/)) so the approach can be discussed
   before you invest in it.
2. Fork, branch from `main`, and keep the change focused on one increment.
3. Make sure `go build`, `go vet`, `go test`, and `gofmt -l` are all clean.
4. Use clear commit messages; [Conventional Commits](https://www.conventionalcommits.org/)
   (`feat:`, `fix:`, `docs:`, `chore:`) are preferred — they drive the changelog.
5. Update `CHANGELOG.md` under `[Unreleased]` for user-facing changes.
6. Open a PR describing what changed and why, and link the issue.

## Security

Never send security issues through a public issue or PR — follow
[`SECURITY.md`](SECURITY.md). Never commit real credentials, Vault tokens, or
SeaweedFS keys; the dev-only values in `configs/example.yaml` and the compose
file are the only secrets that belong in the tree, and only because they are
throwaway local-dev values.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
