# Four-exchange-instance frontier — October 24

These are public-seed exploratory capacity attempts, **not an official baseline**.
These capacity trials are not a predeclared nine-run calibration set. Their
latency values must not enter an epoch baseline, significance calculation or
ranking. Keeping unsuccessful attempts prevents selective reporting. All exchange
instances run on the single evaluation host, not four physical machines.

**Mode correction discovered at 18:01 UTC:** the frozen executable does not
forward `--no-impair` to sharded fleets, and those child processes default to
impairment enabled. The table and raw sidecars record the **requested** mode;
every requested-off condition below is invalid, including clean whole-process
exits. Those attempts cannot support an on/off contrast or selection of an
impairment-off scale. The raw artifacts remain unchanged. `INDEX.json` marks
`impairment_mode_valid: false` and excludes them from eligible capacity points.
The nine-run, explicitly impairment-on calibration is not affected by this bug.

| Providers | Clients | Requested impairment | Completion | Reason |
| --- | --- | --- | --- | --- |
| 7,200 | 200 | off | incomplete | `unclean_teardown`: shard 0 required KILL after TERM |
| 15,000 | 200 | on | incomplete | `matchmaking_probe_failed`: provider lookup exhausted its HTTP attempts |
| 1,800 | 200 | on | complete | controller exit 0; 461 seconds, clean teardown |
| 1,800 | 200 | off | complete | controller exit 0; 435 seconds, clean teardown |
| 7,200 | 200 | on | simulation complete; controller failed | controller exit 127 after a live script edit displaced Bash's unread tail |
| 7,200 | 200 | off | complete, separate fresh attempt | immutable controller exit 0; clean teardown; no diagnostic signal |
| 15,000 | 200 | off | incomplete | probe 6 failed; off mode was not honored by fleet children |

All points used four exchange hosts, four fleet shards, 80 arrivals/minute, a public
seed of 20261024, quality window 2, a 60-second ramp, synthetic 13-hour
reliability prewarm, 60-second settle, a 180-second measurement window, and
120-second request timeouts. All warmed all 200 clients. The 15,000-provider
point stopped during measurement. The 7,200-provider point completed measurement
but failed teardown; completed measurement alone is not a successful evaluation.

Only the clean 1,800-provider impairment-on trial is currently an eligible
capacity point under its requested condition, not a frozen scale or threshold.
The 7,200-provider on trial completed the simulation, wrote its
completion marker and drained every shard without intervention. Its host
controller was edited while its long Docker command was running; after that
command returned, Bash read a shifted tail fragment (`nput/measure.sh`) and
returned 127. Preserve that exit status, not an invented exit 0 or a claim of
submission tampering. The private calibration now uses a separate root-owned,
read-only controller bundle with before/after digests. Deterministic Go tests use
FIFO barriers to reproduce the live-script failure and verify the snapshot
mitigation. No signal was delivered to this trial either.

The separate `p7200-off-handoff-fix` attempt completed at approximately
17:33 UTC. It used the root-owned immutable controller and the same public seed,
fresh services and workload flags; all 200 clients warmed and all four shards
drained without forced termination. The diagnostic observer found the runner
already gone before its late stack-capture point, so no signal was sent. Its
sidecar, completion marker, exact CSV, compressed providers and complete log are
retained here. The original failed attempt remains unchanged and still counts
as a failure: this single success does not resolve or explain an intermittent
shutdown defect and must not be substituted into a calibration set.
Its requested off condition was later invalidated by the mode-propagation
finding above; a clean exit does not repair incorrect experiment configuration.

The later 15,000-provider requested-off attempt warmed 200 clients, then failed
probe 6 during measurement and cleaned up, with controller exit 1. Although that
resembled the earlier on failure, it does **not** rule out impairment: both
actually ran impaired fleet children. Preserve the failure; rerun a true off
condition only after the fixed executable is built and qualified.

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
