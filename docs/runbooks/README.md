# mysql-keeper runbooks

| Scenario                                           | Runbook                                         | Severity         |
| -------------------------------------------------- | ----------------------------------------------- | ---------------- |
| **Master incident response & recovery (start here)** | [incident-response-playbook.md](incident-response-playbook.md) | all severities |
| Switchover stuck in `SwitchingOver`                | [switchover-stuck.md](switchover-stuck.md)      | warning/critical |
| Both clusters writable at the same time            | [split-brain-recovery.md](split-brain-recovery.md) | critical (page)  |
| Planned DR drill (quarterly / post-upgrade)        | [dr-drill.md](dr-drill.md)                      | informational    |
| Chaos drills (D1–D5) on staging                    | [chaos-drills.md](chaos-drills.md)              | informational    |
| Go-live DR drill plan (master checklist)           | [go-live-drill-plan.md](go-live-drill-plan.md)  | release gate     |
| GTID lag monitoring — flow, metrics, alerts        | [gtid-lag-monitoring.md](gtid-lag-monitoring.md) | reference        |
| Remote-unreachable auto-failover — config & risks  | [remote-unreachable-failover.md](remote-unreachable-failover.md) | reference |

## When to disable `autoFailover`

Set `Spec.AutoFailover=false` (manual only) if **any** of the following is
true for either cluster:

- Replication channel is broken or more than 5 seconds behind.
- `binlog_expire_logs_seconds < 24*3600` on either side.
- PXC cluster is undergoing maintenance (node restart, SST, upgrade).
- MANO API is down (and MANO is the state-change backend).
- Any `C5_GTIDSubset` / `C7_RemoteLogReplicaUpdates` preflight alert is firing.
- A mysql-keeper release younger than 48h has not yet completed soak.
- Cross-cluster network shows latency or loss alerts.
- Local ProxySQL healthy instance count is at exactly `ProxySQLMinHealthy`
  (no margin for an additional failure during the cutover).

Re-enable only after each condition above is clear for at least one full
cooldown cycle.
