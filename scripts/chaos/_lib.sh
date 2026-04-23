#!/usr/bin/env bash
# Common helpers sourced by every chaos drill. Not meant to be run directly.

set -euo pipefail

CSP_NAME="${CSP_NAME:-dc-dr-policy}"
NAMESPACE="${NAMESPACE:-mysql-keeper-system}"

# Defaults — override per-drill via environment.
KUBECONFIG_DC="${KUBECONFIG_DC:-$HOME/.kube/config-dc}"
KUBECONFIG_DR="${KUBECONFIG_DR:-$HOME/.kube/config-dr}"
DC_MYSQL_DSN="${DC_MYSQL_DSN:?must set DC_MYSQL_DSN to a user:pw@tcp(host:port)/ DSN}"
DR_MYSQL_DSN="${DR_MYSQL_DSN:?must set DR_MYSQL_DSN to a user:pw@tcp(host:port)/ DSN}"

SKIP_RESTORE="${SKIP_RESTORE:-0}"
LOG_DIR="${LOG_DIR:-/tmp}"

# Per-drill log file. Caller sets DRILL_ID; the logfile path is stable so
# operators can `tail -F` it in a second terminal.
DRILL_ID="${DRILL_ID:-unknown}"
LOG_FILE="${LOG_DIR}/chaos-${DRILL_ID}-$(date -u +%Y%m%dT%H%M%SZ).log"
exec > >(tee -a "${LOG_FILE}") 2>&1

log()   { printf "[%s] %s\n" "$(date -u +%FT%TZ)" "$*"; }
fatal() { log "FATAL: $*"; exit 1; }

# kctl_dc / kctl_dr pick the right cluster.
kctl_dc() { kubectl --kubeconfig="${KUBECONFIG_DC}" "$@"; }
kctl_dr() { kubectl --kubeconfig="${KUBECONFIG_DR}" "$@"; }

# mysql_dc / mysql_dr shell out to the mysql client. We require that to be
# on PATH rather than exec'ing through kubectl because kubectl-exec loses
# the password prompt in many base images.
mysql_dc() { mysql --defaults-extra-file=<(printf "[client]\nuser=%s\npassword=%s\nhost=%s\nport=%s\n" \
  "$(extract_user "${DC_MYSQL_DSN}")" \
  "$(extract_pass "${DC_MYSQL_DSN}")" \
  "$(extract_host "${DC_MYSQL_DSN}")" \
  "$(extract_port "${DC_MYSQL_DSN}")") -Nse "$*"; }
mysql_dr() { mysql --defaults-extra-file=<(printf "[client]\nuser=%s\npassword=%s\nhost=%s\nport=%s\n" \
  "$(extract_user "${DR_MYSQL_DSN}")" \
  "$(extract_pass "${DR_MYSQL_DSN}")" \
  "$(extract_host "${DR_MYSQL_DSN}")" \
  "$(extract_port "${DR_MYSQL_DSN}")") -Nse "$*"; }

# extract_* parse user:pass@tcp(host:port)/ without requiring awk regex
# extensions. Breaks on passwords with special characters — staging drills
# should use a dedicated replication account.
extract_user() { echo "${1%%:*}"; }
extract_pass() { local t="${1#*:}"; echo "${t%%@*}"; }
extract_host() { local t="${1#*@tcp(}"; echo "${t%%:*}"; }
extract_port() { local t="${1#*@tcp(*:}"; echo "${t%%)*}"; }

# csp_phase echoes Status.Phase for the configured CSP on the DC side.
csp_phase() {
  kctl_dc get csp "${CSP_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || echo ""
}

# csp_current_step echoes Status.SwitchoverProgress.CurrentPhase.
csp_current_step() {
  kctl_dc get csp "${CSP_NAME}" -o jsonpath='{.status.switchoverProgress.currentPhase}' 2>/dev/null || echo ""
}

# both_writable returns 0 if both DC and DR are writable (split-brain).
both_writable() {
  local dc dr
  dc="$(mysql_dc 'SELECT @@read_only' 2>/dev/null || echo 1)"
  dr="$(mysql_dr 'SELECT @@read_only' 2>/dev/null || echo 1)"
  if [[ "${dc}" == "0" && "${dr}" == "0" ]]; then
    return 0
  fi
  return 1
}

# restore_baseline issues STOP REPLICA + SET GLOBAL read_only=ON on whatever
# cluster was temporarily modified by the drill. Safe to call multiple times.
restore_baseline() {
  if [[ "${SKIP_RESTORE}" == "1" ]]; then
    log "SKIP_RESTORE=1 — leaving state as-is for manual inspection"
    return
  fi
  log "restore: START REPLICA on DR, re-assert DC read_only=OFF / DR read_only=ON"
  mysql_dr "START REPLICA FOR CHANNEL 'dc-to-dr'" || true
  mysql_dc "SET GLOBAL read_only=OFF; SET GLOBAL super_read_only=OFF" || true
  mysql_dr "SET GLOBAL super_read_only=ON; SET GLOBAL read_only=ON" || true
}

# banner prints a drill-level divider.
banner() {
  log "==============================="
  log "$*"
  log "==============================="
}
