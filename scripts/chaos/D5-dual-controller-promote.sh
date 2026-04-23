#!/usr/bin/env bash
# D5 — Two controllers race to promote. Exercises the keeper.leader lease.
#
# We simulate "peer went away" by bouncing the DR controller at the exact
# moment we also trigger the DC manual switchover. The outcome depends on
# which controller renews the lease first:
#
#   - If DC sees a fresh DR lease:      DC refuses (CheckPeerLease).
#   - If DC sees a stale DR lease:      DC takes over with epoch+1.
#
# Either way, only ONE switchover may complete. The drill is unhappy only if
# both complete (split-brain) or neither (deadlock).
#
# Expected outcome:
#   - After 2 minutes, exactly one cluster is writable.
#   - keeper.leader.epoch has advanced by at least 1 since baseline.
#   - Neither CSP is stuck in SwitchingOver longer than ResumeStuckTimeout.
set -euo pipefail
DRILL_ID="D5"
source "$(dirname "$0")/_lib.sh"

banner "D5 starting — dual-controller race on keeper.leader"

log "record baseline lease state"
baseline_epoch="$(mysql_dc 'SELECT epoch FROM keeper.leader WHERE id=1' 2>/dev/null || echo 0)"
baseline_owner="$(mysql_dc 'SELECT owner FROM keeper.leader WHERE id=1' 2>/dev/null || echo none)"
log "baseline: owner=${baseline_owner}, epoch=${baseline_epoch}"

log "freeze DR controller (scale to 0) then restore, to age its lease heartbeat"
kctl_dr -n "${NAMESPACE}" scale deploy/mysql-keeper-controller-manager --replicas=0
sleep 60
kctl_dr -n "${NAMESPACE}" scale deploy/mysql-keeper-controller-manager --replicas=2

log "simultaneously trigger manual switchover on BOTH sides"
kctl_dc patch csp "${CSP_NAME}" --type=merge \
  -p '{"spec":{"manualSwitchoverTarget":"promote-remote"}}' &
kctl_dr patch csp "${CSP_NAME}" --type=merge \
  -p '{"spec":{"manualSwitchoverTarget":"promote-remote"}}' &
wait

log "observe for 2 minutes"
sleep 120

log "checking writable state"
dc_ro="$(mysql_dc 'SELECT @@read_only')"
dr_ro="$(mysql_dr 'SELECT @@read_only')"
log "DC read_only=${dc_ro}  DR read_only=${dr_ro}"

if [[ "${dc_ro}" == "0" && "${dr_ro}" == "0" ]]; then
  fatal "FAIL: both clusters writable — split-brain"
fi
if [[ "${dc_ro}" == "1" && "${dr_ro}" == "1" ]]; then
  fatal "FAIL: neither cluster is writable — deadlock"
fi

log "inspecting lease on the new source"
new_epoch=""
new_owner=""
if [[ "${dc_ro}" == "0" ]]; then
  new_epoch="$(mysql_dc 'SELECT epoch FROM keeper.leader WHERE id=1')"
  new_owner="$(mysql_dc 'SELECT owner FROM keeper.leader WHERE id=1')"
else
  new_epoch="$(mysql_dr 'SELECT epoch FROM keeper.leader WHERE id=1')"
  new_owner="$(mysql_dr 'SELECT owner FROM keeper.leader WHERE id=1')"
fi
log "winner: owner=${new_owner}, epoch=${new_epoch}"

if (( new_epoch <= baseline_epoch )); then
  fatal "FAIL: epoch did not advance (baseline=${baseline_epoch}, new=${new_epoch})"
fi

banner "D5 PASS — exactly one controller won; epoch advanced from ${baseline_epoch} to ${new_epoch}"
restore_baseline
