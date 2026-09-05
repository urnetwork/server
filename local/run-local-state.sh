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
