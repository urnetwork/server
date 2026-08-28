#!/usr/bin/env bash

# End-to-end development smoke: derive a harmless one-file candidate, start a
# constrained runner/PostgreSQL/Redis Compose project, and start a networkless
# scorer project. It verifies live Docker state, not only rendered YAML.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly RESOURCE_BOUNDARY="$SCRIPT_DIR/resource-boundary.sh"
readonly HASH_LOCAL_MOUNT="$SCRIPT_DIR/hash-local-mount.sh"
readonly DOCKER_ID_MAP="$SCRIPT_DIR/docker-id-map.sh"
readonly POSTGRES_IMAGE='postgres:18@sha256:06cad38a5d9f5d24b4d83d86def30795d5e4b757fedbf5281172b576dedcd941'
readonly REDIS_IMAGE='redis:8-bookworm@sha256:c22af04bb576503bf16b3e34a1fd2fd82de0f765afd866d2e380145e0af30d78'

base_image="${1:-urnetwork/sim-latency-evaluator-base:dev}"
for command in awk date git install jq mktemp realpath sha256sum stat sudo sync taskset; do
    command -v "$command" >/dev/null 2>&1 || { printf 'missing command: %s\n' "$command" >&2; exit 1; }
done
[ -x "$RESOURCE_BOUNDARY" ] || { printf 'resource boundary is not executable\n' >&2; exit 1; }
[ -x "$HASH_LOCAL_MOUNT" ] || { printf 'local-mount digest helper is not executable\n' >&2; exit 1; }
[ -x "$DOCKER_ID_MAP" ] || { printf 'Docker id-map resolver is not executable\n' >&2; exit 1; }
resource_boundary_json="$($RESOURCE_BOUNDARY)"
SMOKE_CPUSET="$(jq -er '.evaluation_cpuset' <<<"$resource_boundary_json")"
management_cpuset="$(jq -er '.management_cpuset' <<<"$resource_boundary_json")"
expected_management_affinity="$(taskset -c "$management_cpuset" awk -F: \
    '$1 == "Cpus_allowed_list" {gsub(/[[:space:]]/, "", $2); print $2}' /proc/self/status)"
if [ "${COMPETITION_MANAGEMENT_CPUSET_APPLIED:-}" != "$management_cpuset" ]; then
    exec env COMPETITION_MANAGEMENT_CPUSET_APPLIED="$management_cpuset" \
        taskset -c "$management_cpuset" "$(realpath -e "$0")" "$@"
fi
actual_management_affinity="$(awk -F: \
    '$1 == "Cpus_allowed_list" {gsub(/[[:space:]]/, "", $2); print $2}' /proc/self/status)"
[ "$actual_management_affinity" = "$expected_management_affinity" ] || exit 1
sudo -n docker info >/dev/null
base_image_id="$(sudo -n docker image inspect --format '{{.Id}}' "$base_image")"
docker_id_map_json="$($DOCKER_ID_MAP --image "$base_image_id" --uid 65532 --gid 65532)"
jq -e --arg image_id "$base_image_id" \
    '.schema == 1 and .kind == "sim-latency-docker-id-map" and .image_id == $image_id and
     .container_uid == 65532 and .container_gid == 65532 and
     (.host_uid | type == "number" and . >= 0) and (.host_gid | type == "number" and . >= 0) and
     (.remapped | type == "boolean")' <<<"$docker_id_map_json" >/dev/null
container_host_uid="$(jq -er '.host_uid' <<<"$docker_id_map_json")"
container_host_gid="$(jq -er '.host_gid' <<<"$docker_id_map_json")"
[ "$(tr ',' '\n' <<<"$SMOKE_CPUSET" | sed '/^$/d' | wc -l)" -eq 10 ]
[ "$(for cpu in ${SMOKE_CPUSET//,/ }; do
        printf '%s:%s\n' \
            "$(cat "/sys/devices/system/cpu/cpu$cpu/topology/physical_package_id")" \
            "$(cat "/sys/devices/system/cpu/cpu$cpu/topology/core_id")"
    done | sort -u | wc -l)" -eq 10 ]

smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-container-smoke.XXXXXXXX")"
runner_env="$smoke_root/runner.env"
source_container=""
active_project=""
evaluation_image=""
evaluation_input_dir="$smoke_root/input"
evaluation_output_dir="$smoke_root/output"
config_local_directory=""
vault_local_directory=""
config_local_sha256=""
vault_local_sha256=""
local_source_mode=fixture

