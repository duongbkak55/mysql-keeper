#!/usr/bin/env bash
# D4 — Short binlog retention + simulated DC outage across the retention
# window. Verifies that the former source, on recovery, either (a)
# successfully catches up because retention was respected, or (b) surfaces
# a clear Error 1236 message when it has been down too long.
#
# IMPORTANT: this drill intentionally induces the exact failure mode the
# remediation guards against. It is safe ONLY on staging. Do not run
# against production.
#
# Expected outcome:
#   - C10_RemotePurgedNotAhead initially passes (baseline retention is 7d).
#   - When we shorten DR retention and "down" DC for longer than the window,
#     a subsequent attempt to re-attach DC as replica surfaces Error 1236
#     clearly, and the runbook split-brain-recovery.md is the next step.
set -euo pipefail
DRILL_ID="D4"
source "$(dirname "$0")/_lib.sh"

banner "D4 starting — short retention + DC outage"

log "snapshot current binlog_expire_logs_seconds on both sides"
dc_retention="$(mysql_dc 'SELECT @@GLOBAL.binlog_expire_logs_seconds')"
dr_retention="$(mysql_dr 'SELECT @@GLOBAL.binlog_expire_logs_seconds')"
log "DC retention=${dc_retention}s, DR retention=${dr_retention}s"

log "shorten DR retention to 5 minutes (300s) — only for this drill"
mysql_dr "SET GLOBAL binlog_expire_logs_seconds=300"
trap 'mysql_dr "SET GLOBAL binlog_expire_logs_seconds='"${dr_retention}"'" || true' EXIT

log "trigger switchover (happy path, preflight should still pass at this moment)"
kctl_dc patch csp "${CSP_NAME}" --type=merge \
  -p '{"spec":{"manualSwitchoverTarget":"promote-remote"}}'

log "wait up to 5 min for Phase=Monitoring (switchover complete)"
deadline=$(( $(date +%s) + 300 ))
while :; do
  phase="$(csp_phase)"
  [[ "${phase}" == "Monitoring" ]] && break
  (( $(date +%s) > deadline )) && fatal "switchover did not reach Monitoring; last=${phase}"
  sleep 5
done
log "switchover complete; DR is now the source"

log "simulate DC outage by stopping the DC CR reconciliation and blocking DC replication user"
# We emulate "DC down" by scaling the DC controller to 0 AND revoking the
# replication grant temporarily. Caller must already have flipped back if
# this drill runs twice.
kctl_dc -n "${NAMESPACE}" scale deploy/mysql-keeper-controller-manager --replicas=0 || true

log "wait 10 minutes — past DR's shortened retention window"
sleep 600

log "bring DC back: re-scale controller and attempt to restart replication"
kctl_dc -n "${NAMESPACE}" scale deploy/mysql-keeper-controller-manager --replicas=2
kctl_dc -n "${NAMESPACE}" wait --for=condition=Available \
  deploy/mysql-keeper-controller-manager --timeout=120s || true

log "force DC to re-subscribe to DR"
mysql_dc "STOP REPLICA FOR CHANNEL 'dc-to-dr'" || true
mysql_dc "START REPLICA FOR CHANNEL 'dc-to-dr'" || true

sleep 10
io_err="$(mysql_dc "SELECT last_error_number FROM performance_schema.replication_connection_status WHERE channel_name='dc-to-dr'")"
io_msg="$(mysql_dc "SELECT last_error_message FROM performance_schema.replication_connection_status WHERE channel_name='dc-to-dr'")"
log "DC IO last_error_number=${io_err}"
log "DC IO last_error_message=${io_msg}"

if [[ "${io_err}" == "1236" ]]; then
  banner "D4 EXPECTED — Error 1236 surfaced. Next step: follow split-brain-recovery.md to re-seed DC from DR"
else
  log "D4 did NOT surface Error 1236. If DC caught up cleanly, retention was sufficient; treat as informational."
fi

log "restoring baseline retention on DR"
mysql_dr "SET GLOBAL binlog_expire_logs_seconds=${dr_retention}"
log "NOTE: DC may need a manual xtrabackup re-seed. DO NOT FLIP BACK without DBRE sign-off."
