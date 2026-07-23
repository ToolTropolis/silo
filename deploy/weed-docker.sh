#!/usr/bin/env bash
# Runs `weed` inside the dev SeaweedFS container.
#
# siloctl issues per-project scoped credentials by shelling out to `weed shell`
# (s3.configure). The binary ships inside the SeaweedFS image rather than on the
# host, so point siloctl at this wrapper:
#
#   siloctl onboard --project=X --weed-binary=./deploy/weed-docker.sh
#
# Override the container name with SILO_SEAWEED_CONTAINER if your compose
# project prefix differs.
set -euo pipefail

container="${SILO_SEAWEED_CONTAINER:-deploy-seaweedfs-1}"

if ! docker inspect "$container" >/dev/null 2>&1; then
  echo "weed-docker.sh: container '$container' not found." >&2
  echo "  Start the dev stack:  docker compose -f deploy/docker-compose.yaml up -d" >&2
  echo "  Or set SILO_SEAWEED_CONTAINER to the right name." >&2
  exit 1
fi

exec docker exec -i "$container" weed "$@"
