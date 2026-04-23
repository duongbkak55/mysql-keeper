# E2E tests

End-to-end tests that run against a real Kubernetes cluster (kind in CI).
They exercise the K8s-native parts of the controller:

- Finalizer-blocked deletion during an in-flight switchover (S25 / 4.13)
- SwitchoverProgress checkpoint surviving a controller pod restart (S6 / 4.6)
- `/metrics` endpoint exposing the keeper Prometheus registry (5.1)

The suite is gated behind `//go:build e2e` so it never runs with the default
`go test ./...` pipeline. MySQL is intentionally stubbed by a single `mysql:8.0`
pod — real replication is covered by Tier B; here we care about controller
plumbing.

## Requirements

- Docker Desktop (or any docker daemon).
- `kind` v0.22+ on `$PATH`.
- `kubectl` on `$PATH`.
- Free host port 30080 (used by the NodePort metrics service for scraping).

## Running

```bash
./scripts/e2e-setup.sh          # build image, create kind, apply manifests
go test -tags=e2e -v -count=1 ./test/e2e/...
./scripts/e2e-teardown.sh       # delete kind cluster
```

CI uses [.github/workflows/e2e.yml](../../.github/workflows/e2e.yml) which
runs the same scripts on ubuntu-latest and dumps controller pod logs on
failure for post-mortem.

## What runs, what it proves

| Test                                                       | Maps to review   | Asserts                                                                        |
| ---------------------------------------------------------- | ---------------- | ------------------------------------------------------------------------------ |
| `TestE2E_FinalizerBlocksDelete`                            | S25 / 4.13       | `DeletionTimestamp` is set but object is not GC'd while finalizer attached     |
| `TestE2E_SwitchoverProgressCheckpointSurvivesRestart`      | S6 / S7 / 4.6    | `Status.SwitchoverProgress.AttemptID` unchanged after scaling the deployment   |
| `TestE2E_MetricsExposed`                                   | 5.1              | `/metrics` returns 200 and contains `mysql_keeper_*` series                     |

## Overriding the test environment

- `KUBECONFIG=/path/to/kubeconfig` — target a cluster other than the
  `kind-keeper-e2e` context.
- `E2E_METRICS_URL=http://…` — skip the NodePort assumption; useful when
  running against a cloud cluster where the NodePort is inaccessible.

## Adding a new test

1. Put the test in a new `*_test.go` with `//go:build e2e` at the top.
2. Use `newClient(t)`, `fixtureCSP(name)`, `waitForFinalizer(…)` from
   `helpers_test.go` — do not build kubeconfig / CR scaffolding in the test
   body.
3. Clean up with `t.Cleanup(func() { removeFinalizerAndDelete(t, c, name) })`
   — the suite runs in a single shared namespace, so a leaking test poisons
   the later ones.
4. If your test restarts the controller, use the `kubectl(t, ...)` helper.
   Do not import the apps/v1 typed client just for a Scale call.

## Known limitations

- The mysql-stub image is **not** a real PXC node. Tests that need wsrep
  status or replication channel state must use Tier B instead.
- A Percona operator is not installed; tests that assume a PXC CRD exist
  (patchIsSource etc.) would fail. The current e2e tests avoid that path by
  driving Status directly.
- Single-namespace; we do not exercise cluster-scoped CSPs.
