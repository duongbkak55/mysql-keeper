# Replication error handling — alarm + auto-skip

mysql-keeper detects replication apply errors on the local replica channel
and optionally auto-skips transactions that match a conservative whitelist
so replication can continue without operator intervention. Repeated skips
trip a quarantine guard that blocks promote until an operator reviews the
divergence.

## Quick reference

| Surface | Where it shows up |
|---|---|
| `kubectl describe csp <name>` | Conditions `ReplicationHealthy`, `ReplicaQuarantined` + status field `replicationErrors` (last error, skip history, quarantine state) |
| K8s events | `ReplicationSQLError`, `ReplicationTransactionSkipped`, `WouldSkipTransaction`, `SkipRateLimited`, `ReplicationSkipFailed`, `ReplicationSkipNoGTID`, `ReplicaQuarantined`, `ReplicaQuarantineCleared`, `ClearQuarantineRefused`, `GTIDGapHigh` |
| Prometheus | `mysql_keeper_replication_error{cluster_role,channel,errno}`, `mysql_keeper_replication_skipped_total{cluster_role,errno}`, `mysql_keeper_replication_skip_blocked_total{cluster_role,reason}`, `mysql_keeper_replication_skip_failed_total{cluster_role,errno}`, `mysql_keeper_replica_quarantined{cluster_role}`, `mysql_keeper_quarantine_clear_refused_total{cluster_role,reason}` |
| PreFlight | New hard check **C12_ReplicaNotQuarantined** blocks promote while quarantined |

## How detection works

Each reconcile, the controller queries the local replica's
`performance_schema.replication_applier_status_by_worker` for any worker that
reports `LAST_ERROR_NUMBER > 0`. Single-thread setups fall back to
`replication_applier_status_by_coordinator`. The first observed error
populates `status.replicationErrors.lastError`; every distinct errno also
sets the `replication_error` Prometheus gauge to 1.

A separate alarm fires when the GTID gap (transactions on the source the
replica has not yet applied) exceeds
`spec.replicationErrorHandling.gtidGapAlertThreshold`. This path is
alarm-only — it never triggers a skip.

## Auto-skip mechanism

When the feature is enabled and an error matches the whitelist, the
controller injects an empty transaction with the failed GTID:

```sql
STOP REPLICA SQL_THREAD FOR CHANNEL '<name>';
SET gtid_next = '<failed-gtid>';
BEGIN;
COMMIT;
SET gtid_next = AUTOMATIC;
START REPLICA SQL_THREAD FOR CHANNEL '<name>';
```

This is the standard MySQL 5.7+ recipe. The empty transaction advances the
applier past the bad row without modifying `gtid_executed` directly. The
GTID literal is validated against `[0-9a-fA-F:-]` before being embedded —
any other character (quote, semicolon, comment) is refused outright.

If any step between `STOP` and the final `START` fails, the controller
makes a best-effort attempt to reset `gtid_next=AUTOMATIC` and restart the
SQL thread, then returns the error to the operator.

## Configuration

Default behaviour for a freshly-created CR that includes
`replicationErrorHandling`:

```yaml
spec:
  replicationErrorHandling:
    gtidGapAlertThreshold: 0          # 0 = alarm disabled
    autoSkip:
      enabled: true
      dryRun: false
      errorCodeWhitelist: [1062, 1032]
      maxSkipsPerWindow: 3
      window: 10m
      maxSkipBeforeQuarantine: 5
      quarantineWindow: 1h
```

**Upgrade safety**: an existing CR that did not declare
`replicationErrorHandling` before this feature shipped is treated as
`enabled=false` regardless of the kubebuilder default — the controller
will alarm but not auto-skip until the operator explicitly opts in. This
prevents silent data divergence on upgrade.

### Per-field reference

| Field | Default | Notes |
|---|---|---|
| `gtidGapAlertThreshold` | **0** | Positive values raise a Warning event when `gtid_missing_transactions` exceeds it |
| `autoSkip.enabled` | **true** (when block present), **false** otherwise | Master toggle. Detection + alarm always run regardless |
| `autoSkip.dryRun` | **false** | When true, emits `WouldSkipTransaction` events but never runs SQL. Useful for validating new clusters |
| `autoSkip.errorCodeWhitelist` | **[1062, 1032]** | MySQL error numbers eligible for auto-skip. Schema-drift codes (1146, 1054) are intentionally absent — they require operator review |
| `autoSkip.maxSkipsPerWindow` | **3** | Soft rate-limit |
| `autoSkip.window` | **10m** | Rolling rate-limit window |
| `autoSkip.maxSkipBeforeQuarantine` | **5** | Hard quarantine threshold (must be ≥ `maxSkipsPerWindow`) |
| `autoSkip.quarantineWindow` | **1h** | Rolling window for the quarantine threshold |

