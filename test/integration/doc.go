// Package integration_test contains end-to-end tests that spin up real MySQL
// (and optionally ProxySQL) instances via testcontainers-go. They are
// intentionally NOT part of the default `go test ./...` run because they
// require a Docker daemon and pull ~200MB of container images on first run.
//
// To run locally:
//
//	docker info                                        # verify Docker is up
//	go test -tags=integration ./test/integration/...
//
// CI:
//
//	The tag is added in .github/workflows/integration.yml (separate workflow
//	from the unit test pipeline so slow-to-start Docker-based runs do not
//	block PR merges).
//
// Each test file runs in the shared containers brought up by TestMain in
// setup_test.go. Tests must clean up any rows they insert (TRUNCATE in a
// t.Cleanup) so the suite can run in any order without cross-test bleed.
package integration_test
