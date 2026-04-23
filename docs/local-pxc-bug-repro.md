# Reproducing the production incident locally

This walks through reproducing the bug that drove the whole remediation —
two PXC clusters linked by async replication, a flip that happens while
the replica is behind, and the eventual `ER_MASTER_HAS_PURGED_REQUIRED_GTIDS`
(Error 1236) on the next attempted catch-up.

Everything runs on one Linux host (or WSL2 Ubuntu). No Kubernetes, no
MANO, no Percona Operator. Just two real `percona/percona-xtradb-cluster:8.0`
containers and a thin CLI that runs the preflight checklist.

**Time budget:** ~5 minutes total. **RAM budget:** ~2 GB.

## Prerequisites

Already installed by `scripts/local/install-prereqs.sh`:

- Docker Engine reachable from the current shell
- Go 1.25 (to build the preflight CLI)

## Step 1 — Bring up the staging

```bash
bash scripts/local/pxc-up.sh
```

Output ends with the DSNs and next-step pointers. Under the hood:

- 2 containers (`keeper-pxc-dc`, `keeper-pxc-dr`) on a compose network
- each bootstraps its own single-node Galera cluster
- DC creates replication grants + `keeper.probe` + `smoketest.pings`
- DR configures `CHANGE REPLICATION SOURCE TO … FOR CHANNEL 'dc-to-dr'`
  pointing at DC's container IP and starts the replica
- DR enforces `read_only=ON` + `super_read_only=ON`

Verify:

```bash
scripts/local/pxc-bug-simulate.sh status
```

Expected: both clusters in `wsrep_cluster_status=Primary`, DC writable,
DR read-only, replication `IO=ON SQL=ON`.

## Step 2 — Prove the preflight passes at baseline

```bash
scripts/local/pxc-preflight.sh
```

Expected: every hard check PASS, overall OK (exit 0). This is what the
controller would observe on a healthy pair.

```
PREFLIGHT CHECK RESULTS
-----------------------
  PASS  [hard] C1_RemoteReachableReadOnly  (Nms)
  PASS  [hard] C3_RemoteReplicationRunning  (Nms)
  PASS  [hard] C5_GTIDSubset  (Nms)
  PASS  [hard] C6_GTIDCatchup  (Nms)
  PASS  [hard] C7_RemoteLogReplicaUpdates  (Nms)
  PASS  [hard] C9_GTIDModeOn  (Nms)
  PASS  [hard] C8_BinlogFormatRow  (Nms)
  PASS  [soft] C11_RemoteBinlogRetention  (Nms)

OVERALL: OK — switchover is safe to proceed.
```

## Step 3 — Inject the bug (GTID lag)

This is the exact situation that caused the production incident: DR is
missing GTIDs that DC already committed.

```bash
scripts/local/pxc-bug-simulate.sh lag
```

Under the hood: stops DR's SQL applier, writes 200 rows on DC. The IO
thread keeps buffering so you can see the gap on both sides, but the
applier is not draining it.

## Step 4 — Preflight must refuse the flip

```bash
scripts/local/pxc-preflight.sh
```

Expected: **exit code 1**, with C5 and C6 failing:

```
  FAIL  [hard] C5_GTIDSubset  (Nms)
          remote missing N chars of GTID set: <uuid>:X-Y
  FAIL  [hard] C6_GTIDCatchup  (Nms)
          replica did not reach target GTID within 8s

OVERALL: BLOCKED — switchover must not proceed.
```

This is the fix in action. Before Sprint 1, `phaseVerify` only checked
"is remote read-only?" and would have allowed the flip — producing the
GTID divergence. The Sprint 1 preflight closes that gap.

## Step 5 — Recover, confirm baseline again

```bash
scripts/local/pxc-bug-simulate.sh recover
sleep 5
scripts/local/pxc-preflight.sh
```

Replication catches up → preflight returns OK again.

## Step 6 (optional) — Reproduce the exact Error 1236 state

The production incident eventually materialised as `ER_MASTER_HAS_PURGED_
REQUIRED_GTIDS`. Preflight C5/C6 block the earlier step that would have
led to this, but it is useful to see the end state once so the alert text
in production logs is familiar:

```bash
bash scripts/local/pxc-bug-simulate.sh purge-gtid
```

What the script does:

1. Stops DR's IO thread (buffering new events halts).
2. Writes 200 rows on DC → binlog grows.
3. `FLUSH BINARY LOGS` rotates to a new file; the old one still has the
   GTIDs DR is missing.
4. `PURGE BINARY LOGS BEFORE NOW()` removes the old file — now DC no
   longer has the events DR needs.
5. Starts DR's IO thread — the replica asks DC for the purged GTIDs, DC
   refuses, the IO thread hits:

   ```
   Got fatal error 1236 from source when reading data from binary log:
     'Cannot replicate because the source purged required binary logs.
      Replicate the missing transactions from elsewhere, or provision
      a new replica from backup. Consider increasing the source's binary
      log expiration period. ...'
   ```

This is an unrecoverable state — the only production fix is re-seeding DR
from an xtrabackup of DC. To reset the local staging, run
`scripts/local/pxc-down.sh` + `scripts/local/pxc-up.sh`.

## Step 7 (optional) — Simulate "both clusters ReadOnly"

In real production the original incident also involved a cluster-wide
quorum loss on DC that set `read_only=ON` automatically. We reproduce
the observable state with an explicit SET:

```bash
scripts/local/pxc-bug-simulate.sh both-ro
scripts/local/pxc-bug-simulate.sh status
```

Both DC and DR now show `read_only=1`. At this state the controller's
`EvaluateSwitchover` logic must return `Blocker=both_readonly` and refuse
to flip — that is exercised by the unit test:

```bash
go test -v -run TestDecide_BothReadOnlyBlocksAuto ./internal/controller/...
```

Restore writability when done:

```bash
scripts/local/pxc-bug-simulate.sh both-ro-clear
```

## Step 8 — Tear down

```bash
scripts/local/pxc-down.sh
```

Removes both containers and their volumes. Host ports 33011 / 33012 are
released.

## What this proves vs. what it doesn't

**Proves:**

- The preflight correctly refuses a flip when the replica is behind.
- `wsrep_cluster_status` is queried against real PXC wire behaviour.
- `log_replica_updates` / `gtid_mode` / `binlog_format` guardrails trip
  when the PXC is configured wrong.
- The "both RO" controller guard works against real PXC.

**Does NOT prove (staging-only):**

- 3-node Galera quorum semantics during a real node crash.
- MANO LCM API end-to-end.
- ProxySQL 2-phase routing apply across 3 real ProxySQL instances.
- Controller pod restart mid-Promote (that is Tier C on kind).

For those, use the real staging PXC environment with the chaos scripts
in `scripts/chaos/`.
