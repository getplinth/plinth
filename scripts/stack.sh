#!/usr/bin/env bash
# A complete local stack to try Plinth against real systems, kept running so you
# can look around (unlike `make e2e`, which checks and deletes everything).
#
#   scripts/stack.sh up [--harbor] [--keycloak]   create what is missing, start the rest, build and run Plinth
#   scripts/stack.sh stop                         stop everything, keep the data
#   scripts/stack.sh down                         delete everything, data included
#   scripts/stack.sh status                       what runs, where, and the credentials
#   scripts/stack.sh logs [name]                  plinth | gitea | postgres | mailpit | keycloak
#
# Always: PostgreSQL (Plinth's state, and the server project databases are made on), Gitea (with
# a golden template), a k3d Kubernetes cluster, Mailpit (catches e-mail), and Plinth built from
# this checkout, with the integrations and two blueprints already set up.
# --harbor adds a real Harbor registry (several GB, slow on Apple Silicon: amd64 under emulation).
# --keycloak adds a real identity provider, registered in Plinth's control panel.
#
# State lives in .stack/ (gitignored): generated passwords in .stack/state.env (mode 600).
# This is for local testing: services listen on 127.0.0.1 only, over plain http.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
DIR="$ROOT/.stack"
STATE="$DIR/state.env"

# ---- defaults (override from the environment) ------------------------------------------------------
: "${STACK_PLINTH_PORT:=8090}"
: "${STACK_PG_PORT:=55432}"
: "${STACK_GITEA_PORT:=53000}"
: "${STACK_MAIL_SMTP_PORT:=51025}"
: "${STACK_MAIL_UI_PORT:=58025}"
: "${STACK_KEYCLOAK_PORT:=58080}"
: "${STACK_CLUSTER:=plinth-stack}"
: "${STACK_GITEA_IMAGE:=gitea/gitea:1.23}"
: "${STACK_PG_IMAGE:=postgres:17}"
: "${STACK_KEYCLOAK_IMAGE:=quay.io/keycloak/keycloak:26.0}"
PREFIX=plinth-stack
PG="$PREFIX-pg"; GITEA="$PREFIX-gitea"; MAIL="$PREFIX-mailpit"; KC="$PREFIX-keycloak"
PLINTH_URL="http://127.0.0.1:$STACK_PLINTH_PORT"

say() { echo "==> $*" >&2; }
die() { echo "error: $*" >&2; exit 1; }
need() { for b in "$@"; do command -v "$b" >/dev/null || die "need $b (see docs/stack.md)"; done; }

# A random alphanumeric string that contains none of the names used as usernames here
# (Plinth refuses a password that contains the username).
rand() { python3 -c 'import secrets,string,sys
a = string.ascii_letters + string.digits
while True:
    s = "".join(secrets.choice(a) for _ in range(int(sys.argv[1])))
    if not any(w in s.lower() for w in ("admin", "alice", "bob", "ada", "plinth")): break
print(s)' "$1"; }
port_free() { python3 -c 'import socket,sys; s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
try: s.bind(("127.0.0.1", int(sys.argv[1])))
except OSError: sys.exit(1)' "$1"; }

# ---- state -----------------------------------------------------------------------------------------------
load() { [ -f "$STATE" ] && . "$STATE" || true; }
save() { # key value
  mkdir -p "$DIR"; touch "$STATE"; chmod 600 "$STATE"
  { grep -v "^$1=" "$STATE" || true; printf '%s=%q\n' "$1" "$2"; } > "$STATE.new" && mv "$STATE.new" "$STATE"; chmod 600 "$STATE"
}
secret() { # key length: generate once, keep
  load; local cur="${!1:-}"
  if [ -z "$cur" ]; then cur="$(rand "${2:-20}")"; save "$1" "$cur"; fi
  eval "$1=\$cur"
}

# ---- containers ------------------------------------------------------------------------------------------
container_state() { local s; s="$(docker inspect -f '{{.State.Status}}' "$1" 2>/dev/null)" || s=""; echo "${s:-none}"; }
ensure_port() { # port container: fail clearly when something else holds it
  [ "$(container_state "$2")" = running ] && return 0
  port_free "$1" || die "port $1 is in use (needed by $2). Free it, or set another with STACK_*_PORT (see docs/stack.md)"
}
run_or_start() { # name docker-run-args...
  local name="$1"; shift
  case "$(container_state "$name")" in
    running) ;;
    none) docker run -d --name "$name" --restart unless-stopped "$@" >/dev/null ;;
    *) docker start "$name" >/dev/null ;;
  esac
}

