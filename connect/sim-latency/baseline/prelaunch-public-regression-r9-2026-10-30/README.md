# Public H1 regression pilot: rejected qualification

This is a completed **failed calibration attempt**, not an accepted reference,
official production epoch baseline, or authority to release scoring. The native
verifier retained `accepted: false`; no successor direction receipt or hidden
seed was produced. All original files and the earlier failed screen remain intact.

The public seed was 20261028. The [complete prospective plan](../../launch/public-pilot-native-2026-10-29/plan.json)
was committed and pushed before measurement in server commit
84319a1aa4161359a29e8962961d3f62e5c2cd5f. The frozen source, evaluator image,
R9, alpha 0.05, 26.9% margin, and 10,800-second deadline were unchanged.

## Actual terminal evidence

- Nine fresh controls and nine candidates completed between October 29
  21:27:46 UTC and October 30 00:27:09 UTC: 10,763 seconds, evaluator exit 0.
- Cleanup restored the exact original three containers and four networks.
- The separate native verification exited 1 after 93.21 seconds. Its protected
  proof reports `public pilot is partial, failed or not placeable` because
  G1 and G2 failed; G3, G4, G5 and G6 passed. This was not a timeout or crash.
- The intentionally regressed H1 writer used one microsecond per user-frame byte.
  It produced a large real regression but was too aggressive for the required
  success/work-volume guards. This is not evidence of a defect in the frozen
  baseline implementation or proof that every failed request has one cause.

## Measurements and required improvement fringe

Raw latency is the per-run scored latency, aggregated over nine replicates.
The minimum takeover boundary shown below applies the unchanged 26.9% margin;
a real improvement must additionally pass all gates and the one-sided Welch test
at alpha 0.05. Its final significance fringe depends on its own variance.

| Scored latency | Fresh control | Deliberate regression | Minimum takeover boundary, lower is better |
| --- | ---: | ---: | ---: |
| Median raw score | 19,254.257 ms | 41,535.195 ms | 14,074.862 ms |
| Mean raw score | 20,102.578 ms | 41,502.475 ms | 14,694.985 ms, before any larger variance-derived requirement |

For this comparison the variance-derived fringe was 24.1454%, below the 26.9%
practical margin. The measured mean improvement was **negative 106.4535%** and
the one-sided improvement p-value was 0.9999990471. The regression was not
statistically significant in the improvement direction and was not eligible.

| Guard metric, medians across replicates | Fresh control | Candidate | Frozen acceptance boundary | Result |
| --- | ---: | ---: | ---: | --- |
| Success rate | 99.933884% | 98.843284% | At least 98.933884%: max(97%, control minus one percentage point) | Fail |
| Requests | 2,954 | 2,662 | 2,806.3 through 3,101.7, inclusive | Fail, down 9.8849% |
| Received bytes | 3,809,115,566 | 3,397,479,478 | 3,618,659,787.7 through 3,999,571,344.3, inclusive | Fail, down 10.8066% |

These G2 failures are at the **lower** bounds. They do not change or resolve the
separate concern about genuine improvements crossing the upper +5% bounds.
No gate was weakened, and a gate failure is not by itself evidence of dishonesty.

## Preservation and scope

`MANIFEST.sha256` covers the exact native nine-control baseline, nine-candidate
score diagnostics, rejected proof, runner terminal, and original/restored host
inventories. These generated records retain their original bytes, including
their original newline conventions. Full raw artifacts and any private ancestor
catalogs remain protected on the evaluator host; this subset does not claim
complete replay-tree retention, a new MinIO retained version, or replica delivery.

The owner subsequently approved one reduced-strength reference and one new
prospectively declared public pilot, without changing submission rules or
authorizing a hidden-seed resample. That approval does not relabel this result.
