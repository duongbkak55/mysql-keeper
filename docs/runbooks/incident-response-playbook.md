# Incident Response & Recovery Playbook

Master on-call reference for all DC-DR MySQL failure scenarios.
Open this first; it routes to the correct recovery path within 2 minutes.

---

## 0. Immediate triage (< 2 min)

Run these four commands to establish ground truth before any action:

```bash
# 1. Controller state
kubectl describe clusterswitchpolicy dc-dr-policy | grep -A 30 "Status:"

# 2. MySQL writable state on both sides
mysql -h <DC_VIP>  -u keeper -p -e "SELECT @@read_only, @@super_read_only, @@hostname, @@global.gtid_executed\G"
mysql -h <DR_VIP>  -u keeper -p -e "SELECT @@read_only, @@super_read_only, @@hostname, @@global.gtid_executed\G"

# 3. ProxySQL routing on every admin port
mysql -h <PROXYSQL_HOST> -P 6032 -u radmin -p \
  -e "SELECT hostgroup_id,hostname,port,status,max_connections FROM runtime_mysql_servers ORDER BY 1,2;"

# 4. Replication status on DR (the replica)
mysql -h <DR_VIP> -u keeper -p -e "SHOW REPLICA STATUS\G"
```

---

## 1. Scenario decision tree

```
                    ┌─ Is DC MySQL reachable? ─────── Yes ──────────────────────────────────────────────────┐
                    │                                                                                         │
                    No                                                          ┌── Is DR MySQL read_only? ──┤
                    │                                                           │                             │
              ┌─────▼─────────────────────────────┐                           Yes                          No
              │  Is DC k8s / controller alive?     │                           │                             │
              └─────┬───────────────┬──────────────┘               ┌──────────▼───┐            ┌────────────▼────────────┐
                   Yes              No                              │ S-1: Normal  │            │ S-4: Split-brain        │
                    │               │                               │ monitoring   │            │ (both writable)         │
         ┌──────────▼──┐    ┌───────▼──────────────────────┐       └──────────────┘            └──────────────────────── ┘
         │ DC k8s up,  │    │ DC site completely down       │                                      → Section 5
         │ MySQL down  │    │ (k8s + MySQL both dark)       │
         │             │    │                               │
         │ Did DC      │    │ Did DR auto-promote via        │
         │ controller  │    │ remoteUnreachableThreshold?   │
         │ fire?       │    └──────┬──────────────┬─────────┘
         └──┬──────────┘          Yes             No
           Yes                    │               │
            │              ┌──────▼──┐    ┌───────▼──────────────┐
            │              │ S-2B    │    │ S-2A: DC dead,       │
     ┌──────▼──┐           │ DC site │    │ no auto-promote yet  │
     │ S-2A:   │           │ down,   │    │ (threshold=0 or not  │
     │ DC MySQL│           │ DR      │    │  reached)            │
     │ crashed,│           │ promoted│    └───────┬──────────────┘
     │ DC k8s  │           └──────┬──┘            │
     │ alive   │                  │        Manual promote needed
     └──┬──────┘                  │             → Section 3
        │                         │
   Auto-failover                  │
   via local-failure     ┌────────▼──────────────────────────────────┐
   path (normal)         │ Was DC reachable from DR during promotion? │
        │                └────────┬──────────────────┬───────────────┘
        │                        No                  Yes (partition healed)
        ▼                         │                   │
   → Section 2          ┌─────────▼────┐    ┌────────▼────────────────┐
                        │ S-3: DC dead │    │ S-4: Network partition   │
                        │ clean        │    │ → split-brain risk       │
                        └─────────────┘    └──────────────────────────┘
                          → Section 3          → Section 5
```

---

## 2. S-2A: DC MySQL crashed, DC k8s alive — normal auto-failover

