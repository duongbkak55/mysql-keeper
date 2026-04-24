# Go-live DR drill plan

**Audience:** DBRE + platform + on-call taking mysql-keeper v0.2.x to
production for the first time. Run this end-to-end on a realistic
staging environment that mirrors production in cluster topology,
network, observability, and MANO integration.

**Duration:** ~1–2 days of elapsed time, with planned pauses between
drills to observe replication catch-up.

**Exit criteria:** every section below is signed off by the named owner
(DBRE, platform, on-call rep). Any FAIL blocks `autoFailover=true` in
production until remediated.

---

## Section 0 — Staging environment spec

| Component             | Required specification                                         | Verified by |
| --------------------- | -------------------------------------------------------------- | ----------- |
| DC Kubernetes cluster | ≥ 3 worker nodes, matches prod K8s version                     | Platform    |
| DR Kubernetes cluster | Separate cluster, cross-region if prod is                     | Platform    |
| DC PXC                | 3-node Percona XtraDB Cluster 8.0 via PXC operator            | DBRE        |
| DR PXC                | 3-node PXC 8.0, async replica of DC                           | DBRE        |
| ProxySQL DC / DR      | ≥ 3 instances each, admin+client creds rotated from defaults  | DBRE        |
| MANO                  | Same endpoint/auth as prod; mysql-keeper account provisioned  | Platform    |
| Cross-cluster network | DC controller can reach DR MySQL:3306 and vice versa          | Platform    |
| Observability         | Prometheus scraping keeper metrics; Grafana dashboards loaded | On-call     |
| Alerting              | `config/observability/prometheus_rules.yaml` applied          | On-call     |

Sign-off baseline state required before proceeding:

- [ ] `ConditionSplitBrainSafe=True` on both CSPs.
- [ ] `mysql_keeper_cluster_healthy == 1` on both scopes (local+remote, both roles).
- [ ] `binlog_expire_logs_seconds ≥ 604800` on both PXC clusters.
- [ ] `SHOW REPLICA STATUS` on DR: IO + SQL Running, zero errors.
- [ ] GTID drift < 1s under nominal load.

---

## Section 1 — Pre-drill automated verification

Run these from a shell that can reach both clusters. No state changes.

```bash
# 1.1 Controller image in use
for ctx in dc dr; do
  kubectl --context=$ctx -n mysql-keeper-system get deploy \
    mysql-keeper-controller-manager \
    -o jsonpath='{"image: "}{.spec.template.spec.containers[0].image}{"\n"}'
done
```

Both should be identical and pinned to a specific `v0.2.x` tag (not `latest`).
**FAIL** if tags differ or `:latest` appears.

```bash
# 1.2 CRD schema matches image version
CRD_VER=$(kubectl --context=dc get crd clusterswitchpolicies.mysql.keeper.io \
  -o jsonpath='{.metadata.annotations.controller-gen\.kubebuilder\.io/version}')
echo "CRD generator: ${CRD_VER}"
```

**FAIL** if the CRD predates your image build.

```bash
# 1.3 Keeper probe + leader tables exist on writable side
kubectl --context=dc exec -n percona-xtradb pxc-dc-0 -- \
  mysql -uroot -p"${DC_ROOT_PW}" -e \
  "SHOW TABLES FROM keeper; SELECT COUNT(*) FROM keeper.leader"
```

Expect `probe`, `leader` tables; `keeper.leader` has exactly 1 row.

```bash
# 1.4 CI gates
gh run list --repo duongbkak55/mysql-keeper --workflow=integration --limit 1
gh run list --repo duongbkak55/mysql-keeper --workflow=e2e --limit 1
```

Both last runs must be `success`. **FAIL** otherwise.

---

## Section 2 — Planned switchover drill (DC → DR)

Happy path. This must succeed cleanly before any fault injection.

### 2.1 Pre-conditions

- [ ] Traffic on DC (reads + writes). Record baseline QPS from the
      ProxySQL `mysql_connection_stats`.
- [ ] `Spec.AutoFailover=false` on both CSPs for the drill duration.
- [ ] On-call channel notified. Drill window announced.

### 2.2 Execute

```bash
kubectl --context=dc patch clusterswitchpolicy dc-dr-policy \
  --type=merge \
  -p '{"spec":{"manualSwitchoverTarget":"promote-remote"}}'
```

Stopwatch starts.

### 2.3 Observe the phase sequence

```bash
watch -n 1 'kubectl --context=dc get clusterswitchpolicy dc-dr-policy \
  -o json | jq ".status | {phase, switchoverProgress: .switchoverProgress.currentPhase, active: .activeCluster}"'
```

Expected progression within wall-clock budgets:

