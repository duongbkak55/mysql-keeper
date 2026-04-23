# Runbook: Split-brain recovery (both clusters writable)

**Severity:** critical — pages immediately
**Alerts that map here:** `MysqlKeeperBothClustersWritable`

## Symptom

Both DC and DR are observed with `@@read_only=OFF` at the same time.
Application traffic may be reaching either side. The two GTID streams diverge
from this moment.

**Every second of delay makes the data reconciliation harder.** Do not wait
for RCA before fencing.

## Fence first, diagnose second

1. Identify the "truth cluster": the one that should continue to receive
   writes. By convention:
   - If the CR's `Status.ActiveCluster` matches business expectation, pick it.
   - Otherwise, pick the cluster that received the most writes since the
     split — compare `binlog-index` file sizes on each side.

2. Immediately fence the other cluster:

   ```sql
   SET GLOBAL super_read_only=ON;
   SET GLOBAL read_only=ON;
   ```

3. If ProxySQL routing is still pointing at the wrong side, move it
   atomically on every ProxySQL admin:

   ```sql
   UPDATE mysql_servers
   SET hostgroup_id = <BLACKHOLE_HG>, max_connections = 0
   WHERE hostname = '<wrong-writer>' AND port = 3306;
   LOAD MYSQL SERVERS TO RUNTIME;
   SAVE MYSQL SERVERS TO DISK;
   ```

4. Pause the controller so it cannot reissue a flip during recovery:

   ```bash
   kubectl scale -n mysql-keeper-system deploy/mysql-keeper-controller-manager --replicas=0
   ```

## Measure the divergence

```sql
-- On TRUTH cluster
SELECT @@GLOBAL.gtid_executed AS truth_executed;

-- On fenced cluster
SELECT @@GLOBAL.gtid_executed AS wrong_executed;
```

Then from any MySQL:

```sql
-- What GTIDs only exist on the fenced cluster (these are the "lost" writes)
SELECT GTID_SUBTRACT('<wrong_executed>', '<truth_executed>') AS only_on_fenced;
```

If `only_on_fenced` is non-empty, those transactions were accepted by the
fenced cluster but never replicated to the truth side. Business must decide
whether they can be dropped, replayed, or reconciled manually.

Export them for audit before destructive recovery:

```bash
# Replace <uuid:range> with the set from only_on_fenced
mysqlbinlog --include-gtids='<uuid:range>' \
  /var/lib/mysql/binlog.000NNN > divergent-writes.sql
```

## Rebuild the fenced cluster

When business accepts "replay later / lose forever" for the divergent set,
re-seed the fenced cluster from the truth cluster:

```bash
# On truth cluster:
xtrabackup --backup --target-dir=/bk --galera-info
xtrabackup --prepare --target-dir=/bk

# Copy to the fenced side, then:
# (on fenced cluster, for each node)
systemctl stop mysql
rsync -a /bk/ /var/lib/mysql/
systemctl start mysql@bootstrap   # first node only
```

Once MySQL is up on the fenced side:

```sql
-- Reset replication state (we are about to re-subscribe)
STOP REPLICA FOR CHANNEL '<channel>';
RESET REPLICA ALL FOR CHANNEL '<channel>';

-- Set gtid_purged from the xtrabackup_binlog_info file
SET GLOBAL gtid_purged = '<gtid from xtrabackup_binlog_info>';

-- Re-subscribe to the truth source
CHANGE REPLICATION SOURCE TO
  SOURCE_HOST='<truth-host>',
  SOURCE_USER='<repl-user>',
  SOURCE_PASSWORD='<repl-pass>',
  SOURCE_AUTO_POSITION=1
FOR CHANNEL '<channel>';

START REPLICA FOR CHANNEL '<channel>';
SHOW REPLICA STATUS\G   -- verify IO+SQL Running, zero errors
```

## Resume the controller

1. Make sure the CR state matches reality (`Status.ActiveCluster`,
   `Spec.ManualSwitchoverTarget` cleared).
2. Scale the controller back up:

   ```bash
   kubectl scale -n mysql-keeper-system deploy/mysql-keeper-controller-manager --replicas=2
   ```

3. Confirm `ConditionSplitBrainSafe=True` on the CR within one reconcile cycle.

## Root cause checklist

The controller has guards against the usual split-brain triggers. If split-
brain happened anyway, one of these was bypassed:

- [ ] `ManualSwitchoverTarget` issued while the peer controller was also
      promoting — was the peer lease stale when the peer flipped? Check
      `keeper.leader.heartbeat_at` at the time of the incident.
- [ ] Operator cleared `read_only=OFF` by hand on the fenced side during
      maintenance. Add a warning to the maintenance runbook.
- [ ] ProxySQL did not fully commit the routing change (see runbook
      `switchover-stuck.md`, case D).
- [ ] `autoFailover=true` without `PreFlight.CatchupTimeout` configured —
      make it an organisational default.
