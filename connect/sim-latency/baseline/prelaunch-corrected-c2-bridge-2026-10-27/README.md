# Corrected C2 public-seed R9/R9 bridge

Accepted as an **operational compatibility witness, not statistical equivalence**.
This is public-seed calibration evidence, not an official production epoch
baseline, independent-reference acceptance, source promotion or launch approval.
The historical A controls and these bridge controls must not be reused as a
future epoch's baseline. Each production epoch needs its own authenticated
same-round control assessment.

The unchanged C2 application completed nine fresh controls and nine fresh no-op
candidates from `2026-10-27T09:32:21Z` to `12:23:51Z`: **10,290 seconds**
(2h51m30s), exit 0, with **510 seconds** inside the 10,800-second cap. This is one
observed execution, not a promise that another workload will fit. All G1–G6
passed; the original three containers/four networks were preserved and owned
resources were cleaned up. The native acceptance receipt records two actual
native scorer replays, 467 full-evidence bindings, seven worker-artifact bindings
and five completion bindings. Those replays used the complete retained original
tree, not the smaller public subset below.

## Release and workload

- Measured source: `a9e8e1a710b24c749e6499a0606938252c83e136`.
- C2 image: `sha256:98c9061c4a43a25e7b581a5193ca6e7f48e49e7b7fc03b7c1b6190a029befee2`.
- Measured simulator/scorer: `efd1077d56f2435027d8358e01f29d43355a4ec1a45917e72602fcee8cce6727`, Go 1.27.3.
- No-op image: `sha256:8385ae6f9d4379985cfbb612e8432ea830b2af0bf94c0265ddd315542486ba1d`.
- Ten-repository canonical lock: `ec322ed2db023e15ea5dc3eea60a6ca5989661bba737c340fea3477d8d1300cf`.
  The preserved raw lock file has the distinct byte SHA
  `239cea7fe6f77f989b75ba6efa677e8731640ba2fdd3690a35e6fe72103f3d11`.
- Public seed **20261024**, 3,600 synthetic providers, 200 established clients
  from a 200-client pool, impairment enabled, 180-second measured windows,
  120,000 ms request timeout, R9 controls and R9 no-op candidates.
- Frozen practical margin **26.9%**, inherited from the separately accepted
  [three-A report](../prelaunch-three-pair-analysis-2026-10-26/report.json).
  The corrected [V2 precommit](lineage/bridge-plan.json) and exact publication
  record are preserved, not replaced by the earlier prospective bridge plan.

Raw run manifests retain their original `source_epoch: 0` and `official: true`
formatter fields. They describe the trusted evaluator's measured public-seed
fixture; they do not establish an admitted API job or an official production
epoch. No raw field has been relabeled to imply otherwise.

## Observed result

Lower raw score is better. Each replicate's raw score is the p95 of measured
end-to-end `total_ms`, charging failed requests the full timeout; the reported
aggregate is the median of nine replicate scores. It is **not TTFB**. Welch
significance uses the replicate means and sample variances, not that median.

| Native diagnostic | Control | No-op candidate |
|---|---:|---:|
| Median raw score, ms | 17,390.9366 | 17,647.55935 |
| Mean replicate raw score, ms | 18,178.88309 | 18,734.22873 |
| Normalized candidate score | — | 98.54585 |

The candidate is placeable, but **not significant and not takeover-eligible**.
Its mean-score improvement is **−3.054894%** (a slowdown), with one-sided Welch
**p = 0.66085237**. The recorded sample-dependent significance fringe is
**12.6245675%** for this comparison; the fixed practical margin remains **26.9%**.
These are separate requirements. The median-score practical boundary recorded
by the scorer is 12,712.7746546 ms. A future comparison's Welch fringe depends
on its actual replicate variances; it cannot be inferred from the margin alone.
No recommendation in this no-op result changes the frozen source/margin ledger.

## Portable public subset and original evidence

`original-artifacts/` contains **279 byte-identical original files / 56,369,948
bytes**. All 18 CSVs, 18 run manifests, 18 completion markers, accounting
snapshots and source-counter sidecars, resource reports, synthetic site settings,
and all 36 compressed matchmaking-statistics segments are retained. The score,
baseline, complete original evidence manifest, typed worker result, request and
containment metadata are also included. The native proof, immutable lineage and
small manager logs are readable companions outside that directory.

[ORIGINAL-ARTIFACTS.json](ORIGINAL-ARTIFACTS.json) inventories **every one of the
479 original files / 380,987,722 bytes**, including SHA-256, byte count, public
selection and explicit omission reason. Exactly 200 files are not copied here:

- 180 duplicate `evidence/runs/*/output/` files, whose scorer-input counterparts
  preserve the measured data;
- 18 verbose per-run `stderr.log` files, retained privately;
- one duplicate providers input and one duplicate draft request.

No original file was changed or deleted. All raw bytes remain in the original
retained bridge tree. The full native 467-binding evidence manifest is preserved
unchanged, but **cannot be checked in full against this deliberately incomplete
public tree**. In particular, omitted stderr means this subset alone cannot
reproduce the original G5/native full-evidence replay. The recorded acceptance
is the native verifier's result on the complete original tree, not a new replay
or a claim made by this curation. See [ARCHIVE-PLAN.md](ARCHIVE-PLAN.md) for the
required private full-archive/readback step; no MinIO upload is claimed here.

No images, application binaries, source checkout, credential material, main
configuration or vault leaf contents are copied. Original metadata paths remain
unaltered and may refer to the originating host; the retained data and local
hash verification do not require those paths to exist on a reader's machine.

## Authenticate this copy

From this directory, run `sha256sum --check --strict MANIFEST.sha256`. The local
manifest covers every file here except itself. The complete-original inventory
is a separate record; it does not pretend the 200 omitted paths are present.
The [copy verification](COPY-VERIFICATION.json) records post-copy checks of all
479 originals, all 279 selected copies and the 182 omitted identical duplicates.
The enclosing baseline-wide manifest must also be refreshed by the publisher
when accepting this new version. No analysis, scorer, Go test or provider was
executed while copying this dataset.

Key immutable identities:

- [Native compatibility receipt](native/bridge-compatibility.json):
  `2af963f0ca0cecef740744da974bd2d0c65a41a24b5d68cac277d076fe84c930`.
- [Original score](original-artifacts/score.json):
  `d19101d53fab7d2387f73ff11d339694bfd3333c892b124912809967bd3c9bfa`.
- [Original control manifest](original-artifacts/baseline.json):
  `38180a09ac25f7794c8b00999197f1578402fb796340def5a82728191a901060`.
- [Original evidence manifest](original-artifacts/evidence-manifest.json):
  `281db6f1e3b23c433eeeab104b0f5bd68c6dbe0fa9e923f461bac88c4cbed340`.
- [Complete original inventory](ORIGINAL-ARTIFACTS.json):
  `e33a2ca2058ee64a6cf8826c415958f98ab1563bc0f22591561dbe833f34bd88`.

This version includes only the completed corrected bridge. Earlier failed
attempts and the original A evidence retain their separate historical identities;
none was silently recovered, rescored or included as a fresh bridge replicate.
