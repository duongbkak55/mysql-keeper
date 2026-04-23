# Runbook: DR drill (planned switchover)

A DR drill validates the switchover end-to-end without a real failure. Run
one at least quarterly, and always after a significant mysql-keeper upgrade.

## Pre-flight (day-of)

Run this checklist before executing the drill:

- [ ] `binlog_expire_logs_seconds ≥ 7*24*3600` on both clusters.
- [ ] Replication is healthy:

      ```sql
      SHOW REPLICA STATUS\G   -- Replica_IO_Running=Yes, SQL_Running=Yes, no error
      ```

- [ ] GTID drift between sides is bounded:

      ```sql
      -- on DC
      SELECT @@GLOBAL.gtid_executed AS dc_exec;
      -- on DR
      SELECT GTID_SUBTRACT('<dc_exec>', @@GLOBAL.gtid_executed) AS missing_on_dr;
      ```

      `missing_on_dr` should be empty or a very small tail.

- [ ] Backup snapshots exist on both sides (xtrabackup within the last 24h).
- [ ] Controller logs are at INFO or DEBUG:

      ```bash
      kubectl -n mysql-keeper-system set env \
        deploy/mysql-keeper-controller-manager LOG_LEVEL=debug
      ```

- [ ] Monitoring dashboards open:
  - switchover_total, preflight_failures_total, fence_failures_total
  - replication_channel_io_running / sql_running
  - cluster_writable by scope

- [ ] Communications channel posted, on-call paged.

## Drill sequence

Set `autoFailover=false` for the duration of the drill so the controller can
only react to the manual trigger:

```bash
kubectl patch csp <name> --type=merge -p '{"spec":{"autoFailover":false}}'
```

Trigger the planned switchover:

```bash
kubectl patch csp <name> --type=merge -p '{"spec":{"manualSwitchoverTarget":"promote-remote"}}'
```

Observe the phase transitions. Expected sequence, with wall-clock budgets:

| Phase           | Budget | What to watch                                                |
| --------------- | ------ | ------------------------------------------------------------ |
| PreFlight       | < 30s  | `Status.LastPreFlight.Checks` all PASS (hard)                |
| Fence           | < 10s  | `@@read_only=ON` on DC; CRD `isSource=false`                 |
| Promote         | < 60s  | `@@read_only=OFF` on DR; `SHOW REPLICA STATUS` is empty      |
| Routing         | < 15s  | `runtime_mysql_servers` updated on every ProxySQL instance   |
| ReverseReplica  | < 15s  | DC now has `SHOW REPLICA STATUS` pointing at DR              |
| Verify          | < 5s   | `IsWritable` on new source returns true                      |

Total walltime target: under 2 minutes.

## Post-switchover verification

```sql
-- on the (new) source — the former DR
SELECT @@hostname, @@read_only;   -- 0

-- on the (new) replica — the former DC
SELECT @@hostname, @@read_only;   -- 1
SHOW REPLICA STATUS\G             -- IO+SQL Running, source = new source

-- write probe through ProxySQL
INSERT INTO smoketest.pings VALUES (NOW());

-- SELECT on both sides should see the row
```

Check `runtime_mysql_servers` on every ProxySQL admin port to make sure the
2-phase apply committed everywhere:

```sql
SELECT hostgroup_id, hostname, port, status, max_connections
FROM runtime_mysql_servers
ORDER BY hostgroup_id, hostname;
```

All three instances must show the same writer host in the RW hostgroup.

## Flip back

Repeat the drill in reverse. Wait at least `spec.switchover.cooldownPeriod`
(default 10 min) before issuing the second manual trigger.

## Drill failure

Abort the drill and run `switchover-stuck.md` if:

- PreFlight fails on any hard check.
- Fence completes but Promote fails at MANO step.
- `runtime_mysql_servers` differs across ProxySQL instances after Routing.
- Application health checks report read failures for longer than 30s.

## Close out

Re-enable auto-failover:

```bash
kubectl patch csp <name> --type=merge -p '{"spec":{"autoFailover":true}}'
```

Fill in a drill report with:

- Walltime for each phase
- Any alerts that fired
- Any manual steps taken
- Latency of application traffic during the cutover window
- Binlog retention margin at the end of the drill

Compare against prior drills. A regression in walltime > 30% is worth an
investigation even if nothing failed outright.