**Indicators:**
- `status.consecutiveLocalFailures` or `consecutiveLocalUnreachable` ≥ `failureThreshold` on DC CR
- DC controller fires, phase transitions `PreFlight → Fence → Promote → Routing → Done`
- `status.activeCluster` flips to `dr`
- DC-side ProxySQL rerouted to DR MySQL automatically

**Expected outcome (no action needed):**

```
DC controller
  Fence:   SET GLOBAL read_only=ON on DC MySQL (or ProxySQL blackhole)
  Promote: DR MySQL read_only=OFF
  Routing: DC-side ProxySQL HG10 → DR MySQL
  Verify:  Write probe to DR MySQL via ProxySQL
  Phase:   Monitoring, activeCluster=dr
```

**Verify it completed correctly:**

```bash
kubectl describe clusterswitchpolicy dc-dr-policy | grep -E "Phase|Active|Progress"
# Expect: Phase=Monitoring, ActiveCluster=dr, SwitchoverProgress=nil
```

```sql
-- DR is now writable
SELECT @@read_only, @@hostname FROM <DR_VIP>;  -- read_only=0

-- DC is fenced
SELECT @@read_only FROM <DC_VIP>;  -- read_only=1

-- ProxySQL routing
SELECT hostgroup_id, hostname FROM runtime_mysql_servers WHERE hostgroup_id = 10;
-- HG 10 → DR MySQL
```

**If the switchover is stuck** → follow `switchover-stuck.md`.

---

## 3. S-3: DC site completely down — manual or threshold-triggered

### 3A. remoteUnreachableThreshold triggered (DR auto-promoted)

**Indicators:**
- `status.consecutiveRemoteUnreachable` reached threshold on DR CR
- DR phase: `SwitchingOver → Monitoring`, `activeCluster=dr`
- DR-side ProxySQL updated
- DC-side ProxySQL has **no controller** → still routing to dead DC MySQL

**Immediate actions:**

#### Step 1 — Update DC-side ProxySQL manually

```sql
-- On every DC-side ProxySQL admin port
-- Move DR MySQL to write hostgroup
UPDATE mysql_servers
  SET hostgroup_id = 10
  WHERE hostname = '<DR_MYSQL_VIP>' AND port = 3306;

-- Move DC MySQL to offline/blackhole
UPDATE mysql_servers
  SET hostgroup_id = 9999, max_connections = 0
  WHERE hostname = '<DC_MYSQL_VIP>' AND port = 3306;

LOAD MYSQL SERVERS TO RUNTIME;
SAVE MYSQL SERVERS TO DISK;
```

#### Step 2 — Verify DR-side ProxySQL (should already be correct)

```sql
-- DR-side ProxySQL HG 10 must point to DR MySQL
SELECT hostgroup_id, hostname, status FROM runtime_mysql_servers ORDER BY 1;
```

#### Step 3 — Monitor DR as standalone

```bash
# No replication running — DR is now standalone source
mysql -h <DR_VIP> -e "SHOW REPLICA STATUS\G"
# Expect: channel stopped (Engine ran STOP REPLICA during Promote phase)

# Watch for write activity
mysql -h <DR_VIP> -e "SHOW STATUS LIKE 'Com_insert%'; SHOW STATUS LIKE 'Com_update%';"
```

---

### 3B. Threshold not reached — manual promotion needed

Use this path when `remoteUnreachableThreshold=0` or DC has been down for less
than the threshold and you decide to promote immediately.

```bash
# Trigger manual promote on DR controller
kubectl patch clusterswitchpolicy dc-dr-policy \
  --type=merge \
  -p '{"spec":{"manualSwitchoverTarget":"promote-remote","allowDataLossFailover":true}}'
```

If GTID lag was zero at the time of DC failure, `allowDataLossFailover` is only
needed if the preflight cannot query DC (which it can't — DC is down). Confirm
the patch then follow Step 1–3 of Section 3A.

---

## 4. S-3 continued: DC recovers after DR promotion

When DC site comes back online, **do not let DC become writable immediately**.
DC's `read_only` state was not changed during promotion (DC was unreachable during
fencing) so it may start writable again.

