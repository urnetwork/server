# Original cohort 03 — completed and preserved

The original attempt ran **2026-10-26 03:41:45–06:31:05 UTC**, exit 0,
10,160 seconds: **640 seconds below its original three-hour limit**. All nine
fresh controls and nine fresh no-op candidates completed; all six gates,
placeability, security checks, artifact bindings and exact cleanup passed.
There was no restart or replacement draw.

This is old-instrument C1/image002 evidence. Under the
[approved amendment](../../../../launch/CALIBRATION-AMENDMENT-2026-10-26.md),
it is the third of three original same-seed pairs, not a revised-instrument
bridge, held-out seed or accepted final margin.

| Observed statistic | Control | No-op | Relevant improvement edge |
| --- | ---: | ---: | --- |
| Mean raw score, ms | 17,180.233139 | 17,770.513828 | 15,892.455154 at this pair's 7.495696% significance fringe, conditional on its observed variances |
| Median raw score, ms | 17,417.496100 | 18,055.805200 | 14,613.279228 at the measured policy's 16.1% practical floor |
| Sample variance | 2,872,787.503035 | 2,004,803.603560 | Used to calculate the mean significance fringe, not itself an improvement target |

The no-op mean was **3.435813% worse**, one-sided p-value **0.7826143965**.
It was not significant or takeover-eligible. Mean significance and median
practical improvement are separate requirements; a new submission uses its own
observed variance and the eventual frozen margin. The figures above describe
the original measured policy, not a newly selected launch threshold.

## Preserved original bytes

Four `cohort.tar.gz.part-*` chunks concatenate to gzip SHA-256
`889ab8e59e304352dafaca77715e85dea62339c429c9e2dfa0284b6325f1d566`.
They contain all **481 original regular files + five companions = 486 files**.
The companions preserve the parent launch log, terminal checker and its log,
and both archive-program sources. Twelve raw readable copies accompany the
archive; generated `summary.json` and byte-identical `pair-summary.json` are
derived metadata, not extra measurements.

The archive verifier reread every archived member against its inventory and
original source, checked exact coverage and the seven worker / 465 evidence /
five completion-marker bindings, native gates, actual `eval_error: null`,
terminal timing and cleanup. Its full retained log is
[`archive-verification.log`](archive-verification.log), SHA-256
`f40c60ee7bf8abe5159d95d7d572ff056703587e89d57ab11853b49098b88b93`.

[`pair-summary.json`](pair-summary.json) SHA-256:
`3245fbeee23d7410bd12f3fb70a447eb533bedaed3765bcc05b79db53366671c`.
[`baseline.json`](baseline.json):
`3dedeac4c21c9686d3907ce042d175bdd5f0aa7e44b9330da9f55ca97f252ae0`.
[`score.json`](score.json):
`179a06a1abe721e84a884806682517d6172b09c2a46d35f0c2bcf53c5d492553`.

No unfavorable outcome was filtered. Production remains unactivated.
