# Runbook: Switchover stuck in `SwitchingOver`

**Severity:** warning → critical depending on duration
**Alerts that map here:** `MysqlKeeperSwitchoverStuck`, `MysqlKeeperManualInterventionRequired`

## Symptom

- `kubectl get clusterswitchpolicy -A` shows `PHASE=SwitchingOver` for longer
  than expected.
- `Status.SwitchoverProgress.CurrentPhase` has not advanced in several minutes.
- No traffic has actually moved (application health checks still point at the
  old writer).

## Triage

1. Identify the attempt:

   ```bash
   kubectl describe clusterswitchpolicy <name>
   # Read Status.SwitchoverProgress (AttemptID, CurrentPhase, StartedAt, Error)
   ```

2. Grab controller logs for the attempt:

   ```bash
   kubectl logs -n mysql-keeper-system -l app.kubernetes.io/name=mysql-keeper \
     --tail=2000 | grep "<AttemptID>"
   ```

   Structured event names to look for:
   - `phase_transition` — where did it stop?
   - `should_switchover` — was the decision taken correctly?
   - `preflight check` — did any check block?

3. Confirm the real MySQL state on both sides:

   ```sql
   SELECT @@read_only, @@super_read_only, @@hostname;
   SHOW STATUS LIKE 'wsrep_cluster_status';
   SHOW STATUS LIKE 'wsrep_cluster_size';
   SELECT @@GLOBAL.gtid_executed;
   SELECT @@GLOBAL.gtid_purged;
   SHOW REPLICA STATUS\G
   ```

4. Check ProxySQL runtime routing on every admin port:

   ```sql
   SELECT hostgroup_id, hostname, port, status, max_connections
   FROM runtime_mysql_servers
   ORDER BY hostgroup_id, hostname;
   ```

## Decision tree

### Case A — stuck in `PreFlight`

The controller is refusing the flip. Read `Status.LastPreFlight.Checks` to see
which rule failed. Common causes:

- `C5_GTIDSubset` / `C6_GTIDCatchup` — DR is behind. Let replication catch up
  or investigate why it stopped.
- `C7_RemoteLogReplicaUpdates` — `log_replica_updates=0` on DR. Fix the PXC
  config before retrying. This is the invariant that prevents the "Error 1236
  on next flip" chain reaction.
- `C10_RemotePurgedNotAhead` — DR has purged binlog entries DC never saw.
  Do not promote; re-seed DR from DC with xtrabackup instead.

### Case B — stuck in `Fence`

- SQL fence failed and ProbeReachable says local is still up → engine aborts
  to avoid split-brain. Fix whatever is preventing `SET GLOBAL read_only=ON`
  and retry.
- SQL fence failed and local is unreachable but blackhole fence also failed →
  a ProxySQL instance rejected the change. Drain/repair that instance; the
  partial-apply message in the logs names which host.

### Case C — stuck in `Promote`

- MANO LCM operation in `PROCESSING` longer than `PollTimeout`. Query MANO:

  ```bash
  curl -H "Authorization: Bearer $TOKEN" \
    "$MANO/cnflcm/v1/lcm-op-occ/<opId>"
  ```

  If MANO eventually COMPLETED, the promotion is in fact applied — the CR is
  stuck in the controller only. Force-clear (see "Force-clear stuck CR" below).

### Case D — stuck in `Routing`

ProxySQL 2-phase apply left partial runtime state. The error message lists the
instances that did and did not commit. Either re-run the commit manually:

```sql
LOAD MYSQL SERVERS TO RUNTIME;
SAVE MYSQL SERVERS TO DISK;
```

or repair/replace the un-commited instance.

### Case E — stuck in `Verify`

The promote flipped MySQL but `@@read_only` query still returned 1. That
usually means the PXC operator immediately reverted `isSource`. Check CRD:

```bash
kubectl get pxc <name> -n <ns> -o jsonpath='{.spec.replication.channels}'
```

and inspect the operator logs.

## Force-clear stuck CR

Only after you have verified by hand that MySQL and ProxySQL are in the
desired state:

```bash
# 1. Pause the controller
kubectl scale -n mysql-keeper-system deploy/mysql-keeper-controller-manager --replicas=0

# 2. Clear the progress checkpoint
kubectl patch clusterswitchpolicy <name> --type=merge --subresource=status \
  -p '{"status":{"phase":"Monitoring","switchoverProgress":null}}'

# 3. Resume the controller
kubectl scale -n mysql-keeper-system deploy/mysql-keeper-controller-manager --replicas=2
```

## Post-incident

1. Export for the post-mortem:
   - Controller logs grepped by AttemptID
   - `kubectl get events -n mysql-keeper-system`
   - MANO LCM history for the relevant `opId`
   - `runtime_mysql_servers` from every ProxySQL instance
2. File an issue with the stuck phase + reproduction.
3. If `binlog_expire_logs_seconds` on either side is below the alert threshold,
   raise it **before** the next DR drill.
