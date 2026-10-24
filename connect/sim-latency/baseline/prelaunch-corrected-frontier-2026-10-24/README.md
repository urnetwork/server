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
