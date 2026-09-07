#!/usr/bin/env bash

# Disposable release-gate dependencies. The foreground gate owns their exact
# Docker IDs until every test child has joined. No shared local service is used.

release_gate_service_docker() {
  local budget=120 remaining
  if (( ${release_gate_service_deadline:-0} > 0 )); then
    remaining=$((release_gate_service_deadline - SECONDS))
    (( remaining > 0 )) || return 124
    if (( remaining < budget )); then budget="$remaining"; fi
  fi
  timeout --foreground "$budget" "${release_gate_docker_command[@]}" "$@"
}

release_gate_service_id() {
  [[ "$1" =~ ^[0-9a-f]{64}$ ]]
}

# Lookup by name is used only to recover an interrupted create acknowledgement;
# every mutation still uses a full immutable ID after checking owner labels.
release_gate_service_find() {
  local service="$1" id recorded name
  name="urnetwork-gate-${release_gate_service_owner}-${service}"
  [[ "$(stat -c '%d:%i:%a:%u' "$release_gate_service_root")" == "$release_gate_service_root_identity" ]] || return 1
  if [[ -s "$release_gate_service_root/$service.cid" ]]; then
    [[ -f "$release_gate_service_root/$service.cid" && ! -L "$release_gate_service_root/$service.cid" ]] || return 1
    id="$(< "$release_gate_service_root/$service.cid")"
  else
    id="$(release_gate_service_docker container ls --all --no-trunc --quiet --filter "name=^/${name}$")" || return 1
    [[ -n "$id" ]] || { RELEASE_GATE_SERVICE_ID=""; return 0; }
  fi
  release_gate_service_id "$id" || return 1
  recorded="$(release_gate_service_docker inspect --type container --format \
    '{{.Id}} {{.Name}} {{index .Config.Labels "urnetwork.release-gate.owner"}} {{index .Config.Labels "urnetwork.release-gate.service"}} {{.HostConfig.RestartPolicy.Name}}' "$id")" || {
    recorded="$(release_gate_service_docker container ls --all --no-trunc --quiet --filter "id=$id")" || return 1
    [[ -z "$recorded" ]] || return 1
    RELEASE_GATE_SERVICE_ID=""
    return 0
  }
  if [[ "$recorded" != "$id /$name $release_gate_service_owner $service no" ]]; then
    printf 'release gate: refusing changed %s container ownership\n' "$service" >&2
    return 1
  fi
  RELEASE_GATE_SERVICE_ID="$id"
}

# Continue through both resources after a failure. Ambiguous ownership remains
# untouched, and the caller cannot report successful qualification or cleanup.
release_gate_services_cleanup() {
  [[ -n "${release_gate_service_root:-}" ]] || return 0
  local service id remaining result=0 owner
  local release_gate_service_deadline=$((SECONDS + 120))
  [[ -f "$release_gate_service_root/owner" && ! -L "$release_gate_service_root/owner" ]] || return 1
  owner="$(< "$release_gate_service_root/owner")" || return 1
  [[ "$owner" == "$release_gate_service_owner" ]] || return 1
  for service in redis postgres; do
    if ! release_gate_service_find "$service"; then result=1; continue; fi
    id="$RELEASE_GATE_SERVICE_ID"
    [[ -n "$id" ]] || continue
    if ! release_gate_service_docker rm --force --volumes "$id" >/dev/null; then result=1; continue; fi
    remaining="$(release_gate_service_docker container ls --all --no-trunc --quiet --filter "id=$id")" || { result=1; continue; }
    [[ -z "$remaining" ]] || result=1
  done
  return "$result"
}

