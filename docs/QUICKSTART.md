# Quickstart

From a fresh clone to storing and reading a piece of agent memory — either in
one command, or step by step.

**What you'll have at the end:** a running Silo stack, one isolated project, and
a memory file you wrote and read back through the API.

> This is the *local dev* stack. It uses throwaway credentials and disabled TLS
> — never point it at anything real. See the [README](../README.md) for what
> production would need instead.

---

## Prerequisites

| Need | Check with | If missing |
|---|---|---|
| Go 1.26+ | `go version` | [go.dev/dl](https://go.dev/dl/) |
| Docker | `docker ps` | Docker Desktop, and make sure it's *running* |
| `jq` (for the examples) | `jq --version` | `brew install jq` |
| `weed` (onboarding only) | `weed version` | `brew install seaweedfs`, or see the note below |

**About `weed`.** Onboarding a project issues a *per-project S3 credential*, scoped
`Read,Write` to that project's bucket alone — the isolation boundary Silo enforces.
Creating that identity is not an S3 API call; it goes through SeaweedFS's own admin
channel (`weed shell` → `s3.configure`), so `siloctl onboard` needs the `weed` binary.
Reads and writes never touch it, so a daemon serving memory does not need it at all.

Against the Docker dev stack there is a wrinkle: SeaweedFS advertises its
*container* address (e.g. `172.24.0.2:9333`), which a `weed` running on your host
cannot route to — it will hang rather than fail. So run it inside the container:

```sh
SW=$(docker compose -f deploy/docker-compose.yaml ps -q seaweedfs)
docker exec -i "$SW" weed shell -master=localhost:9333
```

`deploy/demo.sh` already does the equivalent, which is why the demo path needs no
`weed` on your PATH. Install it natively when you point Silo at a SeaweedFS cluster
whose advertised addresses you can actually reach.

---

## Get the repo

Both paths below start here:

```bash
git clone https://github.com/ToolTropolis/silo.git
cd silo
```

<details>
<summary>Don't want to clone? (no Go toolchain needed either)</summary>

Silo runs on SeaweedFS, rqlite, and Vault, so you need the compose file that
defines them — but not the whole repo. Five files from `deploy/` are enough:

```bash
mkdir -p silo-demo/deploy && cd silo-demo
BASE=https://raw.githubusercontent.com/ToolTropolis/silo/main/deploy
for f in docker-compose.yaml bootstrap-dev.sh demo.sh vault.hcl weed-docker.sh; do
  curl -fsSL "$BASE/$f" -o "deploy/$f"
done
chmod +x deploy/*.sh
```

With no sources present, `demo.sh` fetches the **released** binaries instead of
building them — checksum-verified against the signed manifest. Same demo, no
clone, no Go. The fast path below then works unchanged; the manual path needs the
sources, so clone for that one.

</details>

---

## The fast path

```bash
deploy/demo.sh
```

Starts the stack, bootstraps Vault, creates an isolated project, runs the daemon,
then **writes and reads back a memory file** — printing every command it runs, so
nothing is hidden. About a minute, most of it Docker.

At the end you get a dashboard URL where that file already has two versions.
Tear it down with `deploy/demo.sh --down`.

---

## The manual path

**Prefer to type it yourself?** The rest of this page is the same sequence by
hand — six steps, two terminals, ~5 minutes. Worth doing once: it's what you'd
actually run against a real deployment, where none of the dev-stack defaults
apply.

## 1. Start the stack (~1 min)

```bash
docker compose -f deploy/docker-compose.yaml up -d
```

That starts five containers: SeaweedFS (object storage), three rqlite nodes (the
tenant registry, with Raft), and Vault (per-project encryption keys).

Give them ~15 seconds, then confirm all five are up:

```bash
docker ps --format '{{.Names}}\t{{.Status}}' | grep deploy-
```

## 2. Bootstrap (~30 s)

```bash
deploy/bootstrap-dev.sh
```

This initializes and unseals Vault and provisions two S3 identities. It prints
the credentials it created.

> **Re-run this after every `docker compose up`.** Vault uses file storage, so it
> **seals on restart** — the script unseals it and is safe to run repeatedly.
> If you skip it, step 3 fails with `Vault is sealed`.

## 3. Create a project (~10 s)

Each project gets its own bucket, its own encryption key, and its own credential
scoped to that bucket alone. That isolation is the whole point of Silo.

```bash
go build -o ./bin/siloctl ./cmd/siloctl

./bin/siloctl onboard --project=quickstart
```

Expect: `onboarded project "quickstart": bucket, per-project key, registry record, and credential provisioned.`

> **Where did the credentials go?** When every endpoint is loopback, `siloctl`
> fills in the dev stack's throwaway Vault token, S3 keys, and the
> `deploy/weed-docker.sh` wrapper (issuing a scoped credential shells out to
> `weed`, which lives *inside* the container). Point any endpoint at a
> non-local host and those defaults switch off — a real deployment always
> states its own credentials.

## 4. Run the daemon

The daemon is what agents talk to. It only needs object access, so it runs as
the lower-privilege `silo-runtime` identity — it cannot create or delete buckets.

```bash
go build -o ./bin/silod ./cmd/silod

./bin/silod --listen 127.0.0.1:8500 --tokens "demo-token=quickstart"
```

Expect `silod: listening on 127.0.0.1:8500 (1 token(s))`. The daemon binds before
printing that, so if you see it, it really is up.

Leave it running and open a second terminal for the next step.

`--tokens` maps `token=projectID`. **A token resolves to exactly one project** —
that's the authorization boundary.

## 5. Write and read memory

In your second terminal:

```bash
TOKEN=demo-token
API=http://127.0.0.1:8500

# Write. Content is base64 in the JSON body.
curl -s -X POST "$API/v1/write" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d "{\"path\":\"memory/notes.md\",\"content\":\"$(printf 'Prefers tabs. Always run gofmt before committing.' | base64)\"}"

# Read it back.
curl -s "$API/v1/read?path=memory/notes.md" \
  -H "Authorization: Bearer $TOKEN" | jq -r '.content' | base64 -d
```

You should see your text printed back. **That's the core loop** — an agent
writing memory that persists, versioned, in an isolated store.

Also try:

```bash
curl -s "$API/v1/list?prefix=memory/" -H "Authorization: Bearer $TOKEN" | jq
curl -s "$API/v1/search?prefix=memory/&q=gofmt" -H "Authorization: Bearer $TOKEN" | jq
```

## 6. See it in the browser

```bash
go build -o ./bin/silo-dashboard ./cmd/silo-dashboard
./bin/silo-dashboard --listen 127.0.0.1:8600
```

Open <http://127.0.0.1:8600>:

- **Registry** — your project, its status, and credential/key *references* (never secrets)
- **Memory** — browse `memory/notes.md` and its version history
- **Distilations** — Distilator proposals awaiting review (empty for now)

Write to the same path again, then reload the memory view — you'll see a second
version. Every write is versioned; nothing is overwritten in place.

---

## What just happened

```
your curl  →  silod  →  bbolt cache (local fast path)
                     →  SeaweedFS   (durable, versioned, per-project bucket)
                     ↑
              rqlite registry (which bucket/key belongs to this project)
              Vault           (this project's encryption key)
```

Writes go through a compare-and-swap path, so two agents writing the same file
concurrently retry rather than clobbering each other. If SeaweedFS is down,
writes queue locally and sync when it returns.

## Cleaning up

Remove the quickstart project (four confirmed steps — teardown is deliberately
manual and destructive):

```bash
./bin/siloctl teardown --project=quickstart --step=revoke-credential
# ...then --step=revoke-key, --step=delete-bucket, --step=deregister
```

Each step prints the next one, and the order is enforced against the registry —
a step won't run early or twice, so an interrupted teardown resumes safely
rather than stranding a bucket. To see where a project stands:

```bash
./bin/siloctl status
```

`delete-bucket` makes you **type the project ID** (not `y`) — a reflexive "y"
shouldn't be able to destroy every version of a project's memory.

Or stop the whole stack:

```bash
docker compose -f deploy/docker-compose.yaml down     # data survives
docker compose -f deploy/docker-compose.yaml down -v  # wipe everything
```

## Where next

| Want to… | Go to |
|---|---|
| Set this up for one of your own repos | [`onboarding-a-repo.md`](onboarding-a-repo.md) |
| Install the binaries against an **existing** Silo deployment | [`installing.md`](installing.md) |
| Understand how it works | [`architecture.md`](architecture.md) |
| Consolidate memory with the Distilator | [README → full cycle](../README.md#exercising-the-full-cycle-by-hand) |

## If something breaks

| Symptom | Fix |
|---|---|
| `address already in use` | Another `silod` owns that port. Find it with `lsof -nP -iTCP:8500 -sTCP:LISTEN`, then `pkill -f silod` — or pick a different `--listen` port. |
| `Vault is sealed` | Re-run `deploy/bootstrap-dev.sh` |
| `create bucket: 403 AccessDenied` | Bootstrap not run — or a non-local endpoint, which disables the dev credential defaults |
| `exec "weed": not found` | A non-loopback endpoint turns the dev defaults off. Pass `--weed-binary=./deploy/weed-docker.sh` explicitly. |
| `leader not found` | rqlite still forming — wait ~15 s |
| Daemon returns `unauthorized` | Token isn't in `--tokens` |
| Daemon returns `not found` for a path you wrote | Wrong project's token — tokens are scoped to one project |
| Teardown says `not ready for <step>` | Steps run in order; it names the step that's due. `siloctl status` shows where the project stands. |