| Phase         | Budget | Check                                                      |
| ------------- | ------ | ---------------------------------------------------------- |
| PreFlight     | < 30s  | `Status.LastPreFlight.Checks` all `passed=true` (hard)     |
| Fence         | < 10s  | DC `@@read_only=1` + CRD isSource flipped                  |
| Promote       | < 60s  | DR `@@read_only=0` + MANO LCM op COMPLETED                 |
| Routing       | < 15s  | All ProxySQL `runtime_mysql_servers` point at DR writer   |
| ReverseReplica| < 15s  | DC now has `SHOW REPLICA STATUS` pointing at DR           |
| Verify        | < 5s   | DR `IsWritable=true`; post-verify passes                  |
| **Total**     | **< 2m** |                                                          |

### 2.4 Pass criteria

- [ ] `Status.Phase == Monitoring` within 2 minutes of the trigger.
- [ ] Write smoke test hits DR:
      ```sql
      INSERT INTO smoketest.pings VALUES (DEFAULT, 'drill-2-4', NOW(6));
      ```
      succeeds through the ProxySQL client port on DC's app side.
- [ ] `GTID_SUBTRACT(dc.gtid_executed, dr.gtid_executed)` returns empty.
- [ ] No alert fired except `MysqlKeeperActiveClusterChanged` (info).
- [ ] Application error rate returned to baseline within 30s of `Monitoring`.

### 2.5 Flip back to baseline

Wait `Spec.Switchover.CooldownPeriod` (default 10 min), then repeat with
the DR CR:

```bash
kubectl --context=dr patch clusterswitchpolicy dc-dr-policy \
  --type=merge \
  -p '{"spec":{"manualSwitchoverTarget":"promote-remote"}}'
```

Same pass criteria. Both directions must complete cleanly.

---

## Section 3 — Failure mode drills (D1 – D5)

Reference: [`chaos-drills.md`](chaos-drills.md) for the scripts.

Run sequentially. After each drill, restore baseline (xtrabackup re-seed
if required by D4) and confirm `ConditionSplitBrainSafe=True` before the
next drill.

### 3.1 D1 — Kill controller pod mid-Promote

```bash
bash scripts/chaos/D1-kill-controller-mid-promote.sh
```

Pass:
- [ ] `Status.Phase` reaches `Degraded` within `ResumeStuckTimeout + 30s`.
- [ ] Both clusters remain NOT simultaneously writable.
- [ ] `manual_intervention_required_total{phase="Promote"}` increments
      by exactly 1.
- [ ] No GTID divergence: `GTID_SUBTRACT` both directions remains
      resolvable without re-seed.

Recovery: follow [switchover-stuck.md](switchover-stuck.md) to clear
`SwitchoverProgress` before the next drill.

### 3.2 D2 — Network partition DC↔DR

```bash
bash scripts/chaos/D2-partition-dc-dr.sh
```

Pass:
- [ ] `Status.LastPreFlight.Checks[C1_RemoteReachableReadOnly].passed == false`.
- [ ] No switchover started; `Status.Phase` stays `Monitoring` (or
      returns to it).
- [ ] `MysqlKeeperReplicationIOBroken` alert fires appropriately.

### 3.3 D3 — Inject GTID lag (root-cause regression)

```bash
bash scripts/chaos/D3-inject-lag-then-trigger.sh
```

Pass:
- [ ] `Status.LastPreFlight.Checks[C5_GTIDSubset].passed == false`.
- [ ] `Status.LastPreFlight.Checks[C6_GTIDCatchup].passed == false`.
- [ ] No switchover proceeded.
- [ ] No Error 1236 appears in any IO thread — because the flip was
      refused.

**This is the single most important drill.** It is the regression test
against the original production incident. A fail here is a release
blocker.

### 3.4 D4 — Binlog retention + DC outage

```bash
bash scripts/chaos/D4-binlog-retention-purge.sh
```

Pass:
- [ ] If retention was sufficient: DC recovers as replica cleanly, no
      Error 1236.
- [ ] If retention was insufficient: IO thread surfaces the
      `Cannot replicate because the source purged required binary logs`
      message AND the runbook [`split-brain-recovery.md`](split-brain-recovery.md)
      successfully re-seeds DC from DR via xtrabackup.
- [ ] Post-recovery: `Conditions.SplitBrainSafe=True` and replication
      back online.

### 3.5 D5 — Dual controller race

```bash
bash scripts/chaos/D5-dual-controller-promote.sh
```

Pass:
- [ ] Exactly one of DC / DR ends up writable (`@@read_only=0`); the
      other is read-only.
- [ ] `keeper.leader.epoch` advanced by 1.
- [ ] No split-brain: `MysqlKeeperBothClustersWritable` alert never
      fired.

---

## Section 4 — Soak test

After all drills pass, run with fault injection OFF and light synthetic
traffic for at least 24 hours.

Collect:

- `switchover_total` counter — should be 0 (no spurious flips).
- `preflight_failures_total` — should be 0.
- `cluster_healthy{scope=local}` — flat at 1.
- `replication_channel_io_running{scope=remote}` — flat at 1.
- `both_clusters_readonly_total` — 0.
- `both_clusters_writable_total` — 0.
- `manual_intervention_required_total` — 0.

Any non-zero counter on the last three is a hard blocker: investigate
the root cause before production.

