#!/usr/bin/env bash
# e2e-setup.sh — spin up the kind cluster, load the controller image, and
# apply every manifest the e2e suite depends on. Idempotent: re-running after
# a change rolls the deployment and re-applies the CRDs.
#
# Usage:
#   ./scripts/e2e-setup.sh
#   go test -tags=e2e -v ./test/e2e/...
#   ./scripts/e2e-teardown.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-keeper-e2e}"
IMAGE="${IMAGE:-mysql-keeper:e2e}"

echo ">>> Ensuring kind cluster '${CLUSTER_NAME}' exists"
if ! kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
  kind create cluster --name "${CLUSTER_NAME}" --config "${REPO_ROOT}/test/e2e/kind-config.yaml"
fi
kubectl config use-context "kind-${CLUSTER_NAME}"

echo ">>> Building controller image"
docker build -t "${IMAGE}" "${REPO_ROOT}"

echo ">>> Loading image into kind"
kind load docker-image "${IMAGE}" --name "${CLUSTER_NAME}"

echo ">>> Applying CRDs"
kubectl apply -f "${REPO_ROOT}/config/crd/" || true

echo ">>> Applying controller manifests (RBAC + Deployment)"
kubectl apply -k "${REPO_ROOT}/deploy/dc/" || true

echo ">>> Applying mysql-stub"
kubectl apply -f "${REPO_ROOT}/test/e2e/mysql-stub.yaml"

echo ">>> Patching controller deployment to use e2e image and expose NodePort metrics"
kubectl -n mysql-keeper-system set image \
  deploy/mysql-keeper-controller-manager \
  manager="${IMAGE}" || true

# Expose /metrics via NodePort 30080 — matches kind-config.yaml extraPortMappings
# so the test suite can scrape http://127.0.0.1:30080/metrics without
# port-forwarding.
kubectl apply -f - <<'YAML'
apiVersion: v1
kind: Service
metadata:
  name: mysql-keeper-metrics-np
  namespace: mysql-keeper-system
spec:
  type: NodePort
  selector:
    app.kubernetes.io/name: mysql-keeper
  ports:
    - port: 8080
      targetPort: 8080
      nodePort: 30080
YAML

echo ">>> Waiting for controller to become available"
kubectl -n mysql-keeper-system rollout status \
  deploy/mysql-keeper-controller-manager --timeout=180s

echo ">>> Waiting for mysql-stub to become ready"
kubectl -n mysql-keeper-e2e rollout status statefulset/mysql-stub --timeout=180s

echo ""
echo "Cluster ready."
echo "  Context: kind-${CLUSTER_NAME}"
echo "  Metrics: http://127.0.0.1:30080/metrics"
echo ""
echo "Run the suite:"
echo "  go test -tags=e2e -v ./test/e2e/..."