compose() {
    sudo -n env \
        "COMPOSE_PROJECT_NAME=$active_project" \
        "EVALUATION_JOB_ID=container-smoke" \
        "EVALUATION_ROUND_ID=container-smoke" \
        "EVALUATION_STAGE=${smoke_stage:?}" \
        "EVALUATION_ACTION=${smoke_action:?}" \
        "EVALUATION_CGROUP_PARENT=urnetwork-evaluation.slice" \
        "EVALUATION_CPUSET=$SMOKE_CPUSET" \
        "EVALUATION_IMAGE=${evaluation_image:?}" \
        "EVALUATOR_BASE_IMAGE=$base_image" \
        "EVALUATION_ENV_FILE=$runner_env" \
        "EVALUATION_CONFIG_LOCAL_DIR=${config_local_directory:?}" \
        "EVALUATION_VAULT_LOCAL_DIR=${vault_local_directory:?}" \
        "EVALUATION_INPUT_DIR=${evaluation_input_dir:?}" \
        "EVALUATION_OUTPUT_DIR=${evaluation_output_dir:?}" \
        "EVALUATION_POSTGRES_INIT=$SCRIPT_DIR/postgres-init.sh" \
        "RUNNER_MEMORY_LIMIT=2g" \
        "RUNNER_CPU_SHARES=1024" \
        "RUNNER_PIDS_LIMIT=4096" \
        "RUNNER_TMP_LIMIT=256m" \
        "RUNNER_WORK_LIMIT=512m" \
        "MIGRATOR_MEMORY_LIMIT=4g" \
        "MIGRATOR_PIDS_LIMIT=4096" \
        "MIGRATOR_TMP_LIMIT=1g" \
        "POSTGRES_MEMORY_LIMIT=2g" \
        "POSTGRES_CPU_SHARES=4096" \
        "POSTGRES_PIDS_LIMIT=1024" \
        "POSTGRES_DATA_LIMIT=2g" \
        "POSTGRES_MAX_CONNECTIONS=128" \
        "POSTGRES_SHARED_BUFFERS=256MB" \
        "REDIS_MEMORY_LIMIT=1g" \
        "REDIS_CPU_SHARES=2048" \
        "REDIS_PIDS_LIMIT=512" \
        "REDIS_DATA_LIMIT=512m" \
        "REDIS_MAX_CLIENTS=4096" \
        "REDIS_TCP_BACKLOG=4096" \
        "SCORER_MEMORY_LIMIT=1g" \
        "SCORER_PIDS_LIMIT=512" \
        "SCORER_TMP_LIMIT=256m" \
        "EVALUATION_NOFILE=1048576" \
        docker compose \
            --env-file "$SCRIPT_DIR/job.env.example" \
            --file "$SCRIPT_DIR/compose.yml" \
            "$@"
}

cleanup() {
    if [ -n "${active_project:-}" ]; then
        compose --profile run --profile score down --volumes --remove-orphans >/dev/null 2>&1 || true
    fi
    if [ -n "${source_container:-}" ]; then
        sudo -n docker rm -f "$source_container" >/dev/null 2>&1 || true
    fi
    if [ -n "${smoke_root:-}" ] && [ -d "$smoke_root" ]; then
        if [ "${SMOKE_KEEP_ARTIFACTS:-no}" = yes ]; then
            sudo -n chown -R "$(id -u):$(id -g)" "$smoke_root" 2>/dev/null || true
            printf 'retained smoke artifacts: %s\n' "$smoke_root" >&2
            return
        fi
        sudo -n chown -R "$(id -u):$(id -g)" "$smoke_root" 2>/dev/null || true
        chmod -R u+w "$smoke_root" 2>/dev/null || true
        rm -rf -- "$smoke_root"
    fi
}
trap cleanup EXIT INT TERM

install -d -m 0700 "$smoke_root/input" "$smoke_root/output"
if [ -n "${SMOKE_CONFIG_LOCAL_DIR:-}" ] || [ -n "${SMOKE_VAULT_LOCAL_DIR:-}" ]; then
    [ -n "${SMOKE_CONFIG_LOCAL_DIR:-}" ] && [ -n "${SMOKE_VAULT_LOCAL_DIR:-}" ] || {
        printf 'SMOKE_CONFIG_LOCAL_DIR and SMOKE_VAULT_LOCAL_DIR must be set together\n' >&2
        exit 1
    }
    config_local_directory="$(realpath -e "$SMOKE_CONFIG_LOCAL_DIR")"
    vault_local_directory="$(realpath -e "$SMOKE_VAULT_LOCAL_DIR")"
    [ -d "$config_local_directory" ] && [[ "$config_local_directory" == */config/local ]] || {
        printf 'SMOKE_CONFIG_LOCAL_DIR must name an existing config/local directory\n' >&2
        exit 1
    }
    [ -d "$vault_local_directory" ] && [[ "$vault_local_directory" == */vault/local ]] || {
        printf 'SMOKE_VAULT_LOCAL_DIR must name an existing vault/local directory\n' >&2
        exit 1
    }
    local_source_mode=direct