### Step 1 — Fence DC immediately on boot

Before DC MySQL accepts any application writes:

```sql
-- DC MySQL (connect immediately on startup)
SET GLOBAL super_read_only = ON;
SET GLOBAL read_only = ON;
```

Automate via `init_file` or PXC operator `isSource: false` in the CRD.

### Step 2 — Assess GTID gap (DC vs DR)

```sql
-- DC: what did we have when we went down?
SELECT @@global.gtid_executed AS dc_gtid;

-- DR: what do we have now?
SELECT @@global.gtid_executed AS dr_gtid;

-- Transactions on DC that DR never received (data loss window)
SELECT GTID_SUBTRACT('<dc_gtid>', '<dr_gtid>') AS lost_on_dr;

-- Transactions DR wrote while DC was down (DC needs to catch up)
SELECT GTID_SUBTRACT('<dr_gtid>', '<dc_gtid>') AS dc_needs_to_apply;
```

| `lost_on_dr` result | `dc_needs_to_apply` result | Meaning |
|---|---|---|
| empty | non-empty | Clean: DC was in sync or behind, DR wrote new data |
| non-empty | non-empty | Split-write window: some DC writes were lost → go to Section 5 |
| non-empty | empty | DC was ahead (unusual): evaluate whether to apply manually |

### Step 3 — Re-establish DC as replica of DR

```sql
-- On DC
STOP REPLICA FOR CHANNEL 'dc-to-dr';    -- old inbound channel (now wrong direction)
RESET REPLICA ALL FOR CHANNEL 'dc-to-dr';

-- Subscribe to DR as the new source
CHANGE REPLICATION SOURCE TO
  SOURCE_HOST = '<DR_VIP>',
  SOURCE_PORT = 3306,
  SOURCE_USER = '<repl_user>',
  SOURCE_PASSWORD = '<repl_pass>',
  SOURCE_AUTO_POSITION = 1
FOR CHANNEL 'dr-to-dc';

START REPLICA FOR CHANNEL 'dr-to-dc';
```

### Step 4 — Wait for DC to catch up

```bash
# Monitor on DC until Seconds_Behind_Source = 0 and gtid_missing = 0
watch -n 5 'mysql -h <DC_VIP> -e "SHOW REPLICA STATUS\G" | grep -E "Seconds_Behind|Running|Error"'

# From DR controller perspective
kubectl describe clusterswitchpolicy dc-dr-policy | grep -A 5 "Gtid Lag"
```

### Step 5 — Update DC-side ProxySQL (if not done in Section 3A)

Confirm DC-side ProxySQL is routing to DR (the current active):

```sql
-- DC-side ProxySQL admin
SELECT hostgroup_id, hostname, status FROM runtime_mysql_servers ORDER BY 1;
-- HG 10 must be DR MySQL, HG 20 (or 9999) must be DC MySQL
```

### Step 6 — Update controller CR to reflect reality

```bash
# DC CR: mark DC as replica, clear any manual trigger
kubectl patch clusterswitchpolicy dc-dr-policy --type=merge \
  -p '{"status":{"activeCluster":"dr","consecutiveRemoteUnreachable":0}}'
```

### Step 7 — Optional: switch back to DC as active

Only after DC has fully caught up (`gtid_missing = 0`, `Seconds_Behind_Source = 0`).
Follow `dr-drill.md` for the planned switchover procedure.

---

## 5. S-4: Network partition → split-brain (both clusters writable)

**This is the highest-severity scenario.** GTID streams are diverging every second.

**Indicators:**
- `mysql_keeper_both_clusters_writable_total` counter increments
- `ConditionSplitBrainSafe = False, Reason = BothWritable` on CR
- k8s Warning event: `CRITICAL: both clusters are read_only=OFF`

### Step 1 — Stop the controller immediately

