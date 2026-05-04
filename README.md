# mysql-keeper

Kubernetes controller for automatic DC-DR MySQL switchover.
Monitors two Percona XtraDB Cluster (PXC) sites and promotes the DR cluster when DC fails — zero-touch failover via the MANO LCM API.

---

## How It Works

```
DC Kubernetes Cluster                      DR Kubernetes Cluster
──────────────────────────────             ──────────────────────────────
mysql-keeper (leader-elected)              mysql-keeper (leader-elected)
     │                                          │
     ├─ LOCAL k8s API ──► PXC CRD              ├─ LOCAL k8s API ──► PXC CRD
     ├─ LOCAL MySQL ─────► DC PXC              ├─ LOCAL MySQL ─────► DR PXC
     ├─ LOCAL ProxySQL ──► DC ProxySQL x3      ├─ LOCAL ProxySQL ──► DR ProxySQL x3
     ├─ REMOTE MySQL ────► DR PXC (3306)       ├─ REMOTE MySQL ────► DC PXC (3306)
     └─ MANO API ─────────► MANO server        └─ MANO API ─────────► MANO server
```

**Key constraint**: Kubernetes API servers are NOT reachable cross-cluster. Only MySQL port 3306 and ProxySQL admin port 6032 are shared between DC and DR networks.

**State changes** (`isSource` on the PXC CRD) are applied exclusively through the MANO LCM API, which then reconciles the PXC Operator to enforce `read_only` state on MySQL. This ensures the CRD stays consistent even after pod restarts or Kubernetes crashes.

### Normal State
| Cluster | read_only | ProxySQL HG10 (write) | ProxySQL HG20 (read) |
|---------|-----------|----------------------|---------------------|
| DC      | OFF (writable) | → DC PXC       | → DC PXC            |
| DR      | ON (replica)   | —              | → DR PXC            |

### After Failover (DR becomes primary)
| Cluster | read_only | ProxySQL HG10 (write) | ProxySQL HG20 (read) |
|---------|-----------|----------------------|---------------------|
| DC      | ON (demoted)   | → DR PXC ext   | → DR PXC ext        |
| DR      | OFF (writable) | → DR PXC       | → DR PXC            |

---

## Prerequisites