else
    config_local_directory="$smoke_root/local-source/config/local"
    vault_local_directory="$smoke_root/local-source/vault/local"
    install -d -m 0700 "$smoke_root/local-source"
    # Production binds its frozen config/local and vault/local directories
    # without copying them. This generator is limited to equivalent throwaway
    # source directories when direct smoke paths were not supplied.
    env \
        EVALUATION_DB_USER=bringyour \
        EVALUATION_DB_PASSWORD=replace-with-random-per-job-value \
        EVALUATION_DB_NAME=bringyour \
        EVALUATION_REDIS_PASSWORD=replace-with-random-per-job-value \
        EVALUATION_DB_MIN_CONNECTIONS=4 \
        EVALUATION_DB_MAX_CONNECTIONS=32 \
        EVALUATION_REDIS_MIN_CONNECTIONS=4 \
        EVALUATION_REDIS_MAX_CONNECTIONS=32 \
        "$SCRIPT_DIR/prepare-runtime.sh" "$smoke_root/local-source"
    # These sentinels deliberately sit beside local. Exact leaf mounts must
    # make every one absent inside the migrator and candidate containers.
    install -d -m 0700 \
        "$smoke_root/local-source/config/all" \
        "$smoke_root/local-source/config/main" \
        "$smoke_root/local-source/vault/all" \
        "$smoke_root/local-source/vault/main" \
        "$smoke_root/local-source/site/local"
    for sentinel in \
        config/all/forbidden \
        config/main/forbidden \
        vault/all/forbidden \
        vault/main/forbidden \
        site/local/forbidden; do
        install -m 0400 /dev/null "$smoke_root/local-source/$sentinel"
    done
    sudo -n chown -R "$container_host_uid:$container_host_gid" "$smoke_root/local-source"
    sudo -n chmod 0700 "$smoke_root/local-source"
fi
sudo -n chown -R "$container_host_uid:$container_host_gid" "$smoke_root/input" "$smoke_root/output"
sudo -n chmod 0700 "$smoke_root/input" "$smoke_root/output"
config_local_sha256="$(sudo -n "$HASH_LOCAL_MOUNT" "$config_local_directory")"
vault_local_sha256="$(sudo -n "$HASH_LOCAL_MOUNT" "$vault_local_directory")"

verify_local_sources_unchanged() {
    [ "$(sudo -n "$HASH_LOCAL_MOUNT" "$config_local_directory")" = "$config_local_sha256" ] || {
        printf 'config/local changed during the smoke test\n' >&2
        return 1
    }
    [ "$(sudo -n "$HASH_LOCAL_MOUNT" "$vault_local_directory")" = "$vault_local_sha256" ] || {
        printf 'vault/local changed during the smoke test\n' >&2
        return 1
    }
}
verify_local_sources_unchanged
source_container="$(sudo -n docker create "$base_image_id")"
sudo -n docker cp "$source_container:/workspace/server" "$smoke_root/server"
sudo -n docker rm "$source_container" >/dev/null
source_container=""
sudo -n chown -R "$(id -u):$(id -g)" "$smoke_root/server"

printf '\n// competitionContainerSmoke verifies deterministic per-patch image derivation.\n' \
    >> "$smoke_root/server/connect/resident_contract_manager.go"
git -C "$smoke_root/server" diff --no-ext-diff --binary -- connect/resident_contract_manager.go \
    > "$smoke_root/canonical.patch"
[ -s "$smoke_root/canonical.patch" ]
install -m 0400 "$SCRIPT_DIR/policy.example.json" "$smoke_root/policy.json"

candidate_json="$($SCRIPT_DIR/build-submission.sh \
    --allow-local-base \
    --base-image "$base_image" \
    --patch "$smoke_root/canonical.patch" \
    --policy "$smoke_root/policy.json")"
candidate_image_id="$(jq -er '.image_id' <<<"$candidate_json")"
candidate_image="$(jq -er '.image' <<<"$candidate_json")"
candidate_sha="$(jq -er '.candidate_sha' <<<"$candidate_json")"
candidate_patch_sha256="$(jq -er '.patch_sha256' <<<"$candidate_json")"

