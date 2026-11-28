package monitor

// On-host privacy reducers read at most 64 KiB plus one overflow byte from
// process argv/environment. Byte rendering avoids shell NUL loss and awk
// implementations whose NUL record separator is not portable. Neither raw
// records nor rendered bytes may leave the owning on-host reduction command.
const boundedNulBytesReader = `
bounded_nul_bytes() {
  if monitor_nul_bytes=$(od -An -v -tu1 -N 65537 "$1" 2>/dev/null); then
    printf '%s\n' "$monitor_nul_bytes"
    return 0
  fi
  [ "${2-}" = allow-sudo ] || return 41
  case "$1" in
    /proc/*/environ|/proc/*/cmdline)
      monitor_nul_pid=${1#/proc/}
      monitor_nul_pid=${monitor_nul_pid%%/*}
      case "$monitor_nul_pid" in ''|0*|*[!0-9]*) return 41 ;; esac
      case "$1" in
        "/proc/$monitor_nul_pid/environ"|"/proc/$monitor_nul_pid/cmdline") ;;
        *) return 41 ;;
      esac
      ;;
    *) return 41 ;;
  esac
  if monitor_nul_bytes=$(sudo -n od -An -v -tu1 -N 65537 "$1" 2>/dev/null); then
    printf '%s\n' "$monitor_nul_bytes"
    return 0
  fi
  return 41
}
`

// Concatenate inside an awk program that defines observeNulRecord(value).
// Its final END rule must emit only fixed enums/allowlisted reductions and
// fail closed on nulInvalid; nulRecords counts complete NUL-delimited records.
// Include this fragment before that final END rule so the EOF check runs first.
const boundedNulRecordsAwk = `
BEGIN {nulBytes=0; nulRecords=0; nulRecord=""; nulLastByte=0; nulInvalid=0}
{
  for (nulField=1; nulField<=NF; nulField++) {
    nulBytes++; nulLastByte=$nulField+0
    if (nulBytes > 65536 || $nulField !~ /^[0-9]+$/ || nulLastByte > 255) {
      nulInvalid=1
      continue
    }
    if (nulLastByte == 0) {
      nulRecords++
      observeNulRecord(nulRecord)
      nulRecord=""
    } else nulRecord=nulRecord sprintf("%c", nulLastByte)
  }
}
END {if (nulLastByte != 0) nulInvalid=1}
`
