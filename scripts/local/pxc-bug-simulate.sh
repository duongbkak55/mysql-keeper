#!/usr/bin/env bash
# pxc-bug-simulate.sh — reproduce the conditions that caused the
# production GTID divergence incident on the local PXC staging.
#
# Subcommands:
#
#   lag          Stop DR's SQL applier + write N rows on DC → DR is now
#                missing GTIDs. Running preflight after this must FAIL C5/C6.
#                (This is the root cause that triggered the incident.)
#
#   recover      Resume the DR SQL applier so replication catches up.
#
#   both-ro      Set read_only=ON on DC. In real 3-node PXC this happens
#                automatically when the cluster loses quorum; we reproduce
#                the observable state so the controller's "both RO →
#                refuse auto-failover" guard can be exercised.
#
#   both-ro-clear  Undo the previous command.
#
#   status       Print a one-line summary of each cluster.
set -euo pipefail

ROOT_PW="stagingpass"
CHANNEL="dc-to-dr"
ROWS="${ROWS:-200}"

mysql_dc() { docker exec -i keeper-pxc-dc mysql -uroot -p"${ROOT_PW}" -Nse "$*" 2>/dev/null; }
mysql_dr() { docker exec -i keeper-pxc-dr mysql -uroot -p"${ROOT_PW}" -Nse "$*" 2>/dev/null; }

case "${1:-}" in
  lag)
    echo ">> Stopping DR SQL applier (IO thread stays on so GTIDs buffer in relay log)"
    mysql_dr "STOP REPLICA SQL_THREAD FOR CHANNEL '${CHANNEL}'"

    echo ">> Writing ${ROWS} rows on DC to widen the DC / DR GTID gap"
    for i in $(seq 1 "${ROWS}"); do
      mysql_dc "INSERT INTO smoketest.pings (cluster_id) VALUES ('dc')" >/dev/null
    done

    dc_gtid=$(mysql_dc 'SELECT @@GLOBAL.gtid_executed')
    dr_gtid=$(mysql_dr 'SELECT @@GLOBAL.gtid_executed')
    missing=$(mysql_dc "SELECT GTID_SUBTRACT('${dc_gtid}', '${dr_gtid}')")
    echo "   DC gtid_executed: ${dc_gtid}"
    echo "   DR gtid_executed: ${dr_gtid}"
    echo "   DC - DR:          ${missing}"
    echo
    echo "Run: scripts/local/pxc-preflight.sh — C5_GTIDSubset + C6_GTIDCatchup must FAIL."
    ;;

  recover)
    echo ">> Starting DR SQL applier to drain relay log"
    mysql_dr "START REPLICA SQL_THREAD FOR CHANNEL '${CHANNEL}'"
    echo "   Allow a few seconds for catch-up, then re-run preflight — expect OK."
    ;;

  both-ro)
    echo ">> Forcing read_only=ON on DC (simulates PXC quorum loss)"
    mysql_dc "SET GLOBAL super_read_only=ON"
    mysql_dc "SET GLOBAL read_only=ON"
    echo
    echo "DC @@read_only=$(mysql_dc 'SELECT @@read_only')"
    echo "DR @@read_only=$(mysql_dr 'SELECT @@read_only')"
    echo
    echo "Both clusters are now RO. The controller's EvaluateSwitchover must"
    echo "return Blocker=both_readonly and refuse auto-failover."
    echo "Run unit test to confirm:"
    echo "  go test -run TestDecide_BothReadOnlyBlocksAuto ./internal/controller/..."
    ;;

  both-ro-clear)
    echo ">> Clearing read_only on DC"
    mysql_dc "SET GLOBAL read_only=OFF"
    mysql_dc "SET GLOBAL super_read_only=OFF"
    ;;

  status)
    printf 'DC: read_only=%s  wsrep_cluster_status=%s  gtid_executed=%s\n' \
      "$(mysql_dc 'SELECT @@read_only')" \
      "$(mysql_dc "SHOW STATUS LIKE 'wsrep_cluster_status'" | awk '{print $2}')" \
      "$(mysql_dc 'SELECT @@GLOBAL.gtid_executed' | tr -d '\n')"
    printf 'DR: read_only=%s  wsrep_cluster_status=%s  gtid_executed=%s\n' \
      "$(mysql_dr 'SELECT @@read_only')" \
      "$(mysql_dr "SHOW STATUS LIKE 'wsrep_cluster_status'" | awk '{print $2}')" \
      "$(mysql_dr 'SELECT @@GLOBAL.gtid_executed' | tr -d '\n')"
    printf 'DR replication: IO=%s SQL=%s\n' \
      "$(mysql_dr "SELECT service_state FROM performance_schema.replication_connection_status WHERE channel_name='${CHANNEL}'")" \
      "$(mysql_dr "SELECT service_state FROM performance_schema.replication_applier_status WHERE channel_name='${CHANNEL}'")"
    ;;

  *)
    echo "Usage: $0 {lag|recover|both-ro|both-ro-clear|status}"
    exit 2
    ;;
esac