cached_candidate_json="$($SCRIPT_DIR/build-submission.sh \
    --allow-local-base \
    --base-image "$base_image" \
    --patch "$smoke_root/canonical.patch" \
    --policy "$smoke_root/policy.json")"
[ "$(jq -er '.image_id' <<<"$cached_candidate_json")" = "$candidate_image_id" ]
[ "$(jq -er '.image_key' <<<"$cached_candidate_json")" = "$(jq -er '.image_key' <<<"$candidate_json")" ]

identity="$(sudo -n docker run --rm \
    --network none \
    --read-only \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    "$candidate_image_id" identity)"
jq -e --arg candidate_sha "$candidate_sha" \
    '.schema == 1 and .image_kind == "submission" and .build_sha == $candidate_sha and (.paths | length == 1)' \
    <<<"$identity" >/dev/null

base_identity="$(sudo -n docker run --rm \
    --network none \
    --read-only \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    "$base_image_id" identity)"
base_sha="$(jq -er '.base_sha' <<<"$base_identity")"
base_simulator_sha256="$(jq -er '.simulator_sha256' <<<"$base_identity")"
candidate_simulator_sha256="$(jq -er '.simulator_sha256' <<<"$identity")"

sudo -n docker run --rm \
    --network none \
    --read-only \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    --user 65532:65532 \
    --entrypoint /opt/urnetwork/bin/sim-latency \
    --volume "$smoke_root/input:/input" \
    "$candidate_image_id" \
    init --out=/input/providers.yml --count=32 --clients=8 --rate=16 --seed=1 --quality-window=8
# Spread the seeded crawls across eight warm clients and eight quality lanes:
# reusing two clients with two lanes manufactured contract contention and
# partial responses in a test intended to validate containment. The smaller
# rate and fleet still yield a real sample while honoring the smoke runner's
# deliberately tight memory limit. The mandatory measured-window probe
# exercises the real FindProviders2 path deterministically.
sudo -n chmod 0400 "$smoke_root/input/providers.yml"
providers_sha256="$(sudo -n sha256sum "$smoke_root/input/providers.yml" | awk '{print $1}')"
microcode_revision="$(awk -F: '$1 ~ /^microcode/ {gsub(/[[:space:]]/, "", $2); print $2}' /proc/cpuinfo | sort -u | paste -sd, -)"
[ -n "$microcode_revision" ]
qualification_sha256="$(printf '%s' 'local-container-smoke-not-qualified' | sha256sum | awk '{print $1}')"
install -m 0600 /dev/null "$runner_env"
printf '%s\n' \
    'EVALUATION_DB_PASSWORD=replace-with-random-per-job-value' \
    'EVALUATION_REDIS_PASSWORD=replace-with-random-per-job-value' \
    "APEX_BASE_SHA=$base_sha" \
    "APEX_BUILD_SHA=$candidate_sha" \
    'APEX_SCORER_BIN=/opt/urnetwork/bin/sim-latency' \
    "APEX_SCORER_SHA256=$candidate_simulator_sha256" \
    'APEX_SIM_BIN=/opt/urnetwork/bin/sim-latency' \
    "APEX_SIM_SHA256=$candidate_simulator_sha256" \
    'APEX_PROVIDERS_FILE=/input/providers.yml' \
    "APEX_PROVIDERS_SHA256=$providers_sha256" \
    'APEX_ARTIFACT_ROOT=/artifacts' \
    'APEX_EVALUATION_ID=container-smoke-preflight' \
    "APEX_API_IMAGE_DIGEST=$candidate_image_id" \
    'APEX_HARDWARE_ID=local-container-smoke-not-qualified' \
    "APEX_HOST_QUALIFICATION_SHA256=$qualification_sha256" \
    "APEX_KERNEL_RELEASE=$(uname -r)" \
    "APEX_MICROCODE_REVISION=$microcode_revision" \
    'APEX_PATCH_FILE=/opt/urnetwork/submission/canonical.patch' \
    "APEX_PATCH_SHA256=$candidate_patch_sha256" \
    'APEX_CPU_COUNT=10' \
    'APEX_DURATION=30s' \
    'APEX_REQUEST_TIMEOUT=30s' \
    'APEX_RAMP=0s' \
    'APEX_PREWARM=5s' \
    'APEX_SETTLE=0s' \
    'APEX_CLIENT_WARMUP_TIMEOUT=1m' \
    'APEX_FLEET_SHARDS=0' \
    'APEX_HOSTS=1' \
    'APEX_PIPELINE_INTERVAL=100ms' \
    'APEX_TEST_TIMEOUT=3s' \
    'APEX_ANNOUNCE_TIMEOUT=2s' \
    'APEX_SITE_LISTEN=127.0.0.1:0' \
    'APEX_API_PORT=7640' \
    'APEX_NO_IMPAIR=yes' \
    'APEX_WALL_TIMEOUT=2m' \
    'APEX_KILL_AFTER=5s' \
    'APEX_CALIBRATION_ACCEPTED=yes' \
    > "$runner_env"