---

## Section 5 — Observability validation

Trigger each alert manually (or observe from the drills above):

- [ ] `MysqlKeeperBothClustersReadOnly` — observe during D1 recovery
      window.
- [ ] `MysqlKeeperReplicationIOBroken` — observe during D2.
- [ ] `MysqlKeeperPreFlightRegression` — observe during D3.
- [ ] `MysqlKeeperBinlogRetentionLow` — observe during D4 setup.
- [ ] `MysqlKeeperManualInterventionRequired` — observe during D1/D4
      recovery.

Each alert must:
- Fire within its configured `for:` window (not immediately on first blip).
- Route to the correct on-call channel.
- Resolve when the underlying condition clears.
- Include a runbook link in the annotation.

---

## Section 6 — Runbook walk-through

Have an on-call engineer who has NOT been involved in the drills do a
dry run of:

- [`switchover-stuck.md`](switchover-stuck.md) — simulate a stuck CR,
  follow every command in the runbook to unstick it.
- [`split-brain-recovery.md`](split-brain-recovery.md) — simulate
  `both_writable`, walk through the recovery steps including xtrabackup
  re-seed.
- [`dr-drill.md`](dr-drill.md) — do a planned drill using only the
  runbook, without direct guidance.

Pass criteria: engineer completes each without needing to ask the
author of mysql-keeper for help.

---

## Section 7 — Production readiness gate

All of the following must be green before flipping
`Spec.AutoFailover=true` in production:

| Gate                                                               | Owner    | Status  |
| ------------------------------------------------------------------ | -------- | ------- |
| Sections 0–6 signed off                                            | DBRE     | ☐       |
| Image `duongbkak55/mysql-keeper:vX.Y.Z` pinned (no `:latest`)      | Platform | ☐       |
| CRD applied from the same release tag                              | Platform | ☐       |
| Alerts verified end-to-end                                         | On-call  | ☐       |
| Runbook walk-through done by on-call                               | On-call  | ☐       |
| Soak test 24h zero spurious events                                  | DBRE     | ☐       |
| `AllowDataLossFailover=false` on both CSPs                          | DBRE     | ☐       |
| `binlog_expire_logs_seconds ≥ 7 days` on both PXC clusters         | DBRE     | ☐       |
| Backup policy covers RPO for worst-case re-seed                    | DBRE     | ☐       |
| Go-live window announced to stakeholders                           | Platform | ☐       |

---

## Section 8 — Canary in production

After the gate passes, enable auto-failover **first** on a lower-tier
production CSP (if you have one) and soak 1 week before enabling on
tier-1 CSPs.

```bash
kubectl --context=prod patch clusterswitchpolicy dc-dr-policy \
  --type=merge \
  -p '{"spec":{"autoFailover":true}}'
```

Watch:
- `switchover_total{result="success|failure|rollback"}` — any non-zero
  during a period without a real incident is a regression.
- `manual_intervention_required_total` — any increment is a page for
  the on-call.

Fallback plan:
```bash
kubectl --context=prod patch clusterswitchpolicy dc-dr-policy \
  --type=merge \
  -p '{"spec":{"autoFailover":false}}'
```
Immediate disarm. Cool-down 24h, post-mortem, then resume the canary.

---

## Common deviations and what they mean

| Observation                                                          | Likely cause                                                                      | Action                                                    |
| -------------------------------------------------------------------- | --------------------------------------------------------------------------------- | --------------------------------------------------------- |
| D3 passes; D4 replication does NOT recover                            | Retention tighter than drill window. Expected for the drill.                      | Re-seed via xtrabackup (follow `split-brain-recovery.md`) |
| Planned switchover hangs at Routing                                   | ProxySQL not cluster-synced; one admin unreachable.                               | Verify all instances listed in CSP respond on admin port  |
| `status.remoteHealth.isWritable: Required value`                      | CRD predates `v0.2.1` or `omitempty` fix not applied                             | `kubectl apply -f config/crd/bases/` from release tag     |
| `Error 1045` on demote old writer during Routing                      | ProxySQL admin user is `admin` (locked to localhost) or password mismatch         | Create dedicated admin user (see ProxySQL security docs)  |
| `Scan error ... []uint8 into type *time.Time`                         | Image predates DSN parseTime fix                                                  | Upgrade to `≥ v0.2.1`                                     |
| `ConsecutiveLocalUnreachable` not visible                             | `omitempty` when value is 0 — not a bug. Use `-o json \| jq` to see the field.   | No action                                                 |
| Failover does not fire when local is hard-down                        | Image predates `v0.2.2` (unreachable counter was tracked but not gating)          | Upgrade to `≥ v0.2.2`                                     |

---

## Sign-off page (detach and file with drill report)

Drill lead: __________________________________ Date: _______________

DBRE sign-off: ____________________________________________________

Platform sign-off: _________________________________________________

On-call sign-off: __________________________________________________

Issues captured (link tickets): _____________________________________

Next drill date (quarterly): _______________________________________
