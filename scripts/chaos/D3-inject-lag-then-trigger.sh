#!/usr/bin/env bash
# D3 — Inject replication lag on DR, then trigger switchover.
#
# This is the direct regression drill for the production incident that drove
# the entire remediation. The only correct outcome is: the switchover does
# NOT proceed because preflight C5 (GTIDSubset) or C6 (GTIDCatchup) refuses.
#
# Expected outcome:
#   - Status.LastPreFlight.Checks contains at least one of:
#       C5_GTIDSubset passed=false
#       C6_GTIDCatchup passed=false
#   - Phase remains Monitoring (or briefly SwitchingOver → Monitoring if
#     the manual trigger was clocked before the reconciler saw the lag).
#   - both_writable never returns 0.
set -euo pipefail
DRILL_ID="D3"
source "$(dirname "$0")/_lib.sh"

banner "D3 starting — inject lag, trigger switchover, expect preflight refusal"

ROW_COUNT="${ROW_COUNT:-500}"

log "pre-check: DR SHOW REPLICA STATUS should have IO+SQL Running"
io_running="$(mysql_dr "SELECT service_state FROM performance_schema.replication_connection_status WHERE channel_name='dc-to-dr'")"
sql_running="$(mysql_dr "SELECT service_state FROM performance_schema.replication_applier_status WHERE channel_name='dc-to-dr'")"
[[ "${io_running}" == "ON" && "${sql_running}" == "ON" ]] \
  || fatal "baseline replication not running: io=${io_running} sql=${sql_running}"

log "stopping DR SQL applier to stall replication"
mysql_dr "STOP REPLICA SQL_THREAD FOR CHANNEL 'dc-to-dr'"
trap 'mysql_dr "START REPLICA SQL_THREAD FOR CHANNEL dc-to-dr" || true' EXIT

log "writing ${ROW_COUNT} sentinel rows on DC to widen the gap"
# Use a loop that is cheap to abort; each row is its own transaction so the
# gap grows monotonically.
for i in $(seq 1 "${ROW_COUNT}"); do
  mysql_dc "INSERT INTO smoketest.pings (cluster_id) VALUES ('dc')"
done

dc_gtid="$(mysql_dc 'SELECT @@GLOBAL.gtid_executed')"
dr_gtid="$(mysql_dr 'SELECT @@GLOBAL.gtid_executed')"
log "DC gtid_executed=${dc_gtid}"
log "DR gtid_executed=${dr_gtid}"

missing="$(mysql_dc "SELECT GTID_SUBTRACT('${dc_gtid}', '${dr_gtid}')")"
log "GTID missing on DR: ${missing}"
if [[ -z "${missing}" ]]; then
  fatal "expected a non-empty gap between DC and DR; got nothing"
fi

log "triggering switchover (should be refused by preflight)"
kctl_dc patch csp "${CSP_NAME}" --type=merge \
  -p '{"spec":{"manualSwitchoverTarget":"promote-remote"}}'

log "waiting 30s for reconciler to record the preflight failure"
sleep 30

log "inspecting LastPreFlight"
snapshot="$(kctl_dc get csp "${CSP_NAME}" -o jsonpath='{.status.lastPreFlight}')"
echo "${snapshot}" | python3 -m json.tool || true

# C5 and/or C6 must fail. We accept either (C6 depends on timing — when
# CatchupTimeout is short C6 also fails; when longer the wait may not fire).
if ! echo "${snapshot}" | grep -q '"name":"C5_GTIDSubset"'; then
  fatal "FAIL: C5_GTIDSubset absent from LastPreFlight.Checks"
fi
if ! echo "${snapshot}" | grep -E -q '"(C5_GTIDSubset|C6_GTIDCatchup)"[^}]*"passed":false'; then
  fatal "FAIL: expected C5 or C6 to be passed=false; got ${snapshot}"
fi

if both_writable; then
  fatal "FAIL: split-brain detected"
fi

banner "D3 PASS — preflight blocked the flip on GTID drift; production incident is regression-guarded"
log "restoring replication"
mysql_dr "START REPLICA SQL_THREAD FOR CHANNEL 'dc-to-dr'"