http_wait() { # url what [seconds]
  local i; for i in $(seq 1 "$(( ${3:-180} / 2 ))"); do curl -fsS -o /dev/null "$1" 2>/dev/null && return 0; sleep 2; done
  die "$2 did not become ready ($1)"
}

# ---- parts -----------------------------------------------------------------------------------------------
start_postgres() {
  ensure_port "$STACK_PG_PORT" "$PG"; secret PG_PASSWORD 24
  say "PostgreSQL on :$STACK_PG_PORT"
  run_or_start "$PG" -e "POSTGRES_PASSWORD=$PG_PASSWORD" -p "127.0.0.1:$STACK_PG_PORT:5432" -v "$PREFIX-pg-data:/var/lib/postgresql/data" "$STACK_PG_IMAGE"
}
wait_postgres() {
  local i ok=0
  for i in $(seq 1 60); do
    if docker exec "$PG" pg_isready -U postgres >/dev/null 2>&1; then ok=$((ok+1)); [ "$ok" -ge 2 ] && break; else ok=0; fi
    sleep 2
  done
  [ "$ok" -ge 2 ] || die "PostgreSQL did not become ready"
  docker exec "$PG" psql -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname='plinth'" | grep -q 1 \
    || docker exec "$PG" psql -U postgres -c "CREATE DATABASE plinth" >/dev/null
}

start_gitea() {
  ensure_port "$STACK_GITEA_PORT" "$GITEA"
  say "Gitea on :$STACK_GITEA_PORT"
  run_or_start "$GITEA" -p "127.0.0.1:$STACK_GITEA_PORT:3000" -v "$PREFIX-gitea-data:/data" \
    -e GITEA__security__INSTALL_LOCK=true -e GITEA__database__DB_TYPE=sqlite3 \
    -e "GITEA__server__ROOT_URL=http://127.0.0.1:$STACK_GITEA_PORT/" -e GITEA__server__DOMAIN=127.0.0.1 \
    -e GITEA__actions__ENABLED=true -e GITEA__service__DISABLE_REGISTRATION=true "$STACK_GITEA_IMAGE"
}
setup_gitea() {
  http_wait "http://127.0.0.1:$STACK_GITEA_PORT/api/healthz" Gitea 180
  secret GITEA_ADMIN_PASSWORD 20; load
  docker exec -u git "$GITEA" gitea admin user create --admin --username plinth-admin --password "$GITEA_ADMIN_PASSWORD" \
    --email admin@example.test --must-change-password=false >/dev/null 2>&1 || true
  if [ -z "${GITEA_TOKEN:-}" ]; then
    GITEA_TOKEN="$(docker exec -u git "$GITEA" gitea admin user generate-access-token --username plinth-admin --token-name "stack-$(date +%s)" --scopes all --raw)"
    save GITEA_TOKEN "$GITEA_TOKEN"
  fi
  # A golden template for new repositories, as the end-to-end test uses.
  local g="http://127.0.0.1:$STACK_GITEA_PORT/api/v1" auth="Authorization: token $GITEA_TOKEN"
  curl -fsS -H "$auth" -H 'Content-Type: application/json' -X POST "$g/orgs" -d '{"username":"platform"}' >/dev/null 2>&1 || true
  curl -fsS -H "$auth" -H 'Content-Type: application/json' -X POST "$g/orgs/platform/repos" \
    -d '{"name":"app-template","auto_init":true,"default_branch":"main"}' >/dev/null 2>&1 || true
  curl -fsS -H "$auth" -H 'Content-Type: application/json' -X POST "$g/repos/platform/app-template/contents/k8s/deployment.yaml" \
    -d "{\"content\":\"$(printf 'name: PROJECT_NAME\n' | base64 | tr -d '\n')\",\"message\":\"add\",\"branch\":\"main\"}" >/dev/null 2>&1 || true
}

