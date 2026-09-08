#!/usr/bin/env bash
# Production acceptance for the deployed proxy control plane and the SOCKS5,
# HTTP CONNECT, and WireGuard data paths implemented in server/proxy.
#
# Usage:
#   ./proxy/test-main.sh
#   ./proxy/test-main.sh --repeat=5
#   ./proxy/test-main.sh --soak-duration=10m --soak-interval=5s
#   ./proxy/test-main.sh --overlap-protocols=false
#   ./proxy/test-main.sh --skip-build
#
# Environment:
#   UR_ACCEPT_VAULT=<path>              default: vault/main/tests.yml
#   UR_ACCEPT_RESULT_FILE=<path>        strict TSV result destination
#   UR_ACCEPT_API_URL=<url>             default: https://api.bringyour.com
#   UR_ACCEPT_PROXY_TARGET_URL=<url>    default: https://connectivitycheck.gstatic.com/generate_204
#   UR_ACCEPT_PROXY_BIN=<path>          cached local runner binary
#   UR_ACCEPT_PROXY_SOAK_DURATION=<dur> default: 5m per protocol
#   UR_ACCEPT_PROXY_SOAK_INTERVAL=<dur> default: 5s between sustained requests
#   UR_ACCEPT_PROXY_OVERLAP_PROTOCOLS=<bool> default: true
#   UR_ACCEPT_PROXY_TIMEOUT=<dur>       default: 2h for the complete runner
set -Eeuo pipefail
umask 077

here="$(cd "$(dirname "$0")" && pwd)"
server_root="$(dirname "$here")"
root="${URNETWORK_ROOT:-$(dirname "$server_root")}"
vault="${UR_ACCEPT_VAULT:-$root/vault/main/tests.yml}"
result_file="${UR_ACCEPT_RESULT_FILE:-}"
repeat_count="${UR_ACCEPT_REPEAT:-1}"
skip_build="${SKIP_BUILD:-0}"
api_url="${UR_ACCEPT_API_URL:-https://api.bringyour.com}"
target_url="${UR_ACCEPT_PROXY_TARGET_URL:-https://connectivitycheck.gstatic.com/generate_204}"
binary="${UR_ACCEPT_PROXY_BIN:-$server_root/temp/acceptance/proxy-main}"
soak_duration="${UR_ACCEPT_PROXY_SOAK_DURATION:-5m}"
soak_interval="${UR_ACCEPT_PROXY_SOAK_INTERVAL:-5s}"
overlap_protocols="${UR_ACCEPT_PROXY_OVERLAP_PROTOCOLS:-true}"
runner_timeout="${UR_ACCEPT_PROXY_TIMEOUT:-2h}"

usage() {
  sed -n '2,/^set -/s/^# \{0,1\}//p' "$0"
}

for arg in "$@"; do
  case "$arg" in
    --repeat=*) repeat_count="${arg#*=}" ;;
    --soak-duration=*) soak_duration="${arg#*=}" ;;
    --soak-interval=*) soak_interval="${arg#*=}" ;;
    --overlap-protocols=*) overlap_protocols="${arg#*=}" ;;
    --skip-build) skip_build=1 ;;
    --headless|--keep-fixture) ;; # accepted for root-runner parity
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $arg (see --help)" >&2; exit 2 ;;
  esac
done
case "$repeat_count" in
  ''|*[!0-9]*) echo "--repeat must be a positive integer" >&2; exit 2 ;;
  0) echo "--repeat must be at least 1" >&2; exit 2 ;;
esac
case "$overlap_protocols" in
  true|false) ;;
  *) echo "--overlap-protocols must be true or false" >&2; exit 2 ;;
esac

network_test_gate="$root/tests/network-intensive-suite-lock.sh"
if [[ ! -x "$network_test_gate" ]]; then
  echo "proxy acceptance suite gate is missing or not executable: $network_test_gate" >&2
  exit 127
fi
if [[ "${URNETWORK_NETWORK_TEST_LOCK_HELD:-}" != 1 ]]; then
  exec "$network_test_gate" main-acceptance proxy-acceptance-main -- "$here/test-main.sh" "$@"
fi
if ! "$network_test_gate" --verify-held main-acceptance; then
  echo "proxy acceptance suite inherited an invalid network-intensive lock" >&2
  exit 70
fi

for command_name in go mkfifo timeout tee; do
  command -v "$command_name" >/dev/null 2>&1 || {
    echo "[proxy acceptance] missing prerequisite: $command_name" >&2
    exit 1
  }
