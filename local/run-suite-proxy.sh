#!/usr/bin/env bash

# Owns two short-lived, direct host-IP proxies to the already-running local
# Compose stores. This is for repository test suites that cannot use the stuck
# interactive launcher's loopback aliases; it never changes that launcher,
# /etc/hosts, host addresses, the Docker network, or the upstream containers.

set -euo pipefail

suite_proxy_die() {
  printf 'run-suite-proxy.sh: %s\n' "$*" >&2
  exit 1
}

suite_proxy_log() {
  printf '[suite-proxy] %s\n' "$*"
}

suite_proxy_random_hex() {
  local value
  value="$(LC_ALL=C od -An -N 24 -tx1 /dev/urandom 2>/dev/null | tr -d ' \n')" || return $?
  if [[ ! "$value" =~ ^[0-9a-f]{48}$ ]]; then
    printf 'run-suite-proxy.sh: could not generate an owner challenge\n' >&2
    return 1
  fi
  SUITE_PROXY_RANDOM_HEX="$value"
}

# One wrapper makes ownership/cleanup behavior deterministic under unit-test
# Docker doubles without giving the production path an alternate contract.
suite_proxy_docker() {
  docker "$@"
}

suite_proxy_inspect_value() {
  local container="$1"
  local template="$2"
  suite_proxy_docker inspect --type container --format "$template" "$container" 2>/dev/null
}

# An inspect transport error is not evidence that a container is gone. Only a
# successful daemon list, filtered by the full immutable id, can prove absence.
suite_proxy_container_absence_proven() {
  local container_id="$1"
  local line
  local listed_id
  local listing
  local count=0

  suite_proxy_container_id_validate "container absence id" "$container_id" || return $?
  listing="$(suite_proxy_docker container ls --all --no-trunc --quiet \
    --filter "id=$container_id")" || return $?
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -n "$line" ]] || continue
    count=$((count + 1))
    listed_id="$line"
  done <<< "$listing"
  if [[ "$count" == 0 ]]; then
    return 0
  fi
  if [[ "$count" != 1 ]]; then
    printf 'run-suite-proxy.sh: ambiguous Docker id lookup for %s\n' "$container_id" >&2
    return 1
  fi
  suite_proxy_container_id_validate "listed container id" "$listed_id" || return $?
  return 1
}

# The Compose network identity is checked independently of the upstreams so a
# same-named replacement cannot become the proxy attachment target.
suite_proxy_verify_network() {
  local expected_id="${1:-}"
  local network_id
  local network_name
  local project
  local compose_network

  network_id="$(suite_proxy_docker network inspect --format '{{.Id}}' urnetwork-local 2>/dev/null)" || {
    printf 'run-suite-proxy.sh: repository Compose network is missing: urnetwork-local\n' >&2
    return 1
  }
  suite_proxy_container_id_validate "network id" "$network_id" || return $?
  if [[ -n "$expected_id" && "$network_id" != "$expected_id" ]]; then
    printf 'run-suite-proxy.sh: repository Compose network identity changed\n' >&2
    return 1
  fi
  network_name="$(suite_proxy_docker network inspect --format '{{.Name}}' "$network_id" 2>/dev/null)" || return 1
  project="$(suite_proxy_docker network inspect \
    --format '{{index .Labels "com.docker.compose.project"}}' \
    "$network_id" 2>/dev/null)" || return 1
  compose_network="$(suite_proxy_docker network inspect \
    --format '{{index .Labels "com.docker.compose.network"}}' \
    "$network_id" 2>/dev/null)" || return 1
  if [[ "$network_name" != urnetwork-local ||
        "$project" != urnetwork-local ||
        "$compose_network" != urnetwork-local ]]; then
    printf 'run-suite-proxy.sh: urnetwork-local is not the repository Compose network\n' >&2
    return 1
  fi
  SUITE_PROXY_INSPECTED_NETWORK_ID="$network_id"
}

# Verifies the immutable upstream identity and the Compose labels/network that
# distinguish the repository's local services from a same-named foreign object.
suite_proxy_verify_upstream() {
  local container_name="$1"
  local compose_service="$2"
  local expected_id="${3:-}"
  local container_id
  local inspected_name
  local running
  local health
  local project
  local service
  local attached
  local attached_network_id

  container_id="$(suite_proxy_inspect_value "$container_name" '{{.Id}}')" || {
    printf 'run-suite-proxy.sh: upstream container is missing: %s\n' "$container_name" >&2
    return 1
  }
  suite_proxy_container_id_validate "upstream container id" "$container_id" || return $?
  if [[ -n "$expected_id" && "$container_id" != "$expected_id" ]]; then
    printf 'run-suite-proxy.sh: upstream container identity changed: %s\n' "$container_name" >&2
    return 1
  fi
  inspected_name="$(suite_proxy_inspect_value "$container_id" '{{.Name}}')" || return 1
  running="$(suite_proxy_inspect_value "$container_id" '{{.State.Running}}')" || return 1
  health="$(suite_proxy_inspect_value "$container_id" \
    '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}')" || return 1
  project="$(suite_proxy_inspect_value "$container_id" \
    '{{index .Config.Labels "com.docker.compose.project"}}')" || return 1
  service="$(suite_proxy_inspect_value "$container_id" \
    '{{index .Config.Labels "com.docker.compose.service"}}')" || return 1
  attached="$(suite_proxy_inspect_value "$container_id" \
    '{{if index .NetworkSettings.Networks "urnetwork-local"}}yes{{else}}no{{end}}')" || return 1
  attached_network_id="$(suite_proxy_inspect_value "$container_id" \
    '{{with index .NetworkSettings.Networks "urnetwork-local"}}{{.NetworkID}}{{end}}')" || return 1
  if [[ "$inspected_name" != "/$container_name" ||
        "$running" != true ||
        "$health" != healthy ||
        "$project" != urnetwork-local ||
        "$service" != "$compose_service" ||
        "$attached" != yes ||
        "$attached_network_id" != "$SUITE_PROXY_NETWORK_ID" ]]; then
    printf 'run-suite-proxy.sh: upstream %s is not the healthy urnetwork-local/%s service\n' \
      "$container_name" "$compose_service" >&2
    return 1
  fi
  SUITE_PROXY_INSPECTED_UPSTREAM_ID="$container_id"
}

