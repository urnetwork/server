#!/bin/bash
# Each capacity point owns a fresh disposable stack and private output leaf.
set -Eeuo pipefail
umask 077
readonly root=/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5
readonly prepared=/home/by/urnetwork/temp/production-20261024.bUpBTuEw
readonly scripts="$prepared/server/connect/sim-latency/evaluator/container"
readonly measure_script="${FRONTIER_MEASURE_SCRIPT:-$root/measure.sh}"
count="$1"
mode="$2"
attempt="${3:-initial}"
case "$count" in 1800|7200|15000|48000|100000) ;; *) exit 2 ;; esac
case "$mode" in on|off) ;; *) exit 2 ;; esac
case "$attempt" in initial|handoff-fix|same-seed-[1-9]) ;; *) exit 2 ;; esac
exec 9>"$root/scoring.lock"
if ! flock -n 9; then
  printf 'another frontier point owns the scoring slot\n' >&2
  exit 90
fi
if systemctl is-active --quiet competition-v2-staging.service; then
  printf 'staging supervisor active; frontier refuses concurrent scoring\n' >&2
  exit 90
fi
active="$(sudo -n docker ps -a --filter name=urnetwork-frontier-63atmxs5 --format '{{.ID}}')"
if test -n "$active"; then
  printf 'frontier containers already exist; reconcile before scoring\n' >&2
  exit 90
fi
suffix=""
if test "$attempt" != initial; then suffix="-$attempt"; fi
readonly point="$root/p$count-$mode$suffix"
mkdir -m700 "$point" "$point/input" "$point/output"
set -a
source "$scripts/job.env.example"
set +a
export COMPOSE_PROJECT_NAME="urnetwork-frontier-63atmxs5-p$count-$mode$suffix"
export EVALUATION_JOB_ID="frontier-63atmxs5-p$count-$mode$suffix"
export EVALUATION_ROUND_ID=prelaunch-frontier-not-official
export EVALUATION_STAGE=baseline EVALUATION_ACTION=run
export EVALUATION_IMAGE=urnetwork/sim-latency-evaluator-base-id:826569fdeb02f72ec4f24ace7c93b16b69a60e1875c31799c5a0217b402e0d6c
export EVALUATOR_BASE_IMAGE="$EVALUATION_IMAGE"
export EVALUATION_SOURCE_DIR="$prepared/calibration/source"
export EVALUATION_CONFIG_LOCAL_DIR="$prepared/calibration/runtime/config/local"
export EVALUATION_VAULT_LOCAL_DIR="$prepared/calibration/runtime/vault/local"
export EVALUATION_INPUT_DIR="$point/input" EVALUATION_OUTPUT_DIR="$point/output"
export EVALUATION_ENV_FILE=/dev/null EVALUATION_POSTGRES_INIT="$scripts/postgres-init.sh"
export FRONTIER_IMPAIRMENT="$mode"
existing="$(sudo -n docker ps -a --filter "label=com.docker.compose.project=$COMPOSE_PROJECT_NAME" --format '{{.ID}}')"
test -z "$existing" || { printf 'frontier project already exists\n' >&2; exit 90; }
"$prepared/sim-latency-retirement" init --count="$count" --clients=200 --rate=80 --seed=20261024 --quality-window=2 --out="$point/input/providers.yml"
install -m0444 "$measure_script" "$point/input/measure.sh"
chmod 0444 "$point/input/providers.yml"
chmod 0555 "$point/input"
sudo -n chown 165532:165532 "$point/output"
compose() {
  sudo -n --preserve-env=COMPOSE_PROJECT_NAME,EVALUATION_JOB_ID,EVALUATION_ROUND_ID,EVALUATION_STAGE,EVALUATION_ACTION,EVALUATION_CGROUP_PARENT,EVALUATION_CPUSET,EVALUATION_IMAGE,EVALUATOR_BASE_IMAGE,EVALUATION_SOURCE_DIR,EVALUATION_CONFIG_LOCAL_DIR,EVALUATION_VAULT_LOCAL_DIR,EVALUATION_INPUT_DIR,EVALUATION_OUTPUT_DIR,EVALUATION_ENV_FILE,EVALUATION_POSTGRES_INIT,RUNNER_MEMORY_LIMIT,RUNNER_PIDS_LIMIT,RUNNER_TMP_LIMIT,RUNNER_WORK_LIMIT,MIGRATOR_MEMORY_LIMIT,MIGRATOR_PIDS_LIMIT,MIGRATOR_TMP_LIMIT,POSTGRES_MEMORY_LIMIT,POSTGRES_PIDS_LIMIT,POSTGRES_DATA_LIMIT,POSTGRES_MAX_CONNECTIONS,POSTGRES_SHARED_BUFFERS,REDIS_MEMORY_LIMIT,REDIS_PIDS_LIMIT,REDIS_DATA_LIMIT,REDIS_MAX_CLIENTS,REDIS_TCP_BACKLOG,SCORER_MEMORY_LIMIT,SCORER_PIDS_LIMIT,SCORER_TMP_LIMIT,EVALUATION_NOFILE,EVALUATION_DB_USER,EVALUATION_DB_ADMIN_PASSWORD,EVALUATION_DB_PASSWORD,EVALUATION_DB_NAME,EVALUATION_REDIS_PASSWORD \
    docker compose --env-file "$scripts/job.env.example" -f "$scripts/compose.yml" "$@"
}
cleanup() {
  original=$?
  trap - EXIT
  if ! compose --profile run down --volumes --remove-orphans; then exit 90; fi
  remaining="$(sudo -n docker ps -a --filter "label=com.docker.compose.project=$COMPOSE_PROJECT_NAME" --format '{{.ID}}')"
  test -z "$remaining" || exit 90
  exit "$original"
}
trap cleanup EXIT
printf 'frontier providers=%s clients=200 arrivals=80 exchange_hosts=4 impairment=%s\n' "$count" "$mode"
date -u +'%Y-%m-%dT%H:%M:%SZ'
cat /proc/sys/kernel/random/boot_id
compose --profile run config --quiet
compose --profile run up -d --wait postgres redis
compose --profile run run --rm --no-deps -T -e EVALUATION_DB_PASSWORD -e EVALUATION_REDIS_PASSWORD migrate
compose --profile run run --rm --no-deps -T -e EVALUATION_DB_PASSWORD -e EVALUATION_REDIS_PASSWORD -e EVALUATION_JOB_ID -e "FRONTIER_IMPAIRMENT=$mode" \
  --entrypoint /bin/bash runner /input/measure.sh
