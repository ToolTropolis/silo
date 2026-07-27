#!/usr/bin/env bash
# Bootstrap the local dev stack after `docker compose up -d`.
#
# Two jobs, both idempotent — safe to re-run after every `up`:
#
#   1. Vault: initialize + unseal, and ensure the well-known dev token exists.
#      Vault runs in SERVER mode with file storage (not `-dev`), so keys survive
#      restarts and `docker compose down`. The tradeoff is that server mode must
#      be initialized and unsealed, which is what this handles. Unsealing is
#      required after every container start.
#
#   2. SeaweedFS: provision the "silo-admin" S3 identity. Once ANY identity
#      exists, anonymous access is disabled — so Silo's own components (the
#      backend adapter, onboarding) must authenticate. Per-project scoped
#      identities are created on top of this by `siloctl onboard`.
#
# DEV ONLY. Unseal keys are written to a file inside the Vault volume and the
# credentials here are throwaway local values. Production uses TLS, auto-unseal
# via a cloud KMS, and issues credentials through real secrets management.
set -euo pipefail

SEAWEED_CONTAINER="${SILO_SEAWEED_CONTAINER:-deploy-seaweedfs-1}"
VAULT_CONTAINER="${SILO_VAULT_CONTAINER:-deploy-vault-1}"
ADMIN_KEY="${SILO_ADMIN_ACCESS_KEY:-SILOADMIN}"
ADMIN_SECRET="${SILO_ADMIN_SECRET_KEY:-SILOADMINSECRET}"
RUNTIME_KEY="${SILO_RUNTIME_ACCESS_KEY:-SILORUNTIME}"
RUNTIME_SECRET="${SILO_RUNTIME_SECRET_KEY:-SILORUNTIMESECRET}"
DEV_TOKEN="${SILO_VAULT_TOKEN:-dev-only-token}"

# Where init output is kept inside the Vault volume so re-runs can unseal.
INIT_FILE="/vault/file/dev-init.json"

vault_exec() { docker exec -e VAULT_ADDR=http://127.0.0.1:8200 -i "$VAULT_CONTAINER" "$@"; }

echo "==> Waiting for Vault to accept connections..."
for _ in $(seq 1 30); do
  if vault_exec vault status >/dev/null 2>&1 || [ $? -eq 2 ]; then break; fi
  sleep 1
done

# `vault status` exits non-zero when sealed (2) or uninitialized (1), so the
# exit code can't gate the parse — read the JSON field itself. `|| true` keeps
# `set -e` from aborting on those expected non-zero exits.
vault_status_field() {
  vault_exec vault status -format=json 2>/dev/null | jq -r "$1" 2>/dev/null || true
}

if [ "$(vault_status_field '.initialized')" != "true" ]; then
  echo "==> Initializing Vault (first run)..."
  vault_exec sh -c "vault operator init -key-shares=1 -key-threshold=1 -format=json > ${INIT_FILE}"
  echo "    init output stored at ${INIT_FILE} (inside the vault-data volume)"
else
  echo "==> Vault already initialized."
fi

# Unseal is required after every container start, not just first init.
if [ "$(vault_status_field '.sealed')" = "true" ]; then
  echo "==> Unsealing Vault..."
  UNSEAL_KEY=$(vault_exec cat "${INIT_FILE}" | jq -r '.unseal_keys_b64[0]')
  vault_exec vault operator unseal "$UNSEAL_KEY" >/dev/null
  echo "    unsealed."
else
  echo "==> Vault already unsealed."
fi

# Log in with the generated root token, then ensure the well-known dev token
# exists so README/tests/configs can keep using a fixed value.
ROOT_TOKEN=$(vault_exec cat "${INIT_FILE}" | jq -r '.root_token')
if [ -z "$ROOT_TOKEN" ]; then
  echo "ERROR: could not read the root token from ${INIT_FILE}" >&2
  exit 1
fi

echo "==> Ensuring the dev token '${DEV_TOKEN}' exists..."
if vault_exec sh -c "VAULT_TOKEN='${ROOT_TOKEN}' vault token lookup '${DEV_TOKEN}'" >/dev/null 2>&1; then
  echo "    already present."
else
  vault_exec sh -c "VAULT_TOKEN='${ROOT_TOKEN}' vault token create -id='${DEV_TOKEN}' -policy=root -period=768h" >/dev/null
  echo "    created."
fi

echo
# Two identities, least privilege (NAV-110):
#   silo-admin   — bucket lifecycle. ONLY siloctl onboard/teardown needs this.
#   silo-runtime — object CRUD only. Denied CreateBucket/DeleteBucket, which is
#                  enforced by SeaweedFS, so a compromised daemon or dashboard
#                  cannot destroy a project's bucket.
echo "==> Provisioning silo-admin identity in ${SEAWEED_CONTAINER}..."
echo "s3.configure -user silo-admin -access_key ${ADMIN_KEY} -secret_key ${ADMIN_SECRET} -actions Admin,Read,Write,List,Tagging -apply" \
  | docker exec -i "${SEAWEED_CONTAINER}" weed shell >/dev/null

echo "==> Provisioning silo-runtime identity (no Admin)..."
echo "s3.configure -user silo-runtime -access_key ${RUNTIME_KEY} -secret_key ${RUNTIME_SECRET} -actions Read,Write,List,Tagging -apply" \
  | docker exec -i "${SEAWEED_CONTAINER}" weed shell >/dev/null

echo
echo "Done."
echo "  S3 admin key:   ${ADMIN_KEY}     (siloctl only — creates/deletes buckets)"
echo "  S3 runtime key: ${RUNTIME_KEY}   (silod/dashboard/distil — object CRUD only)"
echo "  Vault token:   ${DEV_TOKEN}"
echo
echo "NOTE: anonymous S3 access is now disabled cluster-wide, by design."
echo "NOTE: data persists across 'docker compose down'. Use 'down -v' to wipe."
