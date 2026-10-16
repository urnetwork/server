# Apex production calibration

Status: **LOCALLY QUALIFIED — technical launch gate open**

Generated: `2026-08-28T11:26:41Z`  
Score schema: `1`  
Source lock: `94c25024a92b5fcb5fa8bf324ff8022fde1074fd62bc210fc0ad5efbba0e4022`
Historical calibration source lock: `0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838`
Attempt-06 remediation amendment: `7971eeeac22c73781c0de1ce34c5296f79b2f223afbfe67d4a7b3fd2642de65d`
Attempt-06 evidence-binding amendment: `40ecb634563fa58fc41e346efdba6b604b2b86c7cb4fea820cc893e363191752`
Season-base equivalence: `6bce6a80cecfee0297bcc11afbaa390576d8f542980d8797e4da33046daa07b3`

This is the terminal local calibration for the sim-latency competition. It
binds the selected workload, baseline noise, takeover policy, independent-seed
reference separability, resource boundary, and production staging evidence to
the frozen source and evaluator identities. Organizational activation and
on-call ownership are separate operational decisions.

## Frozen identity and environment

- Public patch-authoring base: `apex-season-1` at
  `eb697281cbe0a19a27d7771fe69fb24c2c3dab8c`
- Entire editable surface: `connect/resident_contract_manager.go`, Git blob
  `66e2d39956b958749dfdfd00f408d4c05f874833` at both the public tag and evaluator commit
- Authoritative evaluator source commit: `46515d82fe98ff666c61b2b5bb1d34a89cf4dad8`
- Evaluator image: `sha256:2abcf145c0f914899debbd2fd52e57a16cf20072165c8d13f04a0ba487198a4c`
- Simulator and scorer SHA-256: `bc843ce2b9cdcc41459362c7a682b08e7a12a8ac896443fe1e8aad94d4b17997`
- Host qualification SHA-256: `acf226db6b8e50d67f8957cddb3903d5d4e9e82566935d61d270ccb5b03463a3`
- The p1800 frontier, baseline-noise, and reference-separability evidence was
  measured under historical source lock `0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838`. The
  authorized correctness remediation `7971eeeac22c73781c0de1ce34c5296f79b2f223afbfe67d4a7b3fd2642de65d` binds it
  to this evaluator through a clean same-round R=9 baseline/no-op bridge.
- The evidence-only staging correction
  `40ecb634563fa58fc41e346efdba6b604b2b86c7cb4fea820cc893e363191752` repaired terminal readiness metadata
  after the running shell had read none of the changed bytes; evaluator,
  orchestration, workload, and all completed measurements remained unchanged.
- Host: one authoritative 12-physical-core, 128 GiB machine; SMT and turbo off;
  performance governor; fixed affinity and IRQ placement.
- Evaluation boundary: physical CPUs `0,2,4,6,8,10,12,14,16,18`, 72 GiB runner
  ceiling, PostgreSQL 16 GiB, and Redis 8 GiB.
- Management reserve: physical CPUs `20,22` and at least 24 GiB, outside the
  untrusted job, retained for orchestration and forced cleanup.
- Candidate containers receive only direct read-only `config/local` and
  `vault/local` leaf mounts. Parent, `all`, `main`, control credentials, Docker
  socket, and external networking are unavailable.

Authoritative source-lock record: [`eval-12c/final-calibration-p1800-cf0fd3a9/source-lock.json`](eval-12c/final-calibration-p1800-cf0fd3a9/source-lock.json).
Public-tag/evaluator equivalence record:
[`eval-12c/final-calibration-p1800-cf0fd3a9/season-base-equivalence.json`](eval-12c/final-calibration-p1800-cf0fd3a9/season-base-equivalence.json).

### Frozen repository commits

- `connect`: `4f3f017f5448c18620b9eb3aab8c1002e869536d`
- `glog`: `13fba7d8f57e2c37274dd2e348578d5de9d66a59`
- `goidenticons`: `d18ccd3ecedee274067577c498affee6a8b06718`
- `proxy`: `17f929c926d49c27138569c85899bd2e98ec0ec1`
- `sdk`: `3c2d56b47155cd026c2f572337d45bd0210f5bf5`
- `server`: `46515d82fe98ff666c61b2b5bb1d34a89cf4dad8`
- `sn`: `420587eafbeb6d5ebd739862678161a7ad9c19a1`
- `userwireguard`: `4e3ead3f712c2557efeb91b0bb5ea6b7909171e7`
- `warp`: `678b25a8bbd2e564cc9d8fa954322b99af6bba0e`

## Frontier and selected workload

The exact evaluator image completed impairment-on and impairment-off runs. The
largest accepted point is p1800; p2700 is the first authenticated upper-bound
rejection because its minimum success rate was
`96.9804%`,
below the 97% floor.