# Checks the labels and immutable id that authorize cleanup. Runtime health and
# port binding are intentionally separate: an exited owned proxy still needs
# to be removable, while a relabeled/replaced object must be retained.
suite_proxy_proxy_ownership_matches() {
  local proxy_id="$1"
  local proxy_name="$2"
  local service="$3"
  local upstream_id="$4"
  local inspected_name
  local owner_token
  local owner_pid
  local owner_start_identity
  local generation
  local inspected_service
  local inspected_upstream_id
  local inspected_network_id
  local attached_network_id
  local inspected_image_id
  local container_image_id
  local marker

  if [[ ( "$service" == postgres && "$proxy_name" != urnetwork-suite-proxy-pg ) ||
        ( "$service" == redis && "$proxy_name" != urnetwork-suite-proxy-redis ) ||
        ( "$service" != postgres && "$service" != redis ) ]]; then
    printf 'run-suite-proxy.sh: refusing non-canonical proxy identity: %s/%s\n' \
      "$service" "$proxy_name" >&2
    return 1
  fi

  if ! inspected_name="$(suite_proxy_inspect_value "$proxy_id" '{{.Name}}')"; then
    if suite_proxy_container_absence_proven "$proxy_id"; then
      return 2
    fi
    printf 'run-suite-proxy.sh: proxy absence could not be proven: %s\n' "$proxy_id" >&2
    return 1
  fi
  marker="$(suite_proxy_inspect_value "$proxy_id" \
    '{{index .Config.Labels "com.urnetwork.server.suite-proxy"}}')" || return 1
  owner_token="$(suite_proxy_inspect_value "$proxy_id" \
    '{{index .Config.Labels "com.urnetwork.server.suite-proxy.owner-token"}}')" || return 1
  owner_pid="$(suite_proxy_inspect_value "$proxy_id" \
    '{{index .Config.Labels "com.urnetwork.server.suite-proxy.owner-pid"}}')" || return 1
  owner_start_identity="$(suite_proxy_inspect_value "$proxy_id" \
    '{{index .Config.Labels "com.urnetwork.server.suite-proxy.owner-start-identity"}}')" || return 1
  generation="$(suite_proxy_inspect_value "$proxy_id" \
    '{{index .Config.Labels "com.urnetwork.server.suite-proxy.generation"}}')" || return 1
  inspected_service="$(suite_proxy_inspect_value "$proxy_id" \
    '{{index .Config.Labels "com.urnetwork.server.suite-proxy.service"}}')" || return 1
  inspected_upstream_id="$(suite_proxy_inspect_value "$proxy_id" \
    '{{index .Config.Labels "com.urnetwork.server.suite-proxy.upstream-id"}}')" || return 1
  inspected_network_id="$(suite_proxy_inspect_value "$proxy_id" \
    '{{index .Config.Labels "com.urnetwork.server.suite-proxy.network-id"}}')" || return 1
  attached_network_id="$(suite_proxy_inspect_value "$proxy_id" \
    '{{with index .NetworkSettings.Networks "urnetwork-local"}}{{.NetworkID}}{{end}}')" || return 1
  inspected_image_id="$(suite_proxy_inspect_value "$proxy_id" \
    '{{index .Config.Labels "com.urnetwork.server.suite-proxy.image-id"}}')" || return 1
  container_image_id="$(suite_proxy_inspect_value "$proxy_id" '{{.Image}}')" || return 1
  if [[ "$inspected_name" != "/$proxy_name" ||
        "$marker" != v1 ||
        "$owner_token" != "$SUITE_PROXY_OWNER_TOKEN" ||
        "$owner_pid" != "$SUITE_PROXY_OWNER_PID" ||
        "$owner_start_identity" != "$SUITE_PROXY_OWNER_START_IDENTITY" ||
        "$generation" != "$SUITE_PROXY_GENERATION" ||
        "$inspected_service" != "$service" ||
        "$inspected_upstream_id" != "$upstream_id" ||
        "$inspected_network_id" != "$SUITE_PROXY_NETWORK_ID" ||
        "$attached_network_id" != "$SUITE_PROXY_NETWORK_ID" ||
        "$inspected_image_id" != "$SUITE_PROXY_IMAGE_ID" ||
        "$container_image_id" != "$SUITE_PROXY_IMAGE_ID" ]]; then
    printf 'run-suite-proxy.sh: refusing non-owned proxy container %s (%s)\n' \
      "$proxy_name" "$proxy_id" >&2
    return 1
  fi
}

