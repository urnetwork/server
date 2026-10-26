# Corrected-release A/A cohort 2

The second fixed, predeclared offline cohort completed nine fresh controls and
nine fresh comment-only candidates. All G1–G6 passed; the pair was placeable
and exact cleanup was verified. This is the second of five cohort observations,
not accepted calibration, a frozen final margin or a production epoch baseline.

UTC interval: 2026-10-25T23:23:57Z–2026-10-26T02:15:37Z. Whole-job duration:
10,300 seconds, leaving 500 seconds inside the original three-hour bound.
The request, source/image, workload, timers and gates were not changed or retried.

Control/candidate means: 17,859.202794444438 / 18,066.03480555555. The no-op mean
was **1.158125665% worse**, despite a lower median:
17,844.3915 / 17,557.35469999998. Sample variances were 5,739,486.722138452 /
3,712,001.7728263806. One-sided Welch p=0.5786459056773868: not significant,
and not takeover-eligible. This unfavorable mean is retained without filtering.

The provisional practical margin remains 16.1%, not a newly calibrated margin.
Its median reference boundary is 14,971.4444685. This pair's mean-based
significance fringe is 10.046374979174612%, corresponding to 16,065.00031342332
at the observed variances. Both the actual practical and statistical conditions
must pass; a future candidate's variance changes its statistical fringe.

Four ordered chunks reconstruct one gzip tar with every original regular file
under `cohort/`: **481 originals plus 7 companions**, 488 members in all.
`original-inventory.json` binds each to its original bytes, size and SHA-256.
Fourteen source copies are separately readable. Root and independent checks
verified all members, 7 worker / 465 evidence / 5 completion-map bindings,
actual `eval_error: null`, all six gates, R9+R9, the UTC duration and cleanup.
The independent regenerated summary was byte-identical to `summary.json`.

The companions include a pre-execution archive-helper generation diagnostic
and its original malformed source. Literal replacement incorrectly expanded
regular-expression text while preparing the helper; formatting rejected it
before execution. Corrected literal handling and the resulting successful
verifier are retained. This was **not an evaluation failure** and did not
modify an original measurement, scoring artifact or frozen instrument.

The request's API/worker/hardware labels are explicit offline fixtures, not live
readiness/provider/host attestations. No next cohort is authorized by this archive.
