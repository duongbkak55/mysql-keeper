#!/usr/bin/env bash
# pxc-up.sh — bring up 2 PXC clusters + configure async replication +
# bootstrap keeper schema. Idempotent: safe to re-run.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE="${SCRIPT_DIR}/pxc-compose.yml"

ROOT_PW="stagingpass"
REPL_USER="keeper_repl"
REPL_PW="keeper_repl_pw"
CHANNEL="dc-to-dr"

log() { printf "\033[1;34m>>\033[0m %s\n" "$*"; }

log "docker compose up (~60s — pulling image + bootstrap Galera)"
docker compose -f "${COMPOSE}" up -d

log "Waiting for DC + DR to become healthy (up to 180s)"
for svc in keeper-pxc-dc keeper-pxc-dr; do
  for i in $(seq 1 60); do
    status=$(docker inspect -f '{{.State.Health.Status}}' "$svc" 2>/dev/null || echo "starting")
    [[ "$status" == "healthy" ]] && break
    sleep 3
  done
  if [[ "$status" != "healthy" ]]; then
    log "ERROR: ${svc} never became healthy — last status=${status}"
    docker logs --tail=80 "${svc}" || true
    exit 1
  fi
done

mysql_dc() { docker exec -i keeper-pxc-dc mysql -uroot -p"${ROOT_PW}" -Nse "$*"; }
mysql_dr() { docker exec -i keeper-pxc-dr mysql -uroot -p"${ROOT_PW}" -Nse "$*"; }

log "Sanity: wsrep_cluster_status on DC should be 'Primary'"
wsrep_state=$(mysql_dc "SHOW STATUS LIKE 'wsrep_cluster_status'" | awk '{print $2}')
if [[ "${wsrep_state}" != "Primary" ]]; then
  log "ERROR: DC wsrep_cluster_status=${wsrep_state} (expected Primary)"
  exit 1
fi
log "DC wsrep_cluster_status=Primary ✓"

log "Creating replication user + smoke-test schema on DC"
mysql_dc "CREATE USER IF NOT EXISTS '${REPL_USER}'@'%' IDENTIFIED WITH mysql_native_password BY '${REPL_PW}'"
mysql_dc "GRANT REPLICATION SLAVE ON *.* TO '${REPL_USER}'@'%'"
mysql_dc "FLUSH PRIVILEGES"
mysql_dc "CREATE DATABASE IF NOT EXISTS smoketest"
mysql_dc "CREATE TABLE IF NOT EXISTS smoketest.pings (id BIGINT AUTO_INCREMENT PRIMARY KEY, cluster_id VARCHAR(8) NOT NULL, ts DATETIME(6) NOT NULL DEFAULT NOW(6))"
mysql_dc "CREATE DATABASE IF NOT EXISTS keeper"
mysql_dc "CREATE TABLE IF NOT EXISTS keeper.probe (id INT NOT NULL DEFAULT 1, ts BIGINT NOT NULL, node VARCHAR(64) NOT NULL, PRIMARY KEY (id)) ENGINE=InnoDB"

# Wait briefly for DR to be responsive beyond just mysqladmin ping — first
# query after bootstrap sometimes times out.
sleep 2

DC_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' keeper-pxc-dc)
log "DC container IP on compose network: ${DC_IP}"

log "Configuring async replication on DR (DC → DR via channel '${CHANNEL}')"
mysql_dr "STOP REPLICA FOR CHANNEL '${CHANNEL}'" 2>/dev/null || true
mysql_dr "RESET REPLICA ALL FOR CHANNEL '${CHANNEL}'" 2>/dev/null || true
mysql_dr "CHANGE REPLICATION SOURCE TO SOURCE_HOST='${DC_IP}', SOURCE_PORT=3306, SOURCE_USER='${REPL_USER}', SOURCE_PASSWORD='${REPL_PW}', SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1, SOURCE_HEARTBEAT_PERIOD=2 FOR CHANNEL '${CHANNEL}'"
mysql_dr "START REPLICA FOR CHANNEL '${CHANNEL}'"

log "Enforcing read_only on DR (replica side)"
mysql_dr "SET GLOBAL super_read_only=ON"
mysql_dr "SET GLOBAL read_only=ON"

log "Waiting for replication to report Running (30s)"
for i in $(seq 1 30); do
  io=$(mysql_dr "SELECT service_state FROM performance_schema.replication_connection_status WHERE channel_name='${CHANNEL}'" | head -1)
  sql=$(mysql_dr "SELECT service_state FROM performance_schema.replication_applier_status WHERE channel_name='${CHANNEL}'" | head -1)
  if [[ "${io}" == "ON" && "${sql}" == "ON" ]]; then
    break
  fi
  sleep 1
done
if [[ "${io}" != "ON" || "${sql}" != "ON" ]]; then
  log "ERROR: replication did not reach Running (IO=${io} SQL=${sql})"
  exit 1
fi

log ""
log "Local PXC staging ready."
log ""
log "Host endpoints:"
log "  DC: 127.0.0.1:33011 (root / ${ROOT_PW})"
log "  DR: 127.0.0.1:33012 (root / ${ROOT_PW})"
log ""
log "DSNs for the preflight CLI:"
log "  export DC_DSN='root:${ROOT_PW}@tcp(127.0.0.1:33011)/?parseTime=true'"
log "  export DR_DSN='root:${ROOT_PW}@tcp(127.0.0.1:33012)/?parseTime=true'"
log ""
log "Next steps:"
log "  scripts/local/pxc-preflight.sh              # run preflight, expect PASS"
log "  scripts/local/pxc-bug-simulate.sh lag       # inject GTID lag"
log "  scripts/local/pxc-preflight.sh              # run preflight, expect FAIL (C5/C6)"
log "  scripts/local/pxc-bug-simulate.sh recover   # resume replication"
log "  scripts/local/pxc-bug-simulate.sh both-ro   # simulate quorum loss on DC"
log "  scripts/local/pxc-down.sh                   # tear down"