chmod 0400 "$runner_env"
evaluation_image="$candidate_image"

sudo -n docker pull "$POSTGRES_IMAGE" >/dev/null
sudo -n docker pull "$REDIS_IMAGE" >/dev/null

smoke_action=identity
smoke_stage=candidate
active_project="urnetwork-container-smoke-run-${candidate_sha:0:12}"
compose --profile run config >/dev/null
compose --profile run up --detach --wait postgres redis >&2
migration_json="$(compose --profile run run --rm --no-deps --no-tty migrate)"
jq -e '.schema == 1 and
       .database_version > 0 and
       .database_version == .migration_count' <<<"$migration_json" >/dev/null
compose --profile run up --no-deps --abort-on-container-exit --exit-code-from runner runner >&2
runner_id="$(compose --profile run ps --all --quiet runner)"
network_id="${active_project}_evaluation"
[ -n "$runner_id" ] && [ -n "$network_id" ]
sudo -n docker inspect "$runner_id" | jq -e --arg cpuset "$SMOKE_CPUSET" \
    --arg config_local "$config_local_directory" \
    --arg vault_local "$vault_local_directory" \
    '.[0].Config.User == "65532:65532" and
     .[0].HostConfig.ReadonlyRootfs == true and
     .[0].HostConfig.CpusetCpus == $cpuset and
     .[0].HostConfig.CpuShares == 1024 and
     .[0].HostConfig.Memory == 2147483648 and
     .[0].HostConfig.MemorySwap == 2147483648 and
     .[0].HostConfig.PidsLimit == 4096 and
     .[0].HostConfig.CgroupParent == "urnetwork-evaluation.slice" and
     (.[0].HostConfig.CapDrop | index("ALL") != null) and
     (.[0].HostConfig.SecurityOpt | index("no-new-privileges:true") != null) and
     ([.[0].Mounts[] | select(.Destination | startswith("/runtime"))] | length == 2) and
     (any(.[0].Mounts[]; .Type == "bind" and .Source == $config_local and
       .Destination == "/runtime/config/local" and .RW == false)) and
     (any(.[0].Mounts[]; .Type == "bind" and .Source == $vault_local and
       .Destination == "/runtime/vault/local" and .RW == false))' >/dev/null
compose --profile run run --rm --no-deps --no-tty \
    --entrypoint /usr/bin/bash runner -Eeuo pipefail -c '
        test -r /runtime/config/local/db.yml
        test -r /runtime/vault/local/pg.yml
        test ! -e /runtime/config/all
        test ! -e /runtime/config/main
        test ! -e /runtime/vault/all
        test ! -e /runtime/vault/main
        test ! -e /runtime/site
        ! touch /runtime/config/local/write-test
        ! touch /runtime/vault/local/write-test
    '
sudo -n docker network inspect "$network_id" | jq -e '.[0].Internal == true' >/dev/null
compose --profile run down --volumes --remove-orphans >&2
active_project=""
verify_local_sources_unchanged

smoke_action=preflight
smoke_stage=candidate
active_project="urnetwork-container-smoke-preflight-${candidate_sha:0:12}"
compose --profile run up --detach --wait postgres redis >&2
migration_json="$(compose --profile run run --rm --no-deps --no-tty migrate)"
jq -e '.schema == 1 and
       .database_version > 0 and
       .database_version == .migration_count' <<<"$migration_json" >/dev/null
compose --profile run up --no-deps --abort-on-container-exit --exit-code-from runner runner >&2
preflight_id="$(compose --profile run ps --all --quiet runner)"
[ -n "$preflight_id" ]
[ "$(sudo -n docker inspect --format '{{.State.ExitCode}}' "$preflight_id")" -eq 0 ]
compose --profile run down --volumes --remove-orphans >&2
active_project=""
verify_local_sources_unchanged

smoke_action=run
smoke_stage=candidate
active_project="urnetwork-container-smoke-simulator-${candidate_sha:0:12}"
compose --profile run up --detach --wait postgres redis >&2
migration_json="$(compose --profile run run --rm --no-deps --no-tty migrate)"
jq -e '.schema == 1 and
       .database_version > 0 and
       .database_version == .migration_count' <<<"$migration_json" >/dev/null
