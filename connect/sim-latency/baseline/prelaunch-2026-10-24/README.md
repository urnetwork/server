# October 24 pre-launch exploration — not an official epoch baseline

Nine same-seed replicates completed against prepared source `67c87445a` and
evaluator image `sha256:826569fdeb02f72ec4f24ace7c93b16b69a60e1875c31799c5a0217b402e0d6c`.
The full ten-repository identity is in `source-record.json`; the exact image
record and image-input source manifest are retained alongside it.

This dataset uses disposable synthetic PostgreSQL/Redis and local-only fixture
resources. No main stores, live credentials, hidden official seed, or invented
API/worker image identity is included. The seed is public (`20261024`). This is
noise-floor evidence, not a scored submission, authenticated control sample,
production qualification, or authority to open the competition. The configured
16.1% historical floor remains provisional.

## Workload and results

1,800 providers; 200 clients; 80 arrivals/minute; quality window 2; four fleet
shards; **one exchange host**; impairment enabled; 60-second ramp; 13-hour **synthetic** reliability
prewarm; 60-second settle; 180-second measured window; 120-second request timeout.
All nine established the full 200-client pool. There were 21,849 measured
requests and 34 failures (0.156% overall). All replicates are preserved, including
the slower seventh replicate; no run was discarded to tighten the estimate.

The configured competition workload has four exchange hosts. This one-host
exploration is not an exact-workload production calibration; use a separately
retained four-host frontier/calibration before freezing production scale.

The primary run-level p95 latency has mean **18,786.12 ms**, standard deviation
**2,254.11 ms**, and between-run CV **12.00%**. The median of the nine run-level
p95 values is **18,206.59 ms**. Mean and median are different statistics: the
production significance test uses run-level means/variances; ranking uses the
scorer's aggregate raw latency. These samples are not yet an official score.

Threshold/fringe comparisons, explicitly illustrative rather than frozen:

| Comparison | Required candidate position |
| --- | --- |
| Historical 16.1% margin applied to this median | At most **15,275.33 ms** aggregate latency |
| Equal-noise R=9-per-side significance estimate from this noise floor | About **1,975.95 ms / 10.52%** lower mean; approximately **16,810.17 ms** candidate mean |

The latter is the calibration estimator, not a promise about any submission:
the actual one-sided Welch test recomputes its fringe using that candidate's
variance and requires `p <= 0.05`. Production also requires its frozen practical
margin and all integrity/resource gates. A smaller numerical result alone does
not establish a significant winner. The SD estimate itself has approximately
25% sampling uncertainty at nine runs; independent/reference and live
qualification remain separate work.

Build/test work ran only between replicates, with explicit pauses after 3 and
5. No timed replicate was interrupted. The retained private orchestration log
SHA-256 is `ea3783c6871dc4477837ad34b9ebda260690e194a244a4080ea486187a3f6aca`.

## Retained evidence

- `runs/`: original CSVs, run metadata, and completion markers, byte-for-byte;
- `analysis/`: file-only Go analyzer output for each retained CSV;
- `input/providers.yml`: complete generated synthetic fleet and public seed;
- `baseline.json`: nine-run noise statistics and convergence diagnostics;
- `source-manifest.yml`, `source-record.json`, `image-record.json`: provenance;
- `INDEX.json`: machine-readable dataset scope and key hashes.

The parent `../MANIFEST.sha256` authenticates all files. Run the parent
`../verify.sh` for exact coverage and checksums. `v1/` was not modified. Raw
metadata retains original container paths; mounting this directory at
`/artifacts` recreates the `runs/` layout for file-only analysis in the pinned
image. No service connection is needed to analyze retained CSVs.
