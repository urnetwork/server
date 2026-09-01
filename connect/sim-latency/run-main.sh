#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
server_root=$(cd -- "$script_dir/../.." && pwd -P)
workspace_root=$(cd -- "$server_root/.." && pwd -P)
source_config=${SIM_LATENCY_SOURCE_CONFIG:-$workspace_root/config/main/sim-latency.yml}
api_url=${SIM_LATENCY_API_URL:-https://api.bringyour.com}
state_dir=${SIM_LATENCY_STATE_DIR:-$workspace_root/.sim-latency-state}
operator_token_file=${SIM_LATENCY_OPERATOR_TOKEN_FILE:-}
reviewer_id=${SIM_LATENCY_REVIEWER_ID:-}
first_opens_at=${SIM_LATENCY_FIRST_OPENS_AT:-}
preparation_seconds=${SIM_LATENCY_PREPARATION_SECONDS:-57600}

usage() {
    cat <<'EOF'
Usage:
  run-main.sh run
  run-main.sh status [--epoch N]
  run-main.sh candidate --epoch N
  run-main.sh approve --epoch N --job-id ID --evidence FILE --reason TEXT
  run-main.sh reject --epoch N --job-id ID --evidence FILE --reason TEXT

Required environment for run:
  SIM_LATENCY_OPERATOR_TOKEN_FILE  private file containing the operator token
  SIM_LATENCY_REVIEWER_ID          stable agent/operator reviewer identity

Optional environment:
  SIM_LATENCY_API_URL              defaults to https://api.bringyour.com
  SIM_LATENCY_SOURCE_CONFIG        defaults to config/main/sim-latency.yml
  SIM_LATENCY_STATE_DIR            defaults outside the repositories at
                                    WORKSPACE/.sim-latency-state
  SIM_LATENCY_FIRST_OPENS_AT       RFC3339 start for epoch 1; default is now
  SIM_LATENCY_PREPARATION_SECONDS  delay before later epochs open; default 57600

Exit 20 means an authenticated significant candidate is waiting for the
mandatory honesty and safety review documented in RUN-MAIN.md.
EOF
}

case ${1:-} in
    -h|--help|help)
        usage
        exit 0
        ;;
esac

fail() {
    printf 'run-main.sh: %s\n' "$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command is unavailable: $1"
}