start_mailpit() {
  ensure_port "$STACK_MAIL_SMTP_PORT" "$MAIL"; ensure_port "$STACK_MAIL_UI_PORT" "$MAIL"
  say "Mailpit (SMTP :$STACK_MAIL_SMTP_PORT, inbox http://127.0.0.1:$STACK_MAIL_UI_PORT)"
  run_or_start "$MAIL" -p "127.0.0.1:$STACK_MAIL_SMTP_PORT:1025" -p "127.0.0.1:$STACK_MAIL_UI_PORT:8025" \
    -e MP_SMTP_AUTH_ACCEPT_ANY=1 -e MP_SMTP_AUTH_ALLOW_INSECURE=1 axllent/mailpit
}

start_cluster() {
  if k3d cluster list "$STACK_CLUSTER" >/dev/null 2>&1; then
    say "Kubernetes cluster $STACK_CLUSTER (starting if stopped)"
    k3d cluster start "$STACK_CLUSTER" --wait >/dev/null 2>&1 || true
  else
    say "Kubernetes cluster $STACK_CLUSTER (k3d, first start takes a minute)"
    k3d cluster create "$STACK_CLUSTER" --wait --no-lb --kubeconfig-update-default=false --kubeconfig-switch-context=false \
      --k3s-arg "--disable=traefik@server:0" >/dev/null 2>&1 || die "could not create the k3d cluster"
  fi
  mkdir -p "$DIR"; k3d kubeconfig get "$STACK_CLUSTER" > "$DIR/kubeconfig"; chmod 600 "$DIR/kubeconfig"
}
setup_cluster() { # Plinth's least-privilege identity, with a long-lived token
  local kc="$DIR/kubeconfig"
  kubectl --kubeconfig "$kc" apply -f deploy/kubernetes/plinth-rbac.yaml >/dev/null
  load
  if [ -z "${K8S_TOKEN:-}" ]; then
    save K8S_TOKEN "$(kubectl --kubeconfig "$kc" -n plinth-system create token plinth --duration=8760h)"
  fi
  save K8S_SERVER "$(kubectl --kubeconfig "$kc" config view --raw --minify -o jsonpath='{.clusters[0].cluster.server}')"
  save K8S_CA "$(kubectl --kubeconfig "$kc" config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d)"
}

