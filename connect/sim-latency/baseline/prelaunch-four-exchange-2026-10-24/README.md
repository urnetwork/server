# Four-exchange-instance calibration — October 24, 2026

Nine predeclared fresh-service runs completed, without replacements, on the
frozen October 24 source. This is **public-seed exploratory evidence**, not an
authenticated epoch scoring baseline. It does not freeze scale or a margin and
cannot replace fresh hidden-seed controls for a real competition round.

The topology is four exchange instances on **one physical evaluation host**,
not four machines. Each run uses 1,800 providers, 200 clients, 80 arrivals/minute,
quality window 2, four fleet shards, impairment enabled, seed 20261024, a
60-second ramp, synthetic 13-hour reliability prewarm, 60-second settle,
180-second measurement window, 120-second request timeout and 20-minute warmup
cap. PostgreSQL/Redis are newly created and removed for every replicate.
All 200 clients warmed in every run. No Go builds or tests overlapped timing.

## What the measurements say

`failure-charged-calibration.json` calls the scorer's actual row reducer on the
retained CSVs. Failed requests contribute the full 120-second timeout to p95;
they are not dropped. CSV digests/lengths and measured counts match each sidecar.
The reducer is unchanged from the evaluated server commit.

| Diagnostic | Observation | Illustrative improvement fringe |
| --- | --- | --- |
| Median run p95, failure-charged | 17,597.57 ms | at most 14,764.36 ms with the historical 16.1% margin |
| Mean per-run TTFB p50 | 152.77 ms | at most 128.17 ms with a 16.1% reduction |
| Mean per-run download throughput p50 | 4.672 Mbps | at least 5.424 Mbps with a 16.1% increase |

The raw p95 sample mean is 17,967.77 ms, sample SD 1,753.81 ms and CV 9.76%.
There are 22,112 measured requests, including 20 failures (0.0904%). Under an
equal-variance approximation, nine runs per side have a one-sided 5% Welch
mean-difference boundary of about 8.03%. That approximation is **not** the
submission's significance decision: the real candidate variance and every gate
still matter. The 16.1% historical floor is illustrative, not newly calibrated.
TTFB/throughput are descriptive successful-transfer metrics, not extra scoring
axes; the throughput is from this impaired synthetic workload, not a product
WAN-speed claim.

The nine whole-run durations were 442, 452, 441, 436, 446, 424, 455, 431 and
430 seconds: 3,957 seconds in total (65.95 minutes). This is useful runtime
evidence, not a guarantee that every candidate or hidden seed fits that duration.

## Full controller outcome, including its error

All nine measurement controllers returned 0, wrote completion markers and
cleaned up. The enclosing coordinator subsequently returned **1**: its optional
host-side descriptive-analysis command omitted the explicit source-config path.
That original diagnostic is retained as `logs/descriptive-analysis.log`.

Analysis was recovered separately inside the same digest-pinned evaluator image,
offline and read-only, using explicit `/opt/urnetwork/sim-latency.yml` and
`/workspace` arguments. Only the nine completed sidecars and a private output
leaf were mounted; no main config/vault or services were exposed. This produced
`descriptive-baseline.json`; its separate log is retained too. **No simulation was
rerun and no sample was discarded.** The descriptive file's success-only p95 is
not interchangeable with the failure-charged raw score above.

The `controller/` scripts are exact historical evidence, not a current runbook.
They contain their original private paths and the original post-processing
mistake. They ran from a separate root-owned, mode-0440 snapshot with before/after
digest checks, avoiding the earlier mutable-shell unread-tail defect.

## Retained identity and limits

The server is `67c87445abe6725b25b3df2a13af7eb99cd4f8ed`; image ID is
`sha256:826569fdeb02f72ec4f24ace7c93b16b69a60e1875c31799c5a0217b402e0d6c`.
The complete ten-repository source, source manifest and image record are included.
`input/providers.yml` is the actual shared public-seed input, checked in every
run; each run's CSV, sidecar, final marker and full compressed diagnostics are
retained. Gzip is deterministic and decompresses to the original log bytes.

No official accounting/resource evidence or hidden round identity is invented
here. Frontier completion, independent-seed/reference calibration, exact-image
production qualification and activation remain separate launch gates. The older
`../prelaunch-2026-10-24/` nine-run set used one exchange instance and must not be
mixed with these four-instance measurements.
