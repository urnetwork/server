#!/bin/bash
# Postbridge opt-in only. The native driver authenticates its own binary/unit
# receipts and owns the canonical scoring lock through all Go/model work.
set -euo pipefail
umask 077

if [[ "${EUID}" != 0 || "${AUTHORIZE_SOURCE_TEST_SNOW_RUNTIME_QUALIFICATION:-}" != yes || "$#" != 2 ]]; then
  echo 'root + explicit source-test runtime authorization + CONFIG SHA256 required' >&2
  exit 2
fi
qualification_config="$1"
qualification_sha256="$2"
if [[ ! "$qualification_sha256" =~ ^[0-9a-f]{64}$ || "$qualification_config" != /* || "$(/usr/bin/readlink -f -- "$qualification_config")" != "$qualification_config" ]]; then
  echo 'canonical pinned source-test runtime configuration required' >&2
  exit 2
fi
if [[ "$(/usr/bin/sha256sum -- "$qualification_config" | /usr/bin/cut -d' ' -f1)" != "$qualification_sha256" ]]; then
  echo 'source-test runtime configuration digest mismatch' >&2
  exit 2
fi
qualification_program="$(/usr/bin/jq -er '.program.path' "$qualification_config")"
qualification_program_sha="$(/usr/bin/jq -er '.program.sha256' "$qualification_config")"
qualification_parent="$(/usr/bin/dirname -- "$(/usr/bin/jq -er '.output_directory' "$qualification_config")")"
if [[ ! "$qualification_program_sha" =~ ^[0-9a-f]{64}$ || "$qualification_program" != /usr/local/libexec/urnetwork/*/operator.test || "$(/usr/bin/readlink -f -- "$qualification_program")" != "$qualification_program" || "$(/usr/bin/sha256sum -- "$qualification_program" | /usr/bin/cut -d' ' -f1)" != "$qualification_program_sha" || "$(/usr/bin/stat -c '%u:%a' -- "$qualification_parent")" != 0:700 ]]; then
  echo 'qualified immutable native source-test runtime driver/root private parent required' >&2
  exit 2
fi

# A wrapper refusal/cancellation is retained, never retried with a fresh output
# directory. The driver's durable original deadline prevents budget resets.
qualification_stdout="$qualification_parent/execution.stdout"
qualification_stderr="$qualification_parent/execution.stderr"
set -o noclobber
exec 3>"$qualification_stdout"
exec 4>"$qualification_stderr"
set +o noclobber
exec /usr/bin/timeout --signal=TERM --kill-after=45s 10900s \
  /usr/bin/taskset -c 20,22 /usr/bin/env -i \
  PATH=/usr/local/go/bin:/usr/sbin:/usr/bin:/sbin:/bin LANG=C LC_ALL=C TZ=UTC \
  SIM_LATENCY_SOURCE_TEST_SNOW_RUNTIME_OPT_IN=1 \
  SIM_LATENCY_SOURCE_TEST_SNOW_RUNTIME_CONFIG="$qualification_config" \
  SIM_LATENCY_SOURCE_TEST_SNOW_RUNTIME_CONFIG_SHA256="$qualification_sha256" \
  "$qualification_program" -test.run='^TestSourceTestSnowRuntimeQualificationCommand$' -test.count=1 -test.timeout=3h1m \
  </dev/null >&3 2>&4
