# Integration tests

Real-MySQL tests that exercise GTID semantics, replication lifecycle, and the
switchover engine end-to-end against two Percona 8.0 containers. They are
gated behind `//go:build integration` so they never run in the default
`go test ./...` pipeline.

## Requirements

- Docker daemon reachable (the tests use [testcontainers-go](https://golang.testcontainers.org/)).
- ~300MB of disk for the `percona/percona-server:8.0` image on first run.
- 2GB free RAM for two MySQL instances.
- Ports: the containers bind random host ports; no fixed port conflicts.

## Running

Local (Mac/Linux):

```bash
docker info                                        # verify Docker is up
go test -tags=integration -v -count=1 ./test/integration/...
```

Local (Windows with Docker Desktop):

```powershell
docker info
go test -tags=integration -v -count=1 .\test\integration\...
```

First run will pull images; expect ~5 min. Subsequent runs are ~90s end-to-end.

## What runs, what it proves

| Test                                   | Maps to review                                | Asserts                                                                 |
| -------------------------------------- | --------------------------------------------- | ----------------------------------------------------------------------- |
| `TestInteg_GTIDSubsetPass`             | happy path                                    | C1, C3, C5, C6, C7, C8, C9 all pass when DC/DR in sync                   |
| `TestInteg_GTIDSubsetFail_LagInjected` | S1 — **production incident root cause**       | C5 + C6 both fail when applier stopped and writes land only on DC       |
| `TestInteg_WaitForGTIDTimeout`         | S1 / C6                                       | `WaitForGTID` returns a non-nil error when the replica cannot catch up  |
| `TestInteg_ReplicationChannelBroken`   | S8 / C3                                       | C3 fails when IO thread is stopped                                      |
| `TestInteg_BinlogRetentionShort`       | S12 / C11 (soft)                              | Soft-fail is flagged but does not block `OK()` — no false-blocking      |
| `TestInteg_LeaderLease_Concurrent`     | S15 / 4.7                                     | Two concurrent acquires → exactly one winner, one `ErrLeaseHeldElsewhere`|
| `TestInteg_FullSwitchoverHappyPath`    | S1 / S2 / S15 / S19 combined                  | End-to-end: read_only flips on both sides, GTID_SUBTRACT(dc,dr) == "" after promote |

## Directory layout

```
test/integration/
  doc.go             // package doc + build-tag convention
  setup_test.go      // TestMain — spin up containers + configure replication
  helpers_test.go    // openRootDB + other cross-test utilities
  gtid_test.go       // C3 / C5 / C6 / C11 checks driven by real SQL
  lease_test.go      // keeper.leader row contention under concurrency
  switchover_test.go // full engine Execute against the live pair
  testdata/
    my-dc.cnf        // Percona config for the DC side
    my-dr.cnf        // Percona config for the DR side (read_only=ON)
```

## Adding a new test

1. Put the test in whichever `*_test.go` best matches its scope, or create a
   new file with the `//go:build integration` tag.
2. Reuse `buildPreflight()`, `waitForReplica(ctx, t, target)`, `writeOnDC(…)`
   helpers from `gtid_test.go` / `setup_test.go` — do NOT re-implement
   replication setup in the test body.
3. If your test mutates DC or DR state (STOP REPLICA, SET GLOBAL …), register
   a `t.Cleanup` that restores the baseline. The suite shares two containers
   across all tests — a dirty state from one test breaks all the later ones.

## Known limitations

- ProxySQL is stubbed (`stubProxy`) in `TestInteg_FullSwitchoverHappyPath`.
  Adding a ProxySQL container is straightforward — left as future work because
  the 2-phase apply logic is already covered by go-sqlmock in
  `internal/proxysql/manager_test.go`.
- The tests assume a single replication channel named `dc-to-dr`. Changing
  that would require passing the name through `switchover.PreFlight.Channel`
  in every helper.
- MANO is not part of these tests. The MANO path is validated in Tier C
  (kind + fake MANO server).
