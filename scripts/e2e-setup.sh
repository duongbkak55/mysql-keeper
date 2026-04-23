#!/usr/bin/env bash
# e2e-setup.sh — spin up the kind cluster, build + load the controller
# image, and apply the minimal set of manifests the e2e suite depends on.
#
# We apply manifests directly (kubectl apply -f) rather than through
# `kubectl apply -k deploy/dc/` because the kustomize overlay reaches into
# ../../config/... which newer kustomize versions reject under their default
# "can't reference files outside the kustomization directory" security rule.
# The real deploy/dc/ path is fine for operators running from a checkout;
# here we just need the raw YAMLs.
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
NAMESPACE="${NAMESPACE:-mysql-keeper-system}"

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
kubectl apply -f "${REPO_ROOT}/config/crd/bases/"

echo ">>> Applying RBAC (ServiceAccount + ClusterRole + ClusterRoleBinding)"
# deployment.yaml already embeds the Namespace object; RBAC ServiceAccount
# references that namespace. Apply the ServiceAccount *after* deployment.yaml
# creates the namespace, or pre-create the namespace ourselves so the SA
# apply does not hit "namespace not found".
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f "${REPO_ROOT}/config/rbac/service_account.yaml"
kubectl apply -f "${REPO_ROOT}/config/rbac/role.yaml"
kubectl apply -f "${REPO_ROOT}/config/rbac/role_binding.yaml"

echo ">>> Applying controller Deployment (patched to use the e2e image)"
sed -e "s|image: ghcr.io/duongnguyen/mysql-keeper.*|image: ${IMAGE}|" \
    -e "s|imagePullPolicy: Always|imagePullPolicy: IfNotPresent|" \
    "${REPO_ROOT}/config/manager/deployment.yaml" \
  | kubectl apply -f -

echo ">>> Applying mysql-stub"
kubectl apply -f "${REPO_ROOT}/test/e2e/mysql-stub.yaml"

echo ">>> Exposing metrics via NodePort 30080"
kubectl apply -f - <<YAML
apiVersion: v1
kind: Service
metadata:
  name: mysql-keeper-metrics-np
  namespace: ${NAMESPACE}
spec:
  type: NodePort
  selector:
    app.kubernetes.io/name: mysql-keeper
  ports:
    - port: 8080
      targetPort: 8080
      nodePort: 30080
YAML

echo ">>> Waiting for controller to become available (180s)"
kubectl -n "${NAMESPACE}" rollout status \
  deploy/mysql-keeper-controller-manager --timeout=180s

echo ">>> Waiting for mysql-stub to become ready (180s)"
kubectl -n mysql-keeper-e2e rollout status statefulset/mysql-stub --timeout=180s

echo ""
echo "Cluster ready."
echo "  Context: kind-${CLUSTER_NAME}"
echo "  Metrics: http://127.0.0.1:30080/metrics"
echo ""
echo "Run the suite:"
echo "  go test -tags=e2e -v ./test/e2e/..."
