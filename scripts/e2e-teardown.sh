#!/usr/bin/env bash
# e2e-teardown.sh — remove the kind cluster used by the e2e suite. Does not
# remove the Docker images; run `docker image prune` separately if you want
# that.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-keeper-e2e}"

if kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
  kind delete cluster --name "${CLUSTER_NAME}"
  echo "Deleted kind cluster '${CLUSTER_NAME}'"
else
  echo "No kind cluster named '${CLUSTER_NAME}' — nothing to do"
fi
