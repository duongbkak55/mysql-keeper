# Runbook: Chaos / DR drills (Tier D)

**Audience:** DBRE on-call who owns the staging environment.
**Not for production.** Every script here induces the exact failure modes
the controller is designed to survive — safe only on a cluster you control.

## When to run

- Before enabling `autoFailover=true` in a new environment (go-live gate).
- After any material change to the engine (Sprint 1/2 scope), at least
  D1 + D3 + D5 once each.
- Quarterly, as part of DRE's operational exercise calendar.
- On demand when a new team member is onboarding — driving one of these
  drills is the fastest way to learn the controller.

## Drill catalogue

| ID | Script                                               | Duration | What it proves                                                       |
| -- | ---------------------------------------------------- | -------- | -------------------------------------------------------------------- |
| D1 | `scripts/chaos/D1-kill-controller-mid-promote.sh`    | ~12 min  | Checkpoint survives pod restart; Phase → Degraded; no split-brain    |
| D2 | `scripts/chaos/D2-partition-dc-dr.sh`                | ~6 min   | PreFlight C1 refuses the flip when DR is unreachable                  |
| D3 | `scripts/chaos/D3-inject-lag-then-trigger.sh`        | ~4 min   | PreFlight C5/C6 refuses the flip when DR is behind (root-cause drill) |
| D4 | `scripts/chaos/D4-binlog-retention-purge.sh`         | ~15 min  | Confirms Error 1236 failure mode surfaces cleanly when it does occur  |
| D5 | `scripts/chaos/D5-dual-controller-promote.sh`        | ~5 min   | keeper.leader lease resolves dual-controller races                    |

## Common prerequisites

All scripts source `scripts/chaos/_lib.sh`, which requires these environment
variables:

```bash
export KUBECONFIG_DC=~/.kube/config-dc-staging
export KUBECONFIG_DR=~/.kube/config-dr-staging
export DC_MYSQL_DSN='admin:password@tcp(10.10.0.100:3306)/'
export DR_MYSQL_DSN='admin:password@tcp(10.20.0.100:3306)/'
export CSP_NAME=dc-dr-policy                 # optional; default
```

Additionally:

- `mysql` client on `$PATH` (we shell out for the admin queries).
- `kubectl` context for both DC and DR.
- For D2: Chaos Mesh installed in DC's `mysql-keeper-system` namespace
  (`helm install chaos-mesh`).
- For D4: expect the staging environment to need an xtrabackup re-seed
  afterwards. Book an hour of DBRE time.

## Execution checklist (every drill)

1. Post an announcement in `#dbre-ops` naming the drill ID, expected
   duration, and environment.
2. Snapshot xtrabackup on both clusters.
3. Confirm `ConditionSplitBrainSafe=True` on the CSP:
   `kubectl get csp $CSP_NAME -o jsonpath='{.status.conditions}'`
4. Open Grafana on the mysql-keeper dashboard and keep it visible.
5. Run the drill. Tail the log file printed at the top of the script
   in a second terminal.
6. Copy the PASS/FAIL banner to the incident channel as the record of
   the run.
7. On failure: do NOT run another drill. Follow the matching recovery
   runbook (`switchover-stuck.md` or `split-brain-recovery.md`).

## D1 — Kill controller mid-Promote

Run: `scripts/chaos/D1-kill-controller-mid-promote.sh`

**What happens:**
1. Manual switchover triggered.
2. Script polls for `CurrentPhase=Promote` and kills all controller pods.
3. A new controller pod starts up but finds SwitchoverProgress with a
   stale StartedAt older than `ResumeStuckTimeout`.
4. Reconciler abandons the attempt and flips `Phase=Degraded`.

**Pass:** Phase=Degraded within ResumeStuckTimeout + 30s, no split-brain.
**Fail actions:** Investigate why the reconciler did not abandon — check
log for `Abandoning stuck switchover`. If both clusters became writable,
execute [split-brain-recovery.md](split-brain-recovery.md) **now**.

## D2 — Network partition

Run: `scripts/chaos/D2-partition-dc-dr.sh` with `DR_REMOTE_IP` set to the
IP the DC cluster uses to reach DR MySQL.

**What happens:**
1. Chaos Mesh applies a partition blocking DC controller → DR MySQL:3306.
2. Manual switchover triggered.
3. PreFlight C1 attempts to reach DR, fails with a connection error.
4. The flip is refused; Phase eventually returns to Monitoring.
5. Chaos rule is removed on script exit.

**Pass:** `Status.LastPreFlight.Checks` contains `C1_RemoteReachableReadOnly`
with `passed=false`; no switchover completed.
**Fail actions:** If the switchover somehow completed despite the partition,
run `switchover-stuck.md` — but this is a severity-1 defect, file it
immediately.

## D3 — Injected GTID lag (root-cause drill)

Run: `scripts/chaos/D3-inject-lag-then-trigger.sh`

**What happens:**
1. DR SQL thread is stopped.
2. Script writes `ROW_COUNT` (default 500) sentinel rows on DC.
3. GTID_SUBTRACT confirms a real gap.
4. Manual switchover triggered.
5. PreFlight C5 / C6 refuses.

**Pass:** C5_GTIDSubset or C6_GTIDCatchup `passed=false` in LastPreFlight.
**Fail actions:** This is the production incident regression. If the flip
proceeds, the controller is **broken** — fence immediately, roll back to a
previous release.

## D4 — Binlog retention purge

Run: `scripts/chaos/D4-binlog-retention-purge.sh`

**What happens:**
1. DR retention shortened to 5 minutes.
2. Happy-path switchover DC → DR.
3. DC controller is scaled to 0 for 10 minutes (past retention).
4. DC brought back and attempts to re-subscribe to DR.
5. Expected Error 1236 surfaced cleanly.

**Pass:** IO thread on DC reports `last_error_number=1236` and the operator
follows [split-brain-recovery.md](split-brain-recovery.md) re-seed flow.
**Note:** This drill leaves staging in a non-baseline state. Plan the
re-seed time into the drill slot.

## D5 — Dual controller race

Run: `scripts/chaos/D5-dual-controller-promote.sh`

**What happens:**
1. Baseline lease state recorded (`keeper.leader` row).
2. DR controller bounced to age its heartbeat.
3. Manual switchover triggered on BOTH sides simultaneously.
4. One controller takes over with epoch+1; the other backs off.

**Pass:** Exactly one writable cluster, `keeper.leader.epoch > baseline`.
**Fail actions:** If both become writable → split-brain, execute
[split-brain-recovery.md](split-brain-recovery.md). If neither, check
controller logs for lease acquisition errors.

## After every drill

- Attach the log file (`/tmp/chaos-<id>-<ts>.log`) to the run record.
- Update the DR drill calendar with the outcome.
- If any failure: file an issue, link the log, page the on-call DBRE.
