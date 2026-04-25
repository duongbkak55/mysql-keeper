# Changelog

All notable changes to `mysql-keeper` are documented in this file. Dates are
ISO-8601. Releases follow semantic versioning for the external CRD API; the
internal Go packages may change without notice between releases.

## [0.3.0] — 2026-04-25 — GTID lag monitoring + dynamic ProxySQL discovery

### Added

- **Continuous GTID lag monitoring** — every reconcile now measures the gap
  between source and replica without waiting for a switchover attempt:
  - `mysql_keeper_gtid_missing_transactions{cluster_role}` — count of
    transactions present on the source but not yet applied by the replica
    (`GTID_SUBTRACT` scalar). Alert on this before preflight C5/C6 ever block
    a flip.
  - `mysql_keeper_replication_lag_seconds{cluster_role,channel}` — estimated
    lag derived from `ORIGINAL_COMMIT_TIMESTAMP` of the last applied
    transaction on the replica. `-1` when no data is available (channel idle
    or not yet configured).
  - `status.gtidLag` on the `ClusterSwitchPolicy` CR — updated every reconcile,
    visible via `kubectl describe` without Prometheus.
  - `spec.healthCheck.gtidLagAlertThresholdTransactions` — when positive, the
    controller emits a `Warning / GTIDLagHigh` Kubernetes event on the CR
    whenever the missing-transaction count exceeds this value. Provides an
    early on-call signal before replication lag grows to a point where a
    switchover would be blocked.
  - `internal/pxc.CountGTIDTransactions(string) int64` — pure GTID set parser;
    tested with 10 cases (ranges, singles, multi-UUID, MySQL newlines).
  - `(*Manager).GetReplicationLagSeconds(ctx, channel)` — queries
    `performance_schema.replication_applier_status_by_coordinator` joined
    with `replication_connection_status`; falls back to connection-status only
    for older schema variants.

- **`proxySQLSelector` — dynamic ProxySQL pod discovery** for
  Deployment-based ProxySQL instances whose pod IPs change on healing:
  - New `spec.proxySQLSelector` field on `ClusterSwitchPolicy` replaces the
    static `spec.proxySQL` list.  The controller lists pods by namespace +
    label selector at every reconcile, filters to Running+Ready+IP pods, and
    builds fresh endpoint slices — endpoints are always current after pod
    restarts or IP changes.
  - `spec.proxySQL` is now optional (omitempty); an error is returned if
    neither field is set.
  - RBAC updated: `pods get/list/watch` permission added.
  - CRD schema updated with `proxySQLSelector` object; `proxySQL` removed from
    `required`.
  - Sample CR (`config/samples/clusterswitchpolicy_dc.yaml`) updated —
    `proxySQLSelector` is now the default (Option A); static list is Option B
    (commented).
  - Unit tests in `internal/controller/proxysql_selector_test.go` using
    `controller-runtime/pkg/client/fake`: Running+Ready pods returned,
    Pending/not-Ready/no-IP/wrong-label excluded, zero-match no error, default
    port, both-nil error, selector routing.

- **Tier B integration tests** (real containers, `//go:build integration`):
  - `internal/proxysql/integration_test.go` — 3 tests with real ProxySQL
    2.7.1: `ApplyFailoverRouting`, `RollbackRouting`, blackhole fence on HG 9999.
  - `internal/pxc/gtid_integration_test.go` — 3 tests with real MySQL 8.0.39:
    GTID snapshot, `IsGTIDSubset`, `WaitForGTID`.  Fixed `ForAll` +
    ping-retry startup to avoid "driver: bad connection" on auth-plugin init.
  - `internal/mano/pxc_manager_integration_test.go` — 3 tests with real
    MySQL + mock MANO HTTP server: write-probe catches operator lag, happy
    path verifies `isSource=true`, `SetReadOnly` does not modify MySQL
    `@@read_only`.

### Changed

- `observeReplicationMetrics` now also computes GTID lag and calls the new
  `observeGTIDLag` helper; the `Recorder` field on the reconciler is populated
  from `mgr.GetEventRecorderFor("mysql-keeper-controller")`.
- The `lagQuerier` interface is local to `replication_metrics.go`, allowing
  `*pxc.Manager` to satisfy it without a breaking change to
  `switchover.ReplicationInspector` — `mano.PXCManager` silently skips the
  lag-seconds metric.

### Upgrade notes

- Re-apply the CRD (`config/crd/`) to pick up `proxySQLSelector` and
  `status.gtidLag`.
- Existing CRs that use `spec.proxySQL` continue to work unchanged.
- Set `spec.healthCheck.gtidLagAlertThresholdTransactions` to a positive
  integer (e.g. `100`) to enable early Warning events for your SLO.