start_keycloak() {
  ensure_port "$STACK_KEYCLOAK_PORT" "$KC"; secret KC_ADMIN_PASSWORD 20
  say "Keycloak on :$STACK_KEYCLOAK_PORT"
  run_or_start "$KC" -p "127.0.0.1:$STACK_KEYCLOAK_PORT:8080" -v "$PREFIX-keycloak-data:/opt/keycloak/data" \
    -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e "KC_BOOTSTRAP_ADMIN_PASSWORD=$KC_ADMIN_PASSWORD" \
    -e KC_HTTP_ENABLED=true -e KC_HOSTNAME_STRICT=false -e KC_HEALTH_ENABLED=true \
    "$STACK_KEYCLOAK_IMAGE" start-dev
}
kcadm() { docker exec "$KC" /opt/keycloak/bin/kcadm.sh "$@"; }
# kc_id RESOURCE FIELD VALUE: the id of the item whose FIELD equals VALUE (exact match, unlike -q which searches).
# awk reads all of the output on purpose: stopping early would break the pipe and, with pipefail, fail the script.
kc_id() { kcadm get "$1" -r plinth --fields "id,$2" --format csv --noquotes | awk -F, -v v="$3" '$2==v && !found {print $1; found=1}'; }
setup_keycloak() {
  http_wait "http://127.0.0.1:$STACK_KEYCLOAK_PORT/realms/master" Keycloak 240
  load
  kcadm config credentials --server http://localhost:8080 --realm master --user admin --password "$KC_ADMIN_PASSWORD" >/dev/null
  if [ "$(kcadm get realms/plinth --fields displayName --format csv --noquotes 2>/dev/null || true)" = "Plinth (ready)" ]; then return 0; fi
  # Not there, or only half set up by an interrupted run: start the realm over.
  kcadm delete realms/plinth >/dev/null 2>&1 || true
  say "configuring the Keycloak realm 'plinth'"
  secret USER_PASSWORD 20; load
  kcadm create realms -s realm=plinth -s enabled=true -s sslRequired=none -s displayName=Plinth >/dev/null
  # Plinth asks for the "groups" scope: Keycloak refuses scopes it does not know, so define it and map group names into the token.
  kcadm create client-scopes -r plinth -s name=groups -s protocol=openid-connect -s 'attributes."include.in.token.scope"=true' >/dev/null
  local sid; sid="$(kc_id client-scopes name groups)"
  kcadm create "client-scopes/$sid/protocol-mappers/models" -r plinth -s name=groups -s protocol=openid-connect \
    -s protocolMapper=oidc-group-membership-mapper -s 'config."full.path"=false' -s 'config."claim.name"=groups' \
    -s 'config."id.token.claim"=true' -s 'config."access.token.claim"=true' -s 'config."userinfo.token.claim"=true' >/dev/null
  # A public client with PKCE: no secret.
  kcadm create clients -r plinth -s clientId=plinth-ui -s publicClient=true -s standardFlowEnabled=true -s directAccessGrantsEnabled=false \
    -s "redirectUris=[\"$PLINTH_URL/callback\"]" -s "webOrigins=[\"$PLINTH_URL\"]" -s 'attributes."pkce.code.challenge.method"=S256' >/dev/null
  local cid; cid="$(kc_id clients clientId plinth-ui)"
  kcadm update "clients/$cid/default-client-scopes/$sid" -r plinth -b '{}' -n >/dev/null
  kcadm create groups -r plinth -s name=plinth-admins >/dev/null
  local u
  for u in alice ada; do
    kcadm create users -r plinth -s "username=$u" -s "email=$u@example.test" -s emailVerified=true -s enabled=true -s "firstName=$u" -s lastName=Stack >/dev/null
    kcadm set-password -r plinth --username "$u" --new-password "$USER_PASSWORD" >/dev/null
  done
  local uid gid
  uid="$(kc_id users username ada)"
  gid="$(kc_id groups name plinth-admins)"
  kcadm update "users/$uid/groups/$gid" -r plinth -s realm=plinth -s "userId=$uid" -s "groupId=$gid" -n >/dev/null
  kcadm update realms/plinth -s 'displayName=Plinth (ready)' >/dev/null  # marks the setup as complete
}

start_harbor() {
  load
  if [ -n "${HARBOR_DIR:-}" ] && [ -f "$HARBOR_DIR/.project" ]; then
    say "Harbor (starting)"; ( cd "$HARBOR_DIR" && docker compose -p "$(cat .project)" up -d >&2 )
  else
    say "Harbor (first start pulls several GB; slow on Apple Silicon)"
    local out; out="$("$ROOT/scripts/harbor.sh" up)" || die "Harbor did not start"
    save HARBOR_URL "$(echo "$out" | sed -n 's/^HARBOR_URL=//p')"
    save HARBOR_ADMIN_USER "$(echo "$out" | sed -n 's/^HARBOR_ADMIN_USER=//p')"
    save HARBOR_ADMIN_PASSWORD "$(echo "$out" | sed -n 's/^HARBOR_ADMIN_PASSWORD=//p')"
    save HARBOR_DIR "$(echo "$out" | sed -n 's/^HARBOR_DIR=//p')"
  fi
  load; http_wait "$HARBOR_URL/api/v2.0/health" Harbor 600
}
setup_harbor() { # a dedicated Plinth identity, as the end-to-end test does
  load; secret HARBOR_PLINTH_PASSWORD 16; load
  local pw="${HARBOR_PLINTH_PASSWORD}Aa1" h="$HARBOR_URL/api/v2.0" ha="$HARBOR_ADMIN_USER:$HARBOR_ADMIN_PASSWORD"
  save HARBOR_PLINTH_LOGIN "$pw"
  if ! curl -fsS -u "$ha" "$h/users/search?username=plinth" | python3 -c 'import sys,json; sys.exit(0 if json.load(sys.stdin) else 1)'; then
    curl -fsS -u "$ha" -X POST "$h/users" -H 'Content-Type: application/json' \
      -d "{\"username\":\"plinth\",\"email\":\"plinth@example.test\",\"realname\":\"plinth\",\"password\":\"$pw\"}" >/dev/null
  fi
  local hid; hid="$(curl -fsS -u "$ha" "$h/users/search?username=plinth" | python3 -c 'import sys,json;print(json.load(sys.stdin)[0]["user_id"])')"
  curl -fsS -u "$ha" -X PUT "$h/users/$hid/sysadmin" -H 'Content-Type: application/json' -d '{"sysadmin_flag":true}' >/dev/null
}

