# Corrected evaluator frontier — exploratory, not official

One completed public-seed capacity trial: 1,800 providers, 200 warmed clients,
80 arrivals/minute, four exchange instances, four fleet shards, impairment off,
seed 20261024, 180-second measurement window. The corrected image is
`sha256:79400f3375c64767f5021e43ef852e9ff9da3ca5a4b79dc03e8e9ed381247eb9`;
frozen server is `3d96d16ddb4fc2fb08d3f9003c52e30b9a47241d`.

The controller and simulation exited 0; all 200 clients warmed. The retained
CSV contains 2,744 measured requests and zero failures. The final marker binds
the exact sidecar; the sidecar binds the exact CSV. All four child processes
received `--no-impair`, independently retained in `process-argv.txt`.
Owned containers and the Compose network were removed. No signal was delivered
for diagnostics and no test/build ran during measurement.

The initial preparation failed before simulation because the standalone
controller omitted source sealing. `preparation-failure.log.gz` preserves it;
it is not a measurement. The separate corrected attempt is `source-seal-fix`.
See `../../launch/CORRECTED-FRONTIER-2026-10-24.md` for controller provenance.

This single trial does not establish repeated-run stability, an official
baseline, final scale, reference separability, a significance margin or full
runtime qualification. No threshold is inferred from this one sample. Do not
compare these impairment-off latencies as product improvements over historical
impairment-on measurements. Additional scale trials remain in progress.

## 7,200-provider impairment-off point

`p7200-off/` preserves the separate larger trial using the same image,
200 clients, seed, ramp/settle/measurement durations and workload rate.
It completed with controller exit 0, all 200 clients warmed, 2,744 measured
requests, zero failures and clean teardown. The sidecar's CSV digest and the
completion marker's sidecar digest match the retained files. The source-sealing
controller's SHA-256 remained unchanged before/after execution.

Preparation took longer than the smaller point. Acknowledgment-lifetime
expirations occurred during client establishment; they did not prevent warmup
or a completed measured run. This single success does not prove stability or
make this the final competition scale. The 15,000-provider true-off trial was
started only after this point exited and cleanup was verified.
## 15,000-provider impairment-off point

`p15000-off/` retains the next point using the same corrected image and
protocol. Controller exit was 0; all 200 clients warmed; final marker/sidecar/
CSV hashes matched; and owned services/network were removed. Of 2,189 requests
starting in the measurement window, 35 failed (1.5989%). The CSV contains
2,626 total rows, including requests outside that window; these denominators
must not be conflated. The failure rate is below the scorer's 3% absolute
baseline rejection floor, but this single point establishes neither repeated-run
stability nor the other scoring gates. Failures must remain charged when
computing the raw score; the sidecar's descriptive latency is not that score.

This true-off result does not reproduce the old defective image's fatal probe
failure. It also does not prove impairment caused that historical failure.
The corresponding corrected impairment-on point was started only after clean
teardown. Final scale and margin remain unselected.

## 15,000-provider impairment-on rejection

`p15000-on/` preserves the corrected image's corresponding impaired attempt.
It warmed all 200 clients (the first pass left 24 missing; the existing
missing-only retry recovered them), but failed matchmaking probe 6 during
measurement: HTTP request attempts exhausted. Teardown additionally required
KILL for shard 1 after it failed to exit on TERM. Controller exit was 1;
the sidecar is incomplete and there is no completion marker. Compose cleanup
removed every owned container and network, and the controller checksum was
unchanged. This point is not eligible for a baseline or a placeable score.

The partial CSV has 637 requests and 97 failures (15.23%). This is partial
failure evidence, not a complete-window estimate. Service diagnostics contain
provide-key uniqueness errors and a PostgreSQL deadlock during registration;
their causal relationship to the fatal matchmaking probe is not established.
An early resource snapshot observed about 10.08 GiB / 72 GiB and 1.44 CPU cores;
it is not a peak-resource measurement or proof that later overload was absent.

The parent also started prewarming before all child constructors completed.
That startup-ordering observation is not yet a reproduced root-cause defect.
Do not claim a fix, change the frozen source or infer that impairment alone
caused every historical failure from these observations. The next lower
7,200-provider impairment-on point started after this attempt exited and
cleanup was independently checked; no parallel scoring, resampling or timeout
relaxation was introduced.
