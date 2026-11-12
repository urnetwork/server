#!/bin/bash
set -euo pipefail
umask 077
unit=competition-distributed-recovery-independent-qa-20261112.service
: "${QA_EVIDENCE:?}"
initial_cache=$(du -s -B1 /home/by/.cache/go-build | awk '{print $1}')
while [[ $(systemctl show "$unit" -p ActiveState --value) =~ ^(active|activating)$ ]]; do
  available=$(df -B1 --output=avail / | tail -1 | tr -d ' ')
  cache=$(du -s -B1 /home/by/.cache/go-build | awk '{print $1}')
  temporary=0; [[ ! -d $QA_EVIDENCE/tmp ]] || temporary=$(du -s -B1 "$QA_EVIDENCE/tmp" | awk '{print $1}')
  printf 'utc=%s root_free=%s cache_growth=%s tmp_bytes=%s\n' "$(date -u +%FT%TZ)" "$available" "$((cache-initial_cache))" "$temporary" >> "$QA_EVIDENCE/guard.log"
  if ((available < 35433480192 || cache-initial_cache >= 2147483648 || temporary >= 1342177280)); then
    printf 'GUARD_STOP exact_unit=%s\n' "$unit" >> "$QA_EVIDENCE/guard.log"
    # Signal only the launcher shell first so its ownership-checked EXIT trap cleans fixtures.
    systemctl kill --kill-who=main --signal=TERM "$unit"
    exit 2
  fi
  sleep 2
done