# ---- Plinth ----------------------------------------------------------------------------------------------
plinth_running() { [ -f "$DIR/plinth.pid" ] && kill -0 "$(cat "$DIR/plinth.pid")" 2>/dev/null; }
stop_plinth() {
  if plinth_running; then kill "$(cat "$DIR/plinth.pid")" 2>/dev/null || true; for _ in $(seq 1 20); do plinth_running || break; sleep 0.5; done; fi
  rm -f "$DIR/plinth.pid"
}
start_plinth() { # keycloak_enabled
  need go
  [ -f web/dist/index.html ] || { say "building the web interface"; make web >/dev/null; }
  say "building Plinth"; mkdir -p "$DIR"; go build -o "$DIR/plinth" ./cmd/plinth
  secret BOOTSTRAP_ADMIN_PASSWORD 20; load
  [ -n "${MASTER_KEY_B64:-}" ] || save MASTER_KEY_B64 "$("$DIR/plinth" genkey)"
  load; stop_plinth
  say "starting Plinth on $PLINTH_URL"
  ( PLINTH_LISTEN_ADDR="127.0.0.1:$STACK_PLINTH_PORT" \
    PLINTH_DATABASE_URL="postgres://postgres:$PG_PASSWORD@127.0.0.1:$STACK_PG_PORT/plinth?sslmode=disable" \
    PLINTH_MASTER_KEY="$MASTER_KEY_B64" \
    PLINTH_BOOTSTRAP_ADMIN_PASSWORD="$BOOTSTRAP_ADMIN_PASSWORD" \
    PLINTH_PUBLIC_URL="$PLINTH_URL" PLINTH_NOTIFY_ALLOW_INSECURE=true \
    PLINTH_SMTP_HOST=127.0.0.1 PLINTH_SMTP_PORT="$STACK_MAIL_SMTP_PORT" PLINTH_SMTP_TLS=none PLINTH_SMTP_FROM="Plinth <plinth@example.test>" \
    PLINTH_OIDC_ALLOW_HTTP="$1" \
    nohup "$DIR/plinth" >> "$DIR/plinth.log" 2>&1 & echo $! > "$DIR/plinth.pid" )
  http_wait "$PLINTH_URL/readyz" Plinth 60
}

