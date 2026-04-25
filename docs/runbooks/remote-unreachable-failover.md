# Remote-Unreachable Auto-Failover

Describes the `remoteUnreachableThreshold` feature — how it works, when to
enable it, and what risks to accept before doing so.

---

## 1. The problem it solves

mysql-keeper has two auto-failover paths:

| Path | Trigger | Typical scenario |
|------|---------|-----------------|
| **Local-failure** (existing) | `consecutiveLocalFailures` OR `consecutiveLocalUnreachable` ≥ `failureThreshold` | DC MySQL crashes; DR controller sees its own local as unhealthy, promotes remote |
| **Remote-unreachable** (this feature) | `consecutiveRemoteUnreachable` ≥ `remoteUnreachableThreshold` | DC (active) site goes completely dark; DR controller sees its local as healthy but can no longer reach DC at all |

The second scenario was a gap: if the entire DC site disappears (k8s +
MySQL both down), the DR controller's **local** cluster is still healthy,
so `consecutiveLocalFailures` never increments and auto-failover never
fires. An operator had to manually patch `manualSwitchoverTarget:
promote-remote`.

---

## 2. How it works

```
                         DC (active)
                          k8s + MySQL
                         ───────────
                              │
                    all down  │
                          ✂ ─ ┘
                              │
                         DR controller
                          (local = DR replica, healthy)
                          ───────────────────────────
  cycle 1  remoteHealth = WritableUnknown  consecutiveRemoteUnreachable = 1
  cycle 2  remoteHealth = WritableUnknown  consecutiveRemoteUnreachable = 2
  ...
  cycle N  consecutiveRemoteUnreachable == remoteUnreachableThreshold
           AND allowDataLossFailover = true
           ──────────────────────────────────────────────────────────
           EvaluateSwitchover → Should=true
           reason: "automatic failover: remote sustained-unreachable for N
                    consecutive checks (threshold=N)"
           ──────────────────────────────────────────────────────────
           preflight runs (C1..C4, C7..C11 — all except C5/C6 GTID)
           fence → promote DR → routing updated → done
```

**Key distinction from the local-failure path:**
- Local-failure path: LOCAL is sick → promote REMOTE
- Remote-unreachable path: REMOTE is dark → promote LOCAL (self-promotion)

Both paths share the same engine (PreFlight → Fence → Promote → Routing →
ReverseReplica → Verify). The difference is only in what trips the trigger.

---

## 3. Counter lifecycle

`status.consecutiveRemoteUnreachable` is updated every reconcile cycle
(default every 15 s):

```
remoteHealth.Writable == WritableUnknown   →  consecutiveRemoteUnreachable++
remoteHealth.Healthy == true               →  consecutiveRemoteUnreachable = 0
remoteHealth reachable but unhealthy       →  consecutiveRemoteUnreachable = 0
                                               consecutiveRemoteFailures++
switchover succeeds                        →  consecutiveRemoteUnreachable = 0
```

`WritableUnknown` is the health checker's TCP-fail state — it means the
controller could not open a MySQL connection at all (connection refused,
timeout, DNS failure). It does NOT mean "replication is broken" or "cluster
is degraded".

---

## 4. Configuration

```yaml
spec:
  autoFailover: true
  allowDataLossFailover: true        # mandatory — see section 5
  healthCheck:
    interval: 15s
    failureThreshold: 3              # existing local-failure gate (unchanged)
    remoteUnreachableThreshold: 10   # new: promote after 10 × 15s = 2.5 min
```

### Choosing the threshold

| Threshold | Interval | Silence before promote |
|-----------|----------|------------------------|
| 4         | 15 s     | ~1 min                 |
| 10        | 15 s     | ~2.5 min               |
| 20        | 15 s     | ~5 min                 |
| 10        | 30 s     | ~5 min                 |

Recommended starting point: **10 × 15 s = 150 s**. This is long enough to
survive a transient network blip (most resolve in < 60 s) without waiting
so long that RTO exceeds SLA.

### Verify visibility

```bash
kubectl describe clusterswitchpolicy dc-dr-policy

# Status section:
Status:
  Consecutive Remote Unreachable: 3   ← increments while DC is dark
  Consecutive Remote Failures:    0

# Events section (when threshold is hit but AllowDataLossFailover=false):
  Warning  RemoteUnreachableDataLossGuard  5s  mysql-keeper-controller
    remote unreachable for 10 consecutive checks (threshold=10)
    but AllowDataLossFailover=false — enable it to allow auto-promotion
```

### Prometheus metric

```
mysql_keeper_consecutive_failures{cluster_role="dr", scope="remote_unreachable"}
```

Recommended alert:

```yaml
- alert: RemoteUnreachableApproachingThreshold
  expr: |
    mysql_keeper_consecutive_failures{scope="remote_unreachable"}
    > 0
  for: 1m
  labels:
    severity: warning
  annotations:
    summary: "Remote cluster unreachable for {{ $value }} consecutive checks"
    description: >
      DC may be down. If this reaches remoteUnreachableThreshold and
      allowDataLossFailover=true, DR will auto-promote.
```

---

## 5. Why `allowDataLossFailover: true` is required

When DC is completely dark, the controller **cannot query DC's
`@@gtid_executed`**. Without that snapshot, preflight C5
(`GTID_SUBSET`) and C6 (`WAIT_FOR_EXECUTED_GTID_SET`) cannot run.

