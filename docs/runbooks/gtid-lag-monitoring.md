# GTID Lag Monitoring

> **Terminology:** *GTID lag* = time-based delay (seconds behind source, measured by `mysql_keeper_replication_lag_seconds`).
> *GTID gap* = transaction count (unapplied GTIDs, measured by `mysql_keeper_gtid_missing_transactions`).
> These are distinct signals — lag can be high with a small gap (large transactions), or gap can be high with low lag (fast replication catching up).
>
> `spec.healthCheck.gtidLagAlertThresholdTransactions` is **deprecated** — use `spec.replicationErrorHandling.gtidGapAlertThreshold` for new deployments (adds CR condition + Prometheus gauge in addition to the K8s event).

Describes how mysql-keeper continuously measures replication lag between the
source (DC) and replica (DR) clusters, how it differs from the switchover
preflight checks, and why both are needed.

---

## 1. Continuous monitoring — every reconcile (`observeGTIDLag`)

Called every 15 s alongside the health checks, **before** any switchover
decision is made.

```
DC (source)                          DR (replica)
─────────────────                    ─────────────────
gtid_executed =                      gtid_executed =
  uuid:1-1000                          uuid:1-995

     │                                     │
     │  GetExecutedGTID()                  │
     │◄────────────────                    │
     │                                     │
     │  MissingGTIDs("uuid:1-1000")        │
     │────────────────────────────────────►│
     │              GTID_SUBTRACT(         │
     │                "uuid:1-1000",       │
     │                @@gtid_executed)     │
     │              = "uuid:996-1000"      │
     │◄────────────────────────────────────│
     │
     │  CountGTIDTransactions("uuid:996-1000") = 5
     │
     ├─► Prometheus: gtid_missing_transactions{cluster_role="dc"} = 5
     ├─► Prometheus: replication_lag_seconds{cluster_role="dc", channel="dc-to-dr"} = 3
     ├─► CR status:  status.gtidLag.missingTransactions = 5
     │               status.gtidLag.lagSeconds = 3
     │               status.gtidLag.measuredAt = <now>
     └─► [if 5 > spec.healthCheck.gtidLagAlertThresholdTransactions]
             k8s Warning event: "GTIDLagHigh"
             "GTID lag: 5 unapplied transactions exceeds threshold 3"
```

**Queries used:**

| Step | SQL |
|------|-----|
| Get source position | `SELECT @@GLOBAL.gtid_executed` on local |
| Compute missing set | `SELECT GTID_SUBTRACT(?, @@GLOBAL.gtid_executed)` on remote |
| Count transactions | Pure Go parser — no DB call |
| Lag seconds | `TIMESTAMPDIFF(SECOND, LAST_APPLIED_TRANSACTION_ORIGINAL_COMMIT_TIMESTAMP, NOW())` via `performance_schema` on remote |

---

## 2. Pre-switchover gate — Preflight C5 and C6

Runs only when a switchover is about to be attempted. Blocks the flip if
the replica has not applied every source transaction.

```
DC (source)                          DR (replica)
─────────────────                    ─────────────────
Snapshot gtid_executed
= "uuid:1-1000"
     │
     │  C5: IsGTIDSubset("uuid:1-1000")
     │────────────────────────────────►
     │      GTID_SUBSET(               │
     │        "uuid:1-1000",           │
     │        @@gtid_executed)         │
     │◄──────────────────────────────── │
     │      = 0  (FALSE — not caught up)│
     │
     │  C6: WaitForGTID("uuid:1-1000", catchupTimeout=30s)
     │────────────────────────────────►
     │      WAIT_FOR_EXECUTED_GTID_SET( │
     │        "uuid:1-1000", 30)        │
     │        ← blocks until DR applies │
     │          all GTIDs or times out  │
     │◄──────────────────────────────── │
     │      = 0 (success) or 1 (timeout)│
     │
     └─► result=0 → proceed to Fence phase
         result=1 → ABORT switchover, surface in status.lastPreFlight
```