# Adds runtime, network, image, and exact host-binding checks to the immutable
# ownership proof used during continuous monitoring.
suite_proxy_verify_proxy() {
  local proxy_id="$1"
  local proxy_name="$2"
  local service="$3"
  local upstream_id="$4"
  local host_port="$5"
  local container_port="$6"
  local running
  local attached
  local attached_network_id
  local configured_image
  local binding
  local binding_template

  suite_proxy_proxy_ownership_matches "$proxy_id" "$proxy_name" "$service" "$upstream_id" || return $?
  running="$(suite_proxy_inspect_value "$proxy_id" '{{.State.Running}}')" || return 1
  attached="$(suite_proxy_inspect_value "$proxy_id" \
    '{{if index .NetworkSettings.Networks "urnetwork-local"}}yes{{else}}no{{end}}')" || return 1
  attached_network_id="$(suite_proxy_inspect_value "$proxy_id" \
    '{{with index .NetworkSettings.Networks "urnetwork-local"}}{{.NetworkID}}{{end}}')" || return 1
  configured_image="$(suite_proxy_inspect_value "$proxy_id" '{{.Config.Image}}')" || return 1
  binding_template="{{with index .NetworkSettings.Ports \"${container_port}/tcp\"}}{{if eq (len .) 1}}{{(index . 0).HostIp}}:{{(index . 0).HostPort}}{{end}}{{end}}"
  binding="$(suite_proxy_inspect_value "$proxy_id" "$binding_template")" || return 1
  if [[ "$running" != true ||
        "$attached" != yes ||
        "$attached_network_id" != "$SUITE_PROXY_NETWORK_ID" ||
        "$configured_image" != "$SUITE_PROXY_IMAGE_ID" ||
        "$binding" != "$SUITE_PROXY_HOST_IP:$host_port" ]]; then
    printf 'run-suite-proxy.sh: proxy %s no longer has its expected runtime or binding\n' \
      "$proxy_name" >&2
    return 1
  fi
}

# Bash 3.2 cannot return an array from the cidfile helper, so the complete
# implementation keeps the one parsed id in a shared scalar.
suite_proxy_load_cidfile() {
  local cidfile="$1"
  local line
  local count=0
  local container_id=""
  if [[ ! -f "$cidfile" || -L "$cidfile" ]]; then
    return 1
  fi
  while IFS= read -r line || [[ -n "$line" ]]; do
    count=$((count + 1))
    container_id="$line"
  done < "$cidfile"
  if [[ "$count" != 1 ]]; then
    printf 'run-suite-proxy.sh: malformed Docker cidfile: %s\n' "$cidfile" >&2
    return 1
  fi
  suite_proxy_container_id_validate "proxy cidfile id" "$container_id" || return $?
  SUITE_PROXY_CIDFILE_ID="$container_id"
}

# Cidfiles live in the private state directory but still cross a Docker/shell
# ownership boundary. Claim and re-parse one before unlinking it.
suite_proxy_remove_owned_cidfile() {
  local cidfile="$1"
  local expected_id="$2"
  local tombstone="${cidfile}.removing.${SUITE_PROXY_OWNER_TOKEN}.${SUITE_PROXY_GENERATION}"

  if [[ ! -e "$cidfile" && ! -L "$cidfile" ]]; then
    return 0
  fi
  suite_proxy_load_cidfile "$cidfile" || return $?
  if [[ "$SUITE_PROXY_CIDFILE_ID" != "$expected_id" ]]; then
    printf 'run-suite-proxy.sh: refusing a cidfile with changed identity: %s\n' "$cidfile" >&2
    return 1
  fi
  suite_proxy_state_require_current_owner \
    "$SUITE_PROXY_STATE_DIR" "$SUITE_PROXY_OWNER_PID" \
    "$SUITE_PROXY_OWNER_START_IDENTITY" "$SUITE_PROXY_OWNER_TOKEN" \
    "$SUITE_PROXY_GENERATION" || return $?
  suite_proxy_move_no_replace "$cidfile" "$tombstone" || return $?
  suite_proxy_load_cidfile "$tombstone" || return $?
  if [[ "$SUITE_PROXY_CIDFILE_ID" != "$expected_id" ]]; then
    printf 'run-suite-proxy.sh: cidfile changed while being removed: %s\n' "$cidfile" >&2
    return 1
  fi
  rm -- "$tombstone"
}

# Creates one fixed-name proxy. Existing names are never treated as stale or
# removed; the operator must resolve their ownership explicitly.
suite_proxy_start_proxy() {
  local proxy_name="$1"
  local service="$2"
  local upstream_name="$3"
  local upstream_id="$4"
  local upstream_port="$5"
  local host_port="$6"
  local cidfile="$7"
  local container_id
  local proxy_command
  local run_status=0

  if suite_proxy_inspect_value "$proxy_name" '{{.Id}}' >/dev/null 2>&1; then
    printf 'run-suite-proxy.sh: proxy name is already owned: %s\n' "$proxy_name" >&2
    return 1
  fi
  if [[ -e "$cidfile" || -L "$cidfile" ]]; then
    printf 'run-suite-proxy.sh: refusing an existing Docker cidfile: %s\n' "$cidfile" >&2
    return 1
  fi
  proxy_command="apk add --no-cache socat >/dev/null && exec socat "
  proxy_command="${proxy_command}TCP-LISTEN:${upstream_port},fork,reuseaddr "
  proxy_command="${proxy_command}TCP:${upstream_name}:${upstream_port}"
  suite_proxy_docker run --detach \
    --cidfile "$cidfile" \
    --name "$proxy_name" \
    --label com.urnetwork.server.suite-proxy=v1 \
    --label "com.urnetwork.server.suite-proxy.owner-token=$SUITE_PROXY_OWNER_TOKEN" \
    --label "com.urnetwork.server.suite-proxy.owner-pid=$SUITE_PROXY_OWNER_PID" \
    --label "com.urnetwork.server.suite-proxy.owner-start-identity=$SUITE_PROXY_OWNER_START_IDENTITY" \
    --label "com.urnetwork.server.suite-proxy.generation=$SUITE_PROXY_GENERATION" \
    --label "com.urnetwork.server.suite-proxy.service=$service" \
    --label "com.urnetwork.server.suite-proxy.upstream-id=$upstream_id" \
    --label "com.urnetwork.server.suite-proxy.network-id=$SUITE_PROXY_NETWORK_ID" \
    --label "com.urnetwork.server.suite-proxy.image-id=$SUITE_PROXY_IMAGE_ID" \
    --network urnetwork-local \
    --publish "$SUITE_PROXY_HOST_IP:$host_port:$upstream_port" \
    "$SUITE_PROXY_IMAGE_ID" \
    sh -eu -c "$proxy_command" \
    >/dev/null || run_status=$?
  if ! suite_proxy_load_cidfile "$cidfile"; then
    if [[ "$run_status" != 0 ]]; then
      return "$run_status"
    fi
    return 1
  fi
  container_id="$SUITE_PROXY_CIDFILE_ID"
  if ! suite_proxy_proxy_ownership_matches \
      "$container_id" "$proxy_name" "$service" "$upstream_id"; then
    return 1
  fi
  SUITE_PROXY_STARTED_PROXY_ID="$container_id"
}