| Field | Accepted value |
|---|---:|
| providers | 1,800 |
| warm clients | 200 |
| arrivals per minute | 80 |
| multi-client quality window | 2 |
| exchange hosts / fleet shards | 4 / 4 |
| measured duration | 180 seconds |
| forward idle timeout | 5 seconds |
| client warmup timeout ceiling | 1,200 seconds |
| maximum frontier mean CPU | 4.606 of 10 evaluation cores |
| maximum frontier peak RSS | 11.851 GiB |
| minimum accepted success rate | 98.680% |

The 1,200-second client warmup ceiling accommodates cold template restoration,
service readiness, and worst-case client construction without charging an
infrastructure delay as candidate latency. The worker's 8,000-second score
timeout covers offline build, reset, warmup, `R=9`
baseline/candidate repetitions, scoring, hashing, TERM grace, and cleanup while
remaining bounded and killable from the two management cores.

## Same-seed baseline and takeover selection

All 12 complete pairs are retained without censoring. The baseline mean is
`43,101.691 ms`; the paired no-op mean is
`42,196.873 ms`. Baseline sample SD is
`4,364.905 ms`, CV is
`10.127%`, median is
`42,910.357 ms`, and the range
is `35,106.569 ms` to
`52,789.430 ms`.

The selected candidate aggregation is the type-7 median of
`R=9` repetitions. The takeover margin and minimum
detectable relative improvement are `16.100%`. A submission
must have an aggregate raw score at or below its same-round baseline times
`0.839` and pass G1–G6. At the observed baseline mean, the
significant-better threshold is `36,162.319 ms`. The selected
bootstrap distribution has CV
`4.014%` and minimum
supported margin
`16.056%`.

Paired no-op ratio mean is
`0.987086` and median is
`0.973042`. Exactly
`8/12` no-op draws were placeable; the
`4` non-placeable complete draws
remain in every noise and quality calculation.

Raw evidence: [`eval-12c/final-calibration-p1800-cf0fd3a9/post-frontier/p1800-c200-r80-q2/same-seed/progress.json`](eval-12c/final-calibration-p1800-cf0fd3a9/post-frontier/p1800-c200-r80-q2/same-seed/progress.json),
[`eval-12c/final-calibration-p1800-cf0fd3a9/post-frontier/p1800-c200-r80-q2/same-seed-analysis.json`](eval-12c/final-calibration-p1800-cf0fd3a9/post-frontier/p1800-c200-r80-q2/same-seed-analysis.json), retained strict analysis
[`eval-12c/final-calibration-p1800-cf0fd3a9/post-frontier/p1800-c200-r80-q2/same-seed-analysis-familywise.json`](eval-12c/final-calibration-p1800-cf0fd3a9/post-frontier/p1800-c200-r80-q2/same-seed-analysis-familywise.json), authorized policy
[`eval-12c/final-calibration-p1800-cf0fd3a9/launch-readiness-placeability-policy-amendment.json`](eval-12c/final-calibration-p1800-cf0fd3a9/launch-readiness-placeability-policy-amendment.json), and authenticated
post-processing repair [`eval-12c/final-calibration-p1800-cf0fd3a9/same-seed-postprocessing-repair.json`](eval-12c/final-calibration-p1800-cf0fd3a9/same-seed-postprocessing-repair.json).

| Pair | baseline ms | no-op ms | no-op / baseline | placeable | failed gates |
|---:|---:|---:|---:|:---:|---|
| 1 | 45946.867 | 39662.794 | 0.863232 | yes | all passed |
| 2 | 40720.581 | 38373.630 | 0.942365 | yes | all passed |
| 3 | 35106.569 | 45795.083 | 1.304459 | no | G1_success, G4_matchmaking |
| 4 | 45734.415 | 38888.968 | 0.850322 | yes | all passed |
| 5 | 40237.688 | 42448.828 | 1.054952 | yes | all passed |
| 6 | 45045.926 | 39491.062 | 0.876684 | no | G2_volume |
| 7 | 39749.446 | 41979.006 | 1.056090 | yes | all passed |
| 8 | 42993.336 | 42063.182 | 0.978365 | yes | all passed |
| 9 | 52789.430 | 49881.605 | 0.944917 | yes | all passed |
| 10 | 41060.758 | 39735.280 | 0.967719 | yes | all passed |
| 11 | 45007.899 | 44053.918 | 0.978804 | no | G4_matchmaking |
| 12 | 42827.379 | 43989.122 | 1.027126 | no | G2_volume, G4_matchmaking |

### Aggregation candidates

The original familywise rule required run noise no greater than one quarter of
the takeover margin and at least a 95% estimated probability that 11 of 12
independent no-op results would be placeable. It failed for every candidate;
at R=9, the estimate was
`86.612%`.
The authorized launch amendment instead requires at least
`94.000%` estimated placeability for one
production evaluation. R=9 passes at
`94.614%`,
an estimated false-rejection probability of
`5.386%`.
This is an explicit launch compromise and is not confidence-equivalent to the
strict familywise rule.