compose --profile run up --no-deps --abort-on-container-exit --exit-code-from runner runner >&2
simulator_id="$(compose --profile run ps --all --quiet runner)"
[ -n "$simulator_id" ]
[ "$(sudo -n docker inspect --format '{{.State.ExitCode}}' "$simulator_id")" -eq 0 ]
sudo -n test -s "$smoke_root/output/container-smoke-preflight/results.csv"
sudo -n test -s "$smoke_root/output/container-smoke-preflight/accounting.source.json"
sudo -n jq -e '.schema == 2 and
       .score_schema == 1 and
       .completion_state == "complete"' \
    "$smoke_root/output/container-smoke-preflight/run.json" >/dev/null
sudo -n jq -e '.schema == 1 and
       .kind == "sim-latency-provider-accounting-source" and
       .evaluation_id == "container-smoke-preflight" and
       .complete == true and
       .provider_egress_bytes >= 0' \
    "$smoke_root/output/container-smoke-preflight/accounting.source.json" >/dev/null
sudo -n jq -e '.schema == 1 and
       .kind == "sim-latency-complete" and
       .score_schema == 1 and
       .evaluation_id == "container-smoke-preflight"' \
    "$smoke_root/output/container-smoke-preflight/run.complete.json" >/dev/null
run_manifest_sha256="$(sudo -n sha256sum \
    "$smoke_root/output/container-smoke-preflight/run.json" | awk '{print $1}')"
sudo -n jq -e --arg run_manifest_sha256 "$run_manifest_sha256" \
    '.run_manifest_sha256 == $run_manifest_sha256' \
    "$smoke_root/output/container-smoke-preflight/run.complete.json" >/dev/null

# The simulator writes a hash-authenticated provider counter source. Only the
# trusted host-side evaluator derives the scorer-facing accounting snapshot;
# candidate code never gets a writable accounting path.
run_dir="$smoke_root/output/container-smoke-preflight"
accounting_tmp="$(mktemp "$smoke_root/accounting.XXXXXXXX.json")"
sudo -n jq \
    '{schema: 1, kind: "sim-latency-accounting",
      evaluation_id: .evaluation_id, complete: .complete,
      measure_start_ms: .measure_start_ms,
      measure_end_ms: .measure_end_ms,
      provider_egress_bytes: .provider_egress_bytes}' \
    "$run_dir/accounting.source.json" > "$accounting_tmp"
sudo -n install -o "$container_host_uid" -g "$container_host_gid" -m 0400 \
    "$accounting_tmp" "$run_dir/accounting.json"
rm -f -- "$accounting_tmp"

runner_inspect="$(sudo -n docker inspect "$simulator_id")"
jq -e --arg cpuset "$SMOKE_CPUSET" \
    '.[0].Config.User == "65532:65532" and
     .[0].HostConfig.ReadonlyRootfs == true and
     .[0].HostConfig.CpusetCpus == $cpuset and
     .[0].HostConfig.CpuShares == 1024 and
     .[0].HostConfig.Memory == 2147483648 and
     .[0].HostConfig.MemorySwap == 2147483648 and
     .[0].HostConfig.PidsLimit == 4096 and
     .[0].HostConfig.CgroupParent == "urnetwork-evaluation.slice" and
     .[0].State.ExitCode == 0 and .[0].State.OOMKilled == false' \
    <<<"$runner_inspect" >/dev/null
resource_start_ms="$(date --date="$(jq -er '.[0].State.StartedAt' <<<"$runner_inspect")" +%s%3N)"
resource_end_ms="$(date +%s%3N)"
resource_tmp="$(mktemp "$smoke_root/resources.XXXXXXXX.json")"
jq -n \
    --arg evaluation_id container-smoke-preflight \
    --arg cgroup_id "urnetwork-evaluation.slice/$active_project" \
    --argjson measurement_start_ms "$resource_start_ms" \
    --argjson measurement_end_ms "$resource_end_ms" \
    '{schema: 1, kind: "sim-latency-resource-report",
      evaluation_id: $evaluation_id, cgroup_id: $cgroup_id,
      measurement_start_ms: $measurement_start_ms,
      measurement_end_ms: $measurement_end_ms,
      complete: true, exit_code: 0, oom_killed: false,
      hard_killed: false, limit_escape: false,
      measurement_missing: false, cpu_seconds: 0,
      peak_rss_bytes: 0}' > "$resource_tmp"
sudo -n install -o "$container_host_uid" -g "$container_host_gid" -m 0400 \
    "$resource_tmp" "$run_dir/resources.json"