# A TCP accept proves only the host-side socat listener. These bounded protocol
# exchanges prove that the forwarder reached each intended upstream service.
suite_proxy_postgres_responds() {
  local response
  response="$(
    set +o pipefail
    printf '\000\000\000\010\004\322\026\057' |
      nc -w "$SUITE_PROXY_PROBE_TIMEOUT_SECONDS" -- "$1" "$2" 2>/dev/null |
      dd bs=1 count=1 2>/dev/null
  )" || return 1
  [[ "$response" == S || "$response" == N ]]
}

suite_proxy_redis_responds() {
  local response
  response="$(
    set +o pipefail
    printf '*1\r\n$4\r\nPING\r\n' |
      nc -w "$SUITE_PROXY_PROBE_TIMEOUT_SECONDS" -- "$1" "$2" 2>/dev/null |
      dd bs=1 count=7 2>/dev/null
  )" || return 1
  [[ "$response" == $'+PONG\r' ]]
}

# Readiness is bounded even when Alpine package startup or host publication is
# broken; upstream health alone cannot publish the harness attestation.
suite_proxy_wait_reachable() {
  local deadline=$((SECONDS + SUITE_PROXY_READY_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    if suite_proxy_verify_network "$SUITE_PROXY_NETWORK_ID" &&
       suite_proxy_verify_upstream \
         urnetwork-local-pg postgres "$SUITE_PROXY_POSTGRES_UPSTREAM_ID" &&
       suite_proxy_verify_upstream \
         urnetwork-local-redis redis "$SUITE_PROXY_REDIS_UPSTREAM_ID" &&
       suite_proxy_verify_proxy \
        "$SUITE_PROXY_POSTGRES_PROXY_ID" "$SUITE_PROXY_POSTGRES_PROXY_NAME" postgres \
        "$SUITE_PROXY_POSTGRES_UPSTREAM_ID" "$SUITE_PROXY_POSTGRES_PORT" 5432 &&
       suite_proxy_verify_proxy \
        "$SUITE_PROXY_REDIS_PROXY_ID" "$SUITE_PROXY_REDIS_PROXY_NAME" redis \
        "$SUITE_PROXY_REDIS_UPSTREAM_ID" "$SUITE_PROXY_REDIS_PORT" 6379 &&
       suite_proxy_postgres_responds "$SUITE_PROXY_HOST_IP" "$SUITE_PROXY_POSTGRES_PORT" &&
       suite_proxy_redis_responds "$SUITE_PROXY_HOST_IP" "$SUITE_PROXY_REDIS_PORT"; then
      return 0
    fi
    if (( SECONDS < deadline )); then
      sleep 1
    fi
  done
  return 1
}

# Removes one immutable id only after its current labels still prove exact
# token/generation/upstream ownership. Missing ids are already clean.
suite_proxy_remove_owned_proxy() {
  local proxy_id="$1"
  local proxy_name="$2"
  local service="$3"
  local upstream_id="$4"
  local ownership_status

  ownership_status=0
  suite_proxy_proxy_ownership_matches \
    "$proxy_id" "$proxy_name" "$service" "$upstream_id" || ownership_status=$?
  if [[ "$ownership_status" == 2 ]]; then
    return 0
  fi
  if [[ "$ownership_status" != 0 ]]; then
    return "$ownership_status"
  fi
  if ! suite_proxy_docker rm --force "$proxy_id" >/dev/null; then
    if suite_proxy_container_absence_proven "$proxy_id"; then
      return 0
    fi
    printf 'run-suite-proxy.sh: proxy removal failed and absence is unproven: %s\n' \
      "$proxy_id" >&2
    return 1
  fi
  if ! suite_proxy_container_absence_proven "$proxy_id"; then
    printf 'run-suite-proxy.sh: owned proxy absence is unproven after removal: %s\n' \
      "$proxy_id" >&2
    return 1
  fi
}

# Recovers ids from Docker-owned cidfiles if startup was interrupted between
# container creation and shell assignment. A daemon-backed canonical-name query
# closes the still-earlier window before Docker writes its cidfile.
suite_proxy_lookup_canonical_proxy() {
  local proxy_name="$1"
  local service="$2"
  local upstream_id="$3"
  local listing
  local line
  local count=0
  local proxy_id=""

  listing="$(suite_proxy_docker container ls --all --no-trunc --quiet \
    --filter "name=^/${proxy_name}$")" || return $?
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -n "$line" ]] || continue
    count=$((count + 1))
    proxy_id="$line"
  done <<< "$listing"
  if [[ "$count" == 0 ]]; then
    SUITE_PROXY_LOOKED_UP_PROXY_ID=""
    return 0
  fi
  if [[ "$count" != 1 ]]; then
    printf 'run-suite-proxy.sh: ambiguous canonical proxy lookup: %s\n' "$proxy_name" >&2
    return 1
  fi
  suite_proxy_container_id_validate "canonical proxy id" "$proxy_id" || return $?
  suite_proxy_proxy_ownership_matches \
    "$proxy_id" "$proxy_name" "$service" "$upstream_id" || return $?
  SUITE_PROXY_LOOKED_UP_PROXY_ID="$proxy_id"
}

