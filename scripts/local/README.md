# Local test environment

Set up Tier C (e2e on kind) and a practical subset of Tier D on a single
Linux / WSL2 host. The full Tier D catalogue (D1, D2, D5) still needs a real
staging cluster with two PXC deployments — the scripts here cover the
subset that runs against a laptop-sized environment.

## Quick start

```bash
# 1. Prereqs: Docker Engine + kubectl + kind + Go 1.25
bash scripts/local/install-prereqs.sh

# 2. Tier C — e2e tests on a fresh kind cluster (takes ~3 minutes)
bash scripts/e2e-setup.sh
go test -tags=e2e -v ./test/e2e/...
bash scripts/e2e-teardown.sh

# 3. Tier B — integration tests with 2 real Percona containers (~2 minutes)
#    Exercises the same regression as Tier D3 but as a Go unit-test runner.
go test -tags=integration -v ./test/integration/...

# 4. Tier D (light) — standalone MySQL replication pair for ad-hoc probing
bash scripts/local/staging-up.sh
#   Then poke at 127.0.0.1:33061 (DC) / 127.0.0.1:33062 (DR)
bash scripts/local/staging-down.sh
```

## What each tier covers locally

| Tier | Runs here? | Uses | Notes |
|------|-----------|------|-------|
| A unit | yes | `go test ./...` | No extra infra. |
| B integration | yes | testcontainers-go + 2 Percona containers | Covers the GTID-subset root-cause regression. |
| C e2e | yes | kind + controller image + mysql-stub | `scripts/e2e-setup.sh` builds image, creates kind, applies manifests. |
| D1 kill controller mid-Promote | **no** | needs controller + PXC + MANO stub | Staging-only. |
| D2 partition DC↔DR | **no** | needs Chaos Mesh + 2 PXC | Staging-only. |
| D3 inject lag → refuse flip | **partial** | Tier B integration already covers it | See `TestInteg_GTIDSubsetFail_LagInjected`. |
| D4 binlog retention purge | **partial** | Tier B has the schema; full flow needs staging | |
| D5 dual controller race | **no** | needs 2 kubeconfigs | Staging-only. |

## On WSL specifically

The `install-prereqs.sh` script adds `$USER` to the `docker` group. After
it runs once, open a **new** WSL shell (or run `newgrp docker`) so docker
commands work without sudo.

`kind` creates a docker-in-docker cluster. Make sure Docker Engine is
running inside WSL (`sudo service docker start` if systemd is disabled),
or use Docker Desktop with WSL integration enabled.

## Troubleshooting

- `docker info` returns permission denied → re-login the shell, or run
  `newgrp docker`, or `sudo usermod -aG docker $USER && exec sg docker`.
- `kind create cluster` times out → Docker Desktop WSL integration isn't
  sharing the `/var/run/docker.sock` properly. Turn the integration off
  and run Docker Engine directly in WSL.
- e2e tests fail with `Error from server (NotFound): namespaces "mysql-
  keeper-system" not found` → your `scripts/e2e-setup.sh` aborted mid-way;
  run `bash scripts/e2e-teardown.sh` and retry.
- Port 30080 on host already bound → another kind cluster is using it;
  run `kind get clusters` and delete strays with `kind delete cluster --name <name>`.
- `go get` fetches Go 1.25 but `go.mod` wants newer → `install-prereqs.sh`
  pins Go 1.25; if go.mod later bumps, update the script's `GO_VER`.

## Why Tier D1/D2/D5 are not here

Those drills assume a realistic production topology:

- **D1** needs the full controller + MANO integration, plus a PXC
  operator managing real Galera nodes. Without that, "kill controller
  mid-Promote" collapses to killing a pod that was never doing a promote
  anyway.
- **D2** needs Chaos Mesh to inject a network partition between the
  controller pod and the remote MySQL. Setting that up locally requires a
  multi-node kind cluster + Chaos Mesh CRDs + specific NodePort plumbing
  — more than a laptop can host usefully.
- **D5** needs two independent kubeconfigs (DC and DR clusters) with
  cross-cluster networking, so both controllers can reach the
  `keeper.leader` table on the peer MySQL cluster. Two kind clusters in a
  single docker network *can* do this, but the setup is intricate enough
  that it belongs in the real staging runbook.

When you have a staging environment, run the scripts in `scripts/chaos/`
directly; they are unchanged by this local setup.
