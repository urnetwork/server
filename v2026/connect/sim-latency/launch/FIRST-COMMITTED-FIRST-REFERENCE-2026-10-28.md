# First committed seed: first reference observation

This is a completed job observation, **not acceptance of the whole reference
screen or approval to launch**. The nominal faster reference did not demonstrate
a statistically significant improvement over its control sample.

Native job `d11e4ae4-c3f8-4687-b481-3b02bc336130` ran nine fresh controls and
nine candidate replicates. It exited zero at **00:19:53 UTC**, after **10,306
seconds (2h 51m 46s)**, before its approved **00:28:07 UTC** deadline. Native
cleanup verified identical pre/post resource inventories.

| Recorded statistic | Value |
| --- | ---: |
| Control mean raw-score latency | 16,890.4943 ms |
| Candidate mean raw-score latency | 17,656.6007 ms |
| Candidate median raw-score latency | 16,466.4632 ms |
| Control sample variance | 1,891,135.0464 ms² |
| Candidate sample variance | 11,595,150.4189 ms² |
| Observed mean improvement | −4.5357% |
| One-sided Welch p-value / alpha | 0.727635 / 0.05 |
| Recorded minimum-significant-improvement fringe | 13.0675% |
| Frozen practical margin / required improvement | 26.9% / 26.9% |
| Placeable / takeover eligible | true / false |

The median used by the fixed reference-ordering check and the means used by
the significance diagnostic are different statistics. Do not describe the
smaller candidate median as proof of a significant improvement. The recorded
fringe describes this sample's variance, not a guaranteed threshold for a
different candidate's variance.

The prospectively frozen first-seed protocol requires all three reference jobs
to complete with correct median raw-score ordering, plus a placeable,
non-takeover no-op. It does not require the nominal faster reference to clear
the production takeover margin. This observation alone neither accepts nor
rejects that complete protocol; do not add or remove criteria after observing it.

The regression reference started at **00:21:05 UTC**, with deadline **03:21:05
UTC**. Its baseline file is byte-identical to the first job's baseline. No new
control sample was measured for that reference.

| Native evidence | SHA-256 |
| --- | --- |
| First job `outcome.json` | `ecaa289619d91127b2f92eec29e709684da96ca0b782ecd38ca5911da881577a` |
| First job `cleanup.json` | `0d50176bb53766237a6cf023afdaa72365e86d15947d007d4307b61416fbfe18` |
| Shared `baseline.json`, both jobs | `129e134a8455264e145803d452f330abcc0d0b5e708c433ae1ad331b07a96dfa` |

Evidence remains under the protected campaign directory recorded in
`current-state.json`. Continue the same native execution; preserve the
unfavorable result, unchanged inputs and unstarted second seed. Do not reroll
or relabel it. The missed public deadline and remaining activation gates are
recorded in [LAUNCH-DEADLINE-2026-10-28.md](LAUNCH-DEADLINE-2026-10-28.md).