# ---- talking to Plinth ---------------------------------------------------------------------------------
TOKEN=""
api() { # method path [json]: prints the status code; the body is in $DIR/last.json
  local m="$1" p="$2" b="${3:-}"
  if [ -n "$b" ]; then
    curl -sS -o "$DIR/last.json" -w '%{http_code}' -X "$m" "$PLINTH_URL$p" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "$b"
  else
    curl -sS -o "$DIR/last.json" -w '%{http_code}' -X "$m" "$PLINTH_URL$p" -H "Authorization: Bearer $TOKEN"
  fi
}
jget() { python3 -c 'import sys,json; d=json.load(open(sys.argv[1]));
for k in sys.argv[2].split("."): d=d[k]
print(d)' "$DIR/last.json" "$1"; }
login() { # user password -> token on stdout; fails when refused
  local code; code="$(curl -sS -o "$DIR/login.json" -w '%{http_code}' -X POST "$PLINTH_URL/api/v1/auth/login" -H 'Content-Type: application/json' \
    -d "$(U="$1" P="$2" python3 -c 'import os,json; print(json.dumps({"username":os.environ["U"],"password":os.environ["P"]}))')")"
  [ "$code" = 200 ] || return 1
  python3 -c 'import json; print(json.load(open("'"$DIR"'/login.json"))["token"])'
}
change_password() { # token current new
  local code; code="$(curl -sS -o "$DIR/last.json" -w '%{http_code}' -X POST "$PLINTH_URL/api/v1/auth/password" -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -d "$(C="$2" N="$3" python3 -c 'import os,json; print(json.dumps({"current_password":os.environ["C"],"new_password":os.environ["N"]}))')")"
  [ "$code" = 204 ] || die "could not change a password: $(cat "$DIR/last.json")"
}
admin_login() {
  secret ADMIN_PASSWORD 20; load
  TOKEN="$(login admin "$ADMIN_PASSWORD" || true)"
  if [ -z "$TOKEN" ]; then # first run: the bootstrap password, to be replaced at once
    local first; first="$(login admin "$BOOTSTRAP_ADMIN_PASSWORD" || true)"
    [ -n "$first" ] || die "cannot sign in as admin: the account exists with another password (run: scripts/stack.sh down, then up)"
    change_password "$first" "$BOOTSTRAP_ADMIN_PASSWORD" "$ADMIN_PASSWORD"
    TOKEN="$(login admin "$ADMIN_PASSWORD")"
  fi
}
has() { # list-path name: is there an item with that name (or username)?
  api GET "$1" >/dev/null
  python3 -c 'import sys,json; d=json.load(open(sys.argv[1])); n=sys.argv[2]
sys.exit(0 if any(x.get("name")==n or x.get("username")==n for x in d) else 1)' "$DIR/last.json" "$2"
}

seed() { # harbor_enabled keycloak_enabled
  load
  say "setting up Plinth: integrations, blueprints, users"
  if ! has /api/v1/integrations stack-git; then
    local body; body="$(U="http://127.0.0.1:$STACK_GITEA_PORT" T="$GITEA_TOKEN" python3 -c 'import os,json; print(json.dumps({"kind":"gitsource","driver":"gitea","name":"stack-git","config":{"base_url":os.environ["U"],"token":os.environ["T"],"allow_http":"true"}}))')"
    [ "$(api POST /api/v1/integrations "$body")" = 201 ] || die "Gitea integration: $(cat "$DIR/last.json")"
  fi
  if ! has /api/v1/integrations stack-cluster; then
    local body; body="$(S="$K8S_SERVER" C="$K8S_CA" T="$K8S_TOKEN" python3 -c 'import os,json; print(json.dumps({"kind":"cluster","driver":"kubernetes","name":"stack-cluster","config":{"server":os.environ["S"],"ca_cert":os.environ["C"],"token":os.environ["T"]}}))')"
    [ "$(api POST /api/v1/integrations "$body")" = 201 ] || die "cluster integration: $(cat "$DIR/last.json")"
  fi
  if ! has /api/v1/integrations stack-db; then
    local body; body="$(P="$STACK_PG_PORT" W="$PG_PASSWORD" python3 -c 'import os,json; print(json.dumps({"kind":"database","driver":"postgres","name":"stack-db","config":{"host":"127.0.0.1","port":os.environ["P"],"admin_user":"postgres","admin_password":os.environ["W"],"sslmode":"disable"}}))')"
    [ "$(api POST /api/v1/integrations "$body")" = 201 ] || die "database integration: $(cat "$DIR/last.json")"
  fi
  local registry=""
  if [ "$1" = 1 ]; then
    registry=stack-registry
    if ! has /api/v1/integrations stack-registry; then
      local body; body="$(U="$HARBOR_URL" P="$HARBOR_PLINTH_LOGIN" python3 -c 'import os,json; print(json.dumps({"kind":"registry","driver":"harbor","name":"stack-registry","config":{"base_url":os.environ["U"],"username":"plinth","password":os.environ["P"],"allow_http":"true"}}))')"
      [ "$(api POST /api/v1/integrations "$body")" = 201 ] || die "Harbor integration: $(cat "$DIR/last.json")"
    fi
  fi
  local name approval
  for name in standard regulated; do
    approval=False; [ "$name" = regulated ] && approval=True
    if ! has /api/v1/blueprints "$name"; then
      local body; body="$(N="$name" A="$approval" R="$registry" python3 -c 'import os,json
bp={"name":os.environ["N"],"git_source":"stack-git","git_parent":"tenants","template_repo":"platform/app-template",
 "template_vars":{"PROJECT_NAME":"{project}"},"extra_branches":["develop"],"requires_approval":os.environ["A"]=="True",
 "environments":[{"name":"dev","cluster":"stack-cluster","database":"stack-db","access_prefix":"oidc:","ingress_namespace":"kube-system"}]}
if os.environ["R"]: bp["registry"]=os.environ["R"]
print(json.dumps(bp))')"
      [ "$(api POST /api/v1/blueprints "$body")" = 201 ] || die "blueprint $name: $(cat "$DIR/last.json")"
    elif [ -n "$registry" ]; then
      # Harbor was switched on after the blueprint was made: give it the registry, leaving everything else as it is.
      api GET "/api/v1/blueprints/$name" >/dev/null
      if [ "$(python3 -c 'import json; print(json.load(open("'"$DIR"'/last.json")).get("registry",""))')" = "" ]; then
        local body; body="$(R="$registry" python3 -c 'import os,json; d=json.load(open("'"$DIR"'/last.json")); d["registry"]=os.environ["R"]; print(json.dumps(d))')"
        [ "$(api PUT "/api/v1/blueprints/$name" "$body")" = 200 ] || die "blueprint $name: $(cat "$DIR/last.json")"
      fi
    fi
  done
  # Two developers with passwords of their own, so teams, approvals and invitations can be tried at once.
  secret USER_PASSWORD 20; load
  local u
  for u in alice bob; do
    if ! has /api/v1/local-users "$u"; then
      local body temp
      body="$(U="$u" python3 -c 'import os,json; print(json.dumps({"username":os.environ["U"],"email":os.environ["U"]+"@local.test","role":"user"}))')"
      [ "$(api POST /api/v1/local-users "$body")" = 201 ] || die "user $u: $(cat "$DIR/last.json")"
      temp="$(jget temporary_password)"
      local t; t="$(login "$u" "$temp")"; change_password "$t" "$temp" "$USER_PASSWORD"
    fi
  done
  if [ "$2" = 1 ]; then
    if ! has /api/v1/oidc-providers keycloak; then
      local body; body="$(I="http://127.0.0.1:$STACK_KEYCLOAK_PORT/realms/plinth" python3 -c 'import os,json; print(json.dumps({"name":"keycloak","display_name":"Keycloak","issuer":os.environ["I"],"client_id":"plinth-ui","admin_group":"plinth-admins"}))')"
      [ "$(api POST /api/v1/oidc-providers "$body")" = 201 ] || die "Keycloak provider: $(cat "$DIR/last.json")"
    fi
  fi
}

# ---- commands --------------------------------------------------------------------------------------------
cmd_up() {
  local harbor=${HARBOR:-0} keycloak=${KEYCLOAK:-0}
  while [ $# -gt 0 ]; do
    case "$1" in
      --harbor) harbor=1 ;; --keycloak) keycloak=1 ;;
      *) die "unknown option $1 (use --harbor and/or --keycloak)" ;;
    esac; shift
  done
  need docker k3d kubectl curl python3 go
  mkdir -p "$DIR"; load
  # Remember what was asked for, so `status` and a later plain `up` still know about Harbor and Keycloak.
  [ "$harbor" = 1 ] && save WANT_HARBOR 1; [ "$keycloak" = 1 ] && save WANT_KEYCLOAK 1; load
  harbor=${WANT_HARBOR:-0}; keycloak=${WANT_KEYCLOAK:-0}

  start_postgres; start_gitea; start_mailpit
  [ "$keycloak" = 1 ] && start_keycloak
  start_cluster
  [ "$harbor" = 1 ] && start_harbor
  say "waiting for PostgreSQL and Gitea"
  wait_postgres; setup_gitea
  setup_cluster
  [ "$harbor" = 1 ] && setup_harbor
  start_plinth "$keycloak"
  [ "$keycloak" = 1 ] && setup_keycloak
  admin_login
  seed "$harbor" "$keycloak"
  say "the stack is ready"
  cmd_status
}

