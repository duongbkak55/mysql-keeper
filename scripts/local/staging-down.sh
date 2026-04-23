#!/usr/bin/env bash
# staging-down.sh — stop the local Tier D staging + wipe volumes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE="${SCRIPT_DIR}/staging-compose.yml"

docker compose -f "${COMPOSE}" down -v --remove-orphans
echo "Local staging torn down."
