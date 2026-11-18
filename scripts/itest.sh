#!/usr/bin/env bash
# Runs the integration tests against a throwaway local cluster.
# Uses kind if installed, otherwise k3d. The cluster is always deleted at the end.
set -euo pipefail
cd "$(dirname "$0")/.."

NAME=plinth-test
KUBECONFIG_FILE="$(mktemp)"
export KUBECONFIG="$KUBECONFIG_FILE"

if command -v kind >/dev/null 2>&1; then
  TOOL=kind
  CONTEXT="kind-$NAME"
  create() { kind create cluster --name "$NAME" --kubeconfig "$KUBECONFIG_FILE" --wait 120s; }
  delete() { kind delete cluster --name "$NAME" --kubeconfig "$KUBECONFIG_FILE" || true; }
elif command -v k3d >/dev/null 2>&1; then
  TOOL=k3d
  CONTEXT="k3d-$NAME"
  create() { k3d cluster create "$NAME" --wait --kubeconfig-update-default=false --kubeconfig-switch-context=false \
             && k3d kubeconfig get "$NAME" > "$KUBECONFIG_FILE"; }
  delete() { k3d cluster delete "$NAME" || true; }
else
  echo "need kind or k3d (and a running Docker)" >&2
  exit 1
fi

cleanup() { delete; rm -f "$KUBECONFIG_FILE"; }
trap cleanup EXIT

echo "==> creating $TOOL cluster $NAME"
create

echo "==> running integration tests on context $CONTEXT"
PLINTH_TEST_CONTEXT="$CONTEXT" go test -tags=integration -race -count=1 -v ./internal/provider/kubernetes/...
