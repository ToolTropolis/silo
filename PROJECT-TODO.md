# Silo — project TODO

Status as of 2026-07-25. Written after two days of work on the storage,
isolation, caching, and admin layers, at the point where the thing they exist to
serve — **an agent in a repo actually using Silo** — turned out not to exist.
That gap is now closed; see the headline.

This file is the honest inventory: what works, what is missing, and what order
to fix it in.

---

## The headline

**Closed (2026-07-25): an agent can now use Silo as memory.**

`cmd/silo-mcp` exposes one project's memory over the Model Context Protocol, so
any MCP-speaking runtime picks up four tools from a `.mcp.json` entry. Verified
live, end to end:

- a memory written in one session is read back by a **separate process**;
- a second project **cannot** read, list, or search the first one's memory,
  while its own memory still works.

Both are covered by `internal/mcpserver/e2e_live_test.go`, which skips when no
daemon is reachable. Sabotage-checked: handing project B project A's token makes
all three isolation assertions fail.

A "project" is still an **isolation boundary**, not a repo — Silo stores no
`repo_url` or path, and the binding is a token plus a naming convention:

```
project ID "myrepo"  ->  bucket silo-myrepo + key + credential + cache file
       token in .mcp.json env       <- the entire linkage
       agent calls silo_read / silo_write via silo-mcp
```

Recording *which* repo a project belongs to is still open (P1/4 below), but that
is metadata for humans, not a functional gap.

---

## What works today (verified live, not just unit-tested)

| Capability | Evidence |
|---|---|
| Per-project isolation: bucket + scoped credential + SSE key | cross-project isolation test |
| CAS write path with ETag retry | `TestSafeWrite_RetriesOnConflict` |
| Offline write queue, drain, 202 `durable:false` | NAV-116, verified with SeaweedFS stopped |
| Generation stamps — a re-onboarded ID cannot read the old tenant's memory | proven live against `gatetest.bbolt` |
| Teardown purges the cache **before** destroying the bucket | `TestTeardown_RefusedPurgeLeavesTheBucket` |
| Cache eviction (TTL / max entries / max bytes) | live: 300 -> 250 entries |
| Compaction | live: 16.0 MiB -> 0.5 MiB, 97% reclaimed |
| Console-driven cache policy, no daemon restart | live, daemon started with zero flags |
| Onboarding wizard with preflight + per-layer rollback display | live, incl. forced failure |
| Admin console: cache, policy, projects, teardown | live |
| **Agent memory over MCP** (`silo-mcp`) | live: separate processes, 4 tools |
| **Memory survives the session that wrote it** | `TestLive_MemorySurvivesTheSession` |
| **Cross-project isolation over MCP** (read/list/search) | `TestLive_ProjectsCannotReadEachOther` |

Test suite: 16 packages green under `-race`, clean `vet` and `gofmt`.

---

## Pending work, in dependency order

### P0 — Make Silo usable from a repo — **DONE**

- [x] **1. MCP server (`cmd/silo-mcp`)** — built on the official
      `github.com/modelcontextprotocol/go-sdk` v1.6.1. Four tools over stdio,
      a thin adapter on `pkg/client` so the daemon HTTP API stays the stable
      contract. 14 unit tests through the SDK's in-memory transport.
- [x] **2. Wizard emits real agent config** — the final step now prints a
      project-scoped `.mcp.json` with the token as `${SILO_TOKEN}`, so it is
      never inlined into a file that gets committed.
- [x] **3. End-to-end acceptance test** — `internal/mcpserver/e2e_live_test.go`
      proves persistence across sessions and cross-project isolation against a
      live stack.

Remaining P0-adjacent polish:

- [ ] **1a. Decide whether `silo_write` should expose CAS semantics.** Today it
      hides them: a write replaces the whole file and the tool description says
      so. If two agents write the same path concurrently the daemon's retry
      resolves it, but the losing agent is never told its read was stale.
