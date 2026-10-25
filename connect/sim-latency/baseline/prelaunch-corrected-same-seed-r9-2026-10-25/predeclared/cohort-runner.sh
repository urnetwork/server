#!/usr/bin/env bash
# Execute one predeclared fresh cohort, once, with joined cleanup/logging.
set -Eeuo pipefail
test "$(id -u)" = 0
test "$#" = 2
cohort_root="$1"
expected_request_sha256="$2"
export PATH=/home/by/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS=-p=2
test "$(go env GOVERSION)" = go1.27.3
test "$(awk '/Cpus_allowed_list/ {print $2}' /proc/self/status)" = 20,22
singleton_lock=/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
test -f "$singleton_lock" && test ! -L "$singleton_lock"
test "$(realpath -e "$singleton_lock")" = "$singleton_lock"
exec 9<>"$singleton_lock"
flock -n 9
test "$(sha256sum "$cohort_root/artifacts/worker-request.json" | cut -d ' ' -f1)" = "$expected_request_sha256"
test ! -e "$cohort_root/artifacts/worker-result.json"
for prior in evaluator.stdout evaluator.stderr manager-terminal.log started; do
    test ! -e "$cohort_root/$prior" && test ! -L "$cohort_root/$prior"
done
! systemctl is-active --quiet competition-v2-staging.service
script=/usr/local/libexec/urnetwork/competition-737dd2371-go1273-isolated-v1/container/evaluator.sh
test "$(sha256sum "$script" | cut -d ' ' -f1)" = 263ee68ab058889c22de51d47d8779d95fd219e92ea7642430f61740d15d8d5e
test "$(sha256sum "${script%/*}/network-boundary.sh" | cut -d ' ' -f1)" = 9ab82a9a8bb2272934461472509c15df4242f14859351dadd6dfcb2a1cd27cb7
test "$(sha256sum "${script%/*}/compose.yml" | cut -d ' ' -f1)" = f4ca3a5eedfd5f081fbb507fc0102fa03ce11c095b406bcc0f7dc0119b5e0464
test "$(jq -r .base_sha "$cohort_root/artifacts/worker-request.json")" = c1f9dda603d6279666fca5d92b99c5663306c5f7
test "$(jq -r .evaluator_image_digest "$cohort_root/artifacts/worker-request.json")" = sha256:002c39040ce23dee27b171c7e657f749c6a6d05b1e9b97d90ac87336470dbaf3
test "$(docker image inspect sha256:002c39040ce23dee27b171c7e657f749c6a6d05b1e9b97d90ac87336470dbaf3 --format '{{.Id}}')" = sha256:002c39040ce23dee27b171c7e657f749c6a6d05b1e9b97d90ac87336470dbaf3
test "$("${script%/*}/hash-local-mount.sh" /home/by/urnetwork/temp/production-20261024.bUpBTuEw/calibration/runtime/config/local)" = b8a0996c29195cad1839fa6f7e32933f01f812d573df6e3bb5f9e7b6276430b7
test "$("${script%/*}/hash-local-mount.sh" /home/by/urnetwork/temp/production-20261024.bUpBTuEw/calibration/runtime/vault/local)" = a68c0e07eaa6d831c9a562128950237e76b0cd0f942afd1fec3d7a32b3aed7c7
test "$(docker ps -a --format '{{.Names}}' | sort)" = "$(printf '%s\n' buildx_buildkit_warp-multiarch0 urnetwork-local-pg urnetwork-local-redis | sort)"
test "$(docker network ls --format '{{.Name}}' | sort)" = "$(printf '%s\n' bridge host none urnetwork-local | sort)"
set -o noclobber
printf 'exclusive single-attempt marker\n' > "$cohort_root/started"
run_cohort() {
    start_epoch=$(date -u +%s)
    date -u -d "@$start_epoch" +start_utc=%Y-%m-%dT%H:%M:%SZ
    date -u -d "@$((start_epoch+10800))" +deadline_utc=%Y-%m-%dT%H:%M:%SZ
    printf 'request_sha256=%s\nexecution_budget_seconds=10800\nkill_grace_seconds=45\n' "$expected_request_sha256"
    set +e
    timeout --signal=TERM --kill-after=45s 10800s "$script" --request "$cohort_root/artifacts/worker-request.json" --result "$cohort_root/artifacts/worker-result.json" > "$cohort_root/evaluator.stdout" 2> "$cohort_root/evaluator.stderr"
    result=$?
    set -e
    test "$(docker ps -a --format '{{.Names}}' | sort)" = "$(printf '%s\n' buildx_buildkit_warp-multiarch0 urnetwork-local-pg urnetwork-local-redis | sort)"
    test "$(docker network ls --format '{{.Name}}' | sort)" = "$(printf '%s\n' bridge host none urnetwork-local | sort)"
    test -z "$(findmnt -rn -o TARGET | awk -v target="$cohort_root/artifacts/.evidence-runtime" '$0==target')"
    echo terminal_cleanup_verified
    end_epoch=$(date -u +%s)
    printf 'evaluator_exit=%s duration_seconds=%s\n' "$result" "$((end_epoch-start_epoch))"
    date -u -d "@$end_epoch" +end_utc=%Y-%m-%dT%H:%M:%SZ
    exit "$result"
}
run_cohort | tee "$cohort_root/manager-terminal.log"