rm -f -- "$resource_tmp"
sudo -n sync -d "$run_dir/accounting.json" "$run_dir/resources.json"
sudo -n jq -e '.kind == "sim-latency-accounting" and .complete == true' \
    "$run_dir/accounting.json" >/dev/null
sudo -n jq -e '.kind == "sim-latency-resource-report" and
       .complete == true and .exit_code == 0 and
       .oom_killed == false and .hard_killed == false and
       .limit_escape == false and .measurement_missing == false' \
    "$run_dir/resources.json" >/dev/null
compose --profile run down --volumes --remove-orphans >&2
active_project=""
verify_local_sources_unchanged

sudo -n install -o "$container_host_uid" -g "$container_host_gid" -m 0400 \
    "$smoke_root/input/providers.yml" "$smoke_root/output/providers.yml"
baseline_output="$smoke_root/baseline-output"
score_output="$smoke_root/score-output"
install -d -m 0700 "$baseline_output" "$score_output"
sudo -n chown "$container_host_uid:$container_host_gid" "$baseline_output" "$score_output"
stats_root="$(sudo -n jq -er '.stats_root' "$run_dir/run.json")"

write_scorer_common_env() {
    chmod 0600 "$runner_env"
    printf '%s\n' \
        'EVALUATION_DB_PASSWORD=replace-with-random-per-job-value' \
        'EVALUATION_REDIS_PASSWORD=replace-with-random-per-job-value' \
        "APEX_BASE_SHA=$base_sha" \
        "APEX_BUILD_SHA=$base_sha" \
        'APEX_SCORER_BIN=/opt/urnetwork/bin/sim-latency' \
        "APEX_SCORER_SHA256=$base_simulator_sha256" \
        'APEX_SIM_BIN=/opt/urnetwork/bin/sim-latency' \
        "APEX_SIM_SHA256=$base_simulator_sha256" \
        'APEX_PROVIDERS_FILE=/artifacts/providers.yml' \
        "APEX_PROVIDERS_SHA256=$providers_sha256" \
        'APEX_ARTIFACT_ROOT=/artifacts' \
        'APEX_EVALUATION_ID=container-smoke-preflight' \
        "APEX_API_IMAGE_DIGEST=$base_image_id" \
        'APEX_HARDWARE_ID=local-container-smoke-not-qualified' \
        "APEX_HOST_QUALIFICATION_SHA256=$qualification_sha256" \
        "APEX_KERNEL_RELEASE=$(uname -r)" \
        "APEX_MICROCODE_REVISION=$microcode_revision" \
        'APEX_PATCH_FILE=/opt/urnetwork/submission/canonical.patch' \
        'APEX_PATCH_SHA256=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855' \
        'APEX_CPU_COUNT=10' \
        'APEX_DURATION=30s' \
        'APEX_REQUEST_TIMEOUT=30s' \
        'APEX_RAMP=0s' \
        'APEX_PREWARM=5s' \
        'APEX_SETTLE=0s' \
        'APEX_CLIENT_WARMUP_TIMEOUT=1m' \
        'APEX_FLEET_SHARDS=0' \
        'APEX_HOSTS=1' \
        'APEX_PIPELINE_INTERVAL=100ms' \
        'APEX_TEST_TIMEOUT=3s' \
        'APEX_ANNOUNCE_TIMEOUT=2s' \
        'APEX_SITE_LISTEN=127.0.0.1:0' \
        'APEX_API_PORT=7640' \
        'APEX_NO_IMPAIR=yes' \
        'APEX_WALL_TIMEOUT=2m' \
        'APEX_KILL_AFTER=5s' \
        'APEX_CALIBRATION_ACCEPTED=yes' \
        > "$runner_env"
}

evaluation_input_dir="$smoke_root/output"
evaluation_output_dir="$baseline_output"
write_scorer_common_env
printf '%s\n' \
    'APEX_ROUND_ID=container-smoke-round' \
    'APEX_TAKEOVER_MARGIN=0.10' \
    'APEX_BASELINE_RUNS=/artifacts/container-smoke-preflight/results.csv' \
    'APEX_BASELINE_STDERR=/artifacts/container-smoke-preflight/stderr.log' \
    'APEX_BASELINE_ACCOUNTING=/artifacts/container-smoke-preflight/accounting.json' \
    "APEX_BASELINE_SAMPLES=$stats_root" \
    'APEX_BASELINE_RESOURCES=/artifacts/container-smoke-preflight/resources.json' \
    'APEX_BASELINE_MARKERS=/artifacts/container-smoke-preflight/run.complete.json' \
    'APEX_BASELINE_MANIFEST=/score-output/baseline.json' \
    >> "$runner_env"