suite_proxy_recover_proxy_id() {
  local current_id="$1"
  local cidfile="$2"
  local proxy_name="$3"
  local service="$4"
  local upstream_id="$5"

  if [[ -n "$current_id" ]]; then
    suite_proxy_proxy_ownership_matches \
      "$current_id" "$proxy_name" "$service" "$upstream_id" || return $?
    SUITE_PROXY_RECOVERED_PROXY_ID="$current_id"
    return 0
  fi
  if [[ -e "$cidfile" || -L "$cidfile" ]]; then
    suite_proxy_load_cidfile "$cidfile" || return $?
    suite_proxy_proxy_ownership_matches \
      "$SUITE_PROXY_CIDFILE_ID" "$proxy_name" "$service" "$upstream_id" || return $?
    SUITE_PROXY_RECOVERED_PROXY_ID="$SUITE_PROXY_CIDFILE_ID"
    return 0
  fi
  suite_proxy_lookup_canonical_proxy "$proxy_name" "$service" "$upstream_id" || return $?
  SUITE_PROXY_RECOVERED_PROXY_ID="$SUITE_PROXY_LOOKED_UP_PROXY_ID"
}

suite_proxy_recover_proxy_ids() {
  suite_proxy_recover_proxy_id \
    "$SUITE_PROXY_POSTGRES_PROXY_ID" "$SUITE_PROXY_POSTGRES_CIDFILE" \
    "$SUITE_PROXY_POSTGRES_PROXY_NAME" postgres "$SUITE_PROXY_POSTGRES_UPSTREAM_ID" || return $?
  SUITE_PROXY_POSTGRES_PROXY_ID="$SUITE_PROXY_RECOVERED_PROXY_ID"
  suite_proxy_recover_proxy_id \
    "$SUITE_PROXY_REDIS_PROXY_ID" "$SUITE_PROXY_REDIS_CIDFILE" \
    "$SUITE_PROXY_REDIS_PROXY_NAME" redis "$SUITE_PROXY_REDIS_UPSTREAM_ID" || return $?
  SUITE_PROXY_REDIS_PROXY_ID="$SUITE_PROXY_RECOVERED_PROXY_ID"
}

# Withdraws readiness before touching containers. Any state or label mismatch
# retains the ambiguous objects and private state for inspection.
suite_proxy_cleanup() {
  local cleanup_failed=0
  if [[ "$SUITE_PROXY_CLEANED" == 1 ]]; then
    return 0
  fi
  SUITE_PROXY_CLEANED=1
  if [[ "$SUITE_PROXY_STATE_OWNED" != 1 ]]; then
    return 0
  fi

  if ! suite_proxy_attestation_remove \
      "$SUITE_PROXY_STATE_DIR" "$SUITE_PROXY_OWNER_PID" \
      "$SUITE_PROXY_OWNER_START_IDENTITY" "$SUITE_PROXY_OWNER_TOKEN" \
      "$SUITE_PROXY_GENERATION" \
      "$SUITE_PROXY_HOST_IP" "$SUITE_PROXY_HOST_IP" "$SUITE_PROXY_POSTGRES_PORT" \
      "$SUITE_PROXY_HOST_IP" "$SUITE_PROXY_REDIS_PORT" \
      "$SUITE_PROXY_POSTGRES_UPSTREAM_ID" "$SUITE_PROXY_REDIS_UPSTREAM_ID" \
      "$SUITE_PROXY_IMAGE_ID" \
      "$SUITE_PROXY_POSTGRES_PROXY_NAME" "$SUITE_PROXY_POSTGRES_PROXY_ID" \
      "$SUITE_PROXY_REDIS_PROXY_NAME" "$SUITE_PROXY_REDIS_PROXY_ID"; then
    printf 'run-suite-proxy.sh: readiness ownership mismatch; retaining proxies and %s\n' \
      "$SUITE_PROXY_STATE_DIR" >&2
    return 1
  fi

  # A published record contains both immutable ids already. An interrupted
  # pre-publication start may instead need cidfile/name recovery, but only
  # after the canonical readiness path has been withdrawn.
  suite_proxy_recover_proxy_ids || cleanup_failed=1

  if [[ -n "$SUITE_PROXY_REDIS_PROXY_ID" ]] &&
      ! suite_proxy_remove_owned_proxy \
        "$SUITE_PROXY_REDIS_PROXY_ID" "$SUITE_PROXY_REDIS_PROXY_NAME" redis \
        "$SUITE_PROXY_REDIS_UPSTREAM_ID"; then
    cleanup_failed=1
  fi
  if [[ -n "$SUITE_PROXY_POSTGRES_PROXY_ID" ]] &&
      ! suite_proxy_remove_owned_proxy \
        "$SUITE_PROXY_POSTGRES_PROXY_ID" "$SUITE_PROXY_POSTGRES_PROXY_NAME" postgres \
        "$SUITE_PROXY_POSTGRES_UPSTREAM_ID"; then
    cleanup_failed=1
  fi
  if [[ "$cleanup_failed" != 0 ]]; then
    printf 'run-suite-proxy.sh: proxy ownership mismatch; retaining private state %s\n' \
      "$SUITE_PROXY_STATE_DIR" >&2
    return 1
  fi

  suite_proxy_remove_owned_cidfile \
    "$SUITE_PROXY_POSTGRES_CIDFILE" "$SUITE_PROXY_POSTGRES_PROXY_ID" || return $?
  suite_proxy_remove_owned_cidfile \
    "$SUITE_PROXY_REDIS_CIDFILE" "$SUITE_PROXY_REDIS_PROXY_ID" || return $?
  suite_proxy_state_release \
    "$SUITE_PROXY_STATE_DIR" "$SUITE_PROXY_OWNER_PID" \
    "$SUITE_PROXY_OWNER_START_IDENTITY" "$SUITE_PROXY_OWNER_TOKEN" \
    "$SUITE_PROXY_GENERATION" || return $?
  SUITE_PROXY_STATE_OWNED=0
}

