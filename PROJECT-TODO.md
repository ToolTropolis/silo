# Silo — project TODO

Status as of 2026-07-27. Written after two days of work on the storage,
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
| **Agent tokens**: mint, list, revoke, hashed at rest | live: minted -> 200 -> revoked -> 401 |
| **Agent memory over MCP** (`silo-mcp`) | live: separate processes, 4 tools |
| **Memory survives the session that wrote it** | `TestLive_MemorySurvivesTheSession` |
| **Cross-project isolation over MCP** (read/list/search) | `TestLive_ProjectsCannotReadEachOther` |

Test suite: 16 packages green under `-race`, clean `vet` and `gofmt`.

---

## Pending work

Tracked in Linear under the **Silo** project. This file records what is *done* and
verified; the backlog lives where it can be prioritised.

| Issue | Scope |
|---|---|
| [NAV-118](https://linear.app/navjyot-labs/issue/NAV-118) | Gaps vs Anthropic's managed-agent memory model — read-only access, size caps, exposed CAS, redaction |
| [NAV-119](https://linear.app/navjyot-labs/issue/NAV-119) | Multi-agent concurrency — CAS retry budget, bbolt contention, per-agent access, attribution honesty |
| [NAV-120](https://linear.app/navjyot-labs/issue/NAV-120) | Install path — `silo-mcp` unshipped, `silo-admin` undocumented, `weed` unusable against the dev stack |
| [NAV-121](https://linear.app/navjyot-labs/issue/NAV-121) | Correctness debt — stale-replay clobber, `silo-distil` cache, LRU, conditional revalidation |
| [NAV-122](https://linear.app/navjyot-labs/issue/NAV-122) | Operator gaps — credential retrieval, fleet view, teardown flow, vocabulary |
| [NAV-123](https://linear.app/navjyot-labs/issue/NAV-123) | Distilator vs Dreams — run lifecycle, cancellation, input bounds, cost, promote conflicts |

**Closed since this file was written:** the MCP server exists and is verified end to
end; agent tokens are minted, hashed, and revocable; the wizard starts from a repository
and records it; the console shows storage, agents, and daemon health.

## Recommended next action

P0 is done: a repo can talk to Silo. The highest-value follow-ups now are
**1b** (ship `silo-mcp` in the demo so the quickstart ends with a working agent)
and **7** (`silo-admin` is undocumented and has no browser story), because both
are gaps between "it works" and "someone else can use it".

**4** is worth doing cheaply — recording which repo a project belongs to costs
one migration and answers an obvious question the console currently cannot.

Everything else in P1–P4 is genuinely deferrable.