| R | bootstrap CV | minimum margin | P(single placeable) | P(at least 11/12 placeable) | strict eligible | launch eligible |
|---:|---:|---:|---:|---:|:---:|:---:|
| 1 | 10.127% | 40.508% | 66.667% | 5.395% | no | no |
| 3 | 6.264% | 25.056% | 81.134% | 30.840% | no | no |
| 5 | 4.903% | 19.612% | 88.159% | 57.562% | no | no |
| 7 | 4.332% | 17.329% | 92.192% | 76.011% | no | no |
| 9 | 4.014% | 16.056% | 94.614% | 86.612% | no | yes |

## Independent seeds and reference separability

Five CSPRNG seeds were committed before the first reference result and
revealed only after the campaign. Each seed ran the pinned better, no-op, and
worse patches in a precommitted randomized order with one candidate replicate
per reference. All three candidate raw scores within a seed use the same
precommitted designated pristine baseline denominator. The ordering gate passed
`4/5`, satisfying the required
`4/5` reference separability threshold.

This launch compromise is not confidence-equivalent to the original design:
it retains 12 same-seed pairs and uses five independent seeds with a 4/5
ordering gate rather than the superseded 12-seed/11-pass compromise or the
original 20-seed/19-pass design. It preserves all complete and non-placeable
outcomes, uses fresh hidden independent-seed material, and leaves the calibrated
competition `R=9` and takeover margin unchanged.

Raw evidence: [`eval-12c/final-calibration-p1800-cf0fd3a9/reference-requalification-v5/hidden-launch-runtime/independent-references/progress.json`](eval-12c/final-calibration-p1800-cf0fd3a9/reference-requalification-v5/hidden-launch-runtime/independent-references/progress.json).

| Seed | better / designated baseline | no-op / designated baseline | worse / designated baseline | ordered |
|---:|---:|---:|---:|:---:|
| 1 | 0.909475 | 0.917591 | 1.548365 | yes |
| 2 | 0.997180 | 1.062536 | 1.243536 | yes |
| 3 | 1.155426 | 1.044815 | 1.741219 | no |
| 4 | 0.974134 | 1.220868 | 1.712369 | yes |
| 5 | 0.840614 | 0.895858 | 1.228916 | yes |

## Production readiness and resource controls

The digest-pinned static API, worker, rebaseline, and migration binaries have
provenance and SBOM records. Service-backed staging verified authenticated
generate/submit/poll, origin-before-local migrations, single-job FIFO, cache
identity across principals, same-round rebaseline, terminal immutability,
worker lease recovery, submission retry, reveal commitment, provider download,
artifact retention, and default-deny networking. Adversarial CPU and memory
bombs remained killable; cleanup is issued from the management reserve.

The seven sealed production records are:

- `artifact_retention`: passed; evidence `f73afac58ec57d4d851ead70e5d290ce528f0fcd75184b1854778d63e2bf74e5`
- `authenticated_api_generate_submit_poll`: passed; evidence `67d02e5db9775bbc1ebbcc2c64e544691965a8095db6d3a19ec25242cc3ad15a`
- `full_staging_round`: passed; evidence `0b92332a83efd09c0e9be5b63c94dcc4c6a8ef5a096c4272b76a82014a7c7242`
- `monitoring_and_recovery`: passed; evidence `26af808a653575c4d23c9aadaecbf56843c9f596a8ca302d20ab81fc7cd0b2b3`
- `no_secrets_audit`: passed; evidence `90052103ffcdcf34ef77bfd6f0b095b8200cab1d4c402cd8193e843838862231`
- `release_artifacts`: passed; evidence `381a5b12432f577689a16d9dfbb878127fc27ec900ac7318243ef9a53ef7a559`
- `service_backed_fifo_cache_failover`: passed; evidence `c36762a44251ebdc67d4097defe34fb8da700002a001859eb5c289548263d965`

Final readiness evidence: [`eval-12c/final-calibration-p1800-cf0fd3a9/production-readiness-final.json`](eval-12c/final-calibration-p1800-cf0fd3a9/production-readiness-final.json).

## Final signed selection

| Field | Accepted value |
|---|---|
| provider/client/arrival scale | 1,800 / 200 / 80 per minute |
| measured duration | 180 seconds |
| hosts / fleet shards / quality window | 4 / 4 / 2 |
| baseline and candidate replicates | median of 9 |
| takeover margin / MDD | 16.100% |
| observed baseline mean | 43,101.691 ms |
| significant-better line at observed mean | 36,162.319 ms |
| raw-score run noise | SD 4,364.905 ms; CV 10.127% |
| production-evaluation no-op placeability | 94.614%; gate 94.000% |
| retained strict familywise result | 86.612%; 95.000% gate failed |
| independent reference separability | 4/5; gate 4/5 |
| independent reference placeability diagnostics | better 3/5; no-op 3/5; worse 0/5; ordering remains shared-baseline raw-score based |
| CPU / RSS headroom evidence | 4.606/10 cores; 11.851 GiB peak |
| evaluator identity | `2abcf145c0f91489…` |
| local technical review | authenticated production-readiness seal `bc56f7b02a1cfcfb…` |

The local technical launch gate is open for this exact source, image, host,
scale, scoring policy, and control-plane release. Any identity or policy change
requires a new content-addressed qualification chain.
