# Epoch 2 summary

**Two of six epochs are complete.** Round 2 published 48 terminal entries:
47 scored submissions and one unscored entry. **45 of 48 entries (93.8%)**
met scoring eligibility at the seal, before integrity review. The approved
winner cut p95 full-request completion time by **98.32%** versus the shared
epoch 2 baseline, from **6.959 s to 117.0 ms**.

## Results against the epoch 2 baseline

| Metric | Shared baseline | Approved winner | Change |
| --- | ---: | ---: | ---: |
| p95 full-request completion time (official raw score) | 6,958.769 ms | 117.002 ms | **98.32% lower** |
| Mean of the nine replicate raw scores | 7,039.836 ms | 117.443 ms | **98.33% lower** |
| Request success rate | 100% | 100% | Unchanged |
| Median request count | 3,100 | 3,251 | 4.87% higher |
| Median bytes received | 3,865,199,778 | 4,068,555,100 | 5.26% higher |
| Expected-page and expected-byte coverage (G7) | 100% / 100% | 100% / 100% | Unchanged |

The official raw score is the median across nine replicates of each run's
failure-penalized p95 full-request time. The significance test compares the
replicate means, which is why the two percentage reductions differ slightly.
The winner passed all seven gates, exceeded the **26.9% takeover margin**, and
passed the one-sided Welch test at alpha 0.05 (`p = 3.55 × 10⁻¹¹`).
Its normalized competition score was **200**.

These are simulated workload results, not deployed product gains. The 98.32%
figure is **not** an improvement in time to first byte (TTFB), and the increase
in bytes received is **not** a transfer-throughput measurement. Do not use
either as a TTFB or Mbps headline. The comparison is against the epoch 2
baseline, not the start-of-competition baseline.

## Why the fastest submission was disqualified

The submission ranked **first by measured performance**, but the integrity
review disqualified it for two correctness problems in its default packet
delivery path:

1. **Premature success.** The new path could report success before confirming
   delivery. A caller could therefore see success without the delivery
   guarantee provided by the baseline.
2. **Unclear responsibility during shutdown.** The new path did not preserve
   which part of the system was responsible for pending packets during shutdown.
   As a result, it did not preserve the baseline's reliable delivery guarantee
   for work still in flight.

Passing the scoring gates did not override these findings: a faster result
must still preserve delivery behavior. The review did **not** find fabricated
measurements or intentional workload suppression.

## Winner and source promotion

The second-ranked entry in the sealed performance table, job
`01a1e7b9-0b4e-cec4-3fad-526342ab01ac`, became the published winner.

The official Astra 6.2 max review approved the winner: it improved benchmark
performance while preserving delivery semantics. Its unchanged patch was
approved as the isolated epoch 3 competition source, not for product release.
As checked on November 20, 2026, the public API reports both integrity review
and source approval as `approved`, with reward settlement still `pending`.

Round 2 accepted submissions from **November 10 at 04:28 UTC** to
**November 17 at 04:28 UTC**, and sealed results on **November 18 at 14:35 UTC**.
See the [public round result](https://api.bringyour.com/competition/v2/rounds/01a1c110-585b-ce38-4f29-ca4860a1c058/result)
for the sealed entries, gate diagnostics and review decisions, and the
[epoch source index](../EPOCH-SOURCE.md#production-epoch-3--source-checkpoint)
for the promoted checkpoint.

Suggested website copy:

> **2/6 epochs complete.** Round 2 scored 47 submissions. The approved winner
> cut p95 full-request completion time from 6.96 s to 117 ms — 98.3% lower
> versus the round baseline — while preserving 100% request success and full
> expected-work coverage in the benchmark. The patch moves forward as the
> next competition baseline; it is not yet a production release.
