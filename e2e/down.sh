#!/usr/bin/env bash
# Stop and remove only the omnara-pulumi-e2e compose project: its containers,
# network, and named volumes (postgres, rustfs, memory). Nothing else on the
# host is touched, and no prune commands are used.
set -euo pipefail

E2E_DIR="$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")"
STATE_DIR="$E2E_DIR/.state"
PROJECT="omnara-pulumi-e2e"

echo "[down] removing compose project ${PROJECT} (containers, network, volumes)" >&2
docker compose -f "$E2E_DIR/compose.yaml" -p "$PROJECT" down --volumes --remove-orphans >&2

# The database is gone, so the token, org, and test-user password are stale.
# Other files in .state (for example harness-verify.txt) are left alone.
rm -f "$STATE_DIR/token" "$STATE_DIR/env" "$STATE_DIR/password"
echo "[down] done" >&2
