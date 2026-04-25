#!/usr/bin/env bash
# pxc-switchover.sh — drive the full mysql-keeper switchover engine
# against the local PXC + ProxySQL staging brought up by pxc-up.sh.
#
# Usage:
#   bash scripts/local/pxc-switchover.sh            # dry-run: preflight only
#   bash scripts/local/pxc-switchover.sh --execute  # full engine, flip DC→DR
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

DC_DSN="${DC_DSN:-root:stagingpass@tcp(127.0.0.1:33011)/?parseTime=true}"
DR_DSN="${DR_DSN:-root:stagingpass@tcp(127.0.0.1:33012)/?parseTime=true}"
DC_PROXY="${DC_PROXY:-127.0.0.1:36032}"
DR_PROXY="${DR_PROXY:-127.0.0.1:36042}"
PROXY_USER="${PROXY_USER:-keeper}"
PROXY_PASS="${PROXY_PASS:-stagingpass}"

BIN="${REPO_ROOT}/bin/switchover-cli"
mkdir -p "${REPO_ROOT}/bin"

if [[ ! -x "${BIN}" || "${REPO_ROOT}/cmd/switchover-cli/main.go" -nt "${BIN}" ]]; then
  echo ">> Building switchover-cli"
  (cd "${REPO_ROOT}" && go build -o "${BIN}" ./cmd/switchover-cli)
fi

mode="dry-run"
if [[ "${1:-}" == "--execute" ]]; then
  mode="execute"
fi

args=(
  -dc-mysql "${DC_DSN}"
  -dr-mysql "${DR_DSN}"
  -dc-proxysql "${DC_PROXY}"
  -dr-proxysql "${DR_PROXY}"
  -proxysql-user "${PROXY_USER}"
  -proxysql-pass "${PROXY_PASS}"
)
if [[ "${mode}" == "dry-run" ]]; then
  args+=(-dry-run)
fi

"${BIN}" "${args[@]}"
