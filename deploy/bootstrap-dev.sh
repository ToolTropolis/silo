#!/usr/bin/env bash
# Bootstrap the local dev stack after `docker compose up`.
#
# Once ANY S3 identity exists in SeaweedFS, anonymous access is disabled — so
# Silo's own components (the backend adapter, onboarding) must authenticate.
# This provisions a single "silo-admin" identity the backend/admin tooling use
# to manage buckets. Per-project scoped identities are then created by
# onboarding (siloctl onboard) on top of this.
#
# DEV ONLY. The credentials here are throwaway local values; production issues
# an admin credential through its own secrets management, never a script.
set -euo pipefail

CONTAINER="${SILO_SEAWEED_CONTAINER:-deploy-seaweedfs-1}"
ADMIN_KEY="${SILO_ADMIN_ACCESS_KEY:-SILOADMIN}"
ADMIN_SECRET="${SILO_ADMIN_SECRET_KEY:-SILOADMINSECRET}"

echo "Provisioning silo-admin identity in ${CONTAINER}..."
echo "s3.configure -user silo-admin -access_key ${ADMIN_KEY} -secret_key ${ADMIN_SECRET} -actions Admin,Read,Write,List,Tagging -apply" \
  | docker exec -i "${CONTAINER}" weed shell >/dev/null

echo "Done. Backend/admin tooling should authenticate as:"
echo "  access key: ${ADMIN_KEY}"
echo "  (secret via SILO_ADMIN_SECRET_KEY / configs)"
echo
echo "NOTE: anonymous S3 access is now disabled cluster-wide, by design."