chmod 0400 "$runner_env"
smoke_action=baseline
smoke_stage=score
active_project="urnetwork-container-smoke-baseline-${candidate_sha:0:12}"
compose --profile score up --abort-on-container-exit --exit-code-from scorer >&2
scorer_id="$(compose --profile score ps --all --quiet scorer)"
[ -n "$scorer_id" ]
sudo -n docker inspect "$scorer_id" | jq -e \
    '.[0].Config.User == "65532:65532" and
     .[0].HostConfig.ReadonlyRootfs == true and
     .[0].HostConfig.NetworkMode == "none" and
     .[0].HostConfig.PidsLimit == 512 and
     (.[0].HostConfig.CapDrop | index("ALL") != null) and
     (.[0].Mounts | any(.Destination == "/artifacts" and .RW == false)) and
     (.[0].Mounts | any(.Destination == "/score-output" and .RW == true))' >/dev/null
sudo -n jq -e '.score_schema == 1 and
       .kind == "sim-latency-score-baseline" and
       (.replicates | length) == 1 and
       ([.replicates[].findproviders_sample_span_fraction |
         type == "number" and isfinite and . >= 0.90 and . <= 1] | all)' \
    "$baseline_output/baseline.json" >/dev/null
compose --profile score down --volumes --remove-orphans >&2
active_project=""
verify_local_sources_unchanged

sudo -n install -o "$container_host_uid" -g "$container_host_gid" -m 0400 \
    "$baseline_output/baseline.json" "$smoke_root/output/baseline.json"
baseline_sha256="$(sudo -n sha256sum "$smoke_root/output/baseline.json" | awk '{print $1}')"
evaluation_output_dir="$score_output"
write_scorer_common_env
printf '%s\n' \
    'APEX_BASELINE_MANIFEST=/artifacts/baseline.json' \
    "APEX_BASELINE_SHA256=$baseline_sha256" \
    'APEX_CANDIDATE_RUNS=/artifacts/container-smoke-preflight/results.csv' \
    'APEX_CANDIDATE_STDERR=/artifacts/container-smoke-preflight/stderr.log' \
    'APEX_CANDIDATE_ACCOUNTING=/artifacts/container-smoke-preflight/accounting.json' \
    "APEX_CANDIDATE_SAMPLES=$stats_root" \
    'APEX_CANDIDATE_RESOURCES=/artifacts/container-smoke-preflight/resources.json' \
    'APEX_CANDIDATE_MARKERS=/artifacts/container-smoke-preflight/run.complete.json' \
    'APEX_SCORE_OUTPUT=/score-output/score.json' \
    >> "$runner_env"
chmod 0400 "$runner_env"
smoke_action=score
smoke_stage=score
active_project="urnetwork-container-smoke-score-${candidate_sha:0:12}"
compose --profile score up --abort-on-container-exit --exit-code-from scorer >&2
sudo -n jq -e '.score_schema == 1 and .placeable == true and
       ([.gates[] | .passed] | all)' "$score_output/score.json" >/dev/null
compose --profile score down --volumes --remove-orphans >&2
active_project=""
verify_local_sources_unchanged

jq -n \
    --arg base_image_id "$base_image_id" \
    --arg candidate_image_id "$candidate_image_id" \
    --arg candidate_sha "$candidate_sha" \
    --arg local_source_mode "$local_source_mode" \
    --arg config_local_sha256 "$config_local_sha256" \
    --arg vault_local_sha256 "$vault_local_sha256" \
    --argjson docker_id_map "$docker_id_map_json" \
    '{schema: 1, passed: true, base_image_id: $base_image_id,
      candidate_image_id: $candidate_image_id, candidate_sha: $candidate_sha,
      local_source_mode: $local_source_mode,
      config_local_sha256: $config_local_sha256,
      vault_local_sha256: $vault_local_sha256,
      docker_id_map: $docker_id_map,
      checks: ["canonical patch revalidation", "offline candidate build",
      "authenticated image cache reuse",
      "deterministic clean candidate commit", "nonroot read-only runner",
      "exact read-only config/local and vault/local mounts",
      "shared cgroup parent and frozen limits", "one thread per physical core",
      "internal run network",
      "fresh PostgreSQL and Redis", "trusted database migrations",
      "authenticated official runner preflight",
      "complete real simulator lifecycle",
      "trusted provider-egress accounting",
      "authenticated external resource report",
      "real baseline manifest and candidate score",
      "networkless pristine scorer"]}'
