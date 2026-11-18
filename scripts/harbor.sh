#!/usr/bin/env bash
# Disposable Harbor for integration tests.
#
#   scripts/harbor.sh up     start Harbor, print HARBOR_URL / HARBOR_ADMIN_USER / HARBOR_ADMIN_PASSWORD / HARBOR_DIR
#   scripts/harbor.sh down DIR   stop it and delete everything under DIR
#
# Needs Docker. On Apple Silicon the (amd64-only) images run under emulation,
# which is slow: allow a few minutes to start.
set -euo pipefail

VERSION="${HARBOR_VERSION:-v2.15.5}"
ADMIN_PASSWORD="Harbor12345-itest"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Harbor images are published for amd64 only.
if [ "$(uname -m)" = "arm64" ] || [ "$(uname -m)" = "aarch64" ]; then
  export DOCKER_DEFAULT_PLATFORM=linux/amd64
fi

free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }

up() {
  # Under the repo (gitignored) because Docker Desktop style VMs share the home directory, not /tmp.
  mkdir -p "$ROOT/.itest"
  local dir; dir="$(mktemp -d "$ROOT/.itest/harbor.XXXXXX")"
  local port; port="$(free_port)"
  local name; name="plinth-itest-harbor-$(basename "$dir" | tr 'A-Z.' 'a-z-')"
  mkdir -p "$dir/data" "$dir/log" "$dir/common/config"

  cat > "$dir/harbor.yml" <<EOF
hostname: localhost
http:
  port: $port
harbor_admin_password: $ADMIN_PASSWORD
database:
  password: itest-db-password
  max_idle_conns: 50
  max_open_conns: 100
data_volume: $dir/data
jobservice:
  max_job_workers: 4
  job_loggers:
    - STD_OUTPUT
  logger_sweeper_duration: 1
notification:
  webhook_job_max_retry: 3
  webhook_job_http_client_timeout: 3
log:
  level: info
  local:
    rotate_count: 5
    rotate_size: 20M
    location: $dir/log
_version: 2.15.0
upload_purging:
  enabled: true
  age: 168h
  interval: 24h
  dryrun: false
cache:
  enabled: false
  expire_hours: 24
EOF

  echo "==> generating Harbor config ($VERSION)" >&2
  docker run --rm --privileged \
    -v /:/hostfs -v "$dir":/input -v "$dir/data":/data \
    -v "$dir":/compose_location -v "$dir/common/config":/config \
    -v "$dir/log":/var/log/harbor \
    "goharbor/prepare:$VERSION" prepare >&2

  # Harbor's cache (Valkey/Redis) segfaults under QEMU emulation. On arm hosts
  # swap only that one service for a native multi-arch image; it is a plain
  # cache, so Harbor's API behaviour under test is unchanged. Compose merges
  # docker-compose.override.yml automatically.
  if [ -n "${DOCKER_DEFAULT_PLATFORM:-}" ]; then
    cat > "$dir/docker-compose.override.yml" <<EOF
services:
  redis:
    image: valkey/valkey:8-alpine
    platform: linux/arm64
    user: root
    command: ["valkey-server", "--save", "", "--appendonly", "no"]
EOF
  fi

  echo "==> starting Harbor on port $port (first start pulls images)" >&2
  echo "$name" > "$dir/.project"
  ( cd "$dir" && docker compose -p "$name" up -d >&2 )

  echo "==> waiting for Harbor to become healthy" >&2
  local i
  for i in $(seq 1 120); do
    if curl -fsS "http://127.0.0.1:$port/api/v2.0/health" 2>/dev/null \
       | python3 -c 'import sys,json; d=json.load(sys.stdin); sys.exit(0 if all(c["status"]=="healthy" for c in d["components"]) else 1)' 2>/dev/null; then
      echo "HARBOR_URL=http://127.0.0.1:$port"
      echo "HARBOR_ADMIN_USER=admin"
      echo "HARBOR_ADMIN_PASSWORD=$ADMIN_PASSWORD"
      echo "HARBOR_DIR=$dir"
      return 0
    fi
    sleep 5
  done
  echo "Harbor did not become healthy in 10 minutes" >&2
  ( cd "$dir" && docker compose -p "$name" logs --tail=30 >&2 ) || true
  down "$dir"
  return 1
}

down() {
  local dir="${1:?usage: harbor.sh down DIR}"
  case "$dir" in "$ROOT"/.itest/harbor.*) ;; *) echo "refusing to delete $dir" >&2; exit 1 ;; esac
  if [ -f "$dir/.project" ]; then
    ( cd "$dir" && docker compose -p "$(cat .project)" down -v --remove-orphans >&2 ) || true
  fi
  # Files created by containers can be owned by another uid; remove them from inside Docker.
  docker run --rm -v "$dir":/w alpine sh -c 'rm -rf /w/* /w/.[!.]* 2>/dev/null' >&2 || true
  rmdir "$dir" 2>/dev/null || rm -rf "$dir"
}

case "${1:-}" in
  up) up ;;
  down) down "${2:-}" ;;
  *) echo "usage: $0 up | down DIR" >&2; exit 2 ;;
esac
