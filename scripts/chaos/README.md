# Chaos scripts (Tier D)

Runnable drills that exercise the switchover engine under real-world failure
conditions on a staging cluster. Each script is designed to be launched by
an operator, not by CI — unlike Tiers A–C the chaos tier needs human
judgement at the end (look at the dashboard, confirm no split-brain, sign
off, restore state).

## Before running any drill

1. **Environment:** staging cluster with two real PXC clusters (3 nodes each
   is the minimum for wsrep quorum). **Never run these in production.**
2. **Snapshot:** xtrabackup on both sides within the last 4h.
3. **Monitoring:** have Grafana open on the mysql-keeper + PXC dashboards,
   and the Prometheus Alertmanager visible so you can confirm
   `MysqlKeeperBothClustersWritable` does NOT fire.
4. **On-call:** post to the DBRE channel before you start; the drill should
   be visible to the team.
5. **Rollback plan:** each script has a matching `restore` step at the
   bottom; if it is skipped the staging environment will stay fenced.

## Drill catalogue

| ID | Script                           | Maps scenario(s) | Purpose                                                                |
| -- | -------------------------------- | ---------------- | ---------------------------------------------------------------------- |
| D1 | `kill-controller-mid-promote.sh` | S6 / S7 / 4.6    | Controller pod dies while SwitchoverProgress.CurrentPhase=Promote      |
| D2 | `partition-dc-dr.sh`             | S3 / S15 / 4.1   | Network partition between DC and DR during / before switchover         |
| D3 | `inject-lag-then-trigger.sh`     | S1 — root cause  | Stop DR applier, write on DC, confirm preflight blocks the flip        |
| D4 | `binlog-retention-purge.sh`      | S12 / C10        | Short binlog retention + DC outage → reverse replica surfaces Error 1236 |
| D5 | `dual-controller-promote.sh`     | S15 / 4.7        | Two controllers race on the keeper.leader table                         |

Each script is idempotent: it prints status after every phase, leaves a
log file under `/tmp/chaos-<id>-<timestamp>.log`, and the final step
restores the baseline state.

## Pass / fail criteria

Reported at the end of each script. If the result differs from the
expected outcome, STOP and page DBRE — do not attempt another drill
against the same environment.

## Environment variables

All scripts read:

- `CSP_NAME` (default `dc-dr-policy`) — name of the CSP to act on.
- `KUBECONFIG_DC` / `KUBECONFIG_DR` — kubeconfigs for the two clusters.
- `DC_MYSQL_DSN` / `DR_MYSQL_DSN` — admin DSNs (user with SUPER).
- `SKIP_RESTORE=1` — only in rare cases when you want to keep the broken
  state for manual investigation. Default restores.

## Relationship to the dr-drill runbook

The dr-drill.md runbook describes the planned-switchover path: happy case,
no injected faults. Tier D adds failures on top. Run a planned drill first
to confirm the environment is healthy, then a Tier D drill to confirm the
controller copes when something goes wrong.