suite_proxy_on_exit() {
  local status=$?
  trap - EXIT
  if ! suite_proxy_cleanup; then
    status=1
  fi
  exit "$status"
}

# Revalidates every upstream/proxy identity, the published snapshot, and both
# direct paths. Any loss withdraws readiness through the normal EXIT cleanup.
suite_proxy_monitor() {
  while :; do
    sleep "$SUITE_PROXY_MONITOR_INTERVAL_SECONDS"
    suite_proxy_verify_network "$SUITE_PROXY_NETWORK_ID" || return $?
    suite_proxy_verify_upstream urnetwork-local-pg postgres "$SUITE_PROXY_POSTGRES_UPSTREAM_ID" || return $?
    suite_proxy_verify_upstream urnetwork-local-redis redis "$SUITE_PROXY_REDIS_UPSTREAM_ID" || return $?
    suite_proxy_verify_proxy \
      "$SUITE_PROXY_POSTGRES_PROXY_ID" "$SUITE_PROXY_POSTGRES_PROXY_NAME" postgres \
      "$SUITE_PROXY_POSTGRES_UPSTREAM_ID" "$SUITE_PROXY_POSTGRES_PORT" 5432 || return $?
    suite_proxy_verify_proxy \
      "$SUITE_PROXY_REDIS_PROXY_ID" "$SUITE_PROXY_REDIS_PROXY_NAME" redis \
      "$SUITE_PROXY_REDIS_UPSTREAM_ID" "$SUITE_PROXY_REDIS_PORT" 6379 || return $?
    suite_proxy_attestation_validate_snapshot \
      "$SUITE_PROXY_STATE_DIR" "$SUITE_PROXY_OWNER_PID" \
      "$SUITE_PROXY_OWNER_START_IDENTITY" "$SUITE_PROXY_OWNER_TOKEN" \
      "$SUITE_PROXY_GENERATION" \
      "$SUITE_PROXY_HOST_IP" "$SUITE_PROXY_HOST_IP" "$SUITE_PROXY_POSTGRES_PORT" \
      "$SUITE_PROXY_HOST_IP" "$SUITE_PROXY_REDIS_PORT" \
      "$SUITE_PROXY_POSTGRES_UPSTREAM_ID" "$SUITE_PROXY_REDIS_UPSTREAM_ID" \
      "$SUITE_PROXY_IMAGE_ID" \
      "$SUITE_PROXY_POSTGRES_PROXY_NAME" "$SUITE_PROXY_POSTGRES_PROXY_ID" \
      "$SUITE_PROXY_REDIS_PROXY_NAME" "$SUITE_PROXY_REDIS_PROXY_ID" || return $?
    suite_proxy_postgres_responds \
      "$SUITE_PROXY_HOST_IP" "$SUITE_PROXY_POSTGRES_PORT" || return $?
    suite_proxy_redis_responds \
      "$SUITE_PROXY_HOST_IP" "$SUITE_PROXY_REDIS_PORT" || return $?
  done
}

