# Four-host frontier — incomplete points, October 24

These are public-seed exploratory capacity attempts, **not an official baseline**.
Neither measured point passed every correctness/completion gate. Their latency
values must not enter a baseline, significance calculation, ranking or launch
readiness claim. Keeping failed attempts prevents selective reporting.

| Providers | Clients | Impairment | Completion | Reason |
| --- | --- | --- | --- | --- |
| 7,200 | 200 | off | incomplete | `unclean_teardown`: shard 0 required KILL after TERM |
| 15,000 | 200 | on | incomplete | `matchmaking_probe_failed`: provider lookup exhausted its HTTP attempts |

Both used four exchange hosts, four fleet shards, 80 arrivals/minute, a public
seed of 20261024, quality window 2, a 60-second ramp, synthetic 13-hour
reliability prewarm, 60-second settle, a 180-second measurement window, and
120-second request timeouts. Both warmed all 200 clients. The 15,000-provider
point stopped during measurement. The 7,200-provider point completed measurement
but failed teardown; completed measurement alone is not a successful evaluation.

The frozen evaluated server is
`67c87445abe6725b25b3df2a13af7eb99cd4f8ed`; the evaluator image is
`sha256:826569fdeb02f72ec4f24ace7c93b16b69a60e1875c31799c5a0217b402e0d6c`.
The complete checkpoint and image records are preserved in the adjacent
`../prelaunch-2026-10-24/` archive. That earlier nine-run dataset used **one**
exchange host and is not a replacement for this topology's calibration.

Each point preserves exact CSV and sidecar bytes, its provider input, and full
runner diagnostics. Gzip files use deterministic compression; decompression
recovers the original bytes. `measure.sh` is the actual common in-container
measurement command. All services were disposable synthetic local-only fixtures;
no main config/vault trees entered the evaluator. Timed measurements did not
overlap Go builds or tests. Both points' Docker stacks were removed. A late
shutdown stack-capture controller was armed for the 15,000-provider point but
observed all shards exit, so **no diagnostic signal was delivered**.

Three earlier harness attempts (1,800 providers on/off and 7,200 on) stopped
before simulation because the container lacked `EVALUATION_JOB_ID`. They are
not measurement samples. Their small failure logs are retained alongside this
archive; their provider inputs are reproducible with the same public seed.
The larger sweep is held between points while restart fixes are tested and
the shutdown failure is investigated. No final scale or margin is frozen here.
