// Shared acquisition for the five direct bundled-child probes. Only a complete
// native listener census may authorize child presence or absence; raw socket
// rows stay on the remote selector pipe and never enter the returned sample.
package monitor

// The status descriptor is distinct from source bytes. The selector emits only
// in END, after the native source writes its status and closes the input pipe.
// Each stage must complete before its output acquires observation authority.
const observabilityListenerScript = `observability_listener_ports() {
  listener_ports=$(
    exec 3>&1
    {
      ss -ltnH 2>/dev/null
      listener_source_status=$?
      printf 'listener_source_status %s\n' "$listener_source_status" >&3
    } | awk -v address="$1" '
      NF == 0 {next}
      $1 != "LISTEN" || NF < 5 {invalid=1; next}
      index($4, address ":") == 1 {
        port=substr($4, length(address)+2)
        if (port !~ /^[0-9]+$/ || length(port) > 5 || port+0 < 1 || port+0 > 65535) {
          invalid=1
          next
        }
        ports[port+0]=1
      }
      END {
        if (invalid) exit 41
        for (port in ports) print port
      }
    '
  )
  listener_projection_status=$?
  if [ "$listener_projection_status" -ne 0 ]; then
    return 1
  fi
  case "$listener_ports" in
    'listener_source_status 0') listener_ports='' ;;
    'listener_source_status 0
'*) listener_ports=${listener_ports#*'
'} ;;
    *) return 1 ;;
  esac
  # Sorting is a separate terminal stage: it cannot hide a failed selector.
  listener_sorted=$(printf '%s\n' "$listener_ports" | sort -n -u)
  listener_sort_status=$?
  if [ "$listener_sort_status" -ne 0 ]; then
    return 1
  fi
  printf '%s\n' "$listener_sorted"
}
`
