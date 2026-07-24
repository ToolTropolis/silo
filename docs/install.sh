#!/bin/sh
# Install the Silo binaries (silod, siloctl, silo-distil, silo-dashboard).
#
#   curl -fsSL https://raw.githubusercontent.com/ToolTropolis/silo/main/docs/install.sh | sh
#
# Downloads a prebuilt release for your OS/arch, verifies it, and installs to
# ~/.silo/bin with a symlink on your PATH. No clone, no Go toolchain.
#
# What it verifies, in order:
#   1. SHA-256 against the release's SHA256SUMS.txt — always, non-negotiable.
#   2. The cosign signature over that manifest, when cosign is on PATH. This
#      proves the manifest was produced by this repo's release workflow rather
#      than by someone who merely reached the bucket, which a checksum alone
#      cannot show: an attacker who replaces a binary can replace its checksum.
#      Skipped with a warning when cosign is absent, so the common case still
#      installs — but if it is present, a bad signature is fatal.
#
# Environment:
#   SILO_VERSION       version to install (default: latest release)
#   SILO_INSTALL_DIR   where binaries land (default: ~/.silo/bin)
#   SILO_BIN_DIR       where the symlinks go (default: ~/.local/bin, else /usr/local/bin)
#
# Reading this before piping it to a shell is the correct instinct. It is POSIX
# sh, touches only the two directories above, and needs sudo solely when the
# link directory is not writable.
set -eu

REPO="ToolTropolis/silo"
BINARIES="silod siloctl silo-distil silo-dashboard"
INSTALL_DIR="${SILO_INSTALL_DIR:-$HOME/.silo/bin}"

# ANSI only when stdout is a terminal — piped output stays clean.
if [ -t 1 ]; then
  B="$(printf '\033[1m')"; DIM="$(printf '\033[2m')"; R="$(printf '\033[0m')"
  GRN="$(printf '\033[1;32m')"; YLW="$(printf '\033[1;33m')"; CYN="$(printf '\033[1;36m')"
else
  B=""; DIM=""; R=""; GRN=""; YLW=""; CYN=""
fi

say()  { printf '%s==>%s %s\n' "$CYN" "$R" "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '%swarning:%s %s\n' "$YLW" "$R" "$*" >&2; }
die()  { printf '%serror:%s %s\n' "$(printf '\033[1;31m')" "$R" "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed."; }
need uname
need tar
command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 ||
  die "either curl or wget is required."

# fetch URL OUTFILE — download to a file, or to stdout when OUTFILE is "-".
fetch() {
  if command -v curl >/dev/null 2>&1; then
    if [ "$2" = "-" ]; then curl -fsSL "$1"; else curl -fsSL -o "$2" "$1"; fi
  else
    if [ "$2" = "-" ]; then wget -qO- "$1"; else wget -qO "$2" "$1"; fi
  fi
}

# ---------------------------------------------------------------- platform ---
OS="$(uname -s)"
case "$OS" in
  Linux)  OS=linux ;;
  Darwin) OS=darwin ;;
  MINGW*|MSYS*|CYGWIN*)
    die "Windows is not supported by this script. Use: go install github.com/tooltropolis/silo/cmd/siloctl@latest" ;;
  *) die "unsupported operating system: $OS" ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) die "unsupported architecture: $ARCH (releases cover amd64 and arm64)" ;;
esac

printf '\n%sSilo installer%s  %s(%s/%s)%s\n\n' "$B" "$R" "$DIM" "$OS" "$ARCH" "$R"

# ----------------------------------------------------------------- version ---
VERSION="${SILO_VERSION:-}"
if [ -z "$VERSION" ]; then
  say "Resolving the latest release"
  # Parse the tag without jq — this script must run on a bare machine.
  VERSION="$(fetch "https://api.github.com/repos/${REPO}/releases/latest" - |
    sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
  [ -n "$VERSION" ] ||
    die "could not determine the latest release. Set SILO_VERSION=vX.Y.Z, or check https://github.com/${REPO}/releases"
  info "$VERSION"
fi
case "$VERSION" in v*) ;; *) VERSION="v${VERSION}" ;; esac

BASE="https://github.com/${REPO}/releases/download/${VERSION}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

# ---------------------------------------------------------------- download ---
say "Downloading ${VERSION}"
for cmd in $BINARIES; do
  asset="${cmd}_${VERSION}_${OS}_${ARCH}"
  fetch "${BASE}/${asset}" "${TMP}/${asset}" ||
    die "failed to download ${asset}. Does ${VERSION} have a build for ${OS}/${ARCH}? See https://github.com/${REPO}/releases"
  info "$cmd"
done

fetch "${BASE}/SHA256SUMS.txt" "${TMP}/SHA256SUMS.txt" ||
  die "could not download SHA256SUMS.txt — refusing to install unverified binaries."

# ------------------------------------------------------------ verification ---
say "Verifying"

# Prefer the platform's checksum tool; both exist widely, neither universally.
if command -v sha256sum >/dev/null 2>&1;   then SUMCMD="sha256sum"
elif command -v shasum >/dev/null 2>&1;    then SUMCMD="shasum -a 256"
else SUMCMD=""
fi

