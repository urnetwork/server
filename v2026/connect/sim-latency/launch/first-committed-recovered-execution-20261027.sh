#!/usr/bin/env bash
set -euo pipefail

# Run inside the approved 8 GiB management scope, never in an evaluator cgroup.
# Recovery must already have archived the failed start using its one-use gate.
/usr/local/libexec/urnetwork/competition-unmeasured-recovery-2b4bf937-20261027-v1/unmeasured-recovery \
  -memory-preflight-only \
  -config /var/lib/urnetwork/competition-first-committed-management-oom-20261027-v1/recovery-config.json \
  -config-sha256 6b60d757550636fb499f0a3520cb60df7cfc2c467566257566b60310ddffa475

export SIM_LATENCY_FIRST_COMMITTED_COMMAND=execute-first-committed
export SIM_LATENCY_FIRST_COMMITTED_REAL_OPT_IN=1
export SIM_LATENCY_FIRST_COMMITTED_CONFIG=/var/lib/urnetwork/competition-held-out-first-committed-c2-a9e8e1a7-20261027-v1/execution.json
export SIM_LATENCY_FIRST_COMMITTED_CONFIG_SHA256=d2415e29650bb291b7d4a3c08e129b670c6c452b9dbe795e14395fc5ff97702b
exec /var/lib/urnetwork/competition-held-out-first-committed-c2-a9e8e1a7-20261027-v1/operator.test \
  -test.run='^TestHeldOutFirstCommittedCommand$' -test.count=1 -test.timeout=9h30m