## Quarantine — why and how to clear it

Auto-skip silently drops rows. A handful of skips during a localised
incident is usually harmless; a sustained burst is a signal the replica is
diverging from the source and **must not be promoted** until an operator
has investigated.

When the combined skip count inside `quarantineWindow` strictly exceeds
`maxSkipBeforeQuarantine`, the controller:

1. Sets `status.replicationErrors.quarantinedSince` and
   `quarantineReason`.
2. Flips condition `ReplicaQuarantined` to True.
3. Sets gauge `mysql_keeper_replica_quarantined{cluster_role} = 1`.
4. Refuses every further auto-skip while quarantined.
5. Causes PreFlight check **C12_ReplicaNotQuarantined** to fail hard,
   blocking both manual and automatic promote until cleared.

Quarantine is **sticky**: it does NOT clear automatically when the count
drifts back below threshold. The operator must explicitly clear it.

### Clearing the quarantine

After investigating the divergence (and reconciling data if needed):

```bash
kubectl annotate clusterswitchpolicy <name> \
  mysql.keeper.io/clear-quarantine="$(date -u +%FT%TZ)" --overwrite
```

The controller will release the quarantine on the next reconcile when:

- The annotation value differs from `status.replicationErrors.lastClearAnnotationValue`
- No SQL apply error is currently observed
- The skip count inside `quarantineWindow` is at or below threshold

If any of these conditions is not met the annotation is preserved; the
controller will retry the release on subsequent reconciles. The
`ReplicaQuarantineCleared` event is emitted on success.

## Structured logs (ELK / Loki)

Every state transition is logged as a single structured line via the
controller-runtime logger so external log pipelines (Fluent Bit → ELK,
Promtail → Loki, etc.) can ingest and alert without parsing K8s events.
The `event` key uniquely identifies the transition.

| `event=` value | Emitted when | Key fields |
|---|---|---|
| `replication_sql_error` | Per-worker SQL apply error observed | `cluster_role`, `channel`, `worker_id`, `errno`, `gtid`, `message` |
| `replication_gtid_gap_high` | Missing transactions exceed `gtidGapAlertThreshold` | `cluster_role`, `channel`, `missing`, `threshold` |
| `replication_transaction_skipped` | Auto-skip succeeded | `cluster_role`, `channel`, `errno`, `gtid`, `message` |
| `replication_would_skip` | Dry-run would-skip | same as above + `dry_run=true` |
| `replication_skip_blocked` | Skip suppressed | `cluster_role`, `channel`, `errno`, `gtid`, `reason` ∈ `{disabled, unsupported_inspector, quarantined, not_whitelisted, missing_gtid, rate_limited}` |
| `replication_skip_failed` | SkipNextTransaction returned error | `cluster_role`, `channel`, `errno`, `gtid` (level=ERROR) |
| `replica_quarantine_entered` | Skip count first crossed `maxSkipBeforeQuarantine` | `cluster_role`, `channel`, `reason` |
| `replica_quarantine_cleared` | Operator annotation released quarantine | `cluster_role`, `channel`, `annotation_value` |
| `replica_quarantine_clear_refused` | Annotation observed but preconditions failed | `cluster_role`, `channel`, `annotation_value`, `reason` |

Example PromQL/LogQL alert: `count_over_time({event="replica_quarantine_entered"}[1h]) > 0`.

## Race safety

The controller uses controller-runtime leader election to ensure only one
Reconcile loop is active per cluster. Cross-cluster races are not possible
because at most one cluster acts as the replica during normal operation.
The auto-skip path is therefore not protected by an additional lease — if
you observe duplicate skips, file a bug.

## Failure modes

| Symptom | Cause | Resolution |
|---|---|---|
| `ReplicationSkipNoGTID` event | MySQL did not expose `LAST_PROCESSED_TRANSACTION` (older server) | Use a server that supports the worker view, or skip manually |
| `ReplicationSkipFailed` event | SQL skip failed mid-flight; controller restored automatic GTID + restarted thread | Investigate the underlying error — repeated failures are not retried automatically |
| `SkipRateLimited` event | Burst of skip-eligible errors exceeded `maxSkipsPerWindow` | Investigate the source data; raise the limit only after triage |
| `ReplicaQuarantined=True` condition | Skip count exceeded `maxSkipBeforeQuarantine` | Reconcile data divergence then clear with the annotation above |

## Related fields

- `spec.healthCheck.gtidLagAlertThresholdTransactions` — pre-existing GTID
  lag alarm. The new `replicationErrorHandling.gtidGapAlertThreshold` is
  intentionally a separate knob so you can tune them independently during
  migration. Consolidation is on the follow-up list.
- `spec.preFlight.catchupTimeout` — already gated by C5/C6, unrelated to
  C12 but applies in the same preflight pass.