suite_proxy_main() {
  local script_dir
  local state_file
  local bash_path
  local command_name
  local owner_token_argument=""
  local generation_argument=""

  script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)" ||
    suite_proxy_die "cannot resolve the local script directory"
  state_file="$script_dir/run-local-state.sh"
  [[ -f "$state_file" ]] || suite_proxy_die "state helper is missing: $state_file"
  source "$state_file"

  if [[ "$#" == 0 ]]; then
    command -v od >/dev/null 2>&1 || suite_proxy_die "od not found on PATH"
    command -v tr >/dev/null 2>&1 || suite_proxy_die "tr not found on PATH"
    suite_proxy_random_hex || suite_proxy_die "could not generate owner token"
    owner_token_argument="v1:$SUITE_PROXY_RANDOM_HEX"
    suite_proxy_random_hex || suite_proxy_die "could not generate generation token"
    generation_argument="v1:$SUITE_PROXY_RANDOM_HEX"
    bash_path="${BASH:-}"
    [[ -x "$bash_path" ]] || bash_path="$(command -v bash)" ||
      suite_proxy_die "bash not found on PATH"
    exec "$bash_path" "$script_dir/run-suite-proxy.sh" \
      "--urnetwork-suite-proxy-owner-token=$owner_token_argument" \
      "--urnetwork-suite-proxy-generation=$generation_argument" ||
      suite_proxy_die "could not re-exec with a live owner challenge"
  fi
  if [[ "$#" != 2 ||
        "$1" != --urnetwork-suite-proxy-owner-token=* ||
        "$2" != --urnetwork-suite-proxy-generation=* ]]; then
    suite_proxy_die "this helper takes no user arguments; configure it with the documented environment"
  fi
  owner_token_argument="${1#--urnetwork-suite-proxy-owner-token=}"
  generation_argument="${2#--urnetwork-suite-proxy-generation=}"
  suite_proxy_token_validate "owner token" "$owner_token_argument" || exit 1
  suite_proxy_token_validate "generation" "$generation_argument" || exit 1

  SUITE_PROXY_HOST_IP="${SUITE_PROXY_HOST_IP:-}"
  SUITE_PROXY_STATE_DIR="${WARP_TEST_ENV_SUITE_PROXY_STATE_DIR:-}"
  SUITE_PROXY_POSTGRES_PORT="${SUITE_PROXY_POSTGRES_PORT:-5432}"
  SUITE_PROXY_REDIS_PORT="${SUITE_PROXY_REDIS_PORT:-6379}"
  SUITE_PROXY_READY_TIMEOUT_SECONDS="${SUITE_PROXY_READY_TIMEOUT_SECONDS:-30}"
  SUITE_PROXY_MONITOR_INTERVAL_SECONDS="${SUITE_PROXY_MONITOR_INTERVAL_SECONDS:-2}"
  SUITE_PROXY_PROBE_TIMEOUT_SECONDS="${SUITE_PROXY_PROBE_TIMEOUT_SECONDS:-3}"
  SUITE_PROXY_IMAGE_TAG=alpine:3.22
  SUITE_PROXY_IMAGE_ID=""
  SUITE_PROXY_NETWORK_ID=""
  SUITE_PROXY_POSTGRES_PROXY_NAME=urnetwork-suite-proxy-pg
  SUITE_PROXY_REDIS_PROXY_NAME=urnetwork-suite-proxy-redis
  SUITE_PROXY_OWNER_PID="$$"
  SUITE_PROXY_OWNER_TOKEN="$owner_token_argument"
  SUITE_PROXY_GENERATION="$generation_argument"
  SUITE_PROXY_OWNER_START_IDENTITY=""
  SUITE_PROXY_POSTGRES_UPSTREAM_ID=""
  SUITE_PROXY_REDIS_UPSTREAM_ID=""
  SUITE_PROXY_POSTGRES_PROXY_ID=""
  SUITE_PROXY_REDIS_PROXY_ID=""
  SUITE_PROXY_POSTGRES_CIDFILE="$SUITE_PROXY_STATE_DIR/postgres.cid"
  SUITE_PROXY_REDIS_CIDFILE="$SUITE_PROXY_STATE_DIR/redis.cid"
  SUITE_PROXY_STATE_OWNED=0
  SUITE_PROXY_CLEANED=0

  [[ -n "$SUITE_PROXY_HOST_IP" ]] || suite_proxy_die "SUITE_PROXY_HOST_IP is required"
  [[ -n "$SUITE_PROXY_STATE_DIR" ]] || suite_proxy_die "WARP_TEST_ENV_SUITE_PROXY_STATE_DIR is required"
  suite_proxy_positive_integer_validate "PostgreSQL port" "$SUITE_PROXY_POSTGRES_PORT" || exit 1
  suite_proxy_positive_integer_validate "Redis port" "$SUITE_PROXY_REDIS_PORT" || exit 1
  suite_proxy_positive_integer_validate "readiness timeout" "$SUITE_PROXY_READY_TIMEOUT_SECONDS" || exit 1
  suite_proxy_positive_integer_validate "monitor interval" "$SUITE_PROXY_MONITOR_INTERVAL_SECONDS" || exit 1
  suite_proxy_positive_integer_validate "protocol probe timeout" "$SUITE_PROXY_PROBE_TIMEOUT_SECONDS" || exit 1
  if [[ "${#SUITE_PROXY_POSTGRES_PORT}" -gt 5 ||
        "${#SUITE_PROXY_REDIS_PORT}" -gt 5 ]]; then
    suite_proxy_die "proxy ports must not exceed 65535"
  fi
  (( SUITE_PROXY_POSTGRES_PORT <= 65535 && SUITE_PROXY_REDIS_PORT <= 65535 )) ||
    suite_proxy_die "proxy ports must not exceed 65535"
  (( SUITE_PROXY_READY_TIMEOUT_SECONDS <= 300 )) ||
    suite_proxy_die "readiness timeout must not exceed 300 seconds"
  (( SUITE_PROXY_MONITOR_INTERVAL_SECONDS <= 60 )) ||
    suite_proxy_die "monitor interval must not exceed 60 seconds"
  (( SUITE_PROXY_PROBE_TIMEOUT_SECONDS <= 10 )) ||
    suite_proxy_die "protocol probe timeout must not exceed 10 seconds"
  suite_proxy_host_ip_validate_assigned "$SUITE_PROXY_HOST_IP" || exit 1
  for command_name in awk dd docker find mv nc ps; do
    command -v "$command_name" >/dev/null 2>&1 || suite_proxy_die "$command_name not found on PATH"
  done
  suite_proxy_docker info >/dev/null || suite_proxy_die "Docker daemon is unavailable"

  trap suite_proxy_on_exit EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  SUITE_PROXY_STATE_OWNED=1
  if ! suite_proxy_state_acquire \
      "$SUITE_PROXY_STATE_DIR" "$SUITE_PROXY_OWNER_PID" \
      "$SUITE_PROXY_OWNER_TOKEN" "$SUITE_PROXY_GENERATION"; then
    SUITE_PROXY_STATE_OWNED=0
    suite_proxy_die "another suite proxy owner holds $SUITE_PROXY_STATE_DIR"
  fi
  SUITE_PROXY_OWNER_START_IDENTITY="$SUITE_PROXY_ACQUIRED_START_IDENTITY"

  suite_proxy_verify_network ||
    suite_proxy_die "urnetwork-local is not the repository Compose network"
  SUITE_PROXY_NETWORK_ID="$SUITE_PROXY_INSPECTED_NETWORK_ID"

  suite_proxy_verify_upstream urnetwork-local-pg postgres ||
    suite_proxy_die "urnetwork-local-pg is not a healthy repository Compose service"
  SUITE_PROXY_POSTGRES_UPSTREAM_ID="$SUITE_PROXY_INSPECTED_UPSTREAM_ID"
  suite_proxy_verify_upstream urnetwork-local-redis redis ||
    suite_proxy_die "urnetwork-local-redis is not a healthy repository Compose service"
  SUITE_PROXY_REDIS_UPSTREAM_ID="$SUITE_PROXY_INSPECTED_UPSTREAM_ID"

  suite_proxy_log "resolving $SUITE_PROXY_IMAGE_TAG to an immutable image id"
  suite_proxy_docker pull "$SUITE_PROXY_IMAGE_TAG" >/dev/null ||
    suite_proxy_die "could not pull $SUITE_PROXY_IMAGE_TAG"
  SUITE_PROXY_IMAGE_ID="$(suite_proxy_docker image inspect --format '{{.Id}}' "$SUITE_PROXY_IMAGE_TAG" 2>/dev/null)" ||
    suite_proxy_die "could not inspect $SUITE_PROXY_IMAGE_TAG"
  suite_proxy_image_id_validate "$SUITE_PROXY_IMAGE_ID" ||
    suite_proxy_die "$SUITE_PROXY_IMAGE_TAG did not resolve to a content-addressed image"

  suite_proxy_log "starting PostgreSQL proxy on $SUITE_PROXY_HOST_IP:$SUITE_PROXY_POSTGRES_PORT"
  suite_proxy_start_proxy \
    "$SUITE_PROXY_POSTGRES_PROXY_NAME" postgres urnetwork-local-pg \
    "$SUITE_PROXY_POSTGRES_UPSTREAM_ID" 5432 "$SUITE_PROXY_POSTGRES_PORT" \
    "$SUITE_PROXY_POSTGRES_CIDFILE" || suite_proxy_die "could not start the PostgreSQL proxy"
  SUITE_PROXY_POSTGRES_PROXY_ID="$SUITE_PROXY_STARTED_PROXY_ID"

  suite_proxy_log "starting Redis proxy on $SUITE_PROXY_HOST_IP:$SUITE_PROXY_REDIS_PORT"
  suite_proxy_start_proxy \
    "$SUITE_PROXY_REDIS_PROXY_NAME" redis urnetwork-local-redis \
    "$SUITE_PROXY_REDIS_UPSTREAM_ID" 6379 "$SUITE_PROXY_REDIS_PORT" \
    "$SUITE_PROXY_REDIS_CIDFILE" || suite_proxy_die "could not start the Redis proxy"
  SUITE_PROXY_REDIS_PROXY_ID="$SUITE_PROXY_STARTED_PROXY_ID"

  suite_proxy_wait_reachable ||
    suite_proxy_die "direct proxy endpoints did not become reachable before the bounded deadline"
  suite_proxy_remove_owned_cidfile \
    "$SUITE_PROXY_POSTGRES_CIDFILE" "$SUITE_PROXY_POSTGRES_PROXY_ID" ||
    suite_proxy_die "PostgreSQL cidfile ownership changed"
  suite_proxy_remove_owned_cidfile \
    "$SUITE_PROXY_REDIS_CIDFILE" "$SUITE_PROXY_REDIS_PROXY_ID" ||
    suite_proxy_die "Redis cidfile ownership changed"
  suite_proxy_attestation_publish \
    "$SUITE_PROXY_STATE_DIR" "$SUITE_PROXY_OWNER_PID" \
    "$SUITE_PROXY_OWNER_START_IDENTITY" "$SUITE_PROXY_OWNER_TOKEN" \
    "$SUITE_PROXY_GENERATION" \
    "$SUITE_PROXY_HOST_IP" "$SUITE_PROXY_HOST_IP" "$SUITE_PROXY_POSTGRES_PORT" \
    "$SUITE_PROXY_HOST_IP" "$SUITE_PROXY_REDIS_PORT" \
    "$SUITE_PROXY_POSTGRES_UPSTREAM_ID" "$SUITE_PROXY_REDIS_UPSTREAM_ID" \
    "$SUITE_PROXY_IMAGE_ID" \
    "$SUITE_PROXY_POSTGRES_PROXY_NAME" "$SUITE_PROXY_POSTGRES_PROXY_ID" \
    "$SUITE_PROXY_REDIS_PROXY_NAME" "$SUITE_PROXY_REDIS_PROXY_ID" ||
    suite_proxy_die "could not publish suite proxy readiness"

  suite_proxy_log "ready: export WARP_TEST_ENV_SUITE_PROXY_STATE_DIR=$SUITE_PROXY_STATE_DIR"
  suite_proxy_log "monitoring exact upstream and proxy identities; Ctrl-C stops only owned proxies"
  suite_proxy_monitor || suite_proxy_die "suite proxy ownership or reachability changed"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  suite_proxy_main "$@"
fi
