# First committed seed: complete, not accepted

The owner-approved recovered screen closed at
**2026-10-28T03:18:27.783615503Z**. The native process exited zero, but its
authoritative result is `complete: true`, `accepted: false`,
`correctly_ordered_seeds: 0`, reason
`first_committed_seed_not_correctly_ordered`. Process `PASS` is not calibration
acceptance. No production activation or new opening is authorized by this record.

## Frozen criterion and observed result

The predeclared criterion requires the three complete references to satisfy
`better median < no-op median < worse median`, valid significance results,
and a placeable, non-takeover no-op. It does not require the nominal better
reference to win a significance test. No criterion has been changed after
observing results.

| Reference | Median raw score, ms (lower is better) | Mean improvement vs control | One-sided p | Takeover eligible |
| --- | ---: | ---: | ---: | --- |
| Better | 16,466.4632 | −4.5357% | 0.727635 | No |
| Worse | 17,895.6194 | −2.7879% | 0.717679 | No |
| No-op | 18,464.6714 | −6.1168% | 0.883538 | No |

All three are complete R9 candidates, placeable, and have empty failure codes.
The worse reference outranked the no-op: the required ordering failed. These
results do not prove a root cause; variance, workload sensitivity and a possible
correctness defect require diagnosis, not a retrospective relabeling.

All jobs used the exact same authenticated nine-run control, SHA-256
`129e134a8455264e145803d452f330abcc0d0b5e708c433ae1ad331b07a96dfa`.
The no-op's mean-control comparison has an **8.6423% statistical improvement
fringe**, with the separate frozen **26.9% practical takeover margin** still
required. Its mean control score was 16,890.4943 ms; these mean-based significance
statistics must not be confused with the median-ordering criterion above.

## Completion and cleanup

The better, worse and no-op evaluator durations were 10,306, 5,282 and 5,255
seconds respectively, all within their original fresh three-hour job clocks.
The final evaluator ended at 03:17:43 UTC. All three cleanup records verify
identical before/after inventory SHA-256
`1b319b1b2a4dede0ec678ea17f82f4d8e2486b479e8d91b22188a3f4914c9d1b`.

At 04:14 UTC the native session was terminal, its original process absent and
its management scope inactive. Docker contained only the expected BuildKit,
local PostgreSQL and local Redis containers; no competition mount remained.
The exact original scoring inode was available to a read-only-opened,
nonblocking exclusive flock. Subsequent administrative tests may proceed;
this is not authorization for another measurement.

The second committed seed remains unstarted, with only its three original input
files per job. It must not be used as a replacement for this failed screen.
Preserve all first-seed outcomes, OOM/recovery forensics and original plans.
Private archive qualification and retained upload are separate from calibration
acceptance; no new full-archive MinIO receipt is claimed here.

## Prospective follow-on, not a changed result

Independent maximum-effort review found no evaluator defect; this is not proof
that variance alone caused the failed ordering. The owner subsequently
[approved a separately precommitted new screen](CALIBRATION-NEW-SCREEN-OWNER-APPROVAL-2026-10-28.md)
with a causally stronger regression reference, fresh control and newly committed
seed, retaining R9, all gates, significance and the 26.9% margin. Exact reference
and protocol review, native qualification and publication remain pending. No new
seed has been drawn or new measurement started. The original second seed remains
unstarted, and this screen's `accepted: false` result is unchanged.

## Exact retained identities

Native root on sille:
`/var/lib/urnetwork/competition-held-out-two-b-c2-a9e8e1a7-20261027-v2/campaign`.

- `campaign-result.json` SHA-256:
  `c85bf64bb18418219b5bd6f2c1ae30d88d02612fab5ff7684e7d9dea5882fdda`.
- `round-01/job-03-no-op/outcome.json` SHA-256:
  `ca5d789cc1faeaa57e22aeec19190c1ff409a97d4288174ae281442ba0a41392`.
- Final cleanup SHA-256:
  `24b711750e75478f36f275c4572102fe16d3c1a56ae6ac4b17ec5b4f1b243f03`.
- Final terminal log SHA-256:
  `69c25e8db52e6540c07eef4ac0aecc86fbf1c858e11c8d491963ba71ee19d1c5`.

See the [first observation](FIRST-COMMITTED-FIRST-REFERENCE-2026-10-28.md),
[prospective amendment](CALIBRATION-AMENDMENT-2026-10-27-FIRST-SEED.md),
[recovery record](UNMEASURED-MANAGEMENT-OOM-2026-10-27.md), and
[launch acceptance index](LAUNCH-ACCEPTANCE.md). The midnight launch was missed;
neither a new opening nor reduced acceptance requirements are implied.