cmd_stop() {
  load; stop_plinth
  say "stopping"
  for c in "$KC" "$MAIL" "$GITEA" "$PG"; do [ "$(container_state "$c")" = running ] && docker stop "$c" >/dev/null || true; done
  k3d cluster stop "$STACK_CLUSTER" >/dev/null 2>&1 || true
  [ -n "${HARBOR_DIR:-}" ] && [ -f "$HARBOR_DIR/.project" ] && ( cd "$HARBOR_DIR" && docker compose -p "$(cat .project)" stop >&2 ) || true
  say "stopped; the data is kept. Start again with: scripts/stack.sh up"
}

cmd_down() {
  load; stop_plinth
  say "deleting the stack and its data"
  docker rm -f "$KC" "$MAIL" "$GITEA" "$PG" >/dev/null 2>&1 || true
  docker volume rm "$PREFIX-pg-data" "$PREFIX-gitea-data" "$PREFIX-keycloak-data" >/dev/null 2>&1 || true
  k3d cluster delete "$STACK_CLUSTER" >/dev/null 2>&1 || true
  [ -n "${HARBOR_DIR:-}" ] && "$ROOT/scripts/harbor.sh" down "$HARBOR_DIR" >/dev/null 2>&1 || true
  rm -rf "$DIR"
  say "gone"
}

