#!/usr/bin/env bash
# pxc-down.sh — stop the local PXC staging + wipe volumes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE="${SCRIPT_DIR}/pxc-compose.yml"

docker compose -f "${COMPOSE}" down -v --remove-orphans
echo "Local PXC staging torn down."