```bash
kubectl scale -n mysql-keeper-system deploy/mysql-keeper-controller-manager --replicas=0
```

### Step 2 — Identify the "truth cluster" (< 1 min)

Pick DR (promoted cluster) as truth unless business has a strong reason otherwise.

```sql
-- Compare transaction counts since split
-- On DC
SELECT COUNT(*) FROM information_schema.INNODB_TRX;
SHOW STATUS LIKE 'Handler_write';

-- On DR
SELECT COUNT(*) FROM information_schema.INNODB_TRX;
SHOW STATUS LIKE 'Handler_write';
```

Default: **DR is truth** (it was intentionally promoted).

### Step 3 — Fence DC immediately

```sql
-- On all DC PXC nodes
SET GLOBAL super_read_only = ON;
SET GLOBAL read_only = ON;
```

### Step 4 — Block DC-side ProxySQL routing

```sql
-- On every DC-side ProxySQL admin port
UPDATE mysql_servers
  SET hostgroup_id = 9999, max_connections = 0
  WHERE hostname = '<DC_MYSQL_VIP>';
LOAD MYSQL SERVERS TO RUNTIME;
SAVE MYSQL SERVERS TO DISK;
```

### Step 5 — Measure divergence

```sql
-- Run from any host that can reach both sides
-- What DC has that DR doesn't (the "lost" writes)
SELECT GTID_SUBTRACT(
  '<dc_gtid_executed>',
  '<dr_gtid_executed>'
) AS only_on_dc;

-- What DR has that DC doesn't (legitimate new writes post-promote)
SELECT GTID_SUBTRACT(
  '<dr_gtid_executed>',
  '<dc_gtid_executed>'
) AS only_on_dr;
```

### Step 6 — Export divergent transactions for audit

```bash
# Extract DC-only writes before discarding them
ONLY_DC_GTIDS="<result from only_on_dc>"

mysqlbinlog \
  --read-from-remote-server \
  --host=<DC_VIP> --user=keeper --password=<pass> \
  --include-gtids="${ONLY_DC_GTIDS}" \
  --to-last-log \
  --stop-never-slave-server-id=99 \
  > /tmp/dc-divergent-writes-$(date +%Y%m%d-%H%M%S).sql

# Review before deciding to replay or discard
head -100 /tmp/dc-divergent-writes-*.sql
```

### Step 7 — Rebuild DC as replica of DR

Two paths depending on divergence size:

#### Path A — Small divergence (< few thousand rows, same schema)

```sql
-- On DC: replay DR-only transactions if any, then re-subscribe
STOP REPLICA;
RESET REPLICA ALL;

-- If dc_gtid is AHEAD of dr_gtid on some UUIDs, must reset
RESET MASTER;
SET GLOBAL gtid_purged = '<dr_gtid_executed>';

CHANGE REPLICATION SOURCE TO
  SOURCE_HOST = '<DR_VIP>',
  SOURCE_USER = '<repl_user>',
  SOURCE_PASSWORD = '<repl_pass>',
  SOURCE_AUTO_POSITION = 1
FOR CHANNEL 'dr-to-dc';

START REPLICA FOR CHANNEL 'dr-to-dc';
SHOW REPLICA STATUS\G
```

#### Path B — Large divergence or schema drift (full reseed)

```bash
# Take xtrabackup from DR (truth)
xtrabackup --backup \
  --host=<DR_VIP> --user=xtrabackup --password=<pass> \
  --target-dir=/backup/dr-seed \
  --galera-info

xtrabackup --prepare --target-dir=/backup/dr-seed

# Copy to DC and restore (stops DC PXC first)
# Then re-subscribe using gtid_purged from xtrabackup_binlog_info
```

See `split-brain-recovery.md` for the full xtrabackup reseed procedure.

### Step 8 — Verify replication health

