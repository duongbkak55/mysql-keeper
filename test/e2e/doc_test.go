//go:build e2e

// Package e2e_test contains end-to-end tests that run against a real
// Kubernetes cluster (kind in CI, any cluster with the CRD installed
// locally). They exercise everything the unit and integration tiers cannot
// — RBAC, leader election, finalizer-blocked deletion, SwitchoverProgress
// checkpointing across pod restarts, and metrics exposure.
//
// The suite is gated behind `//go:build e2e` so it never runs in the default
// `go test ./...` pipeline. MySQL is represented by a minimal "stub" image
// that accepts any admin command without actually doing the work — these
// tests are about controller correctness, not MySQL correctness (that is
// Tier B's job).
//
// To run locally:
//
//	./scripts/e2e-setup.sh      # creates the kind cluster + applies manifests
//	go test -tags=e2e -v ./test/e2e/...
//	./scripts/e2e-teardown.sh
//
// CI:
//
//	.github/workflows/e2e.yml runs the same script on ubuntu-latest.
package e2e_test
