#!/bin/bash
# Fresh services and source for each point; never overwrite previous attempts.
set -Eeuo pipefail
umask 077
readonly root=/home/by/urnetwork/temp/provide-frontier-20261024.B0P1kUpz
readonly scripts=/home/by/urnetwork/temp/provide-evaluator-build-20261024.Aq1Q7w99/server/connect/sim-latency/evaluator/container
readonly generator=/home/by/urnetwork/temp/provide-write-backport-20261024.7GlsHf3C/sim-latency-smoke
readonly expected_image=sha256:d526a183a2ecf96793f736e40ef52eadf1bf0ef212efc1ea66c1d8f296f951bf
readonly image_tag=urnetwork/sim-latency-evaluator-base:provide-fix-6261c154
readonly runtime=/home/by/urnetwork/temp/production-20261024.bUpBTuEw/calibration/runtime
test "$#" -eq 3 || exit 2
count="$1"
mode="$2"
attempt="$3"
case "$count" in 1800|7200|15000|48000|100000) ;; *) exit 2 ;; esac
case "$mode" in on|off) ;; *) exit 2 ;; esac
case "$attempt" in initial|same-seed-[1-9]) ;; *) exit 2 ;; esac
boundary="$("$scripts/resource-boundary.sh")"
management="$(jq -er .management_cpuset <<<"$boundary")"
expected_affinity="$(taskset -c "$management" awk -F: '$1 == "Cpus_allowed_list" {gsub(/[[:space:]]/, "", $2); print $2}' /proc/self/status)"
actual_affinity="$(awk -F: '$1 == "Cpus_allowed_list" {gsub(/[[:space:]]/, "", $2); print $2}' /proc/self/status)"
if test "$actual_affinity" != "$expected_affinity"; then
  exec taskset -c "$management" /bin/bash "$0" "$@"
fi
exec 9>/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
flock -n 9 || { printf 'scoring slot is occupied\n' >&2; exit 90; }
if systemctl is-active --quiet competition-v2-staging.service; then
  printf 'staging supervisor active; refusing concurrent scoring\n' >&2
  exit 90
