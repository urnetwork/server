# Four-exchange-instance frontier — October 24

These are public-seed exploratory capacity attempts, **not an official baseline**.
These capacity trials are not a predeclared nine-run calibration set. Their
latency values must not enter an epoch baseline, significance calculation or
ranking. Keeping unsuccessful attempts prevents selective reporting. All exchange
instances run on the single evaluation host, not four physical machines.

| Providers | Clients | Impairment | Completion | Reason |
| --- | --- | --- | --- | --- |
| 7,200 | 200 | off | incomplete | `unclean_teardown`: shard 0 required KILL after TERM |
| 15,000 | 200 | on | incomplete | `matchmaking_probe_failed`: provider lookup exhausted its HTTP attempts |
| 1,800 | 200 | on | complete | controller exit 0; 461 seconds, clean teardown |
| 1,800 | 200 | off | complete | controller exit 0; 435 seconds, clean teardown |
| 7,200 | 200 | on | simulation complete; controller failed | controller exit 127 after a live script edit displaced Bash's unread tail |

All points used four exchange hosts, four fleet shards, 80 arrivals/minute, a public
seed of 20261024, quality window 2, a 60-second ramp, synthetic 13-hour
reliability prewarm, 60-second settle, a 180-second measurement window, and
120-second request timeouts. All warmed all 200 clients. The 15,000-provider
point stopped during measurement. The 7,200-provider point completed measurement
but failed teardown; completed measurement alone is not a successful evaluation.

The two 1,800-provider trials are usable capacity evidence, not a frozen scale or
threshold. The 7,200-provider on trial completed the simulation, wrote its
completion marker and drained every shard without intervention. Its host
controller was edited while its long Docker command was running; after that
command returned, Bash read a shifted tail fragment (`nput/measure.sh`) and
returned 127. Preserve that exit status, not an invented exit 0 or a claim of
submission tampering. The private calibration now uses a separate root-owned,
read-only controller bundle with before/after digests. Deterministic Go tests use
FIFO barriers to reproduce the live-script failure and verify the snapshot
mitigation. No signal was delivered to this trial either.

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
overlap Go builds or tests. All points' Docker stacks were removed. A late
shutdown stack-capture controller was armed for the 15,000-provider point but
observed all shards exit, so **no diagnostic signal was delivered**.

Three earlier harness attempts (1,800 providers on/off and 7,200 on) stopped
before simulation because the container lacked `EVALUATION_JOB_ID`. They are
not measurement samples. Their small failure logs are retained alongside this
archive; their provider inputs are reproducible with the same public seed.
The larger sweep is held between points while restart fixes are tested and
the shutdown failure is investigated. No final scale or margin is frozen here.
