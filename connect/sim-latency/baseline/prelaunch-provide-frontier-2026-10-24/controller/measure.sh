#!/bin/bash
# Exploratory public-seed evidence, not an authenticated epoch baseline.
set -Eeuo pipefail
source <(sed '/# --- dispatch ---/,$d' /usr/local/libexec/competition-container)
attest_job_services
export APEX_SERVER_ROOT=/workspace/server
impairment_args=()
case "$FRONTIER_IMPAIRMENT" in
  on) ;;
  off) impairment_args=(--no-impair) ;;
  *) exit 2 ;;
esac
exec /opt/urnetwork/bin/sim-latency run --epoch=0 \
  --source-config=/opt/urnetwork/sim-latency.yml --repos-root=/workspace \
  --providers=/input/providers.yml --site-home=/work/site \
  --ramp=60s --prewarm=13h --settle=60s --duration=180s \
  --request-timeout=120s --client-warmup-timeout=20m --fleet-shards=4 \
  --hosts=4 --api-port=7640 --site-listen=127.0.0.1:0 \
  --pipeline-interval=10s --test-timeout=3s --announce-timeout=2s \
  --reset --meta=/artifacts/run.json --evaluation-id="$EVALUATION_JOB_ID" \
  "${impairment_args[@]}" > /artifacts/results.csv
