# Onboarding your repo to Silo

How to give one of your repositories its own isolated memory silo, and read and
write that memory. Everything here has been run end to end against the dev
stack.

> **Scope.** Agents reach Silo over the **Model Context Protocol**: `silo-mcp`
> exposes a project's memory as four tools, so any MCP-speaking runtime picks
> them up from a `.mcp.json` entry — see
> [Wiring an agent to Silo](#wiring-an-agent-to-silo-mcp). Steps 1–4 below set up
> the project and show the raw HTTP/Go SDK surface underneath; skip to the MCP
> section if you only want to connect a repo.

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
# Admin credentials — needed ONLY for onboarding/teardown, which create and
# delete buckets. The runtime binaries use the lower-privilege silo-runtime
# identity instead (see step 3).
export SILO_S3_ACCESS_KEY=SILOADMIN
export SILO_S3_SECRET_KEY=SILOADMINSECRET

go build -o ./bin/siloctl ./cmd/siloctl

./bin/siloctl onboard --project=myrepo --vault-token=dev-only-token
```

This provisions four things in one command, rolling back cleanly if any step
fails partway:

| Step | What it creates |
|---|---|
| 1 | Registry record in rqlite (status `active`) |
| 2 | A per-project SSE encryption key in Vault |
| 3 | A versioned S3 bucket, `silo-myrepo` |
| 4 | An S3 credential scoped **Read/Write to that bucket only** |

> **Why the S3 credentials are required:** onboarding creates buckets. Once any
> identity exists in SeaweedFS, anonymous access is disabled cluster-wide — a
> deliberate consequence of the isolation guarantee.

Verify:

```bash
curl -s -G "http://localhost:4001/db/query" \
  --data-urlencode "q=SELECT project_id,status,bucket_name FROM projects" | jq
```

## 3. Run the daemon

The daemon needs only object-level access, so run it as `silo-runtime` — it is
denied bucket creation/deletion by SeaweedFS:

```bash
go build -o ./bin/silod ./cmd/silod

export SILO_RUNTIME_ACCESS_KEY=SILORUNTIME
export SILO_RUNTIME_SECRET_KEY=SILORUNTIMESECRET

./bin/silod \
  --listen 127.0.0.1:8500 \
  --tokens "myrepo-token=myrepo" \
  --cache-dir ./data/cache
```

`--tokens` takes `token=projectID` pairs (comma-separated for several). **A
token resolves to exactly one project** — that is the authorization boundary.
A caller holding `myrepo-token` cannot reach another project's memory no matter
what it puts in the request.

> For anything but a quick local test, mint tokens from the console instead and
> start the daemon with `--rqlite` (see
> [Where the token comes from](#where-the-token-comes-from)). Registry-issued
> tokens are revocable and are stored only as hashes; a `--tokens` value is
> visible in the process list and needs a restart to change.

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

## Wiring an agent to Silo (MCP)

`silo-mcp` exposes one project's memory over the **Model Context Protocol**, so
an agent runtime launches it as a subprocess and calls four tools. No file sync,
no framework hooks, no port opened.

```bash
go build -o ./bin/silo-mcp ./cmd/silo-mcp
```

Add `.mcp.json` to the repo you want to give memory to:

```json
{
  "mcpServers": {
    "silo": {
      "command": "silo-mcp",
      "env": {
        "SILO_TOKEN": "${SILO_TOKEN}",
        "SILO_PROJECT": "myrepo",
        "SILO_DAEMON_ADDR": "http://127.0.0.1:8500"
      }
    }
  }
}
```

`${SILO_TOKEN}` is expanded from your shell at launch, so the token never lands
in a file you might commit — set it with `export SILO_TOKEN=…`. The rest of the
file is safe to check in.

### Where the token comes from

Mint one from the console — the onboarding wizard's **Connect** step, or a
project's page under **Projects → Manage**. It is shown **once**: only a SHA-256
hash is stored, so a leaked registry dump yields nothing usable, and the token
cannot be retrieved afterwards. Lost it? Mint another and revoke the old one.

Give each machine or CI job its own labelled token, so a single leak is revoked
without disturbing anything else.

### Read-only tokens

A token can be minted **read-only** — tick the box on the project page. It reads,
lists, and searches; a write is refused with `403` and `token is read-only`,
enforced in the daemon rather than asked of the agent.

Use one wherever an agent handles input you don't control. A prompt injection can
make an agent *try* to write, and content planted in memory is read back as
trusted fact by every later session; a read-only token makes that impossible
rather than merely discouraged. Reference material an agent should consult but
never edit is the other natural fit.

The wizard's **Connect** step always mints read-write, since a repo's own agents
need to record what they learn. Mint read-only deliberately from the project
page, where the token list shows every token's access alongside its label.

### Capping entry size

A single memory can be capped, so one agent cannot write a 50 MB file that blows
out the local cache and every later read of that path. Set **Max entry** on the
settings page — per project, or on `_fleet` for a default — or start the daemon
with `--max-entry-bytes`.

Precedence is per-project → fleet → flag, so a console value takes effect without
restarting a daemon. It is picked up on the next policy refresh, which shares
`--evict-interval` (5m by default).

**Blank and `0` mean different things.** Blank inherits from the next level down;
`0` is an explicit policy that refuses *every* write to that project, which is
the lockdown to reach for after a leak. `--max-entry-bytes 0` is the exception —
a flag cannot express "explicitly zero", so it reads as unset and a project or
fleet value still applies.

An oversized write is refused with `413` before it reaches the backend, so no
version is created, and the message names both the actual and permitted sizes.
The cap applies during a backend outage too — otherwise an outage would be the
way around it, and the oversized entry would replay into the bucket on recovery.

Prefer many small focused files to a few large ones: a write replaces the whole
file, so a large one is re-sent in full on every update.

### Editing without clobbering

A write replaces the whole file, so two agents editing the same memory means
last-writer-wins — silently. Pass the hash you read back as a precondition and
the write is refused instead:

```bash
# Read returns content_sha256 alongside the content.
HASH=$(curl -s "$API/v1/read?path=memory/notes.md" \
  -H "Authorization: Bearer $TOKEN" | jq -r .content_sha256)

# The write applies only if nothing changed in between; 409 if it did.
curl -X POST "$API/v1/write" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d "{\"path\":\"memory/notes.md\",\"content\":\"$(printf 'edited' | base64)\",
       \"if_content_sha256\":\"$HASH\"}"
```

On `409`, re-read, reapply your change to the new content, and write back with
the hash from *that* read. Retrying the same content would discard the other
change.

The hash of empty content —
`e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`, exported as
`client.HashOfAbsent` — means "create only if nothing exists yet".

Agents get this through `silo_read`'s `content_sha256` and `silo_write`'s
`if_content_sha256`. Omitting the field keeps the old unconditional behaviour.

It is SHA-256 of the content, not the backend's ETag: an ETag is S3-specific and
is not a content hash for multipart uploads, so it would leak the storage layer
into the agent-facing API.

**During a backend outage a conditional write is refused**, because the local
cache may be stale and a hash matching it proves nothing about what is stored.
Unconditional writes still queue normally.

A daemon started with `--rqlite` verifies these without a restart:

```bash
silod --rqlite http://localhost:4001 --listen 127.0.0.1:8500
```

`--tokens` still works and is checked first, which keeps the dev flow going even
if the registry is briefly unreachable. Verified tokens are cached for
`--token-cache-ttl` (default 1m) — that is the window in which a revoked token
still works, so shorten it if you want revocation to bite faster.

Teardown revokes a project's tokens at `revoke-credential`, the first step, so
they cannot outlive the project they address.

Restart the agent and it discovers:

| Tool | What it does |
|---|---|
| `silo_read` | Recall a memory file written in an earlier session |
| `silo_write` | Store a memory file (replaces the whole file, so read first) |
| `silo_list` | Discover what has been remembered for this project |
| `silo_search` | Find a specific fact without reading everything |

**One server serves exactly one project.** The token resolves to one project on
the daemon, so a server holding project A's token cannot reach project B's
memory even if a tool call asks it to. Give a second repo its own project, its
own token, and its own `.mcp.json` entry.

Both properties are covered by an acceptance test
(`internal/mcpserver/e2e_live_test.go`) that runs against a live stack:

```bash
SILO_TEST_TOKEN_A=<project-a-token> SILO_TEST_TOKEN_B=<project-b-token> \
  go test ./internal/mcpserver/ -run TestLive -v
```

It proves memory survives the session that wrote it, and that a second project
cannot read, list, or search the first one's memory. The test skips when no
daemon is reachable, so `go test ./...` stays green without the stack.

### Other integration shapes

Not built, and not needed for an MCP-speaking runtime:

| Approach | Notes |
|---|---|
| **File sync** (`pull` / `push`) | Materialize Silo memory into the repo's agent-memory files before a session and write changes back after. Works with tooling that cannot speak MCP. |
| **Framework hooks** | Where a framework exposes session-start/stop hooks, useful for one-directional sync. Cannot serve reads mid-session. |

---

## Redacting a leaked secret

A credential written into memory is in that object version forever, and the
`Instructions` warning against storing secrets is persuasion, not a control.
The console removes one version permanently: **Projects → a project → Redact a
leaked version**, or `/redact?project=<id>`.

Pick the path, pick a historical version, give a reason, and type the version ID
to confirm. The content is destroyed; the rest of the history is untouched.

Three things worth knowing before you use it:

- **The current version cannot be redacted.** Removing it would silently revert
  the path to older content. Write a clean replacement first, then redact the
  version that leaked.
- **It is not reversible and there is no tombstone.** SeaweedFS deletes a
  version outright — there is no way to blank one in place — so the bytes really
  are gone. That is the point when a credential leaks.
- **The audit survives the content.** Who redacted what, when, and why is
  recorded in rqlite, not beside the object, so destroying the object cannot
  destroy the evidence. Those rows are never deleted.

Rotate the leaked credential regardless. Redaction removes it from Silo; it does
nothing about wherever else it has already been read.

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
| `Vault is sealed` / `connection refused` on 8201 | Re-run `deploy/bootstrap-dev.sh`; Vault seals on restart. |
| `leader not found` from rqlite | Cluster still forming — wait ~15s after `up`. If it persists, check all three `rqlite` containers are running. |
| SDK returns `ErrUnauthorized` | Token isn't in the daemon's `--tokens` map. |
| SDK returns `ErrReadOnly` / daemon returns `403 token is read-only` | The token was minted read-only. It is valid — retrying will not help. Mint a read-write token if the write needs to persist. |
| SDK returns `ErrTooLarge` / daemon returns `413 entry exceeds the size limit` | The content is over the project's per-entry cap. Split it across several paths, or raise **Max entry** on the settings page. |
| SDK returns `ErrConflict` / daemon returns `409 content hash precondition failed` | The path changed since you read it. Re-read, reapply your edit, and write back with the new `content_sha256` — do not retry the same content. |
| SDK returns `ErrNotFound` for a path you wrote | Writing under a different project's token — tokens are scoped to one project. |
| Distilator: `is a credential configured?` | Run `ant auth login`, then `ant auth status` to confirm the active source. |
