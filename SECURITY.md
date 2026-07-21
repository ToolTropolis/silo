# Security Policy

Silo stores persistent, multi-tenant agent memory, and its core security
property is strict per-project isolation (bucket-per-project, per-project scoped
credentials, per-project encryption keys). We take reports affecting that
isolation, or any other vulnerability, seriously.

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report privately through one of:

- **GitHub Security Advisories** — the "Report a vulnerability" button under the
  repository's **Security** tab (preferred; keeps the report private and
  threaded).
- **Email** — `nishant.navjyot@gmail.com` with subject `SILO SECURITY`.

Include, where you can: affected version/commit, a description of the issue, a
proof-of-concept or reproduction steps, and the impact you observed — especially
if it involves one project reading, writing, or inferring another project's
data.

## Coordinated disclosure and response timeframes

We follow coordinated disclosure. Our commitments:

| Stage | Target |
|---|---|
| Acknowledge your report | within **3 business days** |
| Initial assessment + severity triage | within **10 business days** |
| Fix or documented mitigation for confirmed high/critical issues | within **90 days** of triage |
| Public disclosure | coordinated with you, normally after a fix ships or 90 days elapse, whichever comes first |

We will keep you updated as triage progresses and credit you in the advisory
unless you ask us not to. If a report turns out to be out of scope or not
reproducible, we'll explain why.

## Supported versions

Security fixes are applied to the latest released minor version. Because Silo is
distributed as a versioned Go module, pin a released version and watch releases
for advisories.

| Version | Supported |
|---|---|
| latest release | ✅ |
| older releases | ⚠️ best-effort; upgrade recommended |

## Scope notes

- **In scope:** cross-tenant data access, credential/key leakage, auth bypass on
  the daemon or dashboard, injection or SSRF in the write path or Distilator,
  and anything that undermines the isolation guarantee.
- **Dev scaffolding is not a finding:** `deploy/docker-compose.yaml` and
  `configs/example.yaml` use dev-mode Vault tokens and unauthenticated local
  services **on purpose**, for local development only. Never use them in
  production — production credentials come from environment variables or a
  secrets manager, never a checked-in file.