If C5 passes immediately (DR already caught up), C6 is skipped.
If C6 times out → the switchover is **aborted before fencing anything**.

---

## 3. Monitoring vs preflight — comparison

| | Monitoring (`observeGTIDLag`) | Preflight C5 / C6 |
|---|---|---|
| **When** | Every 15 s, continuously | Only at switchover start |
| **Purpose** | Early warning, observability | Hard gate — prevents unsafe flip |
| **Query** | `GTID_SUBTRACT` → count | `GTID_SUBSET` + `WAIT_FOR_EXECUTED_GTID_SET` |
| **Action on lag** | Metric increment + Warning event | Abort switchover attempt |
| **Waits for catchup?** | No | Yes — up to `catchupTimeout` (default 30 s) |
| **Blocks traffic?** | Never | Blocks the flip, not traffic |

---

## 4. Why both are needed

Without continuous monitoring, the operator only discovers a replication lag
problem when the switchover is already in progress — at the worst possible
moment.

```
Timeline example — network issue causes DR to fall behind:

  t=0 m   DR begins lagging (network degradation)
  t=0.5m  gtid_missing_transactions > threshold
           → Warning/GTIDLagHigh event on CR       ← on-call sees it early
  t=2 m   Prometheus alert GTIDLagGrowing fires    ← ticket / page created
  t=5 m   DC health checks start failing
  t=5.75m consecutiveLocalFailures = 3
           → auto-failover triggered
           → Preflight C5: DR still behind
           → C6: wait 30 s … timeout
           → switchover ABORTED                    ← safe, but DC is still down
```

The monitoring layer gives on-call a **2–5 minute head start** to fix the
replication issue before DC actually fails. If it is fixed in time, the
switchover is not needed and the preflight never runs.

If DC fails before the lag is resolved:
- Preflight C5/C6 protects against promoting a stale replica.
- `spec.allowDataLossFailover: true` can override this as an explicit
  business decision (documented risk acceptance).

---

## 5. Alerting rules (Prometheus)

```yaml
groups:
  - name: mysql-keeper-gtid
    rules:
      - alert: GTIDLagGrowing
        expr: mysql_keeper_gtid_missing_transactions > 100
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.cluster_role }}: {{ $value }} transactions unapplied"
          description: >
            The DR replica is falling behind. If this persists, a future
            switchover will be blocked by preflight C5/C6.

      - alert: ReplicationLagHigh
        expr: >
          mysql_keeper_replication_lag_seconds >= 0
          and mysql_keeper_replication_lag_seconds > 30
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Replication lag {{ $value }}s on {{ $labels.channel }}"
          description: >
            Lag exceeds 30 s. Preflight C6 allows up to catchupTimeout (default
            30 s) to catch up — sustained lag beyond this will abort switchovers.

      - alert: ReplicationLagCritical
        expr: >
          mysql_keeper_replication_lag_seconds >= 0
          and mysql_keeper_replication_lag_seconds > 120
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "Replication lag {{ $value }}s — failover will lose data"
```

---

## 6. kubectl visibility

```bash
kubectl describe clusterswitchpolicy dc-dr-policy

# Look for:
Status:
  Gtid Lag:
    Lag Seconds:           3
    Measured At:           2026-04-25T10:30:00Z
    Missing Transactions:  5

# Events section:
  Warning  GTIDLagHigh  2m  mysql-keeper-controller
    GTID lag: 150 unapplied transactions exceeds threshold 100
```

---

## 7. Configuring the early-warning threshold

```yaml
spec:
  healthCheck:
    interval: 15s
    failureThreshold: 3
    gtidLagAlertThresholdTransactions: 100   # emit Warning event above this
```

Set to `0` (default) to disable the k8s event. The Prometheus metrics are
always exported regardless of this setting.
