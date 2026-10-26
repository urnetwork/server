#!/usr/bin/env bash
# One authenticated native reduction; original artifacts remain read-only.
set -Eeuo pipefail
umask 077
[[ $# == 3 && $(id -u) == 0 ]] || exit 2
readonly binary=$1 expected_binary_sha256=$2 expected_inputs_sha256=$3
readonly output_root=/home/by/urnetwork/temp/three-a-reduction-20261026.sZqbiy1S
readonly campaign=/home/by/urnetwork/temp/canceled-stop-calibration-20261025.0G7LKkuh
readonly approval=/usr/local/libexec/urnetwork/competition-calibration-3a-2f46d3cc6/approval.md
readonly plan=/usr/local/libexec/urnetwork/competition-calibration-3a-2f46d3cc6/original-plan.json
[[ $expected_binary_sha256 =~ ^[0-9a-f]{64}$ && $expected_inputs_sha256 =~ ^[0-9a-f]{64}$ ]]
[[ -f $binary && -x $binary && ! -L $binary ]]
[[ $(sha256sum "$binary" | cut -d ' ' -f1) == "$expected_binary_sha256" ]]
[[ $(sha256sum "$campaign/analysis-inputs-three-a-v1.json" | cut -d ' ' -f1) == "$expected_inputs_sha256" ]]
[[ $(awk '/Cpus_allowed_list/ {print $2}' /proc/self/status) == 20,22 ]]
set -o noclobber
printf 'binary_sha256=%s\ninputs_sha256=%s\nstarted_at=%s\n' "$expected_binary_sha256" "$expected_inputs_sha256" "$(date -u +%FT%TZ)" > "$output_root/started"
set +e
timeout --signal=TERM --kill-after=30s 1200s "$binary" calibrate \
  --calibration-inputs="$campaign/analysis-inputs-three-a-v1.json" \
  --calibration-inputs-sha256="$expected_inputs_sha256" \
  --calibration-lock=/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock \
  --calibration-cpuset=20,22 \
  --calibration-protocol=launch-3a-bridge-3b-2026-10-26-v1 \
  --calibration-approval="$approval" \
  --calibration-approval-sha256=a794618968d840163952b7b2058100fd87b71ef7ccc44919ba590c5a253f643c \
  --calibration-original-plan="$plan" \
  --calibration-original-plan-sha256=24f9b6ea9fe6fbfb4381ca29535b81a1cd4966b397c701b04f5b09ac74eb11d8 \
  > "$output_root/report.json" 2> "$output_root/analysis.stderr"
status=$?
set -e
printf 'exit=%s\nended_at=%s\n' "$status" "$(date -u +%FT%TZ)" > "$output_root/terminal"
sha256sum "$output_root/report.json" "$output_root/analysis.stderr" "$output_root/started" "$output_root/terminal" > "$output_root/outputs.sha256"
exit "$status"
