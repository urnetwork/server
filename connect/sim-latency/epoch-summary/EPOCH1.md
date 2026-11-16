# Epoch 1 summary

URnetwork's first public sim-latency epoch completed with **69 scored
submissions**. **14 submissions (20.3%) passed the full scoring acceptance
threshold**: all six gates, the takeover margin, and statistical significance.
The elected winner passed the official honesty, safety, traffic-preservation,
and production-benefit review. **One of six epochs is complete.**

The winning submission reduced measured p95 full-request completion time by
**63.9%** and increased median transfer throughput by **103.5%** versus the
competition's starting baseline. These are competition benchmark results,
not a claim that the changes are already deployed to production or improve
every latency metric.

## Results at a glance

| Metric | Starting baseline | Winning submission | Change |
| --- | ---: | ---: | ---: |
| p95 full-request completion time — official raw-score objective | 19.556 s | 7.062 s | **63.9% lower** |
| Median transfer throughput | 4.987 Mbps | 10.150 Mbps | **103.5% higher** |
| Aggregate goodput | 152.455 Mbps | 157.542 Mbps | 3.3% higher |
| Median time to first byte (TTFB) | 172.277 ms | 284.084 ms | 64.9% higher — worse |
| p95 TTFB | 2,168.952 ms | 4,190.036 ms | 93.2% higher — worse |
| Request success rate | 99.964% | 100% | +0.036 percentage points |

TTFB and full-request completion are different measurements. The winner
improved completion time and throughput, but **did not improve TTFB**. Avoid
describing the outcome as a blanket reduction in latency.

The official raw score is the median across nine replicates of each run's
failure-penalized p95 full-request duration. Its 63.9% reduction is distinct
from the **62.4% reduction in mean replicate raw score** used by the statistical
comparison. That comparison exceeded the frozen **26.9% takeover requirement**
and passed the one-sided Welch test at alpha 0.05
(`p = 6.8673 × 10⁻⁷`). The 14 accepted submissions are scoring-eligible
submissions, not 14 independently honesty-approved winners.

## What the winner changed

The patch modifies 19 existing files in `connect`. Its main mechanisms are:

- Bounded scheduling of provider return traffic to reduce contention between
  packet flows while preserving already-read data.
- Less redundant replay and physical-frame coalescing to reduce unnecessary
  work and improve batching.
- Capped parallelism for cold connections and desynchronized discovery refresh
  to reduce synchronized stalls and contention.

The official review identified a real production execution path and found no
deliberate traffic suppression, benchmark detection, forged measurements,
protected-instrument tampering, or credential escape. It also recorded QoS
and high-bandwidth/high-RTT trade-offs; this is not a promise of universal
per-flow improvement.

## Publication and next steps

The submission window ran from **October 29, 2026 at 13:06 UTC** through
**November 5, 2026 at 13:06 UTC**. Scoring continued after intake closed.
The epoch is sealed, and all 69 result entries are publicly available,
including scores, gates, significance, and scoring eligibility. Public score
reveal does not mean every submitted patch body is public.

As verified on **November 9, 2026**, the API reports the winner's
`integrity_review` and `source_approval` as `approved`; settlement is still
`pending`. The reviewed epoch-2 checkpoint is now published to all ten
`sim-latency` branches; intake activation remains pending. See the
[epoch source index](../EPOCH-SOURCE.md#production-epoch-2--published-checkpoint-intake-not-yet-open).
The plan is to open epoch 2 for a fresh week with grading paused while its
fresh baseline and two-host evaluator qualification finish in parallel.

Epoch 2's approved rules remove both G2 volume ceilings while retaining
95% request/byte floors. A separate expected-work coverage gate will require
page and byte coverage of at least 95% of that epoch's baseline. Intentional
traffic suppression remains prohibited regardless of numerical tolerances.

Epoch 1's calibration remains **unaccepted**. The operator explicitly approved
the scoped finalization exception and official Astra max review; this summary
does not represent calibration as accepted or the incomplete Qwen/Gemma
honesty review as completed.

## Sources and identifiers

- [Public epoch 1 result](https://api.bringyour.com/competition/v2/rounds/01a1855d-49bd-0250-90db-d0c0885f0e46/result)
  — entries are under `seal.entries`; official score, significance, review
  states, and settlement status can be checked here.
- Winner job: `01a1a440-fd2e-538a-365c-f4b23eed4739`.
- [Official review summary](../launch/EPOCH1-ASTRA-OFFICIAL-REVIEW-2026-11-08.md).
- [Recorded score comparison](../launch/PROVISIONAL-WINNER-2026-11-07.json)
  — its historical pending-review/control fields are not current status.
- Throughput, goodput, and TTFB figures above are the retained evaluation
  diagnostics, supplementary to the public sealed raw-score comparison.
- [Epoch source index](../EPOCH-SOURCE.md).

Suggested short website copy:

> **1/6 epochs complete.** Epoch 1 scored 69 submissions; 14 passed every
> scoring acceptance requirement. The approved winner cut p95 full-request
> time from 19.56 s to 7.06 s (63.9% lower) and raised median throughput from
> 4.99 to 10.15 Mbps (103.5% higher), versus the starting benchmark baseline.
> These are evaluation results, not deployed product gains; TTFB did not improve.
