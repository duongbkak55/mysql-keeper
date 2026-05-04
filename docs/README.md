# mysql-keeper documentation

## Reference docs

| Doc | What it covers |
|-----|----------------|
| [Replication error handling](replication-error-handling.md) | Detection, auto-skip, quarantine, alarms, structured logs, Prometheus metrics |
| [Architecture & failover summary](mysql-keeper-summary.md) | Component layout, health counters, the 8-gate failover decision flow, phase state machine |

## Observability quick reference

Three layers report the same events — use whichever fits your tooling:

| Layer | Where to look | Best for |
|-------|---------------|---------|
| **K8s events** | `kubectl describe csp <name>` → Events section | Real-time operator triage |
| **CR conditions** | `kubectl get csp -o yaml` → `.status.conditions` | Automated alerts, GitOps reconciliation |
| **Structured logs** | `event=` key in controller logs (Fluent Bit → ELK/Loki) | Log-based alerting, audit trail |
| **Prometheus** | `/metrics` on port 8080 | Dashboards, long-term trending, SLO alerts |

See [replication-error-handling.md](replication-error-handling.md) for the full event/metric/log table.

## Runbooks

| Scenario | Runbook | Severity |
|----------|---------|---------|
| **Start here for any incident** | [incident-response-playbook.md](runbooks/incident-response-playbook.md) | all |
| Switchover stuck in `SwitchingOver` | [switchover-stuck.md](runbooks/switchover-stuck.md) | warning/critical |
| Both clusters writable (split-brain) | [split-brain-recovery.md](runbooks/split-brain-recovery.md) | critical |
| GTID lag/gap monitoring | [gtid-lag-monitoring.md](runbooks/gtid-lag-monitoring.md) | reference |
| Remote-unreachable auto-failover | [remote-unreachable-failover.md](runbooks/remote-unreachable-failover.md) | reference |
| Planned DR drill (quarterly) | [dr-drill.md](runbooks/dr-drill.md) | informational |
| Chaos drills D1–D5 (staging) | [chaos-drills.md](runbooks/chaos-drills.md) | informational |
| Go-live DR drill checklist | [go-live-drill-plan.md](runbooks/go-live-drill-plan.md) | release gate |

## Preflight checks (C1–C12)

The switchover engine runs these checks before any promote. Hard failures abort immediately; soft failures are logged but do not block.

| Check | Name | Hard? | What it verifies |
|-------|------|-------|-----------------|
| C1 | `RemoteReachableReadOnly` | ✓ | Remote is reachable and `read_only=ON` (split-brain guard) |
| C3 | `RemoteReplicationRunning` | ✓ | Remote SQL thread is running |
| C5 | `GTIDSubset` | ✓ | Remote GTID set ⊇ local GTID set |
| C6 | `GTIDCatchup` | ✓ | Remote catches up within `preFlight.catchupTimeout` |
| C7 | `RemoteLogReplicaUpdates` | ✓ | `log_replica_updates=ON` on remote |
| C8 | `BinlogFormatRow` | ✓ | `binlog_format=ROW` on remote |
| C9 | `GTIDModeOn` | ✓ | `gtid_mode=ON` on remote |
| C10 | `RemotePurgedNotAhead` | ✓ | Remote has not purged binlogs the local still needs |
| C11 | `RemoteBinlogRetention` | soft | `binlog_expire_logs_seconds` ≥ `minBinlogRetentionSeconds` |
| C12 | `ReplicaNotQuarantined` | ✓ | Local replica is not quarantined (see [replication-error-handling.md](replication-error-handling.md)) |

> **Note:** C2 and C4 are intentionally reserved for future expansion and do not currently exist.
