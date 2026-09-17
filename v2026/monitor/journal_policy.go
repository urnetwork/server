package monitor

// Validate the manager-loaded age-vacuum policy without returning unit paths,
// machine identity, argv or journal lines. Hashes bind the owned Xops units;
// drop-ins or a pending daemon reload cannot inherit their authority.
const journalVacuumPolicyCommand = `
journal_vacuum_policy_state=unavailable
journal_vacuum_runtime_state=unavailable
journal_vacuum_last_success_age_seconds=0
journal_vacuum_property() {
  printf '%s\n' "$1" | LC_ALL=C awk -F= -v key="$2" '
    $1 == key {value=substr($0, length(key)+2); count++}
    END {if (count != 1) exit 1; print value}
  '
}
journal_vacuum_observe() {
  vacuum_service=$(timeout 5s systemctl show warp-journal-vacuum.service --no-pager \
    -p LoadState -p FragmentPath -p DropInPaths -p NeedDaemonReload -p ActiveState \
    -p Result -p ExecMainCode -p ExecMainStatus \
    -p ExecMainStartTimestampMonotonic -p ExecMainExitTimestampMonotonic 2>/dev/null) || return
  vacuum_timer=$(timeout 5s systemctl show warp-journal-vacuum.timer --no-pager \
    -p LoadState -p FragmentPath -p DropInPaths -p NeedDaemonReload \
    -p ActiveState -p UnitFileState -p ActiveEnterTimestampMonotonic 2>/dev/null) || return
  for vacuum_properties in "$vacuum_service" "$vacuum_timer"; do
    vacuum_load=$(journal_vacuum_property "$vacuum_properties" LoadState) || return
    case "$vacuum_load" in
      loaded) ;;
      not-found|masked|error) journal_vacuum_policy_state=drift; journal_vacuum_runtime_state=failed; return ;;
      *) return ;;
    esac
    vacuum_dropins=$(journal_vacuum_property "$vacuum_properties" DropInPaths) || return
    vacuum_reload=$(journal_vacuum_property "$vacuum_properties" NeedDaemonReload) || return
    if [ -n "$vacuum_dropins" ] || [ "$vacuum_reload" != no ]; then
      journal_vacuum_policy_state=drift
      return
    fi
  done
  vacuum_service_path=$(journal_vacuum_property "$vacuum_service" FragmentPath) || return
  vacuum_timer_path=$(journal_vacuum_property "$vacuum_timer" FragmentPath) || return
  if [ "$vacuum_service_path" != /etc/systemd/system/warp-journal-vacuum.service ] ||
     [ "$vacuum_timer_path" != /etc/systemd/system/warp-journal-vacuum.timer ]; then
    journal_vacuum_policy_state=drift
    return
  fi
  vacuum_service_hash=$(timeout 3s sha256sum /etc/systemd/system/warp-journal-vacuum.service 2>/dev/null) || return
  vacuum_timer_hash=$(timeout 3s sha256sum /etc/systemd/system/warp-journal-vacuum.timer 2>/dev/null) || return
  vacuum_service_hash=${vacuum_service_hash%% *}
  vacuum_timer_hash=${vacuum_timer_hash%% *}
  if [ "$vacuum_service_hash" != fa8638d4be117b9743c19daacff52717e54f6b7df19eed1ac6773e9abac182b3 ] ||
     [ "$vacuum_timer_hash" != 70914fddcbdd70fa962dea5f2091b6c3cf9ab5abb66a727835d1c535d3e95802 ]; then
    journal_vacuum_policy_state=drift
    return
  fi
  journal_vacuum_policy_state=valid
  vacuum_timer_active=$(journal_vacuum_property "$vacuum_timer" ActiveState) || return
  vacuum_timer_enabled=$(journal_vacuum_property "$vacuum_timer" UnitFileState) || return
  if [ "$vacuum_timer_active" != active ] || [ "$vacuum_timer_enabled" != enabled ]; then
    journal_vacuum_runtime_state=failed
    return
  fi
  # /proc/uptime includes suspend time; systemd's monotonic timestamps do not.
  # Python3 is an owning Ansible host prerequisite; absence is unknown, not zero.
  vacuum_now_us=$(timeout 3s python3 -c 'import time; print(time.clock_gettime_ns(time.CLOCK_MONOTONIC) // 1000)' 2>/dev/null) || return
  vacuum_timer_enter=$(journal_vacuum_property "$vacuum_timer" ActiveEnterTimestampMonotonic) || return
  vacuum_start=$(journal_vacuum_property "$vacuum_service" ExecMainStartTimestampMonotonic) || return
  vacuum_end=$(journal_vacuum_property "$vacuum_service" ExecMainExitTimestampMonotonic) || return
  for vacuum_timestamp in "$vacuum_now_us" "$vacuum_timer_enter" "$vacuum_start" "$vacuum_end"; do
    case "$vacuum_timestamp" in ''|*[!0-9]*) return ;; esac
    [ "${#vacuum_timestamp}" -le 16 ] || return
    [ "$vacuum_timestamp" -le "$vacuum_now_us" ] || return
  done
  vacuum_active=$(journal_vacuum_property "$vacuum_service" ActiveState) || return
  vacuum_result=$(journal_vacuum_property "$vacuum_service" Result) || return
  vacuum_code=$(journal_vacuum_property "$vacuum_service" ExecMainCode) || return
  vacuum_status=$(journal_vacuum_property "$vacuum_service" ExecMainStatus) || return
  if [ "$vacuum_active" = activating ] || [ "$vacuum_active" = active ]; then
    if [ "$vacuum_start" -gt 0 ] && [ "$((vacuum_now_us - vacuum_start))" -le 35000000 ]; then
      journal_vacuum_runtime_state=running
    else
      journal_vacuum_runtime_state=failed
    fi
  elif [ "$vacuum_active" = inactive ] && [ "$vacuum_result" = success ] &&
       [ "$vacuum_code" = 1 ] && [ "$vacuum_status" = 0 ] &&
       [ "$vacuum_start" -gt 0 ] && [ "$vacuum_end" -ge "$vacuum_start" ]; then
    # Round up: fractional age beyond seven minutes must not certify freshness.
    journal_vacuum_last_success_age_seconds=$(( (vacuum_now_us - vacuum_end + 999999) / 1000000 ))
    if [ "$journal_vacuum_last_success_age_seconds" -le 420 ]; then
      journal_vacuum_runtime_state=complete
    else
      journal_vacuum_runtime_state=stale
    fi
  elif [ "$vacuum_start" -eq 0 ] && [ "$vacuum_code" = 0 ] &&
       [ "$vacuum_timer_enter" -gt 0 ] && [ "$((vacuum_now_us - vacuum_timer_enter))" -le 130000000 ]; then
    journal_vacuum_runtime_state=pending
  else
    journal_vacuum_runtime_state=failed
  fi
}
journal_vacuum_observe

# A package label is affirmative only for the vendor source audited against
# upstream #33944. Never infer missing backports from the major version alone.
journal_retention_build=unverified
if journal_systemd_package=$(timeout 3s dpkg-query --show '--showformat=${Version}' systemd 2>/dev/null); then
  if [ "$journal_systemd_package" = 255.4-1ubuntu8.17 ]; then
    journal_retention_build=known-affected
  fi
fi
journal_retention_rotation_state=unavailable
journal_retention_rotations_5m=0
journal_retention_lines=$(timeout 5s journalctl -b -u systemd-journald.service \
  -n 401 --since '5 minutes ago' --no-pager --quiet -o cat \
  --grep='^Retention time reached, rotating[.]$' 2>&1)
journal_retention_status=$?
if [ "$journal_retention_status" -eq 0 ] ||
   { [ "$journal_retention_status" -eq 1 ] && [ -z "$journal_retention_lines" ]; }; then
  journal_retention_count=$(printf '%s\n' "$journal_retention_lines" | LC_ALL=C awk '
    $0 == "" {next}
    $0 != "Retention time reached, rotating." {invalid=1; exit 1}
    {count++; if (count > 401) {invalid=1; exit 1}}
    END {if (!invalid) print count+0}
  ')
  if [ "$?" -eq 0 ]; then
    journal_retention_rotations_5m=$journal_retention_count
    if [ "$journal_retention_count" -ge 401 ]; then
      journal_retention_rotation_state=truncated
    else
      journal_retention_rotation_state=complete
    fi
  fi
fi
`
