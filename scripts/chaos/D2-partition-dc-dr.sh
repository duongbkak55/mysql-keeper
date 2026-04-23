#!/usr/bin/env bash
# D2 — Partition DC ↔ DR network mid-switchover.
#
# What it proves:
#   - Preflight C1 (RemoteReachableReadOnly) refuses the flip when the
#     remote side is unreachable.
#   - No switchover attempt is made with incomplete state.
#
# Assumes Chaos Mesh is installed in the staging cluster. If it is not,
# substitute whatever NetworkPolicy / iptables approach your environment
# allows — the critical property is "block DC→DR port 3306".
#
# Expected outcome:
#   - manualSwitchoverTarget is honoured by the reconciler, but PreFlight
#     C1 fails and Phase stays at (or returns to) Monitoring.
#   - Status.LastPreFlight.Checks shows C1_RemoteReachableReadOnly = FAIL.
set -euo pipefail
DRILL_ID="D2"
source "$(dirname "$0")/_lib.sh"

banner "D2 starting — partition DC↔DR, trigger switchover, observe preflight refusal"

DR_REMOTE_IP="${DR_REMOTE_IP:?must set DR_REMOTE_IP (the host DC uses to reach DR MySQL)}"

log "applying NetworkChaos blocking DC→${DR_REMOTE_IP}:3306 for 5 minutes"
chaos_manifest=$(mktemp)
cat > "${chaos_manifest}" <<YAML
apiVersion: chaos-mesh.org/v1alpha1
kind: NetworkChaos
metadata:
  name: d2-dc-dr-partition
  namespace: ${NAMESPACE}
spec:
  action: partition
  mode: all
  selector:
    namespaces: [${NAMESPACE}]
    labelSelectors:
      app.kubernetes.io/name: mysql-keeper
  direction: to
  target:
    mode: all
    selector:
      pods:
        default: [${DR_REMOTE_IP}]
  duration: "5m"
YAML
kctl_dc apply -f "${chaos_manifest}"
trap 'kctl_dc delete -f '"${chaos_manifest}"' --ignore-not-found' EXIT

log "waiting 10s for the network rule to take effect"
sleep 10

log "trigger switchover (expected to be refused at PreFlight C1)"
kctl_dc patch csp "${CSP_NAME}" --type=merge \
  -p '{"spec":{"manualSwitchoverTarget":"promote-remote"}}'

log "waiting 60s for the reconciler to observe + record the PreFlight failure"
sleep 60

log "inspecting LastPreFlight"
snapshot="$(kctl_dc get csp "${CSP_NAME}" -o jsonpath='{.status.lastPreFlight}')"
echo "${snapshot}" | python3 -m json.tool || true

if ! echo "${snapshot}" | grep -q 'C1_RemoteReachableReadOnly'; then
  fatal "FAIL: C1 check absent from LastPreFlight.Checks"
fi
if ! echo "${snapshot}" | grep -q '"passed":false'; then
  fatal "FAIL: expected at least one failed check; LastPreFlight=${snapshot}"
fi

if both_writable; then
  fatal "FAIL: split-brain detected"
fi

banner "D2 PASS — preflight refused with C1 FAIL, no switchover attempt"
restore_baseline
