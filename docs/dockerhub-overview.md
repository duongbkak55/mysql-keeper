# mysql-keeper

Kubernetes controller for **automatic DC–DR switchover** of Percona XtraDB
Cluster (PXC) deployments. Monitors two PXC sites and promotes the DR
cluster when DC fails — zero-touch failover gated by a production-safe
preflight checklist that refuses to flip while the replica is behind.

**Source:** [github.com/duongbkak55/mysql-keeper](https://github.com/duongbkak55/mysql-keeper)
**License:** Apache 2.0

## Why this exists

Built in response to a real production incident: two PXC clusters with
async replication diverged after an automated flip, eventually surfacing
as `ER_MASTER_HAS_PURGED_REQUIRED_GTIDS` (Error 1236). This controller
closes the gap by:

- Refusing a flip unless the replica has every GTID the source has
  (`GTID_SUBSET` preflight C5 + C6 wait-for-catchup).
- Refusing a flip when both clusters are ReadOnly (cluster-wide incident,
  not a failover scenario).
- Checking `log_replica_updates`, binlog retention, replication channel
  health, and gtid_purged coverage **before** fencing anything.
- Checkpointing every phase to the CR status so a controller pod restart
  mid-switchover can resume or abandon, never silently forget.

Full production-readiness review, engine design, and 26 failure
scenarios mapped to code:
[docs/mysql-keeper-production-review.md](https://github.com/duongbkak55/mysql-keeper/blob/main/mysql-keeper-production-review.md).

## Image

| Item        | Value                                                 |
| ----------- | ----------------------------------------------------- |
| Base image  | `scratch` (distroless — no shell, no package manager) |
| Binary size | ~39 MB compressed, single static Go binary            |
| Runs as     | `65532:65532` (non-root)                              |
| Entrypoint  | `/manager`                                            |
| Platforms   | `linux/amd64`, `linux/arm64`                          |

## Tags

| Tag pattern | When it's pushed |
|-------------|-----------------|
| `latest`, `main` | Every push to `main` |
| `sha-<12char>`   | Every build — immutable reference |
| `<semver>`, `<major>.<minor>` | Every `v*` git tag |

Pin a specific SHA or semver in production. Avoid `:latest`.

## Quick start

### 1. Pull

```bash
docker pull duongbkak55/mysql-keeper:latest
```

### 2. Deploy to Kubernetes

```bash
git clone https://github.com/duongbkak55/mysql-keeper.git
cd mysql-keeper

# DC cluster
kubectl apply -f config/crd/bases/
kubectl apply -f config/rbac/
kubectl apply -f config/manager/deployment.yaml
kubectl apply -f config/samples/clusterswitchpolicy_dc.yaml

# DR cluster (separate kubeconfig)
KUBECONFIG=/path/to/dr-kubeconfig kubectl apply -f config/crd/bases/
KUBECONFIG=/path/to/dr-kubeconfig kubectl apply -f config/rbac/
KUBECONFIG=/path/to/dr-kubeconfig kubectl apply -f config/manager/deployment.yaml
KUBECONFIG=/path/to/dr-kubeconfig kubectl apply -f config/samples/clusterswitchpolicy_dr.yaml
```

Before applying, edit the CRs to match your environment (secret refs,
MySQL endpoints, ProxySQL hosts). See the sample files linked below.

### 3. Trigger a planned switchover

```bash
kubectl patch clusterswitchpolicy dc-dr-policy \
  --type=merge \
  -p '{"spec":{"manualSwitchoverTarget":"promote-remote"}}'
```

The engine runs `PreFlight → Fence → Promote → Routing → ReverseReplica
→ Verify` in sequence; each phase is checkpointed on the CR status.

## Where are the CR samples?

In the GitHub repo:

| File                                                                                                                                         | Role                                                           |
| -------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------- |
| [`config/samples/clusterswitchpolicy_dc.yaml`](https://github.com/duongbkak55/mysql-keeper/blob/main/config/samples/clusterswitchpolicy_dc.yaml) | CR for the DC cluster controller. Sets `clusterRole: dc`.       |
| [`config/samples/clusterswitchpolicy_dr.yaml`](https://github.com/duongbkak55/mysql-keeper/blob/main/config/samples/clusterswitchpolicy_dr.yaml) | CR for the DR cluster controller. Sets `clusterRole: dr`.       |

Both samples are annotated with every field the CRD accepts and safe
production defaults (cooldown 10m, preflight catchup 30s, binlog
retention ≥ 7 days, `allowDataLossFailover: false`).

## CRD spec reference

| File | Description |
|------|-------------|
| [`config/crd/bases/mysql.keeper.io_clusterswitchpolicies.yaml`](https://github.com/duongbkak55/mysql-keeper/blob/main/config/crd/bases/mysql.keeper.io_clusterswitchpolicies.yaml) | The full OpenAPI v3 schema for `ClusterSwitchPolicy`. Apply this once per cluster before deploying the controller. |
| [`api/v1alpha1/clusterswitchpolicy_types.go`](https://github.com/duongbkak55/mysql-keeper/blob/main/api/v1alpha1/clusterswitchpolicy_types.go) | Go source of truth — every field comment explains what the field does and why it exists. |

## Operational docs

- [`docs/runbooks/switchover-stuck.md`](https://github.com/duongbkak55/mysql-keeper/blob/main/docs/runbooks/switchover-stuck.md) — unstick a CR in `SwitchingOver`
- [`docs/runbooks/split-brain-recovery.md`](https://github.com/duongbkak55/mysql-keeper/blob/main/docs/runbooks/split-brain-recovery.md) — both clusters writable
- [`docs/runbooks/dr-drill.md`](https://github.com/duongbkak55/mysql-keeper/blob/main/docs/runbooks/dr-drill.md) — quarterly planned drill
- [`docs/runbooks/chaos-drills.md`](https://github.com/duongbkak55/mysql-keeper/blob/main/docs/runbooks/chaos-drills.md) — D1–D5 fault-injection drills
- [`config/observability/`](https://github.com/duongbkak55/mysql-keeper/tree/main/config/observability) — PrometheusRule + ServiceMonitor

## Metrics

Exposed on `:8080/metrics`. Keeper-specific series (all `mysql_keeper_` prefix):

**Health & switchover**
- `cluster_healthy`, `cluster_writable` — tri-state gauges per (role, scope)
- `switchover_total` by result, `switchover_duration_seconds` histogram
- `preflight_failures_total` by phase, `fence_failures_total` by path
- `both_clusters_readonly_total`, `both_clusters_writable_total` — safety counters
- `cooldown_block_total`, `manual_intervention_required_total`

**Replication health (updated every reconcile)**
- `replication_channel_{io,sql}_running` — 1 if thread running, 0 otherwise
- `binlog_expire_logs_seconds` — current retention window
- `gtid_missing_transactions{cluster_role}` — transactions on the source not yet applied by the replica (`GTID_SUBTRACT` count). Alert when this is non-zero for more than a few minutes.
- `replication_lag_seconds{cluster_role,channel}` — estimated lag in seconds from `ORIGINAL_COMMIT_TIMESTAMP` of the last applied transaction. `-1` = no data (channel idle or not yet configured).

**Recommended alerting rules:**
```yaml
- alert: GTIDLagGrowing
  expr: mysql_keeper_gtid_missing_transactions > 100
  for: 5m
  annotations:
    summary: "Replica is falling behind — {{ $value }} transactions unapplied"

- alert: ReplicationLagHigh
  expr: mysql_keeper_replication_lag_seconds > 30
  for: 5m
  annotations:
    summary: "Replication lag {{ $value }}s — switchover may be blocked by preflight C6"
```

## Supported MySQL versions

Developed and tested against Percona XtraDB Cluster 8.0 with GTID
replication. Works against standalone MySQL 8.0 too (PXC-specific checks
degrade gracefully — see the `wsrep_cluster_status` handling in
`internal/pxc/manager.go`).

## Contributing / Issues

Issues + PRs welcome at
[github.com/duongbkak55/mysql-keeper/issues](https://github.com/duongbkak55/mysql-keeper/issues).
Every PR runs four CI tiers before merge: unit, integration (2 Percona
containers via testcontainers), e2e (kind cluster), and `docker-publish`
for main-branch pushes.
