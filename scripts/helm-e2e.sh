#!/usr/bin/env bash
# Installs the Helm chart on a throwaway k3d cluster (with a database and a
# stand-in identity provider inside it) and runs deploy/helm/e2e_test.go
# against it. Needs Docker, k3d, kubectl, helm, python3. The cluster and the
# images it used are always removed at the end.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT="$PWD"
RUN="helm$RANDOM"
CLUSTER="plinth-$RUN"
WORK="$(mktemp -d)"
export KUBECONFIG="$WORK/kubeconfig"
PIDS=()

for bin in docker k3d kubectl helm python3 go; do
  command -v "$bin" >/dev/null || { echo "need $bin" >&2; exit 1; }
done
log() { echo "==> $*" >&2; }
free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }

cleanup() {
  log "cleaning up"
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
  k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
  docker rmi -f "plinth:helm-e2e" "plinth-devidp:helm-e2e" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

log "building images (the server with the web interface, and the stand-in identity provider)"
docker build -q -t plinth:helm-e2e . >/dev/null
docker build -q --target devidp -t plinth-devidp:helm-e2e . >/dev/null

log "creating cluster $CLUSTER"
k3d cluster create "$CLUSTER" --wait --no-lb \
  --kubeconfig-update-default=false --kubeconfig-switch-context=false \
  --k3s-arg "--disable=traefik@server:0" >/dev/null 2>&1
k3d kubeconfig get "$CLUSTER" > "$KUBECONFIG"
# Load the two images built above straight into the node. (`k3d image import`
# can silently do nothing when Docker uses the containerd image store, so the
# result is checked.) The cluster pulls postgres and busybox itself.
for img in plinth:helm-e2e plinth-devidp:helm-e2e; do
  docker save "$img" | docker exec -i "k3d-$CLUSTER-server-0" ctr -n k8s.io images import - >/dev/null
  repo="${img%%:*}"
  # Capture first: `grep -q` closing the pipe early would fail the pipeline under pipefail.
  present="$(docker exec "k3d-$CLUSTER-server-0" crictl images)"
  grep -q "$repo" <<<"$present" || { echo "image $img did not reach the cluster node" >&2; exit 1; }
done

log "starting the database and the identity provider"
kubectl apply -f deploy/helm/ci/e2e-deps.yaml >/dev/null
kubectl -n db rollout status deploy/postgres --timeout=300s >/dev/null
kubectl -n idp rollout status deploy/devidp --timeout=300s >/dev/null

log "creating the two Secrets the chart expects"
kubectl create namespace platform >/dev/null
MASTER_KEY="$(docker run --rm plinth:helm-e2e genkey)"
kubectl -n platform create secret generic plinth-master-key --from-literal=master-key="$MASTER_KEY" >/dev/null
kubectl -n platform create secret generic plinth-database \
  --from-literal=database-url='postgres://plinth:plinth@postgres.db.svc.cluster.local:5432/plinth?sslmode=disable' >/dev/null

log "helm install"
helm install plinth deploy/helm/plinth -n platform -f deploy/helm/ci/e2e-values.yaml --wait --timeout 4m >/dev/null

log "port-forwarding the identity provider (the test forwards the application itself, as pods are replaced)"
IDP_PORT="$(free_port)"
kubectl -n idp port-forward svc/devidp "$IDP_PORT:9100" >/dev/null 2>&1 & PIDS+=($!)
for _ in $(seq 1 60); do
  curl -fsS "http://127.0.0.1:$IDP_PORT/keys" >/dev/null 2>&1 && break
  sleep 1
done

export HELM_E2E_KUBECONFIG="$KUBECONFIG" HELM_E2E_NS=platform
export HELM_E2E_IDP="http://127.0.0.1:$IDP_PORT"
export HELM_E2E_CHART="$ROOT/deploy/helm/plinth" HELM_E2E_VALUES="$ROOT/deploy/helm/ci/e2e-values.yaml"
export HELM_E2E_MASTER_KEY="$MASTER_KEY"

log "running the checks"
go test -tags=helme2e -count=1 -timeout 15m -v ./deploy/helm/ -run TestInstalledChart
