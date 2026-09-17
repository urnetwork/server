#!/usr/bin/env bash

# Reversible local-hostname and launcher ownership helpers for run-local.sh.
# All file transformations operate on explicit paths so unit tests never need
# privileged access to /etc/hosts.

local_state_error() {
  printf 'run-local state: %s\n' "$*" >&2
}

# Prints a hosts file without the one complete block owned by this launcher.
# Zero, multiple, incomplete, or mismatched blocks are ambiguous and fail closed.
local_hosts_strip_managed_blocks() {
  local source_path="$1"
  local marker_begin="$2"
  local marker_end="$3"
  awk -v marker_begin="$marker_begin" -v marker_end="$marker_end" '
    $0 == marker_begin {
      begin_count++
      if (in_block == 1 || begin_count != 1) {
        malformed = 1
        exit 2
      }
      in_block = 1
      next
    }
    $0 == marker_end {
      end_count++
      if (in_block != 1 || end_count != 1) {
        malformed = 1
        exit 2
      }
      in_block = 0
      next
    }
    in_block != 1 { print }
    END {
      if (malformed == 1 || in_block == 1 || begin_count != 1 || end_count != 1) {
        exit 2
      }
    }
  ' "$source_path"
}

# Refuses every active unmanaged spelling of the two service aliases. Changing
# an operator-owned mapping implicitly could hide a production tunnel; the
# launcher instead stops before mutation and leaves remediation explicit.
local_hosts_require_unowned() {
  local source_path="$1"
  local postgres_host="$2"
  local redis_host="$3"
  local marker_begin="$4"
  local marker_end="$5"
  if awk -v postgres_host="$postgres_host" \
      -v redis_host="$redis_host" \
      -v marker_begin="$marker_begin" \
      -v marker_end="$marker_end" '
    function canonical(host) {
      host = tolower(host)
      sub(/[.]$/, "", host)
      return host
    }
    $0 == marker_begin || $0 == marker_end { found = 1 }
    {
      data = $0
      sub(/#.*/, "", data)
      field_count = split(data, fields, /[[:space:]]+/)
      address_seen = 0
      for (i = 1; i <= field_count; i++) {
        if (fields[i] == "") {
          continue
        }
        if (address_seen == 0) {
          address_seen = 1
          continue
        }
        host = canonical(fields[i])
        if (host == canonical(postgres_host) || host == canonical(redis_host)) {
          found = 1
        }
      }
    }
    END { exit found == 1 ? 0 : 1 }
  ' "$source_path"; then
    local_state_error "hosts file already contains a managed block or local service alias"
    return 1
  else
    local awk_status=$?
    if [[ "$awk_status" == 1 ]]; then
      return 0
    fi
    local_state_error "could not inspect hosts ownership"
    return "$awk_status"
  fi
}

# Prints the unique dedicated mappings after an ownership preflight has proved
# that the input contains no managed or unmanaged copy of either name.
local_hosts_render_applied() {
  local source_path="$1"
  local hosts_ip="$2"
  local postgres_host="$3"
  local redis_host="$4"
  local marker_begin="$5"
  local marker_end="$6"

  awk '{ print }' "$source_path" || return $?
  printf '%s\n' "$marker_begin"
  printf '%s\t%s\n' "$hosts_ip" "$postgres_host"
  printf '%s\t%s\n' "$hosts_ip" "$redis_host"
  printf '%s\n' "$marker_end"
}

# Verifies that both active names occur exactly once and only at the dedicated
# address before a generated file can replace the system resolver input.
local_hosts_validate_applied() {
  local source_path="$1"
  local hosts_ip="$2"
  local postgres_host="$3"
  local redis_host="$4"
  local marker_begin="$5"
  local marker_end="$6"
  awk -v hosts_ip="$hosts_ip" \
      -v postgres_host="$postgres_host" \
      -v redis_host="$redis_host" \
      -v marker_begin="$marker_begin" \
      -v marker_end="$marker_end" '
    function canonical(host) {
      host = tolower(host)
      sub(/[.]$/, "", host)
      return host
    }
    $0 == marker_begin {
      begin_count++
      if (in_block == 1 || begin_count != 1) {
        malformed = 1
      }
      in_block = 1
      next
    }
    $0 == marker_end {
      end_count++
      if (in_block != 1 || end_count != 1) {
        malformed = 1
      }
      in_block = 0
      next
    }
    {
      data = $0
      sub(/#.*/, "", data)
      field_count = split(data, fields, /[[:space:]]+/)
      address = ""
      for (i = 1; i <= field_count; i++) {
        if (fields[i] == "") {
          continue
        }
        if (address == "") {
          address = fields[i]
          continue
        }
        host = canonical(fields[i])
        if (host == canonical(postgres_host)) {
          postgres_count++
          if (address != hosts_ip || in_block != 1) {
            bad_address = 1
          }
        }
        if (host == canonical(redis_host)) {
          redis_count++
          if (address != hosts_ip || in_block != 1) {
            bad_address = 1
          }
        }
      }
    }
    END {
      if (malformed == 1 || in_block == 1 ||
          begin_count != 1 || end_count != 1 ||
          postgres_count != 1 || redis_count != 1 || bad_address == 1) {
        exit 2
      }
    }
  ' "$source_path"
}

# Replaced by run-local.sh with its narrow sudo copy. Tests intentionally keep
# this unprivileged default and operate only on temporary files.
local_hosts_replace_file() {
  cp "$1" "$2"
}

# Snapshots the unowned original, generates the unique managed state, and
# refuses to overwrite an input changed while the transaction was prepared.
local_hosts_install() {
  local hosts_file="$1"
  local backup_path="$2"
  local applied_path="$3"
  local hosts_ip="$4"
  local postgres_host="$5"
  local redis_host="$6"
  local marker_begin="$7"
  local marker_end="$8"
  local observed_path

  LOCAL_HOSTS_FILE_MUTATED=0
  observed_path="$(mktemp -t urnetwork-hosts-observed.XXXXXX)" || return $?
  cp "$hosts_file" "$observed_path" || { rm -f "$observed_path"; return 1; }
  if ! local_hosts_require_unowned \
      "$observed_path" "$postgres_host" "$redis_host" "$marker_begin" "$marker_end"; then
    rm -f "$observed_path"
    return 1
  fi
  cp "$observed_path" "$backup_path" || { rm -f "$observed_path"; return 1; }
  if [[ ! -s "$backup_path" ]]; then
    local_state_error "refusing to replace an empty hosts file"
    rm -f "$observed_path"
    return 1
  fi
  if ! local_hosts_render_applied \
      "$backup_path" "$hosts_ip" "$postgres_host" "$redis_host" "$marker_begin" "$marker_end" \
      > "$applied_path"; then
    rm -f "$observed_path"
    return 1
  fi
  if ! local_hosts_validate_applied \
      "$applied_path" "$hosts_ip" "$postgres_host" "$redis_host" "$marker_begin" "$marker_end"; then
    local_state_error "generated hosts file does not uniquely select the dedicated address"
    rm -f "$observed_path"
    return 1
  fi
  if ! cmp -s "$hosts_file" "$observed_path"; then
    local_state_error "hosts file changed while managed mappings were prepared"
    rm -f "$observed_path"
    return 1
  fi
  if ! local_hosts_replace_file "$applied_path" "$hosts_file"; then
    rm -f "$observed_path"
    return 1
  fi
  LOCAL_HOSTS_FILE_MUTATED=1
  rm -f "$observed_path"
}

# Restores byte-for-byte only while the applied state is still owned. If an
# external edit occurred, remove only our marked block, preserve that edit, and
# report a non-exact restore so the caller retains the original backup.
local_hosts_restore() {
  local hosts_file="$1"
  local backup_path="$2"
  local applied_path="$3"
  local marker_begin="$4"
  local marker_end="$5"
  local observed_path
  local restored_path

  LOCAL_HOSTS_RESTORE_EXACT=0
  observed_path="$(mktemp -t urnetwork-hosts-observed.XXXXXX)" || return $?
  restored_path="$(mktemp -t urnetwork-hosts-restored.XXXXXX)" || {
    rm -f "$observed_path"
    return 1
  }
  cp "$hosts_file" "$observed_path" || {
    rm -f "$observed_path" "$restored_path"
    return 1
  }

  if cmp -s "$observed_path" "$applied_path"; then
    cp "$backup_path" "$restored_path" || {
      rm -f "$observed_path" "$restored_path"
      return 1
    }
    LOCAL_HOSTS_RESTORE_EXACT=1
  elif ! local_hosts_strip_managed_blocks \
      "$observed_path" "$marker_begin" "$marker_end" > "$restored_path"; then
    local_state_error "hosts file does not contain exactly one owned managed block; leaving it untouched"
    rm -f "$observed_path" "$restored_path"
    return 1
  fi

  if [[ ! -s "$restored_path" ]]; then
    local_state_error "refusing to restore an empty hosts file"
    rm -f "$observed_path" "$restored_path"
    return 1
  fi
  if ! cmp -s "$hosts_file" "$observed_path"; then
    local_state_error "hosts file changed again while restore was prepared"
    rm -f "$observed_path" "$restored_path"
    return 1
  fi
  if ! cmp -s "$hosts_file" "$restored_path" &&
      ! local_hosts_replace_file "$restored_path" "$hosts_file"; then
    rm -f "$observed_path" "$restored_path"
    return 1
  fi
  if ! cmp -s "$hosts_file" "$restored_path"; then
    local_state_error "hosts file changed while restore was applied"
    rm -f "$observed_path" "$restored_path"
    return 1
  fi
  rm -f "$observed_path" "$restored_path"
}

# Uses atomic directory creation as a portable Darwin/Linux single-owner lock.
# Existing locks are never guessed stale: manual inspection is safer than two
# launchers independently owning destructive test-service aliases.
local_run_lock_acquire() {
  local lock_dir="$1"
  local owner_token="$2"
  if [[ -z "$owner_token" || "$owner_token" == *[[:space:]]* ]]; then
    local_state_error "refusing malformed local launcher ownership"
    return 1
  fi
  if ! (umask 077 && mkdir "$lock_dir") 2>/dev/null; then
    local_state_error "local launcher lock is already held: $lock_dir"
    return 1
  fi
  if ! printf '%s\n' "$owner_token" > "$lock_dir/owner"; then
    rmdir "$lock_dir" 2>/dev/null || true
    return 1
  fi
}

# Reads the opaque owner without treating a pid as a stale-lock heuristic.
local_run_lock_read_owner() {
  local lock_dir="$1"
  local line
  local lines=()
  local observed_owner
  if [[ ! -f "$lock_dir/owner" || ! -r "$lock_dir/owner" ]]; then
    local_state_error "local launcher lock has no readable owner: $lock_dir"
    return 1
  fi
  while IFS= read -r line || [[ -n "$line" ]]; do
    lines+=("$line")
  done < "$lock_dir/owner"
  if [[ "${#lines[@]}" != 1 ]]; then
    local_state_error "local launcher lock has malformed ownership: $lock_dir"
    return 1
  fi
  observed_owner="${lines[0]}"
  if [[ -z "$observed_owner" || "$observed_owner" == *[[:space:]]* ]]; then
    local_state_error "local launcher lock has malformed ownership: $lock_dir"
    return 1
  fi
  LOCAL_RUN_LOCK_OWNER="$observed_owner"
}

# Confirms ownership immediately before a caller mutates launcher state.
local_run_lock_require_owner() {
  local lock_dir="$1"
  local owner_token="$2"
  local_run_lock_read_owner "$lock_dir" || return $?
  if [[ "$LOCAL_RUN_LOCK_OWNER" != "$owner_token" ]]; then
    local_state_error "local launcher lock ownership changed: $lock_dir"
    return 1
  fi
}

# Parses the fixed, non-executable readiness record into LOCAL_RUN_READY_*.
# Exact line count and field names make partial writes and format drift fail
# closed.
local_run_attestation_read() {
  local attestation_path="$1"
  local line
  local lines=()
  local value
  if [[ ! -f "$attestation_path" ]]; then
    local_state_error "local launcher readiness attestation is missing: $attestation_path"
    return 1
  fi
  while IFS= read -r line || [[ -n "$line" ]]; do
    lines+=("$line")
  done < "$attestation_path"
  if [[ "${#lines[@]}" != 7 ]] ||
      [[ "${lines[0]}" != "format=urnetwork-server-run-local-ready-v1" ]] ||
      [[ "${lines[1]}" != owner_token=* ]] ||
      [[ "${lines[2]}" != host_ip=* ]] ||
      [[ "${lines[3]}" != postgres_host=* ]] ||
      [[ "${lines[4]}" != postgres_port=* ]] ||
      [[ "${lines[5]}" != redis_host=* ]] ||
      [[ "${lines[6]}" != redis_port=* ]]; then
    local_state_error "local launcher readiness attestation is malformed: $attestation_path"
    return 1
  fi

  LOCAL_RUN_READY_OWNER_TOKEN="${lines[1]#owner_token=}"
  LOCAL_RUN_READY_HOST_IP="${lines[2]#host_ip=}"
  LOCAL_RUN_READY_POSTGRES_HOST="${lines[3]#postgres_host=}"
  LOCAL_RUN_READY_POSTGRES_PORT="${lines[4]#postgres_port=}"
  LOCAL_RUN_READY_REDIS_HOST="${lines[5]#redis_host=}"
  LOCAL_RUN_READY_REDIS_PORT="${lines[6]#redis_port=}"
  for value in \
      "$LOCAL_RUN_READY_OWNER_TOKEN" \
      "$LOCAL_RUN_READY_HOST_IP" \
      "$LOCAL_RUN_READY_POSTGRES_HOST" \
      "$LOCAL_RUN_READY_POSTGRES_PORT" \
      "$LOCAL_RUN_READY_REDIS_HOST" \
      "$LOCAL_RUN_READY_REDIS_PORT"; do
    if [[ -z "$value" || "$value" == *[[:space:]]* ]]; then
      local_state_error "local launcher readiness attestation has an invalid value: $attestation_path"
      return 1
    fi
  done
  if [[ ! "$LOCAL_RUN_READY_POSTGRES_PORT" =~ ^[0-9]+$ ]] ||
      [[ ! "$LOCAL_RUN_READY_REDIS_PORT" =~ ^[0-9]+$ ]]; then
    local_state_error "local launcher readiness attestation has an invalid port: $attestation_path"
    return 1
  fi
}

# Discards an unpublished record only while the surrounding lock still proves
# that the caller owns every path in its private state directory.
local_run_attestation_remove_pending() {
  local lock_dir="$1"
  local owner_token="$2"
  local pending_path="$lock_dir/ready.pending"

  local_run_lock_require_owner "$lock_dir" "$owner_token" || return $?
  if [[ -f "$pending_path" ]]; then
    rm "$pending_path" || return $?
  elif [[ -e "$pending_path" ]]; then
    local_state_error "local launcher pending readiness is not a regular file: $pending_path"
    return 1
  fi
}

# Publishes readiness with a same-directory rename only after the caller has
# completed container-health and host-reachability checks.
local_run_attestation_publish() {
  local lock_dir="$1"
  local owner_token="$2"
  local hosts_ip="$3"
  local postgres_host="$4"
  local postgres_port="$5"
  local redis_host="$6"
  local redis_port="$7"
  local attestation_path="$lock_dir/ready"
  local pending_path="$lock_dir/ready.pending"

  local_run_lock_require_owner "$lock_dir" "$owner_token" || return $?
  if [[ -e "$attestation_path" || -e "$pending_path" ]]; then
    local_state_error "local launcher readiness state already exists: $lock_dir"
    return 1
  fi
  if ! (umask 077 && {
    printf '%s\n' "format=urnetwork-server-run-local-ready-v1"
    printf 'owner_token=%s\n' "$owner_token"
    printf 'host_ip=%s\n' "$hosts_ip"
    printf 'postgres_host=%s\n' "$postgres_host"
    printf 'postgres_port=%s\n' "$postgres_port"
    printf 'redis_host=%s\n' "$redis_host"
    printf 'redis_port=%s\n' "$redis_port"
  } > "$pending_path"); then
    local_run_attestation_remove_pending "$lock_dir" "$owner_token" || true
    return 1
  fi
  if ! local_run_attestation_read "$pending_path" ||
      [[ "$LOCAL_RUN_READY_OWNER_TOKEN" != "$owner_token" ]] ||
      [[ "$LOCAL_RUN_READY_HOST_IP" != "$hosts_ip" ]] ||
      [[ "$LOCAL_RUN_READY_POSTGRES_HOST" != "$postgres_host" ]] ||
      [[ "$LOCAL_RUN_READY_POSTGRES_PORT" != "$postgres_port" ]] ||
      [[ "$LOCAL_RUN_READY_REDIS_HOST" != "$redis_host" ]] ||
      [[ "$LOCAL_RUN_READY_REDIS_PORT" != "$redis_port" ]]; then
    local_state_error "refusing to publish invalid local launcher readiness state"
    local_run_attestation_remove_pending "$lock_dir" "$owner_token" || true
    return 1
  fi
  if ! local_run_lock_require_owner "$lock_dir" "$owner_token"; then
    local_run_attestation_remove_pending "$lock_dir" "$owner_token" || true
    return 1
  fi
  if [[ -e "$attestation_path" ]] || ! mv "$pending_path" "$attestation_path"; then
    local_state_error "could not atomically publish local launcher readiness state"
    local_run_attestation_remove_pending "$lock_dir" "$owner_token" || true
    return 1
  fi
  local_run_lock_require_owner "$lock_dir" "$owner_token" || return $?
}

# Removes pending or published readiness only while both the lock and complete
# attestation still carry the caller's opaque owner token.
local_run_attestation_remove() {
  local lock_dir="$1"
  local owner_token="$2"
  local attestation_path="$lock_dir/ready"

  local_run_lock_require_owner "$lock_dir" "$owner_token" || return $?
  if [[ -e "$attestation_path" ]]; then
    local_run_attestation_read "$attestation_path" || return $?
    if [[ "$LOCAL_RUN_READY_OWNER_TOKEN" != "$owner_token" ]]; then
      local_state_error "local launcher readiness ownership changed: $attestation_path"
      return 1
    fi
    local_run_lock_require_owner "$lock_dir" "$owner_token" || return $?
    rm "$attestation_path" || return $?
  fi
  local_run_attestation_remove_pending "$lock_dir" "$owner_token" || return $?
}

# Validates a stable owner/attestation snapshot and the two unique aliases that
# the launcher owns before an integration harness is allowed to probe services.
local_run_attestation_validate() {
  local lock_dir="$1"
  local hosts_file="$2"
  local postgres_host="$3"
  local postgres_port="$4"
  local redis_host="$5"
  local redis_port="$6"
  local marker_begin="$7"
  local marker_end="$8"
  local owner_token
  local hosts_ip

  local_run_lock_read_owner "$lock_dir" || return $?
  owner_token="$LOCAL_RUN_LOCK_OWNER"
  local_run_attestation_read "$lock_dir/ready" || return $?
  hosts_ip="$LOCAL_RUN_READY_HOST_IP"
  if [[ "$LOCAL_RUN_READY_OWNER_TOKEN" != "$owner_token" ]]; then
    local_state_error "local launcher readiness owner does not match its lock: $lock_dir"
    return 1
  fi
  if [[ "$LOCAL_RUN_READY_POSTGRES_HOST" != "$postgres_host" ||
        "$LOCAL_RUN_READY_POSTGRES_PORT" != "$postgres_port" ||
        "$LOCAL_RUN_READY_REDIS_HOST" != "$redis_host" ||
        "$LOCAL_RUN_READY_REDIS_PORT" != "$redis_port" ]]; then
    local_state_error "local launcher readiness endpoints do not match the selected test resources"
    return 1
  fi
  if ! local_hosts_validate_applied \
      "$hosts_file" "$hosts_ip" "$postgres_host" "$redis_host" "$marker_begin" "$marker_end"; then
    local_state_error "local service aliases are not unique launcher-managed mappings to $hosts_ip"
    return 1
  fi

  # Cleanup removes readiness before any hosts, listener, or alias mutation.
  # Re-read both records to reject a cleanup that began during validation.
  local_run_lock_require_owner "$lock_dir" "$owner_token" || return $?
  local_run_attestation_read "$lock_dir/ready" || return $?
  if [[ "$LOCAL_RUN_READY_OWNER_TOKEN" != "$owner_token" ||
        "$LOCAL_RUN_READY_HOST_IP" != "$hosts_ip" ||
        "$LOCAL_RUN_READY_POSTGRES_HOST" != "$postgres_host" ||
        "$LOCAL_RUN_READY_POSTGRES_PORT" != "$postgres_port" ||
        "$LOCAL_RUN_READY_REDIS_HOST" != "$redis_host" ||
        "$LOCAL_RUN_READY_REDIS_PORT" != "$redis_port" ]]; then
    local_state_error "local launcher readiness changed during validation"
    return 1
  fi
  local_run_lock_require_owner "$lock_dir" "$owner_token" || return $?
}

# Removes only a lock whose opaque ownership token still matches this process.
local_run_lock_release() {
  local lock_dir="$1"
  local owner_token="$2"
  local_run_lock_require_owner "$lock_dir" "$owner_token" || return $?
  rm "$lock_dir/owner" || return $?
  if ! rmdir "$lock_dir"; then
    printf '%s\n' "$owner_token" > "$lock_dir/owner" 2>/dev/null || true
    local_state_error "local launcher lock contains unexpected state: $lock_dir"
    return 1
  fi
}

# Accepts only an ordinary unicast IPv4 address. The suite proxy deliberately
# does not create a loopback alias, so 127/8 and unspecified/multicast ranges
# cannot be selected as its direct host endpoint.
suite_proxy_host_ip_validate_format() {
  local host_ip="$1"
  local first
  local second
  local third
  local fourth
  local rest
  local octet

  if [[ -z "$host_ip" || "$host_ip" == *[!0-9.]* ]]; then
    local_state_error "suite proxy host IP is not a valid IPv4 address: $host_ip"
    return 1
  fi
  IFS=. read -r first second third fourth rest <<< "$host_ip"
  if [[ -n "$rest" || -z "$first" || -z "$second" || -z "$third" || -z "$fourth" ]]; then
    local_state_error "suite proxy host IP is not a valid IPv4 address: $host_ip"
    return 1
  fi
  for octet in "$first" "$second" "$third" "$fourth"; do
    if [[ "${#octet}" -gt 3 || "$octet" == *[!0-9]* ||
          ( "${#octet}" -gt 1 && "$octet" == 0* ) ]] ||
        (( octet < 0 || 255 < octet )); then
      local_state_error "suite proxy host IP is not a valid IPv4 address: $host_ip"
      return 1
    fi
  done
  if (( first == 0 || first == 127 || 224 <= first )); then
    local_state_error "suite proxy host IP must be a non-loopback unicast address: $host_ip"
    return 1
  fi
}

# Requires the selected address on a non-loopback host interface. Routing to an
# address is insufficient: a tunnel or remote route must never satisfy this
# ownership boundary.
suite_proxy_host_ip_validate_assigned() {
  local host_ip="$1"
  suite_proxy_host_ip_validate_format "$host_ip" || return $?

  if command -v ip >/dev/null 2>&1; then
    if ip -o -4 addr show 2>/dev/null | awk -v host_ip="$host_ip" '
      $3 == "inet" && $2 !~ /^lo[0-9]*(:|$)/ {
        split($4, address, "/")
        if (address[1] == host_ip) {
          found = 1
        }
      }
      END { exit found == 1 ? 0 : 1 }
    '; then
      return 0
    fi
  fi
  if command -v ifconfig >/dev/null 2>&1; then
    if ifconfig 2>/dev/null | awk -v host_ip="$host_ip" '
      /^[^[:space:]]/ {
        interface_name = $1
        sub(/:$/, "", interface_name)
      }
      $1 == "inet" && $2 == host_ip && interface_name !~ /^lo[0-9]*$/ {
        found = 1
      }
      END { exit found == 1 ? 0 : 1 }
    '; then
      return 0
    fi
  else
    local_state_error "suite proxy cannot inspect assigned host addresses (need ip or ifconfig)"
    return 1
  fi

  local_state_error "suite proxy host IP is not assigned to a non-loopback interface: $host_ip"
  return 1
}

# Validates one whitespace-free ownership field before it can enter a fixed
# state record or Docker label.
suite_proxy_value_validate() {
  local field_name="$1"
  local value="$2"
  if [[ -z "$value" || "${#value}" -gt 256 || "$value" == *[[:space:]]* ]]; then
    local_state_error "suite proxy $field_name is malformed"
    return 1
  fi
}

# Tokens also become owner-specific tombstone path suffixes, so restrict them
# to a portable filename and Docker-label alphabet.
suite_proxy_token_validate() {
  local field_name="$1"
  local value="$2"
  if [[ "${#value}" -gt 128 || ! "$value" =~ ^[a-zA-Z0-9_.:-]+$ ]]; then
    local_state_error "suite proxy $field_name is malformed"
    return 1
  fi
}

# Validates the decimal fields whose shell arithmetic is safe only after a
# strict spelling check.
suite_proxy_positive_integer_validate() {
  local field_name="$1"
  local value="$2"
  if [[ "${#value}" -gt 10 || ! "$value" =~ ^[1-9][0-9]*$ ]]; then
    local_state_error "suite proxy $field_name is malformed"
    return 1
  fi
}

# Binds a pid to the process instance rather than trusting the reusable number
# alone. `ps lstart` is available on both Darwin and Linux; normalizing its
# fixed fields yields a whitespace-free stable identity.
suite_proxy_process_start_identity() {
  local owner_pid="$1"
  local start_identity
  suite_proxy_positive_integer_validate "owner pid" "$owner_pid" || return $?
  start_identity="$(LC_ALL=C ps -o lstart= -p "$owner_pid" 2>/dev/null | LC_ALL=C awk '
    NF == 5 {
      printf "%s-%s-%s-%s-%s", $1, $2, $3, $4, $5
      found = 1
    }
    END { if (found != 1) exit 1 }
  ')" || {
    local_state_error "suite proxy owner process is not live: $owner_pid"
    return 1
  }
  suite_proxy_value_validate "owner start identity" "$start_identity" || return $?
  SUITE_PROXY_PROCESS_START_IDENTITY="$start_identity"
}

# The unguessable owner token is also an exact argv field on the long-running
# foreground helper. This live challenge closes same-second pid-reuse ambiguity
# left by the portable, second-resolution process start time.
suite_proxy_process_challenge_validate() {
  local owner_pid="$1"
  local owner_token="$2"
  local expected_argument="--urnetwork-suite-proxy-owner-token=$owner_token"

  suite_proxy_positive_integer_validate "owner pid" "$owner_pid" || return $?
  suite_proxy_token_validate "owner token" "$owner_token" || return $?
  if ! LC_ALL=C ps -ww -o command= -p "$owner_pid" 2>/dev/null | LC_ALL=C awk \
      -v expected_argument="$expected_argument" '
    {
      for (i = 1; i <= NF; i++) {
        if ($i == expected_argument) {
          found = 1
        }
      }
    }
    END { exit found == 1 ? 0 : 1 }
  '; then
    local_state_error "suite proxy owner process challenge is not live: $owner_pid"
    return 1
  fi
}

suite_proxy_current_process_matches() {
  local owner_pid="$1"
  local owner_start_identity="$2"
  local owner_token="$3"

  if [[ "$owner_pid" != "$$" ]]; then
    local_state_error "suite proxy state mutation requires its owning process"
    return 1
  fi
  suite_proxy_process_start_identity "$owner_pid" || return $?
  if [[ "$SUITE_PROXY_PROCESS_START_IDENTITY" != "$owner_start_identity" ]]; then
    local_state_error "suite proxy owner process instance changed: $owner_pid"
    return 1
  fi
  suite_proxy_process_challenge_validate "$owner_pid" "$owner_token"
}

# Container ids are recorded in their complete Docker spelling so cleanup can
# address an immutable object rather than a reusable name.
suite_proxy_container_id_validate() {
  local field_name="$1"
  local container_id="$2"
  if [[ ! "$container_id" =~ ^[0-9a-f]{64}$ ]]; then
    local_state_error "suite proxy $field_name is malformed"
    return 1
  fi
}

# Docker image ids include their content-address algorithm prefix.
suite_proxy_image_id_validate() {
  local image_id="$1"
  if [[ ! "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    local_state_error "suite proxy image id is malformed"
    return 1
  fi
}

# Atomically claims a private state directory and publishes its immutable
# process/token/generation owner record before any Docker mutation is allowed.
suite_proxy_state_acquire() {
  local state_dir="$1"
  local owner_pid="$2"
  local owner_token="$3"
  local generation="$4"
  local owner_path="$state_dir/owner"
  local owner_start_identity

  suite_proxy_positive_integer_validate "owner pid" "$owner_pid" || return $?
  if [[ "$owner_pid" != "$$" ]]; then
    local_state_error "suite proxy ownership can only be acquired by the current process"
    return 1
  fi
  suite_proxy_process_start_identity "$owner_pid" || return $?
  owner_start_identity="$SUITE_PROXY_PROCESS_START_IDENTITY"
  suite_proxy_token_validate "owner token" "$owner_token" || return $?
  suite_proxy_token_validate "generation" "$generation" || return $?
  suite_proxy_process_challenge_validate "$owner_pid" "$owner_token" || return $?
  if [[ -z "$state_dir" || "$state_dir" != /* ]]; then
    local_state_error "suite proxy state directory must be an absolute path"
    return 1
  fi
  if ! (umask 077 && mkdir "$state_dir") 2>/dev/null; then
    local_state_error "suite proxy state directory is already owned: $state_dir"
    return 1
  fi
  if ! (umask 077 && set -o noclobber && {
    printf '%s\n' "format=urnetwork-server-suite-proxy-owner-v1"
    printf 'owner_pid=%s\n' "$owner_pid"
    printf 'owner_start_identity=%s\n' "$owner_start_identity"
    printf 'owner_token=%s\n' "$owner_token"
    printf 'generation=%s\n' "$generation"
  } > "$owner_path"); then
    # An exclusive-create failure is ambiguous: another same-uid actor may
    # have installed the path. Never unlink it; the directory remains a
    # fail-closed tombstone unless it is still empty.
    rmdir "$state_dir" 2>/dev/null || true
    return 1
  fi
  SUITE_PROXY_ACQUIRED_START_IDENTITY="$owner_start_identity"
}

# Parses the non-executable, fixed-line owner record.
suite_proxy_state_read_owner_path() {
  local owner_path="$1"
  local line
  local lines=()

  if [[ ! -f "$owner_path" || -L "$owner_path" || ! -r "$owner_path" ]]; then
    local_state_error "suite proxy state has no readable regular owner: $owner_path"
    return 1
  fi
  while IFS= read -r line || [[ -n "$line" ]]; do
    lines+=("$line")
  done < "$owner_path"
  if [[ "${#lines[@]}" != 5 ]] ||
      [[ "${lines[0]}" != "format=urnetwork-server-suite-proxy-owner-v1" ]] ||
      [[ "${lines[1]}" != owner_pid=* ]] ||
      [[ "${lines[2]}" != owner_start_identity=* ]] ||
      [[ "${lines[3]}" != owner_token=* ]] ||
      [[ "${lines[4]}" != generation=* ]]; then
    local_state_error "suite proxy owner record is malformed: $owner_path"
    return 1
  fi

  SUITE_PROXY_OWNER_PID="${lines[1]#owner_pid=}"
  SUITE_PROXY_OWNER_START_IDENTITY="${lines[2]#owner_start_identity=}"
  SUITE_PROXY_OWNER_TOKEN="${lines[3]#owner_token=}"
  SUITE_PROXY_OWNER_GENERATION="${lines[4]#generation=}"
  suite_proxy_positive_integer_validate "owner pid" "$SUITE_PROXY_OWNER_PID" || return $?
  suite_proxy_value_validate "owner start identity" "$SUITE_PROXY_OWNER_START_IDENTITY" || return $?
  suite_proxy_token_validate "owner token" "$SUITE_PROXY_OWNER_TOKEN" || return $?
  suite_proxy_token_validate "generation" "$SUITE_PROXY_OWNER_GENERATION" || return $?
}

suite_proxy_state_read_owner() {
  suite_proxy_state_read_owner_path "$1/owner"
}

suite_proxy_owner_file_matches() {
  local owner_path="$1"
  local owner_pid="$2"
  local owner_start_identity="$3"
  local owner_token="$4"
  local generation="$5"

  suite_proxy_state_read_owner_path "$owner_path" || return $?
  if [[ "$SUITE_PROXY_OWNER_PID" != "$owner_pid" ||
        "$SUITE_PROXY_OWNER_START_IDENTITY" != "$owner_start_identity" ||
        "$SUITE_PROXY_OWNER_TOKEN" != "$owner_token" ||
        "$SUITE_PROXY_OWNER_GENERATION" != "$generation" ]]; then
    local_state_error "suite proxy owner tombstone does not match its immutable snapshot"
    return 1
  fi
}

# Mutation rechecks include both immutable state and the actual current owner
# process; consumers use the read-only require helper above.
suite_proxy_state_require_current_owner() {
  local state_dir="$1"
  local owner_pid="$2"
  local owner_start_identity="$3"
  local owner_token="$4"
  local generation="$5"

  suite_proxy_state_require_owner \
    "$state_dir" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" || return $?
  suite_proxy_current_process_matches "$owner_pid" "$owner_start_identity" "$owner_token" || return $?
  suite_proxy_state_require_owner \
    "$state_dir" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation"
}

# GNU and BSD spell "rename this exact path without following or replacing the
# destination" differently. Verify postconditions because both -n variants
# intentionally report success when the destination already exists.
suite_proxy_move_no_replace() {
  local source_path="$1"
  local destination_path="$2"
  local move_status=0

  if [[ ! -e "$source_path" && ! -L "$source_path" ]]; then
    local_state_error "suite proxy move source is missing: $source_path"
    return 1
  fi
  if mv --version >/dev/null 2>&1; then
    mv -n -T -- "$source_path" "$destination_path" || move_status=$?
  else
    mv -n -h "$source_path" "$destination_path" || move_status=$?
  fi
  if [[ "$move_status" != 0 || -e "$source_path" || -L "$source_path" ||
        ( ! -e "$destination_path" && ! -L "$destination_path" ) ]]; then
    local_state_error "suite proxy could not claim an owner-specific tombstone"
    return 1
  fi
}

# Confirms all immutable owner fields immediately before state mutation.
suite_proxy_state_require_owner() {
  local state_dir="$1"
  local owner_pid="$2"
  local owner_start_identity="$3"
  local owner_token="$4"
  local generation="$5"
  if [[ ! -d "$state_dir" || -L "$state_dir" ]]; then
    local_state_error "suite proxy state directory is missing or not regular: $state_dir"
    return 1
  fi
  suite_proxy_state_read_owner "$state_dir" || return $?
  if [[ "$SUITE_PROXY_OWNER_PID" != "$owner_pid" ||
        "$SUITE_PROXY_OWNER_START_IDENTITY" != "$owner_start_identity" ||
        "$SUITE_PROXY_OWNER_TOKEN" != "$owner_token" ||
        "$SUITE_PROXY_OWNER_GENERATION" != "$generation" ]]; then
    local_state_error "suite proxy state ownership changed: $state_dir"
    return 1
  fi
}

# Parses the complete direct-endpoint and immutable-container readiness record.
# No record is ever sourced or evaluated.
suite_proxy_attestation_read() {
  local attestation_path="$1"
  local line
  local lines=()

  if [[ ! -f "$attestation_path" || -L "$attestation_path" || ! -r "$attestation_path" ]]; then
    local_state_error "suite proxy readiness attestation is missing or not regular: $attestation_path"
    return 1
  fi
  while IFS= read -r line || [[ -n "$line" ]]; do
    lines+=("$line")
  done < "$attestation_path"
  if [[ "${#lines[@]}" != 17 ]] ||
      [[ "${lines[0]}" != "format=urnetwork-server-suite-proxy-ready-v1" ]] ||
      [[ "${lines[1]}" != owner_pid=* ]] ||
      [[ "${lines[2]}" != owner_start_identity=* ]] ||
      [[ "${lines[3]}" != owner_token=* ]] ||
      [[ "${lines[4]}" != generation=* ]] ||
      [[ "${lines[5]}" != host_ip=* ]] ||
      [[ "${lines[6]}" != postgres_host=* ]] ||
      [[ "${lines[7]}" != postgres_port=* ]] ||
      [[ "${lines[8]}" != redis_host=* ]] ||
      [[ "${lines[9]}" != redis_port=* ]] ||
      [[ "${lines[10]}" != postgres_upstream_id=* ]] ||
      [[ "${lines[11]}" != redis_upstream_id=* ]] ||
      [[ "${lines[12]}" != proxy_image_id=* ]] ||
      [[ "${lines[13]}" != postgres_proxy_name=* ]] ||
      [[ "${lines[14]}" != postgres_proxy_id=* ]] ||
      [[ "${lines[15]}" != redis_proxy_name=* ]] ||
      [[ "${lines[16]}" != redis_proxy_id=* ]]; then
    local_state_error "suite proxy readiness attestation is malformed: $attestation_path"
    return 1
  fi

  SUITE_PROXY_READY_OWNER_PID="${lines[1]#owner_pid=}"
  SUITE_PROXY_READY_OWNER_START_IDENTITY="${lines[2]#owner_start_identity=}"
  SUITE_PROXY_READY_OWNER_TOKEN="${lines[3]#owner_token=}"
  SUITE_PROXY_READY_GENERATION="${lines[4]#generation=}"
  SUITE_PROXY_READY_HOST_IP="${lines[5]#host_ip=}"
  SUITE_PROXY_READY_POSTGRES_HOST="${lines[6]#postgres_host=}"
  SUITE_PROXY_READY_POSTGRES_PORT="${lines[7]#postgres_port=}"
  SUITE_PROXY_READY_REDIS_HOST="${lines[8]#redis_host=}"
  SUITE_PROXY_READY_REDIS_PORT="${lines[9]#redis_port=}"
  SUITE_PROXY_READY_POSTGRES_UPSTREAM_ID="${lines[10]#postgres_upstream_id=}"
  SUITE_PROXY_READY_REDIS_UPSTREAM_ID="${lines[11]#redis_upstream_id=}"
  SUITE_PROXY_READY_IMAGE_ID="${lines[12]#proxy_image_id=}"
  SUITE_PROXY_READY_POSTGRES_PROXY_NAME="${lines[13]#postgres_proxy_name=}"
  SUITE_PROXY_READY_POSTGRES_PROXY_ID="${lines[14]#postgres_proxy_id=}"
  SUITE_PROXY_READY_REDIS_PROXY_NAME="${lines[15]#redis_proxy_name=}"
  SUITE_PROXY_READY_REDIS_PROXY_ID="${lines[16]#redis_proxy_id=}"

  suite_proxy_positive_integer_validate "readiness owner pid" "$SUITE_PROXY_READY_OWNER_PID" || return $?
  suite_proxy_value_validate "readiness owner start identity" "$SUITE_PROXY_READY_OWNER_START_IDENTITY" || return $?
  suite_proxy_token_validate "readiness owner token" "$SUITE_PROXY_READY_OWNER_TOKEN" || return $?
  suite_proxy_token_validate "readiness generation" "$SUITE_PROXY_READY_GENERATION" || return $?
  suite_proxy_host_ip_validate_format "$SUITE_PROXY_READY_HOST_IP" || return $?
  suite_proxy_value_validate "PostgreSQL host" "$SUITE_PROXY_READY_POSTGRES_HOST" || return $?
  suite_proxy_positive_integer_validate "PostgreSQL port" "$SUITE_PROXY_READY_POSTGRES_PORT" || return $?
  suite_proxy_value_validate "Redis host" "$SUITE_PROXY_READY_REDIS_HOST" || return $?
  suite_proxy_positive_integer_validate "Redis port" "$SUITE_PROXY_READY_REDIS_PORT" || return $?
  if [[ "${#SUITE_PROXY_READY_POSTGRES_PORT}" -gt 5 ||
        "${#SUITE_PROXY_READY_REDIS_PORT}" -gt 5 ]] ||
      (( 65535 < SUITE_PROXY_READY_POSTGRES_PORT || 65535 < SUITE_PROXY_READY_REDIS_PORT )); then
    local_state_error "suite proxy readiness attestation has an invalid port: $attestation_path"
    return 1
  fi
  suite_proxy_container_id_validate "PostgreSQL upstream id" "$SUITE_PROXY_READY_POSTGRES_UPSTREAM_ID" || return $?
  suite_proxy_container_id_validate "Redis upstream id" "$SUITE_PROXY_READY_REDIS_UPSTREAM_ID" || return $?
  suite_proxy_image_id_validate "$SUITE_PROXY_READY_IMAGE_ID" || return $?
  suite_proxy_value_validate "PostgreSQL proxy name" "$SUITE_PROXY_READY_POSTGRES_PROXY_NAME" || return $?
  suite_proxy_container_id_validate "PostgreSQL proxy id" "$SUITE_PROXY_READY_POSTGRES_PROXY_ID" || return $?
  suite_proxy_value_validate "Redis proxy name" "$SUITE_PROXY_READY_REDIS_PROXY_NAME" || return $?
  suite_proxy_container_id_validate "Redis proxy id" "$SUITE_PROXY_READY_REDIS_PROXY_ID" || return $?
  if [[ "$SUITE_PROXY_READY_POSTGRES_PROXY_NAME" != urnetwork-suite-proxy-pg ||
        "$SUITE_PROXY_READY_REDIS_PROXY_NAME" != urnetwork-suite-proxy-redis ]]; then
    local_state_error "suite proxy readiness has non-canonical proxy names"
    return 1
  fi
}

# Compares one owner/readiness read to a caller's immutable snapshot.
# Compares one parsed readiness record to the caller's immutable values.
suite_proxy_attestation_file_matches() {
  local attestation_path="$1"
  local owner_pid="$2"
  local owner_start_identity="$3"
  local owner_token="$4"
  local generation="$5"
  local host_ip="$6"
  local postgres_host="$7"
  local postgres_port="$8"
  local redis_host="$9"
  shift 9
  local redis_port="$1"
  local postgres_upstream_id="$2"
  local redis_upstream_id="$3"
  local image_id="$4"
  local postgres_proxy_name="$5"
  local postgres_proxy_id="$6"
  local redis_proxy_name="$7"
  local redis_proxy_id="$8"

  suite_proxy_attestation_read "$attestation_path" || return $?
  if [[ "$SUITE_PROXY_READY_OWNER_PID" != "$owner_pid" ||
        "$SUITE_PROXY_READY_OWNER_START_IDENTITY" != "$owner_start_identity" ||
        "$SUITE_PROXY_READY_OWNER_TOKEN" != "$owner_token" ||
        "$SUITE_PROXY_READY_GENERATION" != "$generation" ||
        "$SUITE_PROXY_READY_HOST_IP" != "$host_ip" ||
        "$SUITE_PROXY_READY_POSTGRES_HOST" != "$postgres_host" ||
        "$SUITE_PROXY_READY_POSTGRES_PORT" != "$postgres_port" ||
        "$SUITE_PROXY_READY_REDIS_HOST" != "$redis_host" ||
        "$SUITE_PROXY_READY_REDIS_PORT" != "$redis_port" ||
        "$SUITE_PROXY_READY_POSTGRES_UPSTREAM_ID" != "$postgres_upstream_id" ||
        "$SUITE_PROXY_READY_REDIS_UPSTREAM_ID" != "$redis_upstream_id" ||
        "$SUITE_PROXY_READY_IMAGE_ID" != "$image_id" ||
        "$SUITE_PROXY_READY_POSTGRES_PROXY_NAME" != "$postgres_proxy_name" ||
        "$SUITE_PROXY_READY_POSTGRES_PROXY_ID" != "$postgres_proxy_id" ||
        "$SUITE_PROXY_READY_REDIS_PROXY_NAME" != "$redis_proxy_name" ||
        "$SUITE_PROXY_READY_REDIS_PROXY_ID" != "$redis_proxy_id" ]]; then
    local_state_error "suite proxy readiness does not match its immutable snapshot: $attestation_path"
    return 1
  fi
}

# Compares one owner/readiness read to a caller's immutable snapshot.
suite_proxy_attestation_snapshot_matches() {
  local state_dir="$1"
  local owner_pid="$2"
  local owner_start_identity="$3"
  local owner_token="$4"
  local generation="$5"
  local ready_dir="$state_dir/ready"
  local unexpected
  local published_unexpected
  shift 5

  suite_proxy_state_require_owner \
    "$state_dir" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" || return $?
  if [[ ! -d "$ready_dir" || -L "$ready_dir" ||
        ! -d "$ready_dir/published" || -L "$ready_dir/published" ]]; then
    local_state_error "suite proxy readiness sentinel is missing or not regular: $ready_dir"
    return 1
  fi
  unexpected="$(find "$ready_dir" -mindepth 1 -maxdepth 1 \
    ! -name record ! -name published -print -quit 2>/dev/null)" || return $?
  published_unexpected="$(find "$ready_dir/published" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" || return $?
  if [[ -n "$unexpected" || -n "$published_unexpected" ]]; then
    local_state_error "suite proxy readiness directory contains unexpected state: $ready_dir"
    return 1
  fi
  suite_proxy_attestation_file_matches \
    "$ready_dir/record" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" "$@"
}

# Validates the same owner/readiness snapshot on both sides of process-instance
# and assigned-address checks, closing pid-reuse and state replacement windows.
suite_proxy_attestation_validate_snapshot() {
  local state_dir="$1"
  local owner_pid="$2"
  local owner_start_identity="$3"
  local owner_token="$4"
  local generation="$5"
  local host_ip="$6"
  shift 6

  suite_proxy_attestation_snapshot_matches \
    "$state_dir" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" \
    "$host_ip" "$@" || return $?
  suite_proxy_process_start_identity "$owner_pid" || return $?
  if [[ "$SUITE_PROXY_PROCESS_START_IDENTITY" != "$owner_start_identity" ]]; then
    local_state_error "suite proxy owner process instance changed: $owner_pid"
    return 1
  fi
  suite_proxy_process_challenge_validate "$owner_pid" "$owner_token" || return $?
  suite_proxy_host_ip_validate_assigned "$host_ip" || return $?
  suite_proxy_attestation_snapshot_matches \
    "$state_dir" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" \
    "$host_ip" "$@" || return $?
  suite_proxy_process_start_identity "$owner_pid" || return $?
  if [[ "$SUITE_PROXY_PROCESS_START_IDENTITY" != "$owner_start_identity" ]]; then
    local_state_error "suite proxy owner process instance changed: $owner_pid"
    return 1
  fi
  suite_proxy_process_challenge_validate "$owner_pid" "$owner_token"
}

# Reads and returns a stable complete readiness snapshot for a harness.
suite_proxy_attestation_validate() {
  local state_dir="$1"
  local owner_pid
  local owner_start_identity
  local owner_token
  local generation
  local host_ip
  local postgres_host
  local postgres_port
  local redis_host
  local redis_port
  local postgres_upstream_id
  local redis_upstream_id
  local image_id
  local postgres_proxy_name
  local postgres_proxy_id
  local redis_proxy_name
  local redis_proxy_id

  if [[ ! -d "$state_dir" || -L "$state_dir" ]]; then
    local_state_error "suite proxy state directory is missing or not regular: $state_dir"
    return 1
  fi
  suite_proxy_state_read_owner "$state_dir" || return $?
  owner_pid="$SUITE_PROXY_OWNER_PID"
  owner_start_identity="$SUITE_PROXY_OWNER_START_IDENTITY"
  owner_token="$SUITE_PROXY_OWNER_TOKEN"
  generation="$SUITE_PROXY_OWNER_GENERATION"
  suite_proxy_attestation_read "$state_dir/ready/record" || return $?
  host_ip="$SUITE_PROXY_READY_HOST_IP"
  postgres_host="$SUITE_PROXY_READY_POSTGRES_HOST"
  postgres_port="$SUITE_PROXY_READY_POSTGRES_PORT"
  redis_host="$SUITE_PROXY_READY_REDIS_HOST"
  redis_port="$SUITE_PROXY_READY_REDIS_PORT"
  postgres_upstream_id="$SUITE_PROXY_READY_POSTGRES_UPSTREAM_ID"
  redis_upstream_id="$SUITE_PROXY_READY_REDIS_UPSTREAM_ID"
  image_id="$SUITE_PROXY_READY_IMAGE_ID"
  postgres_proxy_name="$SUITE_PROXY_READY_POSTGRES_PROXY_NAME"
  postgres_proxy_id="$SUITE_PROXY_READY_POSTGRES_PROXY_ID"
  redis_proxy_name="$SUITE_PROXY_READY_REDIS_PROXY_NAME"
  redis_proxy_id="$SUITE_PROXY_READY_REDIS_PROXY_ID"
  if [[ "$postgres_host" != "$host_ip" || "$redis_host" != "$host_ip" ]]; then
    local_state_error "suite proxy readiness endpoints are not direct host-IP endpoints"
    return 1
  fi

  suite_proxy_attestation_validate_snapshot \
    "$state_dir" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" \
    "$host_ip" "$postgres_host" "$postgres_port" "$redis_host" "$redis_port" \
    "$postgres_upstream_id" "$redis_upstream_id" "$image_id" \
    "$postgres_proxy_name" "$postgres_proxy_id" \
    "$redis_proxy_name" "$redis_proxy_id" || return $?

  # Export the original values, not globals from the confirming re-read.
  SUITE_PROXY_SNAPSHOT_OWNER_PID="$owner_pid"
  SUITE_PROXY_SNAPSHOT_OWNER_START_IDENTITY="$owner_start_identity"
  SUITE_PROXY_SNAPSHOT_OWNER_TOKEN="$owner_token"
  SUITE_PROXY_SNAPSHOT_GENERATION="$generation"
  SUITE_PROXY_SNAPSHOT_HOST_IP="$host_ip"
  SUITE_PROXY_SNAPSHOT_POSTGRES_HOST="$postgres_host"
  SUITE_PROXY_SNAPSHOT_POSTGRES_PORT="$postgres_port"
  SUITE_PROXY_SNAPSHOT_REDIS_HOST="$redis_host"
  SUITE_PROXY_SNAPSHOT_REDIS_PORT="$redis_port"
  SUITE_PROXY_SNAPSHOT_POSTGRES_UPSTREAM_ID="$postgres_upstream_id"
  SUITE_PROXY_SNAPSHOT_REDIS_UPSTREAM_ID="$redis_upstream_id"
  SUITE_PROXY_SNAPSHOT_IMAGE_ID="$image_id"
  SUITE_PROXY_SNAPSHOT_POSTGRES_PROXY_NAME="$postgres_proxy_name"
  SUITE_PROXY_SNAPSHOT_POSTGRES_PROXY_ID="$postgres_proxy_id"
  SUITE_PROXY_SNAPSHOT_REDIS_PROXY_NAME="$redis_proxy_name"
  SUITE_PROXY_SNAPSHOT_REDIS_PROXY_ID="$redis_proxy_id"
}

# Creates an exclusive private readiness directory, validates its record, and
# publishes it by atomically creating an empty `published` directory. A reader
# never accepts the pre-publication directory, and mkdir cannot dereference a
# raced destination symlink the way two-operand ln/mv can.
suite_proxy_attestation_publish() {
  local state_dir="$1"
  local owner_pid="$2"
  local owner_start_identity="$3"
  local owner_token="$4"
  local generation="$5"
  local host_ip="$6"
  local postgres_host="$7"
  local postgres_port="$8"
  local redis_host="$9"
  shift 9
  local redis_port="$1"
  local postgres_upstream_id="$2"
  local redis_upstream_id="$3"
  local image_id="$4"
  local postgres_proxy_name="$5"
  local postgres_proxy_id="$6"
  local redis_proxy_name="$7"
  local redis_proxy_id="$8"
  local ready_dir="$state_dir/ready"
  local record_path="$ready_dir/record"
  local published_path="$ready_dir/published"
  local unexpected

  suite_proxy_state_require_current_owner \
    "$state_dir" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" || return $?
  suite_proxy_host_ip_validate_assigned "$host_ip" || return $?
  if [[ "$postgres_host" != "$host_ip" || "$redis_host" != "$host_ip" ]]; then
    local_state_error "suite proxy readiness endpoints must use the assigned host IP"
    return 1
  fi
  suite_proxy_container_id_validate "PostgreSQL upstream id" "$postgres_upstream_id" || return $?
  suite_proxy_container_id_validate "Redis upstream id" "$redis_upstream_id" || return $?
  suite_proxy_image_id_validate "$image_id" || return $?
  suite_proxy_container_id_validate "PostgreSQL proxy id" "$postgres_proxy_id" || return $?
  suite_proxy_container_id_validate "Redis proxy id" "$redis_proxy_id" || return $?
  if [[ "$postgres_proxy_name" != urnetwork-suite-proxy-pg ||
        "$redis_proxy_name" != urnetwork-suite-proxy-redis ]]; then
    local_state_error "suite proxy readiness requires canonical proxy names"
    return 1
  fi
  if [[ -e "$ready_dir" || -L "$ready_dir" ]]; then
    local_state_error "suite proxy readiness state already exists: $state_dir"
    return 1
  fi
  if ! (umask 077 && mkdir "$ready_dir"); then
    local_state_error "could not exclusively create suite proxy readiness directory"
    return 1
  fi
  if ! (umask 077 && set -o noclobber && {
    printf '%s\n' "format=urnetwork-server-suite-proxy-ready-v1"
    printf 'owner_pid=%s\n' "$owner_pid"
    printf 'owner_start_identity=%s\n' "$owner_start_identity"
    printf 'owner_token=%s\n' "$owner_token"
    printf 'generation=%s\n' "$generation"
    printf 'host_ip=%s\n' "$host_ip"
    printf 'postgres_host=%s\n' "$postgres_host"
    printf 'postgres_port=%s\n' "$postgres_port"
    printf 'redis_host=%s\n' "$redis_host"
    printf 'redis_port=%s\n' "$redis_port"
    printf 'postgres_upstream_id=%s\n' "$postgres_upstream_id"
    printf 'redis_upstream_id=%s\n' "$redis_upstream_id"
    printf 'proxy_image_id=%s\n' "$image_id"
    printf 'postgres_proxy_name=%s\n' "$postgres_proxy_name"
    printf 'postgres_proxy_id=%s\n' "$postgres_proxy_id"
    printf 'redis_proxy_name=%s\n' "$redis_proxy_name"
    printf 'redis_proxy_id=%s\n' "$redis_proxy_id"
  } > "$record_path"); then
    local_state_error "could not exclusively create suite proxy readiness record"
    return 1
  fi
  if ! suite_proxy_attestation_file_matches \
      "$record_path" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" \
      "$host_ip" "$postgres_host" "$postgres_port" "$redis_host" "$redis_port" \
      "$postgres_upstream_id" "$redis_upstream_id" "$image_id" \
      "$postgres_proxy_name" "$postgres_proxy_id" "$redis_proxy_name" "$redis_proxy_id"; then
    local_state_error "refusing to publish invalid suite proxy readiness"
    return 1
  fi
  unexpected="$(find "$ready_dir" -mindepth 1 -maxdepth 1 ! -name record -print -quit 2>/dev/null)" || {
    local_state_error "could not inspect unpublished suite proxy readiness"
    return 1
  }
  if [[ -n "$unexpected" ]]; then
    local_state_error "unpublished suite proxy readiness contains unexpected state"
    return 1
  fi
  suite_proxy_state_require_current_owner \
    "$state_dir" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" || return $?
  suite_proxy_attestation_file_matches \
    "$record_path" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" \
    "$host_ip" "$postgres_host" "$postgres_port" "$redis_host" "$redis_port" \
    "$postgres_upstream_id" "$redis_upstream_id" "$image_id" \
    "$postgres_proxy_name" "$postgres_proxy_id" "$redis_proxy_name" "$redis_proxy_id" || return $?
  if ! (umask 077 && mkdir "$published_path"); then
    local_state_error "could not atomically publish suite proxy readiness"
    return 1
  fi
  suite_proxy_attestation_validate_snapshot \
    "$state_dir" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" \
    "$host_ip" "$postgres_host" "$postgres_port" "$redis_host" "$redis_port" \
    "$postgres_upstream_id" "$redis_upstream_id" "$image_id" \
    "$postgres_proxy_name" "$postgres_proxy_id" "$redis_proxy_name" "$redis_proxy_id"
}

# Withdraws readiness with one no-replace rename before inspecting or removing
# its contents. Any swapped or malformed object remains in the owner-specific
# tombstone rather than being unlinked through the canonical path.
suite_proxy_attestation_remove() {
  local state_dir="$1"
  local owner_pid="$2"
  local owner_start_identity="$3"
  local owner_token="$4"
  local generation="$5"
  local ready_path="$state_dir/ready"
  local tombstone_path="$state_dir/ready.removing.$owner_token.$generation"
  local record_path="$tombstone_path/record"
  local record_tombstone_path="$tombstone_path/record.removing"
  local published_path="$tombstone_path/published"
  local published_tombstone_path="$tombstone_path/published.removing"
  local unexpected
  local published_unexpected
  shift 5

  suite_proxy_state_require_current_owner \
    "$state_dir" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" || return $?
  if [[ -e "$ready_path" || -L "$ready_path" ]]; then
    suite_proxy_move_no_replace "$ready_path" "$tombstone_path" || return $?
    if [[ ! -d "$tombstone_path" || -L "$tombstone_path" ]]; then
      local_state_error "suite proxy readiness tombstone is not an owned directory"
      return 1
    fi
    unexpected="$(find "$tombstone_path" -mindepth 1 -maxdepth 1 \
      ! -name record ! -name published -print -quit 2>/dev/null)" || return $?
    if [[ -n "$unexpected" || ! -f "$record_path" || -L "$record_path" ]]; then
      local_state_error "suite proxy readiness tombstone contains unexpected state"
      return 1
    fi
    if [[ -e "$published_path" || -L "$published_path" ]]; then
      if [[ ! -d "$published_path" || -L "$published_path" ]]; then
        local_state_error "suite proxy published sentinel is not an empty owned directory"
        return 1
      fi
      published_unexpected="$(find "$published_path" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" || return $?
      if [[ -n "$published_unexpected" ]]; then
        local_state_error "suite proxy published sentinel is not an empty owned directory"
        return 1
      fi
    fi
    suite_proxy_attestation_file_matches \
      "$record_path" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" "$@" || return $?
    suite_proxy_state_require_current_owner \
      "$state_dir" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" || return $?
    if [[ -d "$published_path" && ! -L "$published_path" ]]; then
      suite_proxy_move_no_replace "$published_path" "$published_tombstone_path" || return $?
      published_unexpected="$(find "$published_tombstone_path" \
        -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" || return $?
      if [[ ! -d "$published_tombstone_path" || -L "$published_tombstone_path" ||
            -n "$published_unexpected" ]]; then
        local_state_error "suite proxy published tombstone is not an empty owned directory"
        return 1
      fi
      rmdir "$published_tombstone_path" || return $?
    fi
    suite_proxy_move_no_replace "$record_path" "$record_tombstone_path" || return $?
    suite_proxy_attestation_file_matches \
      "$record_tombstone_path" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" "$@" || return $?
    suite_proxy_state_require_current_owner \
      "$state_dir" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" || return $?
    rm -- "$record_tombstone_path" || return $?
    rmdir "$tombstone_path" || return $?
  fi
}

# Releases only an otherwise-empty directory with the same live owner. The
# owner record itself is first moved without replacement, then re-parsed from
# its owner-specific tombstone before deletion. Any mismatch stays fail closed.
suite_proxy_state_release() {
  local state_dir="$1"
  local owner_pid="$2"
  local owner_start_identity="$3"
  local owner_token="$4"
  local generation="$5"
  local owner_path="$state_dir/owner"
  local tombstone_path="$state_dir/owner.releasing.$owner_token.$generation"
  local unexpected

  suite_proxy_state_require_current_owner \
    "$state_dir" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" || return $?
  unexpected="$(find "$state_dir" -mindepth 1 -maxdepth 1 ! -name owner -print -quit 2>/dev/null)" || {
    local_state_error "could not inspect suite proxy state before release: $state_dir"
    return 1
  }
  if [[ -n "$unexpected" ]]; then
    local_state_error "suite proxy state contains unexpected files: $state_dir"
    return 1
  fi
  suite_proxy_state_require_current_owner \
    "$state_dir" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" || return $?
  suite_proxy_move_no_replace "$owner_path" "$tombstone_path" || return $?
  suite_proxy_owner_file_matches \
    "$tombstone_path" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" || return $?
  suite_proxy_current_process_matches "$owner_pid" "$owner_start_identity" "$owner_token" || return $?
  unexpected="$(find "$state_dir" -mindepth 1 -maxdepth 1 \
    ! -name "owner.releasing.$owner_token.$generation" -print -quit 2>/dev/null)" || return $?
  if [[ -n "$unexpected" ]]; then
    local_state_error "suite proxy state changed during release: $state_dir"
    return 1
  fi
  suite_proxy_owner_file_matches \
    "$tombstone_path" "$owner_pid" "$owner_start_identity" "$owner_token" "$generation" || return $?
  rm -- "$tombstone_path" || return $?
  if ! rmdir "$state_dir"; then
    local_state_error "suite proxy state release left a fail-closed tombstone: $state_dir"
    return 1
  fi
}