```sql
-- On DC after restart as replica
SHOW REPLICA STATUS\G
-- Replica_IO_Running: Yes
-- Replica_SQL_Running: Yes
-- Seconds_Behind_Source: 0
-- Last_Error: (empty)
```

### Step 9 — Resume controller

```bash
kubectl scale -n mysql-keeper-system deploy/mysql-keeper-controller-manager --replicas=2

# Verify split-brain condition cleared
kubectl describe clusterswitchpolicy dc-dr-policy | grep SplitBrainSafe
# ConditionSplitBrainSafe: True
```

---

## 6. ProxySQL recovery reference

All scenarios involving DC site failure require manual ProxySQL updates on
the DC side (the DR controller only manages DR-side ProxySQL).

### 6A. DC-side ProxySQL: route to DR (new active)

```sql
-- Admin port: DC-side ProxySQL
BEGIN;

UPDATE mysql_servers
  SET hostgroup_id = 10, max_connections = 1000, status = 'ONLINE'
  WHERE hostname = '<DR_MYSQL_VIP>' AND port = 3306;

UPDATE mysql_servers
  SET hostgroup_id = 9999, max_connections = 0, status = 'OFFLINE_HARD'
  WHERE hostname = '<DC_MYSQL_VIP>' AND port = 3306;

LOAD MYSQL SERVERS TO RUNTIME;
SAVE MYSQL SERVERS TO DISK;
```

### 6B. DC-side ProxySQL: restore DC as active (after DC recovers)

Only run after DC has fully replicated from DR (Section 4, Steps 3–4):

```sql
UPDATE mysql_servers
  SET hostgroup_id = 10, max_connections = 1000, status = 'ONLINE'
  WHERE hostname = '<DC_MYSQL_VIP>' AND port = 3306;

UPDATE mysql_servers
  SET hostgroup_id = 20, max_connections = 1000, status = 'ONLINE'
  WHERE hostname = '<DR_MYSQL_VIP>' AND port = 3306;

LOAD MYSQL SERVERS TO RUNTIME;
SAVE MYSQL SERVERS TO DISK;
```

### 6C. Verify all ProxySQL instances are consistent

```bash
for HOST in <PROXYSQL_1> <PROXYSQL_2> <PROXYSQL_3>; do
  echo "=== $HOST ==="
  mysql -h $HOST -P 6032 -u radmin -p<pass> -e \
    "SELECT hostgroup_id,hostname,port,status,max_connections
     FROM runtime_mysql_servers ORDER BY 1,2;"
done
```

All instances must show identical hostgroup assignments.

### 6D. ProxySQL blackhole fence — emergency write block

Use when MySQL SQL fence fails (local unreachable) or as immediate traffic stop:

```sql
-- Blackhole: no queries reach MySQL, all connections are rejected
UPDATE mysql_servers
  SET hostgroup_id = 9999, max_connections = 0
  WHERE hostname = '<TARGET_HOST>' AND port = 3306;
LOAD MYSQL SERVERS TO RUNTIME;
-- Do NOT SAVE until you are sure — this survives restarts
```

To restore the server from blackhole:

```sql
UPDATE mysql_servers
  SET hostgroup_id = <ORIGINAL_HG>, max_connections = 1000, status = 'ONLINE'
  WHERE hostname = '<TARGET_HOST>' AND port = 3306;
LOAD MYSQL SERVERS TO RUNTIME;
SAVE MYSQL SERVERS TO DISK;
```

---

## 7. Post-incident verification checklist

Run after ANY recovery path:

