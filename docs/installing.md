# Installing Silo

Silo ships four binaries:

| Binary | What it's for |
|---|---|
| `silod` | the daemon agents talk to (CAS writes, cache, offline queue) |
| `siloctl` | admin CLI — onboard, status, teardown |
| `silo-distil` | the Distilator consolidation runner |
| `silo-dashboard` | read/review web surface |

## Do you actually need these?

**Binaries alone don't run Silo.** It needs a storage stack — SeaweedFS, rqlite,
and Vault — and without one, every command fails on connect:

```
siloctl status: connect registry: ... connection refused
```

| You are… | Use |
|---|---|
| Trying Silo for the first time | [QUICKSTART.md](QUICKSTART.md) — `deploy/demo.sh` runs the stack *and* the binaries |
| Running an agent host that talks to a Silo someone else deployed | this page — you need `silod` only |
| Administering a deployed Silo from your laptop | this page — you need `siloctl` |
| Deploying Silo itself | this page, plus your own SeaweedFS/rqlite/Vault |

If you're in the first row, stop here and read the quickstart instead.

---

## Install script (macOS, Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/ToolTropolis/silo/main/docs/install.sh | sh
```

Detects your OS/arch, downloads that release, verifies it, installs to
`~/.silo/bin`, and symlinks into `~/.local/bin` (or `/usr/local/bin`).

| Variable | Default | Purpose |
|---|---|---|
| `SILO_VERSION` | latest release | pin a version, e.g. `v0.1.0` |
| `SILO_INSTALL_DIR` | `~/.silo/bin` | where binaries land |
| `SILO_BIN_DIR` | `~/.local/bin`, else `/usr/local/bin` | where symlinks go |

```bash
# pin a version, install somewhere else
SILO_VERSION=v0.1.0 SILO_INSTALL_DIR=/opt/silo/bin \
  sh -c "$(curl -fsSL https://raw.githubusercontent.com/ToolTropolis/silo/main/docs/install.sh)"
```

### Piping to a shell

Reading a script before running it is the right instinct, and this one is small
enough to read in a minute:

```bash
curl -fsSL https://raw.githubusercontent.com/ToolTropolis/silo/main/docs/install.sh -o install.sh
less install.sh
sh install.sh
```

It's POSIX `sh`, writes only to the two directories above, and uses `sudo` only
when the link directory isn't writable — it says so first.

If you'd rather not run it at all, use `go install` or the manual steps below.

---

## What gets verified

Two independent checks, because they prove different things:

**1. Checksums — always.** Every binary is SHA-256'd against the release's
`SHA256SUMS.txt`. A mismatch aborts the install. If neither `sha256sum` nor
`shasum` exists, the script warns rather than pretending it checked.

**2. Signature — when `cosign` is present.** A checksum only proves the binary
matches the manifest; if an attacker can replace the binary, they can replace
the manifest too. The signature proves *who produced* the manifest. Silo's
release workflow signs it with [cosign](https://docs.sigstore.dev/) keyless
signing, so verification checks the identity — this repo's `release.yaml` at a
tag — not merely that some valid certificate exists.

Without cosign installed the script says it skipped that check. **With cosign
installed, a bad signature is fatal** — it will not install.

### Verifying by hand

```bash
VERSION=v0.1.0
BASE="https://github.com/ToolTropolis/silo/releases/download/${VERSION}"
curl -fsSLO "${BASE}/SHA256SUMS.txt" -O "${BASE}/SHA256SUMS.txt.bundle"

cosign verify-blob \
  --bundle SHA256SUMS.txt.bundle \
  --certificate-identity-regexp '^https://github.com/ToolTropolis/silo/\.github/workflows/release\.yaml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS.txt

# then check a downloaded binary against the verified manifest
sha256sum -c SHA256SUMS.txt --ignore-missing
```

> Signatures ship as a **Sigstore bundle** (`.bundle`) — certificate and Rekor
> inclusion proof in one file. cosign v3 no longer emits the older detached
> `.sig`/`.pem` pair, so older snippets using those flags won't apply here.

---

## `go install`

Works on every platform Go supports, **including Windows**. Builds from source,
so it needs Go 1.26+ — and there's nothing to verify, since you compiled it:

```bash
go install github.com/tooltropolis/silo/cmd/siloctl@latest
go install github.com/tooltropolis/silo/cmd/silod@latest
go install github.com/tooltropolis/silo/cmd/silo-distil@latest
go install github.com/tooltropolis/silo/cmd/silo-dashboard@latest
```

Binaries land in `$(go env GOPATH)/bin`. Pin a version with `@v0.1.0`.

---

## Manual download

Grab a binary from the [releases page](https://github.com/ToolTropolis/silo/releases).
Assets are named `<cmd>_<version>_<os>_<arch>`:

```bash
VERSION=v0.1.0; OS=darwin; ARCH=arm64
curl -fsSLO "https://github.com/ToolTropolis/silo/releases/download/${VERSION}/siloctl_${VERSION}_${OS}_${ARCH}"
chmod +x "siloctl_${VERSION}_${OS}_${ARCH}"
mv "siloctl_${VERSION}_${OS}_${ARCH}" /usr/local/bin/siloctl
```

Verify against `SHA256SUMS.txt` first — see above.

---

## Build from source

```bash
git clone https://github.com/ToolTropolis/silo.git
cd silo
go build -o ./bin/siloctl ./cmd/siloctl
# ...or all four:
for c in silod siloctl silo-distil silo-dashboard; do go build -o "./bin/$c" "./cmd/$c"; done
```

---

## Windows

The install script doesn't support Windows. Use `go install` (above), or
download from the releases page — though releases currently cover **linux and
darwin only**. On Windows, build from source or use WSL.

---

## Uninstalling

```bash
rm -rf ~/.silo
for c in silod siloctl silo-distil silo-dashboard; do rm -f ~/.local/bin/$c; done
```

Adjust the paths if you set `SILO_INSTALL_DIR` or `SILO_BIN_DIR`. Installed via
`go install`? Remove them from `$(go env GOPATH)/bin` instead.