If DC had transactions that DR has not yet applied (replication lag > 0
at the moment DC died), those transactions are **permanently lost** once
DR becomes the new master.

`allowDataLossFailover: true` is an explicit acknowledgement of this risk.
It bypasses C5/C6 only — all other preflight checks (C1 remote-readonly
check is skipped for the remote-unreachable path since remote is
unreachable, but C3 channel health, C7 log_replica_updates, C8–C11 binlog
retention etc.) run as normal on the local cluster.

### Risk table

| Condition at failover | Data loss? |
|-----------------------|-----------|
| DR was fully caught up (lag = 0) | None |
| DR had 1–N unapplied transactions | Yes — N transactions lost |
| DR replication was broken before DC died | Unknown — potentially large |

**Mitigation:** monitor `mysql_keeper_gtid_missing_transactions` and
`mysql_keeper_replication_lag_seconds`. If either is non-zero when DC goes
dark, data loss is guaranteed. The `GTIDLagAlertThresholdTransactions`
setting can fire a Warning event before this becomes critical.

---

## 6. Network partition risk

The main risk with remote-unreachable promotion is promoting on a **network
partition** rather than an actual DC failure:

```
DC alive ─────✂──── network cut ─────  DR cannot reach DC
                                        → DR auto-promotes
                                        → SPLIT-BRAIN: 2 active masters
```

**Mitigations in order of effectiveness:**

1. **Set a long-enough threshold.** Most network partitions heal in < 60 s.
   A threshold of 10 × 15 s = 150 s survives all but sustained outages.

2. **Monitor GTID lag.** If DC-to-DR replication is flowing normally when
   the partition starts, the `remoteUnreachable` counter climbs but
   `gtid_missing_transactions` stays near 0. If both climb simultaneously,
   DC is more likely actually dead (replication + k8s connectivity both
   lost).

3. **ProxySQL corroboration (future).** A planned enhancement: before
   promoting, attempt TCP connect to DC's ProxySQL admin port. If that is
   also unreachable, the probability of a real failure (not just a MySQL
   port block) is higher.

4. **External arbiter (future).** For zero-risk split-brain prevention, an
   external witness (etcd, ZooKeeper) is the only fully reliable solution.

**For environments where network partitions are frequent**, keep
`remoteUnreachableThreshold: 0` (disabled) and use the manual trigger
after human confirmation instead.

---

## 7. Interaction with other features

| Feature | Interaction |
|---------|------------|
| `autoFailover: false` | Remote-unreachable path is disabled (checked first) |
| Cooldown | Applies — remote-unreachable promotion respects the cooldown window |
| Manual trigger | Manual trigger bypasses all auto-gates including this one |
| `allowDataLossFailover: false` | Blocks remote-unreachable path with blocker `remote_unreachable_dataloss_guard` |
| Local-failure path | Independent — either path alone can trigger a switchover |
| Peer-lease gate | Still checked after `EvaluateSwitchover` returns `Should=true` |
| GTID lag monitoring | Not affected — continues reporting lag even when remote is unreachable (will report stale or absent data) |

---

## 8. Operational runbook

### DC goes dark — expected sequence

```
t=0      DC k8s + MySQL unreachable
t=15s    consecutiveRemoteUnreachable = 1
t=30s    consecutiveRemoteUnreachable = 2
...
t=150s   consecutiveRemoteUnreachable = 10 == remoteUnreachableThreshold
         EvaluateSwitchover: Should=true
         reason: "automatic failover: remote sustained-unreachable for 10
                  consecutive checks (threshold=10)"
t=150s+  Phase: SwitchingOver
           PreFlight: C3 channel check (local only), C7, C8–C11
           Fence: local ProxySQL blackhole (DC unreachable, no SQL fence)
           Promote: local DR becomes read_only=OFF
           Routing: ProxySQL updated to point to local DR
           ReverseReplica: channel STOP (will re-establish when DC returns)
           Verify: write-probe on new active
         Phase: Monitoring
         status.activeCluster = "dr"
         consecutiveRemoteUnreachable = 0
```

### DC recovers after DR promotion

When DC comes back online it will start as a second active node — potential
split-brain. The reverse-replication channel was stopped during promotion
but DC's `read_only` was not changed (DC was unreachable during fencing).

**Manual steps required after DC recovery:**

```bash
# 1. Fence DC immediately — set read_only=ON on all PXC nodes
#    (via MANO, kubectl patch isSource=false, or direct SQL)

# 2. Re-establish replication DC←DR (DR is now the source)
#    Configure CHANGE REPLICATION SOURCE TO on DC pointing to DR

# 3. Wait for DC to catch up (monitor gtid_missing_transactions on DC)

# 4. Optionally: plan a DR drill to switch back DC → active
```

A future enhancement will automate step 2 (full ReverseReplica wiring).

---

## 9. Disabling the feature

Set `remoteUnreachableThreshold: 0` (the default). No restart required —
takes effect on the next reconcile after the CR is patched.

```bash
kubectl patch clusterswitchpolicy dc-dr-policy \
  --type=merge \
  -p '{"spec":{"healthCheck":{"remoteUnreachableThreshold":0}}}'
```
