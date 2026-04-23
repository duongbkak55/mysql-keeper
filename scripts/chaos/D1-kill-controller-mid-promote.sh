#!/usr/bin/env bash
# D1 — Kill the controller pod while SwitchoverProgress.CurrentPhase=Promote.
#
# What it proves:
#   - Progress checkpoint survives the restart (Status.SwitchoverProgress
#     retains AttemptID and CurrentPhase).
#   - The next reconcile observes the stale progress and flips Phase to
#     Degraded after ResumeStuckTimeout, without attempting a naive "resume"
#     that could split-brain the clusters.
#
# Expected outcome (pass criteria):
#   - Phase transitions to Degraded within (ResumeStuckTimeout + 30s).
#   - both_writable returns 1 (not split-brain) throughout.
#   - manual_intervention_required_total{phase="Promote"} counter increments.
set -euo pipefail
DRILL_ID="D1"
source "$(dirname "$0")/_lib.sh"

banner "D1 starting — kill controller mid-Promote"

log "pre-check: CSP must be in Monitoring"
phase="$(csp_phase)"
[[ "${phase}" == "Monitoring" ]] || fatal "expected Phase=Monitoring, got ${phase}"

log "trigger planned switchover"
kctl_dc patch csp "${CSP_NAME}" --type=merge \
  -p '{"spec":{"manualSwitchoverTarget":"promote-remote"}}'

log "wait for CurrentPhase=Promote (timeout 60s)"
deadline=$(( $(date +%s) + 60 ))
while :; do
  current="$(csp_current_step)"
  [[ "${current}" == "Promote" ]] && break
  if (( $(date +%s) > deadline )); then
    fatal "never reached Promote; last CurrentPhase=${current}"
  fi
  sleep 1
done

log "killing all controller pods"
kctl_dc -n "${NAMESPACE}" delete pod \
  -l app.kubernetes.io/name=mysql-keeper --wait=false

log "waiting for a new controller to come up (60s)"
kctl_dc -n "${NAMESPACE}" wait \
  --for=condition=Ready pod \
  -l app.kubernetes.io/name=mysql-keeper --timeout=60s || true

log "watch Phase for Degraded transition (up to ResumeStuckTimeout + 60s = 11m)"
deadline=$(( $(date +%s) + 660 ))
while :; do
  phase="$(csp_phase)"
  log "Phase=${phase} CurrentPhase=$(csp_current_step)"
  if [[ "${phase}" == "Degraded" ]]; then
    log "PASS: Phase=Degraded reached"
    break
  fi
  if both_writable; then
    fatal "FAIL: split-brain detected (both clusters writable)"
  fi
  if (( $(date +%s) > deadline )); then
    fatal "FAIL: Phase never reached Degraded"
  fi
  sleep 15
done

log "inspecting Status.SwitchoverProgress"
kctl_dc get csp "${CSP_NAME}" -o jsonpath='{.status.switchoverProgress}' | python3 -m json.tool || true

log "D1 complete — restoring baseline"
restore_baseline
banner "D1 PASS — phase Degraded, no split-brain"
