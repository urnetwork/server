# Warm-reader reference pilot: directional screen failed

The bounded evaluation completed successfully, but the proposed optimization
**did not qualify as a faster reference**. Both its median and mean raw score
were worse than its nine fresh controls. No retry for a favorable draw, source
promotion or production activation follows this result.

## Execution and scope

The [predeclared protocol](../../../launch/REFERENCE-PILOT-2026-10-25.md)
was committed as server `86615df67` before the first observation. This attempt
ran from **2026-10-25T06:25:17Z to 09:20:38Z**, exit 0, in **10,521 seconds**
(2h55m21s), leaving **279 seconds** inside the unchanged three-hour bound.

The frozen release is server `0d58d0d91b069c62bcfb39cd72166c26925b786a`,
ten-repository canonical lock
`37555f58bbf1235f10600f8c1de7dafeef356985f719f5e029fdb8c2e69c9b4a`,
actual Go 1.27.3 and evaluator image
`sha256:effe9ba5dc9cc337b4f250977c5f921ea42e4a336b30e3a5b54a9fd1bbe7f793`.
Workload: 3,600 providers, 200 clients, impairment on, public seed 20261024,
nine fresh controls, offline candidate build and nine fresh candidate runs.
No earlier baseline measurements were reused.

Candidate patch SHA-256:
`2957ef507f7edc9dadcb09abd6425664e88748612917959a9a65243a0365ebe4`.
Its selected correctness screen passed before this performance pilot;
correctness-test execution time was not treated as performance evidence.

## Result and comparison thresholds

All G1–G6 and all fifteen typed security booleans passed. The candidate is
placeable, not statistically significantly better and not takeover-eligible.
Original-only containers/networks and absence of the owned evidence mount
were verified after completion.

| Raw-score statistic | Fresh control | Candidate | Comparison line |
| --- | ---: | ---: | ---: |
| Run-level mean, ms | 17,514.05 | 20,637.62 | 14,694.29 or lower, using the combined 16.1% required improvement for this comparison |
| Canonical median, ms | 17,213.74 | 20,742.37 | 14,442.33 practical-margin line; the separate mean/variance significance test must also pass |

Candidate mean is **17.8346% worse**, median **20.4989% worse**. One-sided
Welch p = **0.99910857**, alpha 0.05. The variance-dependent significance fringe
is **8.25769%**; the combined requirement is the larger **16.1%** provisional
practical margin. These raw scores are not TTFB. Neither this comparison nor the
historical preparation floor establishes the final production margin.

## Preserved evidence

The four `artifacts.tar.gz.part-00` through `-03` ordinary Git blobs reconstruct
the complete original artifact archive, in that order. Each is at most 32 MiB.
The archive uses a relative `artifacts/` root; extract only into a fresh private
directory. Its reconstructed gzip SHA-256 is
`4f828987b3d835aeb0c1f39bb0f0b01186b7c960e8c2ebb885672e50cc39db86`.
Readable score, baseline, result, evidence, completion, terminal and fixture
companions are retained alongside it. Original root-owned evidence is unchanged.
Independent Go-only archive verification **passed**: all 440 original regular
files, 429 evidence-manifest bindings, seven worker-result bindings and seven
readable companions match. No unsafe paths, links, duplicate entries, extra
or missing files were found. Explicit checks retain the worse mean and median.
The [verification log](archive-verification.log) SHA-256 is
`07730ca6e4c9c6ae4acae0e87728104dcfffc32631e342566e1c1e07cdb78f31`;
executed private helper SHA-256 is
`2f121179acb34658e3cefbd5d8553b9070d12161c9df1c089c2b385ff890d1da`.

Worker result SHA-256:
`ddc542871678ecbc95e8e26ae469a384aa2882d4dec8f85bf63cba5d2fb2a8f0`.
Evidence manifest SHA-256:
`4e0928bca9df630ff96cb05360b21a2f230b416f3229ade518bf391ed5d1c840`.
Completion SHA-256:
`5441cef5167b659db4ba33de03adbab4f059aff51266984aa3248944702ffca1`.
Manager terminal SHA-256:
`a88ea65f873dde4b82fbae77c6f3ba0d9c926b17d91bb6e0a7b9b3be3c7fe68b`.

This is an offline public-seed research result, **not** an official epoch
baseline, hidden-seed validation, live readiness/provider receipt, or evidence
that the evaluated source was promoted. Synthetic API, worker and hardware
labels remain explicitly synthetic. Formal calibration and a qualified faster
reference are still outstanding.
