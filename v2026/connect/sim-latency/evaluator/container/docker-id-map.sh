#!/usr/bin/env bash

# Resolve a container UID/GID through the live Docker daemon's user-namespace
# maps. Host bind directories must be owned by these translated ids; assuming
# that container UID 65532 is host UID 65532 breaks as soon as userns-remap is
# enabled.

set -Eeuo pipefail
umask 077

die() {
    printf '[docker-id-map] ERROR: %s\n' "$*" >&2
    exit 1
}

translate_id() {
    local map_path="$1" container_id="$2"
    [ -f "$map_path" ] && [ ! -L "$map_path" ] || return 1
    [[ "$container_id" =~ ^[0-9]+$ ]] && [ "$container_id" -le 4294967294 ] || return 1
    awk -v target="$container_id" '
        function uint(value) { return value ~ /^[0-9]+$/ }
        {
            if (NF != 3 || !uint($1) || !uint($2) || !uint($3) || $3 + 0 <= 0) {
                invalid = 1
                next
            }
            container_start = $1 + 0
            host_start = $2 + 0
            range_length = $3 + 0
            if (target >= container_start && target - container_start < range_length) {
                found++
                translated = host_start + (target - container_start)
            }
        }
        END {
            if (invalid || found != 1 || translated < 0 || translated > 4294967294) {
                exit 1
            }
            printf "%.0f\n", translated
        }
    ' "$map_path"
}

if [ "${1:-}" = --translate ]; then
    [ "$#" -eq 3 ] || die 'usage: docker-id-map --translate MAP_FILE CONTAINER_ID'
    translate_id "$2" "$3" || die 'container id is not covered by exactly one valid map range'
    exit 0
fi

image=""
container_uid=""
container_gid=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        --image|--uid|--gid)
            [ "$#" -ge 2 ] || die 'missing option value'
            case "$1" in
                --image) image="$2" ;;
                --uid) container_uid="$2" ;;
                --gid) container_gid="$2" ;;
            esac
            shift 2
            ;;
        *) die 'usage: docker-id-map --image SHA256_ID --uid ID --gid ID' ;;
    esac
done

[[ "$image" =~ ^sha256:[0-9a-f]{64}$ ]] || die 'image must be an immutable local image id'
[[ "$container_uid" =~ ^[0-9]+$ ]] && [ "$container_uid" -le 4294967294 ] ||
    die 'container UID is invalid'
[[ "$container_gid" =~ ^[0-9]+$ ]] && [ "$container_gid" -le 4294967294 ] ||
    die 'container GID is invalid'
for command in awk docker jq seq sha256sum sleep sudo; do
    command -v "$command" >/dev/null 2>&1 || die "required command missing: $command"
done

[ "$(sudo -n docker image inspect --format '{{.Id}}' "$image" 2>/dev/null)" = "$image" ] ||
    die 'image is unavailable or its identity changed'

probe_name="urnetwork-docker-id-map-$$-$RANDOM"
probe_id=""
cleanup() {
    if [ -n "$probe_id" ]; then
        sudo -n docker rm -f "$probe_id" >/dev/null 2>&1 || true
    else
        sudo -n docker rm -f "$probe_name" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT INT TERM

probe_id="$(sudo -n docker run --detach \
    --name "$probe_name" \
    --label com.urnetwork.competition.id-map-probe=true \
    --network none \
    --read-only \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    --pids-limit 16 \
    --memory 32m \
    --memory-swap 32m \
    --user 0:0 \
    --entrypoint /usr/bin/sleep \
    "$image" 60)" || die 'could not start the namespace-map probe'
[[ "$probe_id" =~ ^[0-9a-f]{64}$ ]] || die 'namespace-map probe id is invalid'

probe_pid=""
for _ in $(seq 1 100); do
    probe_pid="$(sudo -n docker inspect --format '{{.State.Pid}}' "$probe_id" 2>/dev/null || true)"
    [[ "$probe_pid" =~ ^[1-9][0-9]*$ ]] && [ -r "/proc/$probe_pid/uid_map" ] && break
    sleep 0.05
done
[[ "$probe_pid" =~ ^[1-9][0-9]*$ ]] || die 'namespace-map probe never entered a live process'
uid_map_path="/proc/$probe_pid/uid_map"
gid_map_path="/proc/$probe_pid/gid_map"
[ -r "$uid_map_path" ] && [ -r "$gid_map_path" ] || die 'live namespace maps are unreadable'

host_uid="$(translate_id "$uid_map_path" "$container_uid")" || die 'container UID is unmapped'
host_gid="$(translate_id "$gid_map_path" "$container_gid")" || die 'container GID is unmapped'
root_host_uid="$(translate_id "$uid_map_path" 0)" || die 'container root UID is unmapped'
root_host_gid="$(translate_id "$gid_map_path" 0)" || die 'container root GID is unmapped'
uid_map_sha256="$(sha256sum "$uid_map_path" | awk '{print $1}')"
gid_map_sha256="$(sha256sum "$gid_map_path" | awk '{print $1}')"
security_options="$(sudo -n docker info --format '{{json .SecurityOptions}}' | jq -cS .)"
remapped=false
if [ "$root_host_uid" != 0 ] || [ "$root_host_gid" != 0 ] ||
   [ "$host_uid" != "$container_uid" ] || [ "$host_gid" != "$container_gid" ]; then
    remapped=true
fi

jq -n \
    --arg image_id "$image" \
    --arg uid_map_sha256 "$uid_map_sha256" \
    --arg gid_map_sha256 "$gid_map_sha256" \
    --argjson container_uid "$container_uid" \
    --argjson container_gid "$container_gid" \
    --argjson host_uid "$host_uid" \
    --argjson host_gid "$host_gid" \
    --argjson root_host_uid "$root_host_uid" \
    --argjson root_host_gid "$root_host_gid" \
    --argjson remapped "$remapped" \
    --argjson daemon_security_options "$security_options" \
    '{schema:1,kind:"sim-latency-docker-id-map",image_id:$image_id,
      container_uid:$container_uid,host_uid:$host_uid,
      container_gid:$container_gid,host_gid:$host_gid,
      root_host_uid:$root_host_uid,root_host_gid:$root_host_gid,
      uid_map_sha256:$uid_map_sha256,gid_map_sha256:$gid_map_sha256,
      remapped:$remapped,daemon_security_options:$daemon_security_options}'