---

## [0.2.0] — 2026-04-23 — production-readiness remediation

This is a **mandatory upgrade** for anyone running `autoFailover=true`. The
previous release could reproduce `ER_MASTER_HAS_PURGED_REQUIRED_GTIDS`
divergence after a DC-DR role flip; see `mysql-keeper-production-review.md`
in the repo root for the full analysis. Every P0 / P1 finding is addressed.

### Added

- Pre-flight checklist (C1, C3, C5, C6, C7, C8, C9, C10, C11) now gates every
  switchover attempt. The result is surfaced on
  `Status.LastPreFlight` and as `preflight_failures_total{phase=PreFlight}`.
- New phase-ordered engine: `PreFlight → Fence → Promote → Routing →
  ReverseReplica → Verify → Done`. `SwitchoverProgress` on the CR status now
  checkpoints every phase boundary for resume after pod restart.
- Leader lease (`keeper.leader` table) maintained on the writable cluster,
  read before auto-failover as a cross-cluster controller lock.
- ProxySQL two-phase `prepare / commit` apply. A single-instance failure
  during `prepare` aborts the whole change before any runtime effect.
- Alternate ProxySQL blackhole fence invoked when the local cluster is
  confirmed unreachable via `ProbeReachable`.
- CRD patch verification: a `Get`-after-`Patch` loop detects PXC operator
  revert of `isSource`.
- Finalizer `mysql.keeper.io/finalizer` prevents the CR from being garbage
  collected mid-switchover.
- `Both clusters ReadOnly` guard refuses auto-failover when DC and DR are
  both RO (the exact condition that triggered the production incident).
- Tri-state health counters: `ConsecutiveLocalUnreachable` is tracked
  separately from `ConsecutiveLocalFailures` so a transient network blip no
  longer contributes to the failover threshold.
- Cooldown period between successive switchovers (default 10 m) prevents
  ping-pong after an incident.
- `AllowDataLossFailover` explicit opt-in required before the controller can
  promote when the local cluster's GTID snapshot cannot be read.
- Observability — new metrics (`both_clusters_readonly_total`,
  `both_clusters_writable_total`, `cooldown_block_total`,
  `preflight_failures_total`, `fence_failures_total`,
  `manual_intervention_required_total`,
  `replication_channel_io_running`, `replication_channel_sql_running`,
  `binlog_expire_logs_seconds`), PrometheusRule manifests under
  `config/observability/`, and a ServiceMonitor.
- Structured events: `switchover`, `phase_transition`, `preflight check`,
  `should_switchover` with `attemptID` correlation.
- Runbooks under `docs/runbooks/`: stuck switchover, split-brain recovery,
  DR drill.

### Changed

- `phaseFence` is no longer best-effort. A SQL-fence failure aborts the
  attempt when the local cluster is reachable (avoids split-brain). Only a
  confirmed-unreachable local cluster triggers the blackhole-fence fallback.
- `SetReadWrite` on the new source runs `STOP REPLICA` + `RESET REPLICA ALL`
  for the configured channel so the new source does not fight its own former
  upstream.
- `ManualSwitchoverTarget` is cleared **before** the engine runs, not after
  success. A failed manual attempt no longer loops forever.
- `EnsureKeeperSchema` is now called from the reconciler on whichever cluster
  is writable. Previously the function existed but was never invoked, causing
  the post-promote write probe to fail on a fresh cluster.

### Removed

- Nothing yet — all previous CR fields are preserved and defaulted.

### Known limitations (tracked for 0.3.0)

- MANO LCM op-id resumption across pod restarts is not yet implemented; a
  restart mid-Promote leaves the MANO side running while the controller
  abandons the attempt after `ResumeStuckTimeout`.
- Cross-cluster `SwitchoverProgress` annotation (SB-2 from the review) is
  not yet wired — only the keeper.leader table (SB-1) is in place.
- Reverse replication setup currently only stops the old channel; full
  `CHANGE REPLICATION SOURCE TO` wiring is deferred to the PXC operator
  reconciling `spec.replication.channels[].sourcesList` after flip.

### Upgrade notes

- Re-apply the CRD (`config/crd/`) before rolling the new controller so the
  new `Status.SwitchoverProgress` / `Status.LastPreFlight` and `Spec.PreFlight`
  fields are recognised.
- Set `Spec.Switchover.CooldownPeriod` and `Spec.PreFlight.CatchupTimeout`
  explicitly on your production CRs to pin behaviour to the values you
  validate during the DR drill.
- Before enabling `autoFailover=true`, run at least one full DR drill using
  `docs/runbooks/dr-drill.md`.

## [0.1.0] — initial MANO release

Initial release with basic health checks, manual + auto switchover via MANO.