fi
active="$(sudo -n docker ps -a --format '{{.Names}}' | sed -n '/^urnetwork-\(frontier\|container-smoke\|build-isolation\|resource-bomb\)/p')"
test -z "$active" || { printf 'another qualification/evaluation project exists\n' >&2; exit 90; }
test "$(sudo -n docker image inspect --format '{{.Id}}' "$image_tag")" = "$expected_image"
test "$(sha256sum "$generator" | awk '{print $1}')" = 9e967c54d65da7e0225b27ae818f3bd38acb6bcb3bc8f2885f703325362df759
verify_runtime() {
  test "$(sudo -n "$scripts/hash-local-mount.sh" "$runtime/config/local")" = b8a0996c29195cad1839fa6f7e32933f01f812d573df6e3bb5f9e7b6276430b7
  test "$(sudo -n "$scripts/hash-local-mount.sh" "$runtime/vault/local")" = a68c0e07eaa6d831c9a562128950237e76b0cd0f942afd1fec3d7a32b3aed7c7
}
verify_runtime
readonly point="$root/p$count-$mode-$attempt"
mkdir -m700 "$point" "$point/input" "$point/output"
set -a
source "$scripts/job.env.example"
set +a
export COMPOSE_PROJECT_NAME="urnetwork-frontier-d526-p$count-$mode-$attempt"
export EVALUATION_JOB_ID="frontier-d526-p$count-$mode-$attempt"
export EVALUATION_ROUND_ID=prelaunch-frontier-not-official
export EVALUATION_STAGE=baseline EVALUATION_ACTION=run
export EVALUATION_IMAGE="$image_tag" EVALUATOR_BASE_IMAGE="$image_tag"
export EVALUATION_SOURCE_DIR="$point/source"
export EVALUATION_CONFIG_LOCAL_DIR="$runtime/config/local"
export EVALUATION_VAULT_LOCAL_DIR="$runtime/vault/local"
export EVALUATION_INPUT_DIR="$point/input" EVALUATION_OUTPUT_DIR="$point/output"
export EVALUATION_ENV_FILE=/dev/null EVALUATION_POSTGRES_INIT="$scripts/postgres-init.sh"
export EVALUATION_CPUSET="$(jq -er .evaluation_cpuset <<<"$boundary")"
export FRONTIER_IMPAIRMENT="$mode"
mapping="$("$scripts/docker-id-map.sh" --image "$expected_image" --uid 65532 --gid 65532)"
host_uid="$(jq -er .host_uid <<<"$mapping")"
host_gid="$(jq -er .host_gid <<<"$mapping")"
printf '%s\n' "$mapping" > "$point/docker-id-map.json"
"$scripts/prepare-evaluation-source.sh" --base-image "$expected_image" --destination "$EVALUATION_SOURCE_DIR"
sudo -n find "$EVALUATION_SOURCE_DIR" -type d -exec chmod 0555 {} +
sudo -n find "$EVALUATION_SOURCE_DIR" -type f -perm /111 -exec chmod 0555 {} +
sudo -n find "$EVALUATION_SOURCE_DIR" -type f ! -perm /111 -exec chmod 0444 {} +
sudo -n chown -R "$host_uid:$host_gid" "$EVALUATION_SOURCE_DIR"
"$generator" init --count="$count" --clients=200 --rate=80 --seed=20261024 --quality-window=2 --out="$point/input/providers.yml"
install -m0444 "$root/measure.sh" "$point/input/measure.sh"
chmod 0444 "$point/input/providers.yml"
chmod 0555 "$point/input"
sudo -n chown "$host_uid:$host_gid" "$point/output"
compose() {
  sudo -n --preserve-env=COMPOSE_PROJECT_NAME,EVALUATION_JOB_ID,EVALUATION_ROUND_ID,EVALUATION_STAGE,EVALUATION_ACTION,EVALUATION_CGROUP_PARENT,EVALUATION_CPUSET,EVALUATION_IMAGE,EVALUATOR_BASE_IMAGE,EVALUATION_SOURCE_DIR,EVALUATION_CONFIG_LOCAL_DIR,EVALUATION_VAULT_LOCAL_DIR,EVALUATION_INPUT_DIR,EVALUATION_OUTPUT_DIR,EVALUATION_ENV_FILE,EVALUATION_POSTGRES_INIT,RUNNER_MEMORY_LIMIT,RUNNER_PIDS_LIMIT,RUNNER_TMP_LIMIT,RUNNER_WORK_LIMIT,MIGRATOR_MEMORY_LIMIT,MIGRATOR_PIDS_LIMIT,MIGRATOR_TMP_LIMIT,POSTGRES_MEMORY_LIMIT,POSTGRES_PIDS_LIMIT,POSTGRES_DATA_LIMIT,POSTGRES_MAX_CONNECTIONS,POSTGRES_SHARED_BUFFERS,REDIS_MEMORY_LIMIT,REDIS_PIDS_LIMIT,REDIS_DATA_LIMIT,REDIS_MAX_CLIENTS,REDIS_TCP_BACKLOG,SCORER_MEMORY_LIMIT,SCORER_PIDS_LIMIT,SCORER_TMP_LIMIT,EVALUATION_NOFILE,EVALUATION_DB_USER,EVALUATION_DB_ADMIN_PASSWORD,EVALUATION_DB_PASSWORD,EVALUATION_DB_NAME,EVALUATION_REDIS_PASSWORD \
    docker compose --env-file "$scripts/job.env.example" -f "$scripts/compose.yml" "$@"
}
cleanup() {
  original=$?
  trap - EXIT
  logs_status=0
  compose --profile run logs --no-color postgres redis > "$point/services.log" 2>&1 || logs_status=$?
  compose --profile run down --volumes --remove-orphans || exit 90
  test -z "$(sudo -n docker ps -a --filter "label=com.docker.compose.project=$COMPOSE_PROJECT_NAME" --format '{{.ID}}')" || exit 90
  verify_runtime || exit 90
  test "$(sudo -n docker image inspect --format '{{.Id}}' "$image_tag")" = "$expected_image" || exit 90
  test "$logs_status" = 0 || exit 90
  exit "$original"
}
trap cleanup EXIT
printf 'public frontier providers=%s clients=200 rate=80 hosts=4 shards=4 impairment=%s image=%s\n' "$count" "$mode" "$expected_image"
date -u +'%Y-%m-%dT%H:%M:%SZ'
cat /proc/sys/kernel/random/boot_id
compose --profile run config --quiet
compose --profile run up -d --wait postgres redis
compose --profile run run --rm --no-deps -T -e EVALUATION_DB_PASSWORD -e EVALUATION_REDIS_PASSWORD migrate
compose --profile run run --rm --no-deps -T -e EVALUATION_DB_PASSWORD -e EVALUATION_REDIS_PASSWORD -e EVALUATION_JOB_ID -e "FRONTIER_IMPAIRMENT=$mode" \
  --entrypoint /bin/bash runner /input/measure.sh