done
timeout "$runner_timeout" true >/dev/null 2>&1 || {
  echo "[proxy acceptance] invalid UR_ACCEPT_PROXY_TIMEOUT: $runner_timeout" >&2
  exit 2
}
[ -f "$vault" ] || {
  echo "[proxy acceptance] acceptance config is missing: $vault" >&2
  exit 1
}
config_reader="$root/tests/read-tests-config.sh"
[ -x "$config_reader" ] || {
  echo "[proxy acceptance] config reader is missing: $config_reader" >&2
  exit 1
}
UR_ACCEPT_VAULT="$vault" "$config_reader" --ready validate

timestamp="$(date +%Y%m%d-%H%M%S)"
artifacts="$server_root/temp/acceptance/proxy/$timestamp-$$"
run_dir="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-proxy-acceptance.XXXXXX")"
mkdir -p "$artifacts" "$(dirname "$binary")"
chmod 700 "$artifacts" "$run_dir"
credentials="$run_dir/credentials"
runner_output_pipe="$run_dir/runner-output"
runner_pid=""
runner_active=0
managed_run=0
pending_signal=""
signal_status=0
cleanup() {
  exit_status=$?
  rm -f "$credentials" "$runner_output_pipe"
  rmdir "$run_dir" 2>/dev/null || true
  exit "$exit_status"
}
forward_signal() {
  local signal_name="$1"
  signal_status=130
  if [ "$managed_run" -ne 1 ]; then
    exit "$signal_status"
  fi
  if [ "$runner_active" -eq 1 ]; then
    kill "-$signal_name" "$runner_pid" 2>/dev/null || true
  elif [ -z "$pending_signal" ]; then
    pending_signal="$signal_name"
  fi
}
wait_for_process() {
  local process_pid="$1"
  local process_status
  while true; do
    wait "$process_pid"
    process_status=$?
    if ! kill -0 "$process_pid" 2>/dev/null; then
      return "$process_status"
    fi
  done
}
trap cleanup EXIT
trap 'forward_signal INT' INT
trap 'forward_signal TERM' TERM

acc_user="$(UR_ACCEPT_VAULT="$vault" "$config_reader" get data_plane_account.email)"
acc_pass="$(UR_ACCEPT_VAULT="$vault" "$config_reader" get data_plane_account.password)"
printf '%s\n%s\n' "$acc_user" "$acc_pass" >"$credentials"
chmod 600 "$credentials"
unset acc_user acc_pass

if [ "$skip_build" -ne 1 ]; then
  echo "[proxy acceptance] building the local proxy acceptance runner"
  (cd "$server_root" && timeout 600 go build -trimpath -o "$binary" ./proxy/cmd/acceptance-main)
elif [ ! -x "$binary" ]; then
  echo "[proxy acceptance] --skip-build requested but cached runner is missing: $binary" >&2
  exit 1
else
  echo "[proxy acceptance] reusing $binary"
fi

if [ -z "$result_file" ]; then
  result_file="$artifacts/results.tsv"
fi
echo "[proxy acceptance] running $repeat_count complete repetition(s) against main"
echo "[proxy acceptance] sustained campaign: $soak_duration per protocol at $soak_interval intervals"
mkfifo "$runner_output_pipe"
set +e
# Keep the runner in this foreground process group. The root suite applies its
# own deadline around this script; without --foreground, terminating the outer
# shell can orphan this timeout and its child while both tee processes keep the
# root pipeline open.
managed_run=1
(
  # A terminal interrupt also targets the foreground log consumer. Keep it
  # alive until the canceled runner has emitted cleanup and result diagnostics.
  trap '' INT TERM
  tee "$artifacts/run.log" <"$runner_output_pipe"
) &
tee_pid=$!
timeout --foreground --signal=TERM --kill-after=60s "$runner_timeout" \
  "$binary" \
    --credentials="$credentials" \
    --result-file="$result_file" \
    --api="$api_url" \
    --target="$target_url" \
    --repeat="$repeat_count" \
    --soak-duration="$soak_duration" \
    --soak-interval="$soak_interval" \
    --overlap-protocols="$overlap_protocols" \
  >"$runner_output_pipe" 2>&1 &
runner_pid=$!
runner_active=1
if [ -n "$pending_signal" ]; then
  kill "-$pending_signal" "$runner_pid" 2>/dev/null || true
  pending_signal=""
fi
runner_status=0
wait_for_process "$runner_pid" || runner_status=$?
runner_active=0
logger_status=0
wait_for_process "$tee_pid" || logger_status=$?
runner_pid=""
managed_run=0
status="$runner_status"
if [ "$logger_status" -ne 0 ]; then
  echo "[proxy acceptance] output logger failed with status $logger_status" >&2
  if [ "$status" -eq 0 ]; then
    status="$logger_status"
  fi
fi
if [ "$signal_status" -ne 0 ]; then
  status="$signal_status"
fi
set -e

echo "[proxy acceptance] artifacts: $artifacts"
exit "$status"
