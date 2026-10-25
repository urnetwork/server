# Corrected-release A/A cohort 1

One predeclared offline cohort completed with nine fresh controls and nine fresh
no-op candidates. It also supplies the declared full-window functional check;
these are not two independent observations. All G1–G6 gates passed, the pair is
placeable, and cleanup completed. This is provisional first-pair evidence, not
accepted five-cohort calibration or an official production baseline.

UTC interval: 2026-10-25T15:28:07Z–18:23:05Z; duration 10,498 seconds.
Control/candidate means: 19,688.243216666662 / 19,136.306394444404.
Medians: 19,410.1045 / 18,612.129599999975. Sample variances:
3,576,675.2698730268 / 5,672,516.848963349. Mean improvement 2.803382791%;
one-sided Welch p=0.2970098480834252. Neither statistical significance nor
takeover was established. The provisional floor is 16.1%; the Welch minimum
significant-improvement fringe is 9.017966861783291%; required improvement
remains 16.1%, not the smaller fringe.

Ordered chunks reconstruct one gzip tar containing every original regular file
under `cohort/` and four additional companions under `companions/`.
`original-inventory.json` binds archive paths to original bytes, size and SHA.
No unfavorable records are omitted. The initial terminal-check log contains a
checker setup mistake: the completion marker's hash map was read as an artifact
array. It is NOT a failed evaluation. The corrected check validates all five map
bindings; both logs and the corrected checker source are retained unchanged.

The original request uses explicit synthetic offline API/worker/hardware labels;
these are not live readiness or host/provider attestations. No subsequent cohort
is authorized by this archive.
