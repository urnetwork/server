#!/usr/bin/env bash
# Execute one predeclared C2 cohort once, with unchanged deadline and joined cleanup.
set -Eeuo pipefail
[[ $(id -u) == 0 && $# == 2 && $2 =~ ^[0-9a-f]{64}$ ]]
cohort_root="$1"
expected_request_sha256="$2"
[[ "$cohort_root" == /* && $(realpath -e "$cohort_root") == "$cohort_root" ]]
export PATH=/home/by/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS=-p=2
[[ $(go env GOVERSION) == go1.27.3 ]]
[[ $(awk '/Cpus_allowed_list/ {print $2}' /proc/self/status) == 20,22 ]]
readonly SINGLETON_LOCK=/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
[[ -f "$SINGLETON_LOCK" && ! -L "$SINGLETON_LOCK" && $(realpath -e "$SINGLETON_LOCK") == "$SINGLETON_LOCK" ]]
exec 9<>"$SINGLETON_LOCK"
flock -n 9
readonly SCRIPT=/usr/local/libexec/urnetwork/competition-c2-a9e8e1a7-control-7c4dc74e5-go1273-v1/container/evaluator.sh
readonly IMAGE=sha256:b1632a55b415f823871c60b220a194947d280894111c9fa765ad88100e206f24
[[ -f "$cohort_root/artifacts/worker-request.json" && ! -L "$cohort_root/artifacts/worker-request.json" ]]
[[ $(sha256sum "$cohort_root/artifacts/worker-request.json" | cut -d' ' -f1) == "$expected_request_sha256" ]]
[[ ! -e "$cohort_root/artifacts/worker-result.json" && ! -L "$cohort_root/artifacts/worker-result.json" ]]
for prior in evaluator.stdout evaluator.stderr manager-terminal.log started; do
    [[ ! -e "$cohort_root/$prior" && ! -L "$cohort_root/$prior" ]]
done
! systemctl is-active --quiet competition-v2-staging.service
[[ $(sha256sum "$SCRIPT" | cut -d' ' -f1) == a6908739dc854bc9d81c8136361d8012eb0c479dedf4460a04b9987607bff6f5 ]]
[[ $(sha256sum "${SCRIPT%/*}/network-boundary.sh" | cut -d' ' -f1) == 9ab82a9a8bb2272934461472509c15df4242f14859351dadd6dfcb2a1cd27cb7 ]]
[[ $(sha256sum "${SCRIPT%/*}/compose.yml" | cut -d' ' -f1) == f4ca3a5eedfd5f081fbb507fc0102fa03ce11c095b406bcc0f7dc0119b5e0464 ]]
builder_identity="$("${SCRIPT%/*}/submission-builder-identity.sh")"
jq -e '.builder_sha256 == "22f60ee6f0ec26f229fb0c1cfe8df3a0c900a4326cc41a8226e7db9a82e7f63c" and
    .patch_validator_sha256 == "3d161d76bace48bff6536164a84f330b1618c82f319775b8551c19872b937e54"' <<< "$builder_identity" >/dev/null
jq -e --arg image "$IMAGE" '
    .schema == 1 and .base_sha == "a9e8e1a710b24c749e6499a0606938252c83e136" and
    .evaluator_image_digest == $image and .source_epoch == 0 and (.staging // false) == false and
    .scorer_version == "sim-latency-score/1" and .evaluation_policy.replicates == 9 and
    .evaluation_policy.takeover_margin == 0.269 and .evaluation_policy.score_timeout_seconds == 10800 and
    .evaluation_policy.scorer_sha256 == "efd1077d56f2435027d8358e01f29d43355a4ec1a45917e72602fcee8cce6727" and
    .evaluation_policy.simulator_sha256 == .evaluation_policy.scorer_sha256
' "$cohort_root/artifacts/worker-request.json" >/dev/null
[[ $(docker image inspect "$IMAGE" --format '{{.Id}}') == "$IMAGE" ]]
[[ $("${SCRIPT%/*}/hash-local-mount.sh" /home/by/urnetwork/temp/production-20261024.bUpBTuEw/calibration/runtime/config/local) == b8a0996c29195cad1839fa6f7e32933f01f812d573df6e3bb5f9e7b6276430b7 ]]
[[ $("${SCRIPT%/*}/hash-local-mount.sh" /home/by/urnetwork/temp/production-20261024.bUpBTuEw/calibration/runtime/vault/local) == a68c0e07eaa6d831c9a562128950237e76b0cd0f942afd1fec3d7a32b3aed7c7 ]]
readonly EXPECTED_CONTAINERS="$(printf '%s\n' buildx_buildkit_warp-multiarch0 urnetwork-local-pg urnetwork-local-redis | sort)"
readonly EXPECTED_NETWORKS="$(printf '%s\n' bridge host none urnetwork-local | sort)"
[[ $(docker ps -a --format '{{.Names}}' | sort) == "$EXPECTED_CONTAINERS" ]]
[[ $(docker network ls --format '{{.Name}}' | sort) == "$EXPECTED_NETWORKS" ]]
set -o noclobber
printf 'exclusive single-attempt marker\n' > "$cohort_root/started"
run_cohort() {
    local start_epoch end_epoch result
    start_epoch=$(date -u +%s)
    date -u -d "@$start_epoch" +start_utc=%Y-%m-%dT%H:%M:%SZ
    date -u -d "@$((start_epoch+10800))" +deadline_utc=%Y-%m-%dT%H:%M:%SZ
    printf 'request_sha256=%s\nexecution_budget_seconds=10800\nkill_grace_seconds=45\n' "$expected_request_sha256"
    printf 'instrument_revision=C2\nbuilder_sha256=22f60ee6f0ec26f229fb0c1cfe8df3a0c900a4326cc41a8226e7db9a82e7f63c\nvalidator_sha256=3d161d76bace48bff6536164a84f330b1618c82f319775b8551c19872b937e54\n'
    set +e
    timeout --signal=TERM --kill-after=45s 10800s "$SCRIPT" \
        --request "$cohort_root/artifacts/worker-request.json" \
        --result "$cohort_root/artifacts/worker-result.json" \
        > "$cohort_root/evaluator.stdout" 2> "$cohort_root/evaluator.stderr"
    result=$?
    set -e
    [[ $(docker ps -a --format '{{.Names}}' | sort) == "$EXPECTED_CONTAINERS" ]]
    [[ $(docker network ls --format '{{.Name}}' | sort) == "$EXPECTED_NETWORKS" ]]
    [[ -z $(findmnt -rn -o TARGET | awk -v target="$cohort_root/artifacts/.evidence-runtime" '$0 == target') ]]
    printf 'terminal_cleanup_verified\n'
    end_epoch=$(date -u +%s)
    printf 'evaluator_exit=%s duration_seconds=%s\n' "$result" "$((end_epoch-start_epoch))"
    date -u -d "@$end_epoch" +end_utc=%Y-%m-%dT%H:%M:%SZ
    exit "$result"
}
run_cohort | tee "$cohort_root/manager-terminal.log"