if [ -n "$SUMCMD" ]; then
  for cmd in $BINARIES; do
    asset="${cmd}_${VERSION}_${OS}_${ARCH}"
    want="$(grep " \{1,2\}\*\{0,1\}${asset}\$" "${TMP}/SHA256SUMS.txt" | awk '{print $1}' | head -n 1)"
    [ -n "$want" ] || die "${asset} is not listed in SHA256SUMS.txt — refusing to install."
    got="$(cd "$TMP" && $SUMCMD "$asset" | awk '{print $1}')"
    [ "$want" = "$got" ] || die "checksum mismatch for ${asset}.
  expected ${want}
  got      ${got}
This binary does not match the signed manifest. Not installing."
  done
  info "${GRN}checksums OK${R}  (${BINARIES})"
else
  warn "no sha256sum or shasum found; cannot verify checksums."
fi

# Signature check. A checksum proves the file matches the manifest; only the
# signature says who wrote the manifest.
if command -v cosign >/dev/null 2>&1; then
  # Identity must match this repo's release workflow at a tag — not merely
  # "some valid Sigstore certificate", which any GitHub Actions run can mint.
  IDENTITY="^https://github.com/${REPO}/\.github/workflows/release\.yaml@refs/tags/"
  ISSUER="https://token.actions.githubusercontent.com"
  sig_result=""

  # Releases publish a Sigstore bundle, which carries the certificate and Rekor
  # inclusion proof in one file. cosign v3 refuses to emit the older detached
  # .sig/.pem pair, so there is no second format to fall back to.
  if fetch "${BASE}/SHA256SUMS.txt.bundle" "${TMP}/SHA256SUMS.txt.bundle" 2>/dev/null; then
    if cosign verify-blob \
         --bundle "${TMP}/SHA256SUMS.txt.bundle" \
         --certificate-identity-regexp "$IDENTITY" \
         --certificate-oidc-issuer "$ISSUER" \
         "${TMP}/SHA256SUMS.txt" >/dev/null 2>&1; then
      sig_result="ok"
    else
      sig_result="failed"
    fi
  fi

  case "$sig_result" in
    ok)
      info "${GRN}signature OK${R}  (cosign keyless, verified against ${REPO})" ;;
    failed)
      die "cosign signature verification FAILED for ${VERSION}.
The checksums manifest is not signed by ${REPO}'s release workflow. Not installing." ;;
    *)
      warn "no signature published for ${VERSION}; verified by checksum only." ;;
  esac
else
  info "${DIM}cosign not found — checksum verified, signature not checked${R}"
  info "${DIM}install cosign to verify provenance: https://docs.sigstore.dev/cosign/installation/${R}"
fi

# ----------------------------------------------------------------- install ---
say "Installing to ${INSTALL_DIR}"
mkdir -p "$INSTALL_DIR" || die "could not create ${INSTALL_DIR}"
for cmd in $BINARIES; do
  mv "${TMP}/${cmd}_${VERSION}_${OS}_${ARCH}" "${INSTALL_DIR}/${cmd}"
  chmod +x "${INSTALL_DIR}/${cmd}"
done
info "$(echo "$BINARIES" | tr ' ' '\n' | wc -l | tr -d ' ') binaries installed"

# Link into a PATH directory. Prefer a user-writable one so the common case
# needs no elevation at all.
if [ -n "${SILO_BIN_DIR:-}" ]; then
  BIN_DIR="$SILO_BIN_DIR"
elif [ -d "$HOME/.local/bin" ] || mkdir -p "$HOME/.local/bin" 2>/dev/null; then
  BIN_DIR="$HOME/.local/bin"
else
  BIN_DIR="/usr/local/bin"
fi

# Try to create it first: a directory that doesn't exist yet is not "writable",
# and testing -w before mkdir escalates to sudo for a path the user could have
# made themselves.
mkdir -p "$BIN_DIR" 2>/dev/null || true

SUDO=""
if [ ! -w "$BIN_DIR" ]; then
  if command -v sudo >/dev/null 2>&1; then
    SUDO="sudo"
    warn "${BIN_DIR} needs elevated permissions; using sudo to create symlinks."
  else
    die "${BIN_DIR} is not writable and sudo is unavailable. Set SILO_BIN_DIR to a writable directory."
  fi
  $SUDO mkdir -p "$BIN_DIR"
fi
for cmd in $BINARIES; do
  $SUDO ln -sf "${INSTALL_DIR}/${cmd}" "${BIN_DIR}/${cmd}"
done
info "linked into ${BIN_DIR}"

# ------------------------------------------------------------------- done ---
printf '\n%sSilo %s installed.%s\n\n' "$B" "$VERSION" "$R"

case ":${PATH}:" in
  *":${BIN_DIR}:"*) ;;
  *)
    warn "${BIN_DIR} is not on your PATH. Add it:"
    printf '    export PATH="%s:$PATH"\n\n' "$BIN_DIR" ;;
esac

info "siloctl --help        admin CLI (onboard, status, teardown)"
info "silod --help          the daemon agents talk to"
printf '\n'
info "Silo needs a storage stack (SeaweedFS, rqlite, Vault). To try it locally:"
printf '    %sgit clone https://github.com/%s.git && cd silo && deploy/demo.sh%s\n' "$DIM" "$REPO" "$R"
printf '\n'
info "Docs: https://github.com/${REPO}/blob/main/docs/QUICKSTART.md"