```
[ ] Exactly one cluster is read_only=OFF
    SELECT @@read_only FROM <DC_VIP>;   -- must be 1
    SELECT @@read_only FROM <DR_VIP>;   -- must be 0 (or vice-versa)

[ ] ProxySQL HG 10 points to exactly one host (the active cluster)
    SELECT hostgroup_id, hostname, status FROM runtime_mysql_servers WHERE hostgroup_id=10;

[ ] Replication running on the replica
    SHOW REPLICA STATUS\G → IO+SQL Running, zero errors

[ ] GTID lag = 0
    kubectl describe clusterswitchpolicy dc-dr-policy | grep "Missing Transactions"
    # Must show: Missing Transactions: 0

[ ] Controller in Monitoring phase
    kubectl get clusterswitchpolicy -A
    # PHASE = Monitoring

[ ] ConditionSplitBrainSafe = True
    kubectl describe clusterswitchpolicy | grep SplitBrainSafe

[ ] No ongoing Prometheus alerts
    GTIDLagGrowing, ReplicationLagHigh, BothClustersWritable,
    RemoteUnreachableApproachingThreshold

[ ] Write a post-mortem entry (timeline, root cause, action items)
```

---

## 8. Scenario summary table

| Scenario | Who detects | Auto-recovery? | DC-side ProxySQL | DR-side ProxySQL | Data loss? |
|---|---|---|---|---|---|
| DC MySQL crash, DC k8s alive | DC controller | Yes (local-failure path) | Auto-updated by DC controller | Unchanged (DC is not active here) | No (GTID C5/C6 gates) |
| DC site fully down, threshold=0 | DR controller | No — manual trigger needed | **Manual update required** | Auto-updated on DR promote | Possible |
| DC site fully down, threshold > 0 | DR controller | Yes (remoteUnreachable path) | **Manual update required** | Auto-updated on DR promote | Possible (no GTID check) |
| Network partition, threshold not reached | Both controllers | No — heals naturally | Unchanged | Unchanged | No |
| Network partition, DR promotes | DR controller | Partial — split-brain! | **Manual fence + update** | Auto-updated, may be wrong | Yes — divergent writes during window |
| Split-brain (both writable) | Both controllers | No — manual intervention | **Manual fence + update** | **Manual fence + update** | Yes — requires audit |

---

## 9. Quick-reference commands

```bash
# Check controller state
kubectl describe clusterswitchpolicy dc-dr-policy

# Pause controller
kubectl scale -n mysql-keeper-system deploy/mysql-keeper-controller-manager --replicas=0

# Resume controller
kubectl scale -n mysql-keeper-system deploy/mysql-keeper-controller-manager --replicas=2

# Manual promote DR
kubectl patch clusterswitchpolicy dc-dr-policy \
  --type=merge -p '{"spec":{"manualSwitchoverTarget":"promote-remote"}}'

# Manual promote DR (with data loss accepted)
kubectl patch clusterswitchpolicy dc-dr-policy \
  --type=merge -p '{"spec":{"manualSwitchoverTarget":"promote-remote","allowDataLossFailover":true}}'

# Disable auto-failover (maintenance / incident triage)
kubectl patch clusterswitchpolicy dc-dr-policy \
  --type=merge -p '{"spec":{"autoFailover":false}}'

# Clear stuck switchover (ONLY after verifying MySQL+ProxySQL state by hand)
kubectl patch clusterswitchpolicy dc-dr-policy \
  --type=merge --subresource=status \
  -p '{"status":{"phase":"Monitoring","switchoverProgress":null}}'

# Watch GTID lag live
watch -n 5 'kubectl describe clusterswitchpolicy dc-dr-policy | grep -A 4 "Gtid Lag"'
```

---

## 10. Cross-references

| Topic | Runbook |
|---|---|
| Switchover stuck in `SwitchingOver` | `switchover-stuck.md` |
| Both clusters writable (split-brain deep-dive) | `split-brain-recovery.md` |
| Planned DR drill procedure | `dr-drill.md` |
| GTID lag monitoring flow and alerts | `gtid-lag-monitoring.md` |
| Remote-unreachable threshold design | `remote-unreachable-failover.md` |
| Pre-go-live checklist | `go-live-drill-plan.md` |
| Replication SQL errors, auto-skip, quarantine guard | `../replication-error-handling.md` |
