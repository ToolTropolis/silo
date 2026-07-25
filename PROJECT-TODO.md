# Silo — project TODO

Status as of 2026-07-25. Written after two days of work in which the storage,
isolation, caching, and admin layers were built and verified end to end, and the
thing they exist to serve — **an agent in a repo actually using Silo** — turned
out not to exist at all.

This file is the honest inventory: what works, what is missing, and what order
to fix it in.

---

## The headline

**You cannot yet point Silo at a repo and have an agent use it.**

There is no repo linkage anywhere in the system. Verified: no `repo`, `git`,
`url`, or `clone` field exists in any migration, and `ProjectRecord` carries
only `ProjectID`, `BucketName`, `CredentialID`, `KeyID`, `CreatedAt`, `Status`,
`Generation`.

A "project" is an **isolation boundary**, not a repo. The only thing binding a
repo to Silo is a naming convention plus a token:

```
project ID "myrepo"  ->  bucket silo-myrepo + key + credential + cache file
       token "myrepo-token=myrepo"   <- the entire linkage
       agent calls POST /v1/write with that token
```

Silo never sees the repo. `docs/onboarding-a-repo.md` §"Wiring an agent" lists
three integration sketches and states plainly that they are **not verified
against current framework APIs**. That gap predates this session; everything
built on top of it is real, and none of it closes it.

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

Test suite: 15 packages green under `-race`, clean `vet` and `gofmt`.

---

## Pending work, in dependency order

### P0 — Make Silo usable from a repo *(nothing else matters until this ships)*

- [ ] **1. MCP server (`cmd/silo-mcp`)** — the missing product.
      Wrap the four daemon endpoints (`read`/`write`/`list`/`search`) as MCP
      tools so an agent calls Silo naturally. ~300 lines. `docs/onboarding-a-repo.md`
      already names this as "likely the best fit".
      - [ ] Decide the target agent runtime first (Claude Code? something else?) —
            this determines whether MCP is even the right protocol.
      - [ ] Tool schemas: `silo_read`, `silo_write`, `silo_list`, `silo_search`.
      - [ ] Token comes from config/env; one server instance serves one project,
            matching the daemon's one-token-one-project boundary.
      - [ ] Decide whether `write` exposes `SafeWrite`'s CAS semantics or hides them.
- [ ] **2. Wizard emits real agent config**, not curl commands.
      The final step should produce the `.mcp.json` (or equivalent) block for the
      project's token, ready to paste — and optionally write it into a repo path
      the operator names.
- [ ] **3. End-to-end smoke test**: onboard a project, wire an agent in a scratch
      repo, have it write a memory, restart the agent, have it read that memory
      back. This is the acceptance test the project currently has no equivalent of.

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

Ship **P0 item 1** (the MCP server) as the single next piece of work, then item 3
(the end-to-end test) to prove it. Do not add more console features until a repo
can actually talk to Silo.

Everything in P1–P4 is genuinely deferrable. P0 is not — without it, the
isolation, caching, and consolidation machinery has no consumer.