# Docker atomically chooses each host port while creating its binding. There
# is no free-port probe followed by a separate bind, nor a hostname fallback.
release_gate_service_endpoint() {
  local service="$1" port="$2" id="$3" binding
  release_gate_service_find "$service" || return 1
  [[ "$RELEASE_GATE_SERVICE_ID" == "$id" ]] || return 1
  binding="$(release_gate_service_docker inspect --type container --format \
    "{{range (index .NetworkSettings.Ports \"$port/tcp\")}}{{.HostIp}}:{{.HostPort}}{{end}}" "$id")" || return 1
  [[ "$binding" =~ ^127\.0\.0\.1:([1-9][0-9]{0,4})$ ]] || return 1
  (( 10#${BASH_REMATCH[1]} <= 65535 )) || return 1
  RELEASE_GATE_SERVICE_ENDPOINT="$binding"
}

# Preserve read-only references to frozen local resources. Service credentials,
# maintenance routing, and startup settings are direct private resources so
# inherited host/site settings cannot replace the daemon-assigned endpoints.
release_gate_service_resources() (
  umask 077
  local workspace="$1" root="$release_gate_service_root/resources" kind entry name
  mkdir -m 700 "$root" "$root/vault" "$root/config" "$root/site" || return 1
  for kind in vault config; do
    for entry in "$workspace/$kind"/*; do
      [[ -e "$entry" ]] || continue
      name="${entry##*/}"
      case "$kind/$name" in
        vault/pg.yml | vault/pg_maintenance.yml | vault/redis.yml | config/settings.yml | config/db.yml | config/db_maintenance.yml | config/redis.yml) continue ;;
      esac
      ln -s -- "$entry" "$root/$kind/$name" || return 1
    done
  done
  printf 'authority: "%s"\nuser: "bringyour"\npassword: "urnetwork-local-test"\ndb: "bringyour"\n' \
    "$release_gate_postgres_endpoint" > "$root/vault/pg.yml" || return 1
  cp -- "$root/vault/pg.yml" "$root/vault/pg_maintenance.yml" || return 1
  printf 'authority: "%s"\npassword: ""\ndb: 0\ncluster: false\n' \
    "$release_gate_redis_endpoint" > "$root/vault/redis.yml" || return 1
  cp -- "$workspace/server/local/testdata/config/local/db.yml" "$root/config/db.yml" || return 1
  cp -- "$root/config/db.yml" "$root/config/db_maintenance.yml" || return 1
  cp -- "$workspace/server/local/testdata/config/local/redis.yml" "$root/config/redis.yml" || return 1
  printf 'all: {}\n' > "$root/config/settings.yml" || return 1
  {
    printf 'export WARP_TEST_ENV_USE_PORTABLE_RESOURCES=1\nexport WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES=1\n'
    printf 'export WARP_TEST_ENV_PORTABLE_ROOT=%q\n' "$root"
    printf 'export WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY=%q\n' "$release_gate_postgres_endpoint"
    printf 'export WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY=%q\n' "$release_gate_redis_endpoint"
    printf 'export BRINGYOUR_POSTGRES_HOSTNAME=127.0.0.1\nexport BRINGYOUR_REDIS_HOSTNAME=127.0.0.1\n'
    printf 'export WARP_SITE_HOME=%q\n' "$root/site"
    printf 'unset WARP_TEST_ENV_SUITE_PROXY_STATE_DIR WARP_TEST_ENV_TCP_PROBE WARP_TEST_ENV_TEST_HOSTS_FILE WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR\n'
  } > "$release_gate_service_root/environment.sh" || return 1
)

# Settings match server/local and the simulator's dependency specs; the
# release lock supplies exact images. Data is tmpfs and restart is disabled.
release_gate_services_start() {
  local gate_root="$1" workspace="$2" lock="$3" service image port id output expected attempt ready
  [[ -z "${release_gate_service_root:-}" ]] || return 1
  [[ -z "${APEX_CONTAINER_EVALUATION:-}" ]] || { echo 'release gate refuses the incompatible APEX credential override' >&2; return 1; }
  release_gate_docker_command=(docker)
  release_gate_service_deadline=$((SECONDS + 180))
  release_gate_service_root="$gate_root/services"
  mkdir -m 700 "$release_gate_service_root" || return 1
  release_gate_service_root_identity="$(stat -c '%d:%i:%a:%u' "$release_gate_service_root")" || return 1
  release_gate_service_owner="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
  [[ "$release_gate_service_owner" =~ ^[0-9a-f]{32}$ ]] || return 1
  (umask 077; printf '%s\n' "$release_gate_service_owner" > "$release_gate_service_root/owner") || return 1
  if ! release_gate_service_docker info >/dev/null 2>&1; then
    release_gate_docker_command=(sudo -n docker)
    release_gate_service_docker info >/dev/null 2>&1 || return 1
  fi
  for service in postgres redis; do
    image="$(sed -n "s/^    $service: //p" "$lock")"
    case "$service" in
      postgres) [[ "$image" =~ ^postgres:18@sha256:[0-9a-f]{64}$ ]] || return 1; port=5432 ;;
      redis) [[ "$image" =~ ^redis:8-alpine@sha256:[0-9a-f]{64}$ ]] || return 1; port=6379 ;;
    esac
    local -a arguments=(create --name "urnetwork-gate-${release_gate_service_owner}-${service}"
      --restart=no
      --label "urnetwork.release-gate.owner=$release_gate_service_owner"
      --label "urnetwork.release-gate.service=$service" --publish "127.0.0.1::$port")
    if [[ "$service" == postgres ]]; then
      arguments+=(--tmpfs /var/lib/postgresql:rw
        --mount "type=bind,src=$workspace/server/local/postgres/initdb,dst=/docker-entrypoint-initdb.d,readonly"
        -e LANG=en_US.UTF-8 -e POSTGRES_INITDB_ARGS=--locale=en_US.UTF-8
        -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=urnetwork-local-test -e POSTGRES_DB=postgres
        -e APP_DB_USER=bringyour -e APP_DB_PASSWORD=urnetwork-local-test -e APP_DB_NAME=bringyour
        "$image" postgres -c max_connections=512 -c shared_buffers=256MB)
    else
      arguments+=(--tmpfs /data:rw --ulimit nofile=65536:65536
        --sysctl net.core.somaxconn=65535 --sysctl net.ipv4.tcp_max_syn_backlog=65535
        "$image" redis-server --io-threads 8 --io-threads-do-reads yes
        --maxclients 32768 --tcp-backlog 65535 --save '' --appendonly no)
    fi
    output="$(release_gate_service_docker "${arguments[@]}")" || return 1
    release_gate_service_find "$service" || return 1
    id="$RELEASE_GATE_SERVICE_ID"
    [[ "$output" == "$id" ]] && release_gate_service_id "$id" || return 1
    # Docker may run through sudo. Record its verified ID as the gate user;
    # never ask a privileged client to create an unreadable root-owned cidfile.
    (umask 077; printf '%s\n' "$id" > "$release_gate_service_root/$service.cid") || return 1
    release_gate_service_docker start "$id" >/dev/null || return 1
    ready=0
    for ((attempt=0; attempt<90; attempt++)); do
      (( SECONDS < release_gate_service_deadline )) || return 124
      release_gate_service_find "$service" || return 1
      [[ "$RELEASE_GATE_SERVICE_ID" == "$id" ]] || return 1
      if [[ "$service" == postgres ]]; then
        expected='512:256MB:en_US.UTF-8:t'
        output="$(release_gate_service_docker exec "$id" env PGPASSWORD=urnetwork-local-test PGCONNECT_TIMEOUT=3 \
          psql -h 127.0.0.1 -U bringyour -d bringyour -Atqc \
          "SELECT current_setting('max_connections') || ':' || current_setting('shared_buffers') || ':' || datcollate || ':' || CASE WHEN rolcreatedb THEN 't' ELSE 'f' END FROM pg_database, pg_roles WHERE datname=current_database() AND rolname=current_user" 2>/dev/null)" || output=''
      else
        expected=PONG
        output="$(release_gate_service_docker exec "$id" redis-cli ping 2>/dev/null)" || output=''
      fi
      if [[ "$output" == "$expected" ]]; then ready=1; break; fi
      sleep 1
    done
    [[ "$ready" == 1 ]] || return 1
    release_gate_service_endpoint "$service" "$port" "$id" || return 1
    if [[ "$service" == postgres ]]; then release_gate_postgres_endpoint="$RELEASE_GATE_SERVICE_ENDPOINT"
    else release_gate_redis_endpoint="$RELEASE_GATE_SERVICE_ENDPOINT"; fi
  done
  [[ "$release_gate_postgres_endpoint" != "$release_gate_redis_endpoint" ]] || return 1
  release_gate_service_resources "$workspace" || return 1
  release_gate_service_deadline=0
}
