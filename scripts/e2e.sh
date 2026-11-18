#!/usr/bin/env bash
# End-to-end test: runs a whole onboarding request through the real drivers
# against a disposable stack (needs Docker, k3d, kubectl, git, python3):
#   2 Kubernetes clusters (dev, prod), PostgreSQL, Gitea, Harbor.
# Everything is deleted at the end, also on failure.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT="$PWD"
RUN="e2e$RANDOM"
WORK="$(mktemp -d)"
HARBOR_DIR=""

for bin in docker k3d kubectl git python3 curl; do
  command -v "$bin" >/dev/null || { echo "need $bin" >&2; exit 1; }
done

free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
log() { echo "==> $*" >&2; }

cleanup() {
  log "cleaning up"
  [ -n "$HARBOR_DIR" ] && "$ROOT/scripts/harbor.sh" down "$HARBOR_DIR" >/dev/null 2>&1 || true
  docker rm -f "plinth-$RUN-gitea" "plinth-$RUN-pg" >/dev/null 2>&1 || true
  k3d cluster delete "plinth-$RUN-dev" "plinth-$RUN-prod" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# ---- PostgreSQL ----------------------------------------------------------------
log "starting PostgreSQL"
PG_PORT="$(free_port)"
docker run -d --rm --name "plinth-$RUN-pg" -e POSTGRES_PASSWORD=e2e-pg-admin \
  -p "127.0.0.1:$PG_PORT:5432" postgres:17 >/dev/null

# ---- Gitea ----------------------------------------------------------------------
log "starting Gitea"
GITEA_PORT="$(free_port)"
GITEA_URL="http://127.0.0.1:$GITEA_PORT"
docker run -d --rm --name "plinth-$RUN-gitea" -p "127.0.0.1:$GITEA_PORT:3000" \
  -e GITEA__security__INSTALL_LOCK=true -e GITEA__database__DB_TYPE=sqlite3 \
  -e "GITEA__server__ROOT_URL=$GITEA_URL/" -e GITEA__server__DOMAIN=127.0.0.1 \
  -e GITEA__actions__ENABLED=true -e GITEA__service__DISABLE_REGISTRATION=true \
  gitea/gitea:1.23 >/dev/null

# ---- Kubernetes clusters (started in the background while the rest boots) ----------
for env in dev prod; do
  (
    k3d cluster create "plinth-$RUN-$env" --wait --no-lb \
      --kubeconfig-update-default=false --kubeconfig-switch-context=false \
      --k3s-arg "--disable=traefik@server:0" >/dev/null 2>&1 \
    && k3d kubeconfig get "plinth-$RUN-$env" > "$WORK/kubeconfig-$env"
  ) &
done

# ---- Harbor ----------------------------------------------------------------------
log "starting Harbor (slow on Apple Silicon: images run emulated)"
HARBOR_OUT="$("$ROOT/scripts/harbor.sh" up)"
eval "$(echo "$HARBOR_OUT" | grep '^HARBOR_')"
HARBOR_DIR="$HARBOR_DIR"

wait  # clusters
for env in dev prod; do [ -s "$WORK/kubeconfig-$env" ] || { echo "cluster $env failed to start" >&2; exit 1; }; done

# ---- wait for Postgres and Gitea ----------------------------------------------------
log "waiting for PostgreSQL and Gitea"
for _ in $(seq 1 60); do
  docker exec "plinth-$RUN-pg" pg_isready -U postgres >/dev/null 2>&1 && { sleep 2; docker exec "plinth-$RUN-pg" pg_isready -U postgres >/dev/null 2>&1 && break; }
  sleep 2
done
for _ in $(seq 1 90); do
  curl -fsS "$GITEA_URL/api/healthz" >/dev/null 2>&1 && break
  sleep 2
done
docker exec -u git "plinth-$RUN-gitea" gitea admin user create --admin --username plinth-admin \
  --password 'e2e-Gitea-pw-123' --email admin@example.test --must-change-password=false >/dev/null
GITEA_TOKEN="$(docker exec -u git "plinth-$RUN-gitea" gitea admin user generate-access-token \
  --username plinth-admin --token-name e2e --scopes all --raw)"

# ---- Harbor: a dedicated Plinth identity ---------------------------------------------
log "preparing Harbor identity"
H="$HARBOR_URL/api/v2.0"; HA="$HARBOR_ADMIN_USER:$HARBOR_ADMIN_PASSWORD"
curl -fsS -u "$HA" -X POST "$H/users" -H 'Content-Type: application/json' \
  -d '{"username":"plinth","email":"plinth@example.test","realname":"plinth","password":"E2e-plinth-Pw1"}' >/dev/null
HID="$(curl -fsS -u "$HA" "$H/users/search?username=plinth" | python3 -c 'import sys,json;print(json.load(sys.stdin)[0]["user_id"])')"
curl -fsS -u "$HA" -X PUT "$H/users/$HID/sysadmin" -H 'Content-Type: application/json' -d '{"sysadmin_flag":true}' >/dev/null

# ---- Kubernetes: Plinth's least-privilege identity ----------------------------------
for env in dev prod; do
  kc="$WORK/kubeconfig-$env"
  kubectl --kubeconfig "$kc" apply -f deploy/kubernetes/plinth-rbac.yaml >/dev/null
  tok="$(kubectl --kubeconfig "$kc" -n plinth-system create token plinth --duration=1h)"
  srv="$(kubectl --kubeconfig "$kc" config view --raw --minify -o jsonpath='{.clusters[0].cluster.server}')"
  ca="$(kubectl --kubeconfig "$kc" config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d)"
  up="$(echo "$env" | tr a-z A-Z)"
  export "E2E_K8S_${up}_SERVER=$srv" "E2E_K8S_${up}_TOKEN=$tok" "E2E_K8S_${up}_CA=$ca" "E2E_K8S_${up}_ADMIN_KUBECONFIG=$kc"
done

export E2E_GITEA_URL="$GITEA_URL" E2E_GITEA_TOKEN="$GITEA_TOKEN"
export E2E_PG_HOST=127.0.0.1 E2E_PG_PORT="$PG_PORT" E2E_PG_ADMIN_PASSWORD=e2e-pg-admin
export E2E_HARBOR_URL="$HARBOR_URL" E2E_HARBOR_USER=plinth E2E_HARBOR_PASSWORD='E2e-plinth-Pw1'

log "running the end-to-end test"
go test -tags=e2e -race -count=1 -timeout 20m -v ./internal/onboarding/ -run TestEndToEnd
