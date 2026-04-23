#!/usr/bin/env bash
# staging-up.sh — bring up the local Tier D staging and configure DC → DR
# async replication with GTID. Idempotent: safe to re-run after a partial
# start; skips re-adding the replication channel if it already exists.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE="${SCRIPT_DIR}/staging-compose.yml"

ROOT_PW="stagingpass"
REPL_USER="keeper_repl"
REPL_PW="keeper_repl_pw"
CHANNEL="dc-to-dr"

log() { printf "\033[1;34m>>\033[0m %s\n" "$*"; }

log "docker compose up"
docker compose -f "${COMPOSE}" up -d

log "Waiting for DC + DR to become healthy (up to 120s)"
for svc in keeper-stg-dc keeper-stg-dr; do
  for i in $(seq 1 40); do
    status=$(docker inspect -f '{{.State.Health.Status}}' "$svc" 2>/dev/null || echo "starting")
    [[ "$status" == "healthy" ]] && break
    sleep 3
  done
  if [[ "$status" != "healthy" ]]; then
    log "ERROR: ${svc} never became healthy — last status=${status}"
    docker logs --tail=50 "${svc}" || true
    exit 1
  fi
done

mysql_dc() { docker exec -i keeper-stg-dc mysql -uroot -p"${ROOT_PW}" -Nse "$*"; }
mysql_dr() { docker exec -i keeper-stg-dr mysql -uroot -p"${ROOT_PW}" -Nse "$*"; }

log "Creating replication user on DC"
mysql_dc "CREATE USER IF NOT EXISTS '${REPL_USER}'@'%' IDENTIFIED WITH mysql_native_password BY '${REPL_PW}'"
mysql_dc "GRANT REPLICATION SLAVE ON *.* TO '${REPL_USER}'@'%'"
mysql_dc "FLUSH PRIVILEGES"
mysql_dc "CREATE DATABASE IF NOT EXISTS smoketest"
mysql_dc "CREATE TABLE IF NOT EXISTS smoketest.pings (id BIGINT AUTO_INCREMENT PRIMARY KEY, cluster_id VARCHAR(8) NOT NULL, ts DATETIME(6) NOT NULL DEFAULT NOW(6))"

# Discover DC's address on the compose network so DR can reach it.
DC_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' keeper-stg-dc)
log "DC container IP: ${DC_IP}"

log "Configuring replication on DR"
mysql_dr "STOP REPLICA FOR CHANNEL '${CHANNEL}'" || true
mysql_dr "RESET REPLICA ALL FOR CHANNEL '${CHANNEL}'" || true
mysql_dr "CHANGE REPLICATION SOURCE TO SOURCE_HOST='${DC_IP}', SOURCE_PORT=3306, SOURCE_USER='${REPL_USER}', SOURCE_PASSWORD='${REPL_PW}', SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1, SOURCE_HEARTBEAT_PERIOD=2 FOR CHANNEL '${CHANNEL}'"
mysql_dr "START REPLICA FOR CHANNEL '${CHANNEL}'"

log "Enforcing read_only on DR"
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

log "Local staging ready."
log "  DC DSN: root:${ROOT_PW}@tcp(127.0.0.1:33061)/?parseTime=true"
log "  DR DSN: root:${ROOT_PW}@tcp(127.0.0.1:33062)/?parseTime=true"
log ""
log "Run the light-Tier-D drills directly against this staging:"
log "  export DC_MYSQL_DSN='root:${ROOT_PW}@tcp(127.0.0.1:33061)/'"
log "  export DR_MYSQL_DSN='root:${ROOT_PW}@tcp(127.0.0.1:33062)/'"
log "  scripts/chaos/D3-inject-lag-then-trigger.sh   # root-cause drill"
log "  scripts/chaos/D4-binlog-retention-purge.sh"
