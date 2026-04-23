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
#
#   purge-gtid   The actual production incident end state: stop DR's IO
#                thread, write on DC, rotate + PURGE BINARY LOGS so the
#                GTIDs DR is missing no longer exist in DC's binlog, then
#                resume DR. Result: Last_IO_Errno=1236
#                (ER_MASTER_HAS_PURGED_REQUIRED_GTIDS).
#                The only clean recovery is re-seed DR from DC via
#                xtrabackup — that is out of scope for this drill, so run
#                scripts/local/pxc-down.sh + pxc-up.sh to reset.
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

  purge-gtid)
    echo ">> Reproducing ER_MASTER_HAS_PURGED_REQUIRED_GTIDS (Error 1236)"
    echo ">> This is the end state the production incident reached — after a flip"
    echo ">> when DR was behind, DC eventually purged the binlogs DR still needed."
    echo

    echo "== STEP 1: stop DR IO thread so no more events land"
    mysql_dr "STOP REPLICA IO_THREAD FOR CHANNEL '${CHANNEL}'"

    echo "== STEP 2: write rows on DC + rotate binlogs"
    # Each mysql_dc call launches a separate docker-exec, so we batch the
    # writes into a single multi-value INSERT. The cluster_id column is
    # VARCHAR(8), so the payload fits in "dc".
    values=$(printf '("dc"),%.0s' $(seq 1 200) | sed 's/,$//')
    mysql_dc "INSERT INTO smoketest.pings (cluster_id) VALUES ${values}"
    # FLUSH rotates — the old file carries the GTIDs DR still needs.
    mysql_dc "FLUSH BINARY LOGS"
    # Write more so the new file is current (retention measures file age).
    mysql_dc "INSERT INTO smoketest.pings (cluster_id) VALUES ${values}"

    echo "== STEP 3: purge DC binlogs — the GTIDs DR is missing are gone"
    # PURGE BINARY LOGS BEFORE NOW() deletes every file strictly older than
    # the current position. Since we rotated above, the file holding the
    # missing GTIDs is now "old" and gets removed.
    mysql_dc "PURGE BINARY LOGS BEFORE NOW()"

    dc_purged=$(mysql_dc "SELECT @@GLOBAL.gtid_purged")
    dc_executed=$(mysql_dc "SELECT @@GLOBAL.gtid_executed")
    dr_executed=$(mysql_dr "SELECT @@GLOBAL.gtid_executed")
    echo "   DC gtid_purged:   ${dc_purged}"
    echo "   DC gtid_executed: ${dc_executed}"
    echo "   DR gtid_executed: ${dr_executed}"

    echo "== STEP 4: start DR IO thread — the IO handshake asks DC for the"
    echo "           purged GTIDs and DC refuses, surfacing Error 1236."
    mysql_dr "START REPLICA IO_THREAD FOR CHANNEL '${CHANNEL}'"

    # Give the IO thread a moment to hit the error.
    sleep 4

    echo
    echo "== STEP 5: inspect DR IO state"
    docker exec -i keeper-pxc-dr mysql -uroot -p"${ROOT_PW}" -e "SHOW REPLICA STATUS\G" 2>/dev/null \
      | grep -E 'Replica_IO_Running|Last_IO_Errno|Last_IO_Error'

    # MySQL 8.0 stores two distinct values on an IO failure:
    #   - Last_IO_Errno: client-side transport code (often 13114)
    #   - Last_IO_Error: message string from the source server, which for
    #     our case begins "Got fatal error 1236 from source ..."
    # The production incident identifier is 1236 in the message, not the
    # transport errno, so we match on the message body.
    io_err_msg=$(mysql_dr "SELECT last_error_message FROM performance_schema.replication_connection_status WHERE channel_name='${CHANNEL}'")
    echo
    if echo "${io_err_msg}" | grep -qE 'error 1236|purged required binary logs'; then
      echo "✅ Reproduced ER_MASTER_HAS_PURGED_REQUIRED_GTIDS."
      echo "   This is the exact failure mode preflight C5 + C6 prevent when the"
      echo "   controller refuses a flip while DR is behind. The only recovery"
      echo "   from this state in production is xtrabackup re-seed of DR."
    else
      echo "⚠  Expected 'error 1236' or 'purged required binary logs' in the"
      echo "   Last_IO_Error message; got: ${io_err_msg}"
      echo "   Binlog may still have the missing GTIDs. Try ROWS=500 and re-run."
    fi
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
    echo "Usage: $0 {lag|recover|both-ro|both-ro-clear|purge-gtid|status}"
    exit 2
    ;;
esac
