#!/bin/bash
unit=competition-distributed-recovery-independent-qa-20261112.service
set -euo pipefail
umask 077
export LC_ALL=C
: "${QA_EVIDENCE:?}"
measurement_attempts=3
root_minimum_bytes=35433480192
cache_growth_limit_bytes=2147483648
temporary_limit_bytes=1342177280
poll_seconds=2
command_timeout_seconds=5
command_kill_after_seconds=1
cache_path=/home/by/.cache/go-build
owner_invocation=
owner_pid=

# Parse a closed native manager projection; absent/duplicate/unknown fields
# cannot grant signal authority. The chain joins this guard before unit reuse.
read_owner() {
  local output line key value
  local -A fields=()
  if ! output=$(/usr/bin/timeout --kill-after="${command_kill_after_seconds}s" "${command_timeout_seconds}s" systemctl show "$unit" -p ActiveState -p InvocationID -p MainPID -p LoadState --all 2>/dev/null); then
    return 1
  fi
  while IFS= read -r line; do
    [[ $line == *=* ]] || return 1
    key=${line%%=*}; value=${line#*=}
    case "$key" in ActiveState|InvocationID|MainPID|LoadState) ;; *) return 1 ;; esac
    [[ ! -v fields[$key] ]] || return 1
    fields[$key]=$value
  done <<< "$output"
  ((${#fields[@]}==4)) || return 1
  current_state=${fields[ActiveState]}
  current_invocation=${fields[InvocationID]}
  current_pid=${fields[MainPID]}
  current_load=${fields[LoadState]}
  [[ $current_pid =~ ^(0|[1-9][0-9]{0,9})$ ]] || return 1
}

# A completed actor needs no signal. A live successor is never this owner.
check_owner() {
  read_owner || return 2
  if [[ $current_state =~ ^(inactive|failed)$ && $current_pid == 0 ]]; then
    [[ $current_load =~ ^(loaded|not-found)$ ]] || return 2
    [[ -z $current_invocation || $current_invocation == "$owner_invocation" ]] || return 2
    return 1
  fi
  [[ $current_load == loaded && $current_state =~ ^(active|activating)$ &&
     $current_invocation == "$owner_invocation" && $current_pid == "$owner_pid" ]] || return 2
}

# Reobserve both invocation and main pid immediately before the exact main-only
# signal. The sole chain launcher cannot reuse this unit until guard join.
stop_owner() {
  local reason=$1 status=0
  check_owner || status=$?
  if ((status==1)); then
    printf 'GUARD_OWNER_TERMINAL reason=%s\n' "$reason" >> "$QA_EVIDENCE/guard.log"
    return 0
  fi
  if ((status!=0)); then
    printf 'GUARD_REFUSE_SIGNAL reason=%s owner_changed_or_unknown=1\n' "$reason" >> "$QA_EVIDENCE/guard.log"
    return 1
  fi
  printf 'GUARD_STOP reason=%s exact_unit=%s invocation=%s main_pid=%s\n' "$reason" "$unit" "$owner_invocation" "$owner_pid" >> "$QA_EVIDENCE/guard.log"
  /usr/bin/timeout --kill-after="${command_kill_after_seconds}s" "${command_timeout_seconds}s" systemctl kill --kill-who=main --signal=TERM "$unit"
}

# A canceled guard cannot leave its admitted actor knowingly unmonitored.
cancel_guard() {
  trap - TERM INT
  stop_owner guard_cancelled || true
  exit 130
}
trap cancel_guard TERM INT

# Canonical decimal values must fit signed shell arithmetic. No blank,
# partial, leading-zero, negative or overflowed output becomes a measurement.
valid_number() {
  [[ $1 =~ ^(0|[1-9][0-9]*)$ ]] || return 1
  ((${#1}<19)) || { ((${#1}==19)) && [[ $1 < 9223372036854775808 ]]; }
}

# Every attempt is a new command and requires full exit-zero acknowledgement.
# Failed du often prints a plausible partial total; those bytes are discarded.
# Status10 means the owned actor ended; status11 means custody became unknown.
measure() {
  local kind=$1 path=$2 attempt output status number owner_status
  for ((attempt=1;attempt<=measurement_attempts;attempt++)); do
    owner_status=0
    check_owner || owner_status=$?
    ((owner_status!=1)) || return 10
    ((owner_status==0)) || return 11
    status=0
    if [[ $kind == du ]]; then
      output=$(/usr/bin/timeout --kill-after="${command_kill_after_seconds}s" "${command_timeout_seconds}s" du -s -B1 -- "$path" 2>/dev/null) || status=$?
      if ((status==0)) && [[ $output == *$'\t'* ]]; then
        number=${output%%$'\t'*}
        if valid_number "$number" && [[ $output == "$number"$'\t'"$path" ]]; then
          printf '%s\n' "$number"
          return 0
        fi
      fi
    else
      output=$(/usr/bin/timeout --kill-after="${command_kill_after_seconds}s" "${command_timeout_seconds}s" df -B1 --output=avail / 2>/dev/null) || status=$?
      if ((status==0)) && [[ $output =~ ^[[:space:]]*Avail[[:space:]]+([0-9]+)[[:space:]]*$ ]]; then
        number=${BASH_REMATCH[1]}
        if valid_number "$number"; then
          printf '%s\n' "$number"
          return 0
        fi
      fi
    fi
    printf 'GUARD_MEASUREMENT_RETRY kind=%s attempt=%s command_status=%s\n' "$kind" "$attempt" "$status" >> "$QA_EVIDENCE/guard.log"
  done
  return 12
}

# All measurement failures stop qualification and preserve its original error.
measurement_failed() {
  local status=$1
  if ((status==10)); then
    exit 0
  fi
  stop_owner measurement_failed || true
  exit 2
}

if ! read_owner; then
  printf 'GUARD_REFUSE_SIGNAL reason=initial_owner_unknown\n' >> "$QA_EVIDENCE/guard.log"
  exit 3
fi
if [[ $current_state =~ ^(inactive|failed)$ && $current_pid == 0 && $current_load =~ ^(loaded|not-found)$ ]]; then
  exit 0
fi
if [[ $current_load != loaded || ! $current_state =~ ^(active|activating)$ ||
      ! $current_invocation =~ ^[0-9a-f]{32}$ || ! $current_pid =~ ^[1-9][0-9]{0,9}$ ]]; then
  printf 'GUARD_REFUSE_SIGNAL reason=initial_owner_not_live\n' >> "$QA_EVIDENCE/guard.log"
  exit 3
fi
owner_invocation=$current_invocation
owner_pid=$current_pid
initial_cache=$(measure du "$cache_path") || measurement_failed $?
while true; do
  owner_status=0
  check_owner || owner_status=$?
  ((owner_status!=1)) || exit 0
  if ((owner_status!=0)); then
    printf 'GUARD_REFUSE_SIGNAL reason=owner_changed_or_unknown\n' >> "$QA_EVIDENCE/guard.log"
    exit 3
  fi
  available=$(measure df /) || measurement_failed $?
  cache=$(measure du "$cache_path") || measurement_failed $?
  temporary=0
  if [[ -d $QA_EVIDENCE/tmp && ! -L $QA_EVIDENCE/tmp ]]; then
    temporary=$(measure du "$QA_EVIDENCE/tmp") || measurement_failed $?
  elif [[ -e $QA_EVIDENCE/tmp || -L $QA_EVIDENCE/tmp ]]; then
    stop_owner temporary_path_uncertain || true
    exit 2
  fi
  printf 'utc=%s root_free=%s cache_growth=%s tmp_bytes=%s\n' "$(date -u +%FT%TZ)" "$available" "$((cache-initial_cache))" "$temporary" >> "$QA_EVIDENCE/guard.log"
  if ((available < root_minimum_bytes || cache-initial_cache >= cache_growth_limit_bytes || temporary >= temporary_limit_bytes)); then
    stop_owner resource_limit || true
    exit 2
  fi
  sleep "$poll_seconds"
done