private_regular_file() {
    local path=$1
    [[ -f $path && ! -L $path && -s $path ]] || return 1
    local mode
    mode=$(stat -c '%a' "$path")
    (( (8#$mode & 0077) == 0 ))
}

remove_candidate_directory() {
    local epoch=$1
    local path=$2
    local base
    base=$(basename -- "$path")
    [[ $path == /* && $path != / && -d $path && ! -L $path &&
        $base == sim-latency-review-epoch-$epoch-* ]] ||
        fail "refusing to remove an unauthenticated candidate directory: $path"
    rm -rf -- "$path"
}

main_environment() {
    export WARP_HOST=${WARP_HOST:-127.0.0.1}
    export WARP_BLOCK=${WARP_BLOCK:-sim}
    export WARP_SERVICE=${WARP_SERVICE:-sim}
    export WARP_VERSION=${WARP_VERSION:-0.0.0-local}
    export WARP_ENV=main
    export WARP_DOMAIN=${WARP_DOMAIN:-bringyour.com}
    export BRINGYOUR_POSTGRES_HOSTNAME=${BRINGYOUR_POSTGRES_HOSTNAME:-127.0.0.1}
    export BRINGYOUR_REDIS_HOSTNAME=${BRINGYOUR_REDIS_HOSTNAME:-127.0.0.1}
}

sim_binary() {
    local goos goarch binary
    goos=$(go env GOOS)
    goarch=$(go env GOARCH)
    binary=$script_dir/build/$goos/$goarch/sim-latency
    if [[ ! -x $binary ]]; then
        make -C "$script_dir" build >&2
    fi
    printf '%s\n' "$binary"
}

latest_source_epoch() {
    awk '
        /^[[:space:]]*- epoch:[[:space:]]*[0-9]+[[:space:]]*$/ { epoch = $3 }
        END {
            if (epoch == "") exit 1
            print epoch
        }
    ' "$source_config" || fail "cannot read the latest source epoch from $source_config"
}

api_request() {
    local method=$1
    local path=$2
    local body=${3:-}
    local token
    private_regular_file "$operator_token_file" ||
        fail "operator token must be a nonempty regular file with mode 0600 or stricter"
    token=$(tr -d '\r\n' < "$operator_token_file")
    [[ -n $token ]] || fail "operator token file is empty"
    if [[ -n $body ]]; then
        printf 'header = "Authorization: Bearer %s"\n' "$token" |
            curl --silent --show-error --fail-with-body --config - \
                --request "$method" --header 'Content-Type: application/json' \
                --data-binary "$body" "$api_url$path"
    else
        printf 'header = "Authorization: Bearer %s"\n' "$token" |
            curl --silent --show-error --fail-with-body --config - \
                --request "$method" "$api_url$path"
    fi
}

write_evidence() {
    local name=$1
    local content=$2
    local path=$state_dir/$name
    local temporary
    temporary=$(mktemp "$state_dir/.evidence.XXXXXX")
    chmod 0600 "$temporary"
    printf '%s\n' "$content" > "$temporary"
    mv -f -- "$temporary" "$path"
}

create_round() {
    local epoch=$1
    local opens_at closes_at request response
    if [[ $epoch == 1 && -n $first_opens_at ]]; then
        opens_at=$first_opens_at
    else
        opens_at=$(date -u -d "+$preparation_seconds seconds" '+%Y-%m-%dT%H:%M:%SZ')
    fi
    opens_at=$(date -u -d "$opens_at" '+%Y-%m-%dT%H:%M:%SZ') ||
        fail "round open time is not RFC3339-compatible: $opens_at"
    local opens_seconds
    opens_seconds=$(date -u -d "$opens_at" '+%s')
    closes_at=$(date -u -d "@$((opens_seconds + 604800))" '+%Y-%m-%dT%H:%M:%SZ')
    request=$(jq -cn --arg opens "$opens_at" --arg closes "$closes_at" \
        '{opens_at:$opens, closes_at:$closes, reveal_at:$closes}')
    response=$(api_request POST /competition/generate-round "$request")
    jq -e --argjson epoch "$epoch" \
        '.epoch == $epoch and .opens_at != null and .closes_at != null and .round_id != null' \
        <<<"$response" >/dev/null || fail "generate-round returned an invalid epoch $epoch record"
    write_evidence "epoch-$epoch-round.json" "$response"
    printf '%s\n' "$response"
}

review_json() {
    local epoch=$1
    local action=${2:-next}
    shift 2 || true
    "$(sim_binary)" epoch-review --epoch="$epoch" "$action" "$@"
}

present_candidate_or_promote_no_winner() {
    local epoch=$1
    local result status winner
    result=$(review_json "$epoch" next)
    status=$(jq -er '.state.status' <<<"$result")
    case $status in
        pending_review)
            write_evidence "epoch-$epoch-review.json" "$result"
            printf '%s\n' "$result"
            printf '\nEpoch %d is paused for mandatory honesty review. See RUN-MAIN.md.\n' "$epoch" >&2
            return 20
            ;;
        finalized)
            winner=$(jq -r '.state.winner_job_id // empty' <<<"$result")
            if [[ -n $winner ]]; then
                fail "epoch $epoch is finalized with winner $winner but its source promotion is absent"
            fi
            "$(sim_binary)" promote --epoch="$epoch" --no-winner \
                --source-config="$source_config" --repos-root="$workspace_root"
            ;;
        evaluating)
            fail "epoch $epoch worker exited while accepted submissions remain nonterminal"
            ;;
        *)
            fail "epoch $epoch returned unknown review state: $status"
            ;;
    esac
}

run_worker() {
    local epoch=$1
    local opens_at=$2
    local worker=$state_dir/bin/competitionworker
    mkdir -p "$state_dir/bin"
    go build -trimpath -buildvcs=true -o "$worker" "$server_root/cli/competitionworker"
    chmod 0500 "$worker"
    printf 'Epoch %d opens at %s. The worker heartbeat starts now; claims wait for the open boundary.\n' \
        "$epoch" "$opens_at" >&2
    "$worker" --worker_id="sim-latency-epoch-$epoch"
}

run_season() {
    local source_epoch epoch info active_epoch status opens_at
    while true; do
        source_epoch=$(latest_source_epoch)
        if (( source_epoch >= 6 )); then
            printf 'All six sim-latency epochs are finalized and promoted.\n'
            return 0
        fi
        epoch=$((source_epoch + 1))
        info=$(api_request GET /competition/info)
        active_epoch=$(jq -r '.active_round.epoch // 0' <<<"$info")
        if (( active_epoch > epoch )); then
            fail "API active epoch $active_epoch is ahead of source ledger epoch $source_epoch"
        fi
        if (( active_epoch < epoch )); then
            create_round "$epoch" >/dev/null
            info=$(api_request GET /competition/info)
            active_epoch=$(jq -r '.active_round.epoch // 0' <<<"$info")
        fi
        (( active_epoch == epoch )) || fail "API did not activate expected epoch $epoch"
        status=$(jq -er '.active_round.status' <<<"$info")
        case $status in
            scheduled|open|grading)
                opens_at=$(jq -er '.active_round.opens_at' <<<"$info")
                run_worker "$epoch" "$opens_at"
                present_candidate_or_promote_no_winner "$epoch" || return $?
                ;;
            finalized)
                present_candidate_or_promote_no_winner "$epoch" || return $?
                ;;
            canceled)
                fail "epoch $epoch was canceled; operator resolution is required"
                ;;
            *)
                fail "API returned unknown epoch status: $status"
                ;;
        esac
    done
}

parse_review_arguments() {
    epoch=
    job_id=
    evidence=
    reason=
    while (( $# )); do
        case $1 in
            --epoch) epoch=${2:-}; shift 2 ;;
            --job-id) job_id=${2:-}; shift 2 ;;
            --evidence) evidence=${2:-}; shift 2 ;;
            --reason) reason=${2:-}; shift 2 ;;
            *) fail "unknown argument: $1" ;;
        esac
    done
}

decide_candidate() {
    local decision=$1
    shift
    parse_review_arguments "$@"
    [[ $epoch =~ ^[1-6]$ ]] || fail "--epoch must be in 1..6"
    [[ -n $job_id && -n $reason ]] || fail "--job-id and --reason are required"
    private_regular_file "$evidence" ||
        fail "--evidence must be a private nonempty regular JSON file"
    [[ -n $reviewer_id ]] || fail "SIM_LATENCY_REVIEWER_ID is required"

    local pending pending_job candidate_dir result status
    pending=$(review_json "$epoch" next)
    pending_job=$(jq -er '.state.candidate.job_id' <<<"$pending")
    [[ $pending_job == "$job_id" ]] ||
        fail "job $job_id is not the current ranked candidate $pending_job"
    candidate_dir=$(jq -er '.candidate_directory' <<<"$pending")

    result=$(review_json "$epoch" "$decision" --job-id="$job_id" \
        --reviewer="$reviewer_id" --reason="$reason" --evidence="$evidence")
    status=$(jq -er '.state.status' <<<"$result")
    write_evidence "epoch-$epoch-$decision-$job_id.json" "$result"
    if [[ $decision == approve ]]; then
        [[ $status == finalized ]] || fail "approval did not finalize epoch $epoch"
        "$(sim_binary)" promote --epoch="$epoch" --winner="$candidate_dir" \
            --winner-job-id="$job_id" --source-config="$source_config" \
            --repos-root="$workspace_root"
    elif [[ $status == finalized ]]; then
        "$(sim_binary)" promote --epoch="$epoch" --no-winner \
            --source-config="$source_config" --repos-root="$workspace_root"
	elif [[ $status == pending_review ]]; then
		remove_candidate_directory "$epoch" "$candidate_dir"
		printf '%s\n' "$result"
		printf '\nEpoch %d advanced to the next candidate; run candidate --epoch %d to materialize it.\n' \
			"$epoch" "$epoch" >&2
		return 20
    else
        fail "rejection returned unexpected review state: $status"
    fi
    remove_candidate_directory "$epoch" "$candidate_dir"
    run_season
}

status_command() {
    local epoch=${1:-}
    local info
    info=$(api_request GET /competition/info)
    printf '%s\n' "$info" | jq .
    if [[ -n $epoch ]]; then
        review_json "$epoch" next | jq .
    fi
}

require_command curl
require_command date
require_command flock
require_command git
require_command go
require_command jq
require_command make
require_command stat
[[ -f $source_config ]] || fail "source config is absent: $source_config"
mkdir -p "$state_dir"
chmod 0700 "$state_dir"
exec 9>"$state_dir/RUN-MAIN.lock"
flock -n 9 || fail "another RUN-MAIN process holds $state_dir/RUN-MAIN.lock"
umask 077
main_environment

command=${1:-}
shift || true
case $command in
    run)
        [[ $# == 0 ]] || fail "run takes no arguments"
        [[ -n $reviewer_id ]] || fail "SIM_LATENCY_REVIEWER_ID is required"
        run_season
        ;;
    status)
        epoch=
        if [[ ${1:-} == --epoch ]]; then
            epoch=${2:-}
            shift 2
        fi
        [[ $# == 0 ]] || fail "status accepts only --epoch N"
        status_command "$epoch"
        ;;
    candidate)
        parse_review_arguments "$@"
        [[ $epoch =~ ^[1-6]$ ]] || fail "--epoch must be in 1..6"
        [[ -z $job_id && -z $evidence && -z $reason ]] || fail "candidate accepts only --epoch N"
        review_json "$epoch" next
        ;;
    approve|reject)
        decide_candidate "$command" "$@"
        ;;
    -h|--help|help)
        usage
        ;;
    *)
        usage >&2
        exit 2
        ;;
esac
