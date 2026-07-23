# Onboarding your repo to Silo

How to give one of your repositories its own isolated memory silo, and read and
write that memory. Everything here has been run end to end against the dev
stack.

> **Scope — read this first.** There is **no Claude Code (or other framework)
> adapter yet**. `pkg/client/adapters/` is empty; the spec parks per-framework
> adapters there as "added as built," and none have been. Nothing in your agent
> framework will automatically read or write Silo memory today.
>
> What this guide gets you: a real, isolated Silo project for your repo, plus
> read/write/list/search access over HTTP or the Go SDK. Wiring that into an
> agent framework is a separate piece of work — see
> [Integrating with an agent framework](#integrating-with-an-agent-framework).

---

## Prerequisites

- Go 1.26+, Docker
- The dev stack running and bootstrapped (below)

Throughout, replace `myrepo` with your project ID. Use something stable and
DNS-safe — it becomes the bucket name (`silo-myrepo`).

---

## 1. Start the stack

```bash
cd /path/to/silo

docker compose -f deploy/docker-compose.yaml up -d
deploy/bootstrap-dev.sh
```

> **Re-run `bootstrap-dev.sh` after every `up`.** Vault runs in server mode with
> file storage, so it **seals on restart** — the script unseals it and is
> idempotent. It also provisions the `silo-admin` S3 identity. Skipping it
> leaves Vault sealed and onboarding will fail.

Data persists across `docker compose down`. Use `down -v` for a clean slate.

## 2. Onboard the project

```bash
export SILO_S3_ACCESS_KEY=SILOADMIN
export SILO_S3_SECRET_KEY=SILOADMINSECRET

go build -o ./bin/siloctl ./cmd/siloctl

./bin/siloctl onboard --project=myrepo \
  --vault-token=dev-only-token \
  --weed-binary=./deploy/weed-docker.sh
```

This provisions four things in one command, rolling back cleanly if any step
fails partway:

| Step | What it creates |
|---|---|
| 1 | Registry record in rqlite (status `active`) |
| 2 | A per-project SSE encryption key in Vault |
| 3 | A versioned S3 bucket, `silo-myrepo` |
| 4 | An S3 credential scoped **Read/Write to that bucket only** |

> **Why `--weed-binary`:** credential issuance shells out to `weed`, which ships
> inside the SeaweedFS container rather than on your host.
> `deploy/weed-docker.sh` wraps `docker exec`. Omitting it fails with
> `executable file not found in $PATH`.

> **Why the S3 credentials are required:** onboarding creates buckets. Once any
> identity exists in SeaweedFS, anonymous access is disabled cluster-wide — a
> deliberate consequence of the isolation guarantee.

Verify:

```bash
curl -s -G "http://localhost:4001/db/query" \
  --data-urlencode "q=SELECT project_id,status,bucket_name FROM projects" | jq
```

## 3. Run the daemon

```bash
go build -o ./bin/silod ./cmd/silod

./bin/silod \
  --listen 127.0.0.1:8500 \
  --tokens "myrepo-token=myrepo" \
  --cache-dir ./data/cache
```

`--tokens` takes `token=projectID` pairs (comma-separated for several). **A
token resolves to exactly one project** — that is the authorization boundary.
A caller holding `myrepo-token` cannot reach another project's memory no matter
what it puts in the request.

> Use a real secret in anything but local dev. These tokens are the only thing
> standing between a caller and a project's memory.

For same-machine agents, pass a path instead of `host:port` to listen on a Unix
socket: `--listen /var/run/silo.sock`.

## 4. Read and write memory

The daemon exposes four endpoints under `/v1`. Every request needs
`Authorization: Bearer <token>`. `content` is **base64-encoded** in JSON.

```bash
TOKEN=myrepo-token
API=http://127.0.0.1:8500

# Write
curl -X POST "$API/v1/write" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d "{\"path\":\"memory/conventions.md\",\"content\":\"$(printf 'Use tabs. Run gofmt before committing.' | base64)\"}"

# Read
curl -s "$API/v1/read?path=memory/conventions.md" \
  -H "Authorization: Bearer $TOKEN" | jq -r '.content' | base64 -d

# List paths under a prefix
curl -s "$API/v1/list?prefix=memory/" -H "Authorization: Bearer $TOKEN" | jq

# Search content
curl -s "$API/v1/search?prefix=memory/&q=gofmt" -H "Authorization: Bearer $TOKEN" | jq
```

### From Go

```go
import "github.com/tooltropolis/silo/pkg/client"

c, err := client.New(client.Config{
    Endpoint: "http://127.0.0.1:8500", // or "/var/run/silo.sock"
    Token:    os.Getenv("SILO_TOKEN"),
})
if err != nil { return err }

if err := c.Write(ctx, "memory/conventions.md", []byte("...")); err != nil { return err }

content, err := c.Read(ctx, "memory/conventions.md")
switch {
case errors.Is(err, client.ErrNotFound):
    // no memory at that path yet
case err != nil:
    return err
}
```

Writes go through the daemon's CAS path, so every write is versioned and
concurrent writers retry rather than silently overwriting.

## 5. Browse it

```bash
go build -o ./bin/silo-dashboard ./cmd/silo-dashboard
./bin/silo-dashboard --listen 127.0.0.1:8600
```

Then <http://127.0.0.1:8600> — registry, memory browser with version history,
and Distilator review. Read-only except approving a Distilator proposal.

## 6. Consolidate memory (optional)

Once the project has captured session transcripts under `_sessions/`, the
Distilator can propose refinements:

```bash
go build -o ./bin/silo-distil ./cmd/silo-distil

./bin/silo-distil run     --project=myrepo --since=24h
./bin/silo-distil show    --project=myrepo --run-id=<id>
./bin/silo-distil promote --project=myrepo --run-id=<id> --paths=memory/conventions.md
```

A run **never modifies the live store** — proposals land in
`_distilations/<run-id>/` and only reach memory when a human promotes them.

The Claude provider authenticates through the Go SDK's standard credential
chain. For subscription-based OAuth with **no API key**, run `ant auth login`
once. A stale exported `ANTHROPIC_API_KEY` silently takes precedence over the
OAuth profile — `ant auth status` shows which source is active.

---

## Integrating with an agent framework

**Not built yet.** The SDK exists; nothing calls it automatically. Options, in
rough order of how well they fit:

| Approach | Notes |
|---|---|
| **MCP server** wrapping the daemon API | Likely the best fit — exposes read/write/list/search as tools an agent calls naturally. Needs a new server binary. |
| **File sync** (`pull` / `push`) | Materialize Silo memory into the repo's agent-memory files before a session and write changes back after. Least elegant, but works with today's tooling and needs no new protocol. |
| **Framework hooks** | Where the framework exposes session-start/stop hooks, useful for one-directional sync. Cannot serve reads mid-session. |

These are design sketches, **not verified against current framework APIs**.
Confirm the integration points before building against them.

---

## Decommissioning

Teardown is deliberately manual — one confirmed layer per invocation, in order.
There is no flag that runs all four:

```bash
./bin/siloctl teardown --project=myrepo --step=revoke-credential
./bin/siloctl teardown --project=myrepo --step=revoke-key
./bin/siloctl teardown --project=myrepo --step=delete-bucket   # IRREVERSIBLE
./bin/siloctl teardown --project=myrepo --step=deregister
```

Reversible steps prompt `[y/N]`. **`delete-bucket` requires typing the project
ID** — a reflexive "y" must not be able to destroy every version of a project's
memory. `--yes` skips prompts for scripted use but never the
one-step-per-invocation rule.

---

## Troubleshooting

| Symptom | Cause |
|---|---|
| `create bucket: 403 AccessDenied` | `SILO_S3_ACCESS_KEY`/`SECRET_KEY` unset, or `bootstrap-dev.sh` not run. |
| `issue credential: exec "weed": not found` | Missing `--weed-binary=./deploy/weed-docker.sh`. |
| `Vault is sealed` / `connection refused` on 8201 | Re-run `deploy/bootstrap-dev.sh`; Vault seals on restart. |
| `leader not found` from rqlite | Cluster still forming — wait ~15s after `up`. If it persists, check all three `rqlite` containers are running. |
| SDK returns `ErrUnauthorized` | Token isn't in the daemon's `--tokens` map. |
| SDK returns `ErrNotFound` for a path you wrote | Writing under a different project's token — tokens are scoped to one project. |
| Distilator: `is a credential configured?` | Run `ant auth login`, then `ant auth status` to confirm the active source. |
