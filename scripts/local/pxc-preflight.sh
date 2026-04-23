#!/usr/bin/env bash
# pxc-preflight.sh — build + run the preflight CLI against the local PXC
# staging. Exits with the CLI's exit code (0 = OK, 1 = blocked).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

DC_DSN="${DC_DSN:-root:stagingpass@tcp(127.0.0.1:33011)/?parseTime=true}"
DR_DSN="${DR_DSN:-root:stagingpass@tcp(127.0.0.1:33012)/?parseTime=true}"
CATCHUP="${CATCHUP:-8s}"

BIN="${REPO_ROOT}/bin/preflight-cli"
mkdir -p "${REPO_ROOT}/bin"

if [[ ! -x "${BIN}" || "${REPO_ROOT}/cmd/preflight-cli/main.go" -nt "${BIN}" ]]; then
  echo ">> Building preflight-cli"
  (cd "${REPO_ROOT}" && go build -o "${BIN}" ./cmd/preflight-cli)
fi

"${BIN}" -dc="${DC_DSN}" -dr="${DR_DSN}" -catchup-timeout="${CATCHUP}"