row() { printf '  %-10s %s\n' "$1" "$2"; }
state_of() { case "$(container_state "$1")" in running) echo up ;; none) echo "not created" ;; *) echo stopped ;; esac; }
cmd_status() {
  load
  [ -f "$STATE" ] || { echo "The stack has not been created. Run: scripts/stack.sh up"; return 0; }
  local ps=stopped; plinth_running && ps=up
  echo
  echo "Plinth      $PLINTH_URL   ($ps)"
  row "admin" "admin / ${ADMIN_PASSWORD:-?}"
  row "users" "alice, bob / ${USER_PASSWORD:-?}   (password accounts; a team to try: request as alice, invite bob)"
  if [ "${WANT_KEYCLOAK:-0}" = 1 ]; then
    echo "Keycloak    http://127.0.0.1:$STACK_KEYCLOAK_PORT   ($(state_of "$KC"))"
    row "sign in" "'Continue with Keycloak': alice (user) and ada (administrator) / ${USER_PASSWORD:-?}"
    row "console" "http://127.0.0.1:$STACK_KEYCLOAK_PORT   admin / ${KC_ADMIN_PASSWORD:-?}"
  fi
  echo "Gitea       http://127.0.0.1:$STACK_GITEA_PORT   ($(state_of "$GITEA"))"
  row "admin" "plinth-admin / ${GITEA_ADMIN_PASSWORD:-?}   (new repositories appear under the organisation of each team)"
  if [ "${WANT_HARBOR:-0}" = 1 ]; then
    echo "Harbor      ${HARBOR_URL:-?}"
    row "admin" "${HARBOR_ADMIN_USER:-admin} / ${HARBOR_ADMIN_PASSWORD:-?}"
  fi
  echo "Mailpit     http://127.0.0.1:$STACK_MAIL_UI_PORT   ($(state_of "$MAIL"))   notifications land here"
  echo "PostgreSQL  127.0.0.1:$STACK_PG_PORT   ($(state_of "$PG"))"
  row "login" "postgres / ${PG_PASSWORD:-?}   (databases: plinth = Plinth's state; one per project is created here)"
  echo "Kubernetes  KUBECONFIG=$DIR/kubeconfig   (k3d cluster $STACK_CLUSTER)"
  row "try" "kubectl --kubeconfig $DIR/kubeconfig get ns"
  echo
  echo "Blueprints 'standard' (runs at once) and 'regulated' (waits for an administrator other than the requester) are ready."
  echo "Stop with: scripts/stack.sh stop     Delete everything with: scripts/stack.sh down"
}

cmd_logs() {
  case "${1:-plinth}" in
    plinth) tail -n 100 -f "$DIR/plinth.log" ;;
    gitea) docker logs -f --tail 100 "$GITEA" ;;
    postgres) docker logs -f --tail 100 "$PG" ;;
    mailpit) docker logs -f --tail 100 "$MAIL" ;;
    keycloak) docker logs -f --tail 100 "$KC" ;;
    *) die "logs: plinth | gitea | postgres | mailpit | keycloak" ;;
  esac
}

case "${1:-}" in
  up) shift; cmd_up "$@" ;;
  stop) cmd_stop ;;
  down) cmd_down ;;
  status) cmd_status ;;
  logs) shift; cmd_logs "$@" ;;
  *) sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