- Kubernetes 1.25+ on both DC and DR clusters
- [Percona XtraDB Operator](https://docs.percona.com/percona-operator-for-mysql/pxc/) installed on both clusters
- PXC clusters deployed with async replication configured (`spec.replication.channels`)
- ProxySQL deployed on both clusters (3 instances each, admin port 6032)
- MANO system reachable from both clusters with CNF resources registered for each PXC cluster
- `kubectl` and `kustomize` (or `kubectl apply -k`) installed locally

---

## Building the Docker Image

The image cross-compiles to `linux/amd64` regardless of your build host architecture.

```bash
# Build
docker build -t mysql-keeper:v0.1.0 .

# Push to your registry
docker tag mysql-keeper:v0.1.0 ghcr.io/<your-org>/mysql-keeper:v0.1.0
docker push ghcr.io/<your-org>/mysql-keeper:v0.1.0
```

Update the image reference in `deploy/dc/kustomization.yaml` and `deploy/dr/kustomization.yaml`:

```yaml
images:
  - name: ghcr.io/duongnguyen/mysql-keeper
    newTag: "v0.1.0"
```

---

## Deployment

Deploy separately to each Kubernetes cluster. Each cluster gets its own `ClusterSwitchPolicy` CR with `spec.clusterRole: dc` or `spec.clusterRole: dr`.

### Step 1 — Create Secrets

Run these commands in each cluster (DC and DR), substituting your real credentials.

**DC cluster:**

```bash
# MySQL admin credentials for the local DC PXC cluster
kubectl create secret generic pxc-dc-admin-creds \
  --namespace mysql-keeper-system \
  --from-literal=username=root \
  --from-literal=password=<dc-mysql-root-password>

# MySQL admin credentials for the remote DR PXC cluster (read via cross-cluster MySQL)
kubectl create secret generic pxc-dr-admin-creds \
  --namespace mysql-keeper-system \
  --from-literal=username=root \
  --from-literal=password=<dr-mysql-root-password>

# ProxySQL admin credentials (same secret used for all 3 DC ProxySQL instances)
kubectl create secret generic proxysql-dc-admin-creds \
  --namespace mysql-keeper-system \
  --from-literal=username=admin \
  --from-literal=password=<proxysql-admin-password>

# MANO credentials (username + password for auto-login via POST /users/auth)
kubectl create secret generic mano-credentials \
  --namespace mysql-keeper-system \
  --from-literal=username=<mano-username> \
  --from-literal=password=<mano-password>
```

**DR cluster** (same structure, swap dc↔dr where appropriate):

```bash
kubectl create secret generic pxc-dr-admin-creds \
  --namespace mysql-keeper-system \
  --from-literal=username=root \
  --from-literal=password=<dr-mysql-root-password>

kubectl create secret generic pxc-dc-admin-creds \
  --namespace mysql-keeper-system \
  --from-literal=username=root \
  --from-literal=password=<dc-mysql-root-password>

kubectl create secret generic proxysql-dr-admin-creds \
  --namespace mysql-keeper-system \
  --from-literal=username=admin \
  --from-literal=password=<proxysql-admin-password>

kubectl create secret generic mano-credentials \
  --namespace mysql-keeper-system \
  --from-literal=username=<mano-username> \
  --from-literal=password=<mano-password>
```

### Step 2 — Edit the ClusterSwitchPolicy CRs

**DC cluster** — edit `config/samples/clusterswitchpolicy_dc.yaml`:

```yaml
spec:
  clusterRole: dc
  pxcNamespace: percona-xtradb
  pxcName: pxc-dc
  replicationChannelName: dc-to-dr       # must match PXC CRD channel name

  localMySQL:
    host: pxc-dc-haproxy.percona-xtradb.svc.cluster.local
    port: 3306
    credentialsSecretRef:
      name: pxc-dc-admin-creds
      namespace: mysql-keeper-system

  remoteMySQL:
    host: 10.20.0.100                    # DR external MySQL IP (cross-cluster reachable)
    port: 3306
    credentialsSecretRef:
      name: pxc-dr-admin-creds
      namespace: mysql-keeper-system

  mano:
    host: https://mano.example.com       # MANO API base URL
    tokenSecretRef:
      name: mano-token
      namespace: mysql-keeper-system
    localCnfName: pxc-dc-cnf            # DC CNF name in MANO
    localVduName: pxc-dc-vdu
    remoteCnfName: pxc-dr-cnf           # DR CNF name in MANO
    remoteVduName: pxc-dr-vdu
    pollInterval: 5s
    pollTimeout: 5m

  autoFailover: true
```

**DR cluster** — edit `config/samples/clusterswitchpolicy_dr.yaml` (note: local/remote CNF names are swapped):

```yaml
spec:
  clusterRole: dr
  mano:
    localCnfName: pxc-dr-cnf            # DR CNF name in MANO (local to this controller)
    remoteCnfName: pxc-dc-cnf           # DC CNF name in MANO (remote)
```

### Step 3 — Deploy

```bash
# DC cluster
kubectl apply -k deploy/dc/

# DR cluster (switch kubeconfig context first)
kubectl apply -k deploy/dr/
```

This installs:
- `mysql-keeper-system` namespace
- `ClusterSwitchPolicies` CRD
- `ClusterRole` / `ClusterRoleBinding` / `ServiceAccount`
- `Deployment` (2 replicas, leader-elected)
- `ClusterSwitchPolicy` CR

### Step 4 — Verify

```bash
# Check controller is running (2 pods, 1 active leader)
kubectl get pods -n mysql-keeper-system

# Check CR status
kubectl get clusterswitchpolicies -o wide
# NAME            ROLE   ACTIVE   PHASE        AGE
# dc-dr-policy    dc     dc       Monitoring   2m

# Describe for full health status
kubectl describe clusterswitchpolicy dc-dr-policy
```

---

## MANO API Integration

mysql-keeper uses the MANO CNF Lifecycle Management API to toggle `spec.replication.channels[].isSource` on PXC CNFs. MANO then triggers the PXC Operator, which enforces `read_only` state on MySQL.

### Why MANO

Patching the PXC CRD directly via the k8s API is unreliable across clusters: the remote k8s API is not reachable, and the PXC Operator would overwrite direct SQL `read_only` changes on its next reconcile. Using MANO:

1. Both the local and remote PXC CNFs are updated atomically
2. Changes survive pod restarts and k8s crashes (stored in etcd)
3. The PXC Operator never fights our state changes

### Flow

```
mysql-keeper                        MANO API                       PXC Operator
     │                                  │                               │
     │─ POST /cnflcm/v1/custom-         │                               │
     │  resources/{cnf}/{vdu}/update ──►│                               │
     │                                  │─ 202 Accepted + lcmOpOccId    │
     │                                  │                               │
     │─ GET /cnflcm/v1/lcm-op-occ/{id} ►│  (poll every pollInterval)    │
     │◄── operationState: COMPLETED ────│                               │
     │                                  │──► reconcile PXC CRD ────────►│
     │                                  │                               │─ SET read_only
```

### Request Format

`POST /cnflcm/v1/custom-resources/{cnfName}/{vduName}/update`

```json
{
  "cnfName": "pxc-dc-cnf",
  "vduName": "pxc-dc-vdu",
  "updateVduOperatorRequest": {
    "description": "mysql-keeper: set isSource=true",
    "timeoutTimer": "5m0s",
    "updateFields": {
      "replicationChannels[0].isSource": "true"
    }
  }
}
```

`GET /cnflcm/v1/lcm-op-occ/{id}` — polled until `operationState` is `COMPLETED`.

### Authentication Modes

**Credentials (recommended)** — the client calls `POST /users/auth` automatically and caches the token:

```yaml
mano:
  host: https://mano.example.com
  credentialsSecretRef:
    name: mano-credentials        # Secret with "username" and "password" keys
    namespace: mysql-keeper-system
```

Login request sent automatically:
```
POST /users/auth
Authorization: Basic base64(username:password)
Content-Type: application/json

{"grant_type": "client_credentials"}
```

The token is cached and refreshed 60 seconds before `expires_in` expires (default 8 hours).

**Static token** — provide a pre-obtained Bearer token:

```yaml
mano:
  host: https://mano.example.com
  tokenSecretRef:
    name: mano-token              # Secret with a "token" key
    namespace: mysql-keeper-system
```

### MANO Config Fields

| Field | Description |
|-------|-------------|
| `mano.host` | MANO API base URL, e.g. `https://mano.example.com` |
| `mano.credentialsSecretRef` | Secret with `username` + `password` keys — auto-login via `POST /users/auth` |
| `mano.tokenSecretRef` | Secret with a `token` key — static Bearer token (no auto-refresh) |
| `mano.localCnfName` | CNF name of the **local** PXC cluster in MANO |
| `mano.localVduName` | VDU name of the **local** PXC cluster in MANO |
| `mano.remoteCnfName` | CNF name of the **remote** PXC cluster in MANO |
| `mano.remoteVduName` | VDU name of the **remote** PXC cluster in MANO |
| `mano.pollInterval` | How often to poll lcm-op-occ (default: `5s`) |
| `mano.pollTimeout` | Max wait for COMPLETED (default: `5m`) |

At least one of `credentialsSecretRef` or `tokenSecretRef` must be set. If both are set, `credentialsSecretRef` takes precedence.

---

## Switchover Operations

### Automatic Failover

Triggered when `spec.autoFailover: true` and the local cluster fails `failureThreshold` consecutive health checks (~45s with default `interval: 15s`, `failureThreshold: 3`).

Before promoting, the controller verifies:
- Remote cluster is reachable
- Remote cluster is currently `read_only=ON` (replica, not already a writer — prevents split-brain)

Failover phases:
1. **Fence** — set local cluster `read_only=ON` via MANO (best-effort, continues even if local is unreachable)
2. **Promote** — set remote cluster `read_only=OFF` via MANO, verify write with probe query
3. **Routing** — update local ProxySQL: route HG10 (write) and HG20 (read) to the promoted cluster

### Manual Planned Switchover

Patch the CR to set `spec.manualSwitchoverTarget: promote-remote`:

```bash
kubectl patch clusterswitchpolicy dc-dr-policy \
  --type=merge \
  -p '{"spec":{"manualSwitchoverTarget":"promote-remote"}}'
```

The controller executes a graceful switchover (waits `drainTimeout` for connections to drain before fencing), then clears `manualSwitchoverTarget` when done.

### Monitor Progress

```bash
# Watch phase transitions
kubectl get clusterswitchpolicy dc-dr-policy -w

# Check events
kubectl describe clusterswitchpolicy dc-dr-policy

# Prometheus metrics (port 8080)
curl http://<pod-ip>:8080/metrics | grep mysql_keeper
```

Key metrics:
- `mysql_keeper_cluster_healthy{cluster="local|remote"}` — health gauge
- `mysql_keeper_switchover_total{result="success|failed|rolledback"}` — switchover counter
- `mysql_keeper_consecutive_failures{cluster="local|remote"}` — failure streak gauge
- `mysql_keeper_replication_error{cluster_role,channel,errno}` — 1 while a SQL applier error is firing
- `mysql_keeper_replication_skipped_total{cluster_role,errno}` — auto-skipped transactions counter
- `mysql_keeper_replication_skip_blocked_total{cluster_role,reason}` — would-be skips that were blocked (`not_whitelisted` / `rate_limited` / `dry_run` / `quarantined`)
- `mysql_keeper_replica_quarantined{cluster_role}` — 1 when PreFlight C12 is blocking promote
- `mysql_keeper_replication_skip_failed_total{cluster_role,errno}` — SkipNextTransaction SQL flow returned an error
- `mysql_keeper_quarantine_clear_refused_total{cluster_role,reason}` — clear-quarantine annotation refused (`active_error` / `burst_in_window`)

---

## Replication Error Handling

mysql-keeper detects SQL apply errors and GTID gaps on the local replica and
optionally auto-skips whitelisted errors so replication can continue without
operator intervention. Repeated skips trip a quarantine guard that blocks
promote (PreFlight C12) until cleared via annotation.

Default config (production-safe):

```yaml
spec:
  replicationErrorHandling:
    autoSkip:
      enabled: true
      errorCodeWhitelist: [1062, 1032]   # duplicate key + row not found
      maxSkipsPerWindow: 3
      window: 10m
      maxSkipBeforeQuarantine: 5
      quarantineWindow: 1h
```

Clear quarantine after operator review:

```bash
kubectl annotate clusterswitchpolicy <name> \
  mysql.keeper.io/clear-quarantine="$(date -u +%FT%TZ)" --overwrite
```

See [`docs/replication-error-handling.md`](docs/replication-error-handling.md)
for the full design (alarm sources, skip mechanism, failure modes,
upgrade safety).

---

## Configuration Reference

```yaml
spec:
  clusterRole: dc                    # "dc" or "dr"
  pxcNamespace: percona-xtradb       # namespace of the local PerconaXtraDBCluster CR
  pxcName: pxc-dc                    # name of the local PerconaXtraDBCluster CR
  replicationChannelName: dc-to-dr   # channel name in spec.replication.channels[].name

  localMySQL:
    host: pxc-dc-haproxy.percona-xtradb.svc.cluster.local
    port: 3306
    credentialsSecretRef:
      name: pxc-dc-admin-creds
      namespace: mysql-keeper-system

  remoteMySQL:
    host: 10.20.0.100                # remote cluster's external MySQL IP
    port: 3306
    credentialsSecretRef:
      name: pxc-dr-admin-creds
      namespace: mysql-keeper-system

  proxySQL:
    - host: proxysql-0.proxysql-dc.percona-xtradb.svc.cluster.local
      adminPort: 6032
      credentialsSecretRef:
        name: proxysql-dc-admin-creds
        namespace: mysql-keeper-system
    # ... repeat for proxysql-1 and proxysql-2

  healthCheck:
    interval: 15s              # health check frequency
    failureThreshold: 3        # failures before declaring unhealthy
    mysqlCheckTimeout: 5s      # per-query MySQL timeout
    proxySQLMinHealthy: 2      # min ProxySQL instances that must be up

  switchover:
    timeout: 5m                # abort switchover if not done within this time
    drainTimeout: 30s          # wait for connections to drain (planned switchover)
    fenceTimeout: 10s          # timeout for the fencing step
    readWriteHostgroup: 10     # ProxySQL HG for write traffic
    readOnlyHostgroup: 20      # ProxySQL HG for read traffic
    remoteWriterHost: 10.20.0.100   # remote cluster IP for ProxySQL routing after failover
    remoteWriterPort: 3306
    localWriterHost: pxc-dc-haproxy.percona-xtradb.svc.cluster.local

  mano:
    host: https://mano.example.com
    credentialsSecretRef:           # recommended: auto-login + token refresh
      name: mano-credentials
      namespace: mysql-keeper-system
    # tokenSecretRef:               # alternative: static Bearer token (no refresh)
    #   name: mano-token
    #   namespace: mysql-keeper-system
    localCnfName: pxc-dc-cnf
    localVduName: pxc-dc-vdu
    remoteCnfName: pxc-dr-cnf
    remoteVduName: pxc-dr-vdu
    pollInterval: 5s
    pollTimeout: 5m

  autoFailover: true
  manualSwitchoverTarget: ""   # set to "promote-remote" to trigger planned switchover
```

---

## PXC Replication Channel Setup

The `replicationChannelName` in the CR must match a channel configured in your `PerconaXtraDBCluster` CRD. Example:

```yaml
# On DC cluster — DC is the source
apiVersion: pxc.percona.com/v1
kind: PerconaXtraDBCluster
metadata:
  name: pxc-dc
  namespace: percona-xtradb
spec:
  replication:
    enabled: true
    channels:
      - name: dc-to-dr
        isSource: true           # DC is the writer/source
```

```yaml
# On DR cluster — DR is the replica
apiVersion: pxc.percona.com/v1
kind: PerconaXtraDBCluster
metadata:
  name: pxc-dr
  namespace: percona-xtradb
spec:
  replication:
    enabled: true
    channels:
      - name: dc-to-dr
        isSource: false          # DR is the replica
        sourcesList:
          - host: 10.10.0.100   # DC external MySQL IP
            port: 3306
            weight: 100
```

After failover, MANO flips `isSource` on both CNFs, and the PXC Operator reconciles the above accordingly.

---

## Troubleshooting

**Controller stays in `Initializing` phase**
- Check secrets exist in `mysql-keeper-system` namespace
- Verify MySQL credentials can connect to local PXC: `mysql -h <haproxy-host> -u root -p`

**Switchover not triggering despite DC being down**
- Check `spec.autoFailover: true` is set
- Check `status.consecutiveLocalFailures` is incrementing: `kubectl get csp dc-dr-policy -o jsonpath='{.status.consecutiveLocalFailures}'`
- Verify DR is reachable via MySQL from DC network: `mysql -h 10.20.0.100 -u root -p`

**MANO operation stuck / timeout**
- Check MANO API is reachable: `curl -H "Authorization: Bearer <token>" https://mano.example.com/cnflcm/v1/lcm-op-occ/<id>`
- Increase `mano.pollTimeout` if MANO operations are slow
- Check controller logs: `kubectl logs -n mysql-keeper-system -l app.kubernetes.io/name=mysql-keeper`

**Split-brain guard — switchover aborted**
- If the remote cluster already has `read_only=OFF`, the controller refuses to promote
- This means both clusters think they are primary — investigate before proceeding
- Manually set `read_only=ON` on the unintended writer, then retry

**ProxySQL routing not updated**
- Check ProxySQL admin credentials are correct
- Verify ProxySQL admin port 6032 is reachable from the controller pod
- Inspect ProxySQL directly: `mysql -h proxysql-0 -P 6032 -u admin -p -e "SELECT * FROM runtime_mysql_servers"`
