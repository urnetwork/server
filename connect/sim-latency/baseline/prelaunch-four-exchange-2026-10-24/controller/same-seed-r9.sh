#!/bin/bash
# Fresh-service public-seed calibration, not an official hidden-round baseline.
# Never discard a failed replicate or silently replace it with another draw.
set -Eeuo pipefail
umask 077
readonly root=/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5
readonly prepared=/home/by/urnetwork/temp/production-20261024.bUpBTuEw
readonly controller="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly case_script="$controller/run-case.sh"
readonly measurement_script="$controller/measure.sh"
test "$(stat -c %u "$case_script")" = 0 || exit 90
test "$(stat -c %u "$measurement_script")" = 0 || exit 90
readonly case_sha="$(sha256sum "$case_script" | awk '{print $1}')"
readonly measurement_sha="$(sha256sum "$measurement_script" | awk '{print $1}')"
readonly commit=67c87445abe6725b25b3df2a13af7eb99cd4f8ed
readonly providers_sha=c7ce283ff9dabd90057fad2d9867d04ec454ef23f857002d1311c54de5f47924
if pgrep -f '^bash /home/by/urnetwork/temp/production-frontier-20261024[.]63AtMXS5/recover-missing-points[.]sh$' >/dev/null; then
  printf 'frontier recovery is active; calibration refuses concurrent scoring\n' >&2
  exit 90
fi
readonly output="$root/same-seed-r9"
mkdir -m700 "$output" "$output/runs"
readonly boot_id="$(< /proc/sys/kernel/random/boot_id)"
paths=""
for replicate in 1 2 3 4 5 6 7 8 9; do
  test "$(< /proc/sys/kernel/random/boot_id)" = "$boot_id" || exit 90
  attempt="same-seed-$replicate"
  point="$root/p1800-on-$attempt"
  test "$(sha256sum "$case_script" | awk '{print $1}')" = "$case_sha" || exit 90
  test "$(sha256sum "$measurement_script" | awk '{print $1}')" = "$measurement_sha" || exit 90
  printf 'starting fresh-service calibration replicate %s/9\n' "$replicate"
  start="$(date +%s)"
  if FRONTIER_MEASURE_SCRIPT="$measurement_script" timeout --signal=TERM --kill-after=45s 40m bash "$case_script" 1800 on "$attempt" > "$root/p1800-on-$attempt.log" 2>&1; then
    result=0
  else
    result=$?
  fi
  finish="$(date +%s)"
  test "$(sha256sum "$case_script" | awk '{print $1}')" = "$case_sha" || exit 90
  test "$(sha256sum "$measurement_script" | awk '{print $1}')" = "$measurement_sha" || exit 90
  printf '{"replicate":%s,"exit_code":%s,"elapsed_seconds":%s,"official":false}\n' \
    "$replicate" "$result" "$((finish-start))" >> "$output/progress.jsonl"
  remaining="$(sudo -n docker ps -a --filter name=urnetwork-frontier-63atmxs5 --format '{{.ID}}')"
  test -z "$remaining" || { printf 'calibration cleanup is incomplete\n' >&2; exit 90; }
  if test "$result" != 0; then
    printf 'calibration stops at failed replicate %s; do not resample or aggregate an incomplete set\n' "$replicate" >&2
    exit "$result"
  fi
  test ! -e "$point/diagnostic-quit.json" || exit 90
  sudo -n jq -e --arg revision "$commit" --arg config_sha "$providers_sha" \
    '.completion_state == "complete" and (.incomplete_code // "") == "" and
     .build_revision == $revision and (.build_modified // false) == false and
     .config_sha256 == $config_sha and .num_cpu == 10 and
     .clients_pool == 200 and .clients_established == 200 and .rows_in_window > 0 and
     .flags.hosts == "4" and .flags.fleet_shards == "4" and
     .flags.impair == "true" and .flags.duration == "3m0s" and .flags.reset == "true"' \
    "$point/output/run.json" >/dev/null
  actual_csv="$(sudo -n sha256sum "$point/output/results.csv" | awk '{print $1}')"
  expected_csv="$(sudo -n jq -er .results_csv_sha256 "$point/output/run.json")"
  test "$actual_csv" = "$expected_csv" || exit 90
  test "$(sudo -n stat -c %s "$point/output/results.csv")" = "$(sudo -n jq -er .results_csv_bytes "$point/output/run.json")" || exit 90
  test "$(sha256sum "$point/input/providers.yml" | awk '{print $1}')" = "$providers_sha" || exit 90
  test "$(sha256sum "$point/input/measure.sh" | awk '{print $1}')" = "$measurement_sha" || exit 90
  sudo -n install -m0400 -o by -g by "$point/output/run.json" "$output/runs/$replicate.run.json"
  sudo -n install -m0400 -o by -g by "$point/output/results.csv" "$output/runs/$replicate.csv"
  paths="${paths:+$paths,}$output/runs/$replicate.run.json"
  printf 'completed calibration replicate %s/9 in %ss\n' "$replicate" "$((finish-start))"
done
# This is legacy descriptive noise-floor analysis, not the authority-bearing
# score-baseline contract. Official failure-charged scoring is analyzed separately.
"$prepared/sim-latency-retirement" baseline --epoch=0 --runs="$paths" --out="$output/descriptive-baseline.json" \
  > "$output/descriptive-analysis.log" 2>&1
printf 'nine fresh-service replicates complete; descriptive analysis retained, official baseline remains unfrozen\n'