- [ ] **1b. Ship `silo-mcp` in the installer and `deploy/demo.sh`**, so the
      quickstart ends with a working agent rather than a curl call.

### P1 — Close the repo-metadata gap

- [ ] **4. Record which repo a project belongs to.**
      `006_project_repo.sql` adding nullable `repo_url` and `local_path`;
      surface both in the wizard and the projects view. Purely informational —
      answers "which repo is `myrepo`?" six months later. Does not change behaviour.
- [ ] **5. Reconcile the vocabulary.** Docs say "onboard a repo"; the system
      onboards a *project*. Pick one and make the docs and UI agree.

### P2 — Finish what this session started

- [ ] **6. Teardown as a guided flow**, mirroring the onboarding wizard.
      Currently four buttons on the projects page. It is already a four-step
      confirmed sequence; it deserves the same rail + per-step explanation.
- [ ] **7. Document and package `silo-admin`.** It exists and works but appears
      in no doc and in `deploy/demo.sh` not at all.
      - [ ] Add to `docs/QUICKSTART.md` and the README.
      - [ ] Start it from `deploy/demo.sh`.
      - [ ] Ship a real browser story: today it needs a bearer token, which a
            browser will not send. Either serve a login form, or document the
            Unix-socket + SSH-tunnel path properly. **The `/tmp` proxy used during
            development is not a solution and must not be committed.**
- [ ] **8. `demo` project caches nothing.** It predates generations, so
      `bindCache` fails closed and refuses to cache. Correct behaviour, confusing
      first impression. Either re-onboard it in `demo.sh` or explain it in the UI.

### P3 — Known gaps and follow-ups

- [ ] **9. `silo-distil` should use `pkg/client`** rather than owning a cache
      directory. Needs the SDK widened for actor/sessionID. Fixes a latent bug
      where a promote during an outage queues into a directory nobody drains.
- [ ] **10. Stale-replay clobber (from NAV-116)**: a queued write can overwrite a
      newer online one, because the queue is ordered by insertion rather than
      wall-clock. Unresolved.
- [ ] **11. True LRU eviction.** Today it is TTL + oldest-written-first. Real LRU
      needs last-access tracking on read, which would turn the read-only `db.View`
      into a write transaction — a serious regression given `Search` does N
      sequential reads. Needs an in-memory access map.
- [ ] **12. Per-entry ETag revalidation.** `backend.Get` already returns an
      `ObjectVersion` with an `ETag`; conditional revalidation would need
      `If-None-Match` in the adapter.
- [ ] **13. Repo social-preview image.** Upload is web-UI only; no API path.

### P4 — Operational hardening

- [ ] **14. `weed` dependency is awkward.** It is required only to issue
      credentials, but against the Docker dev stack a host-native `weed` **hangs**
      rather than failing, because SeaweedFS advertises a container-internal
      address (e.g. `172.24.0.2:9333`) the host cannot route to. Documented in
      three places now; the real fix is either an advertise-address flag in
      `deploy/docker-compose.yaml` (mirroring what rqlite already does with
      `-http-adv-addr`) or dropping the `weed shell` dependency entirely.
- [ ] **15. Secret storage for issued credentials.** Secrets go to Vault via
      `SecretStore`, but nothing surfaces them to an operator who needs to hand a
      credential to a service. No retrieval path exists.
- [ ] **16. Multi-daemon fleet view.** The console talks to exactly one daemon
      socket. A real fleet has many, and cache stats are per-host.

---

## Recommended next action

P0 is done: a repo can talk to Silo. The highest-value follow-ups now are
**1b** (ship `silo-mcp` in the demo so the quickstart ends with a working agent)
and **7** (`silo-admin` is undocumented and has no browser story), because both
are gaps between "it works" and "someone else can use it".

**4** is worth doing cheaply — recording which repo a project belongs to costs
one migration and answers an obvious question the console currently cannot.

Everything else in P1–P4 is genuinely deferrable.
