# Competition epoch source index

This public index records the exact branch and repository commits used by each
production epoch. Each activated epoch gets its own section, including epochs
that carry forward an unchanged source checkpoint. Historical entries are
preserved; moving branch heads and current `main` are not source authority.

These source identities may be shared. Operational config, vault material,
hidden seeds, and runtime credentials are not part of this public extract.
For submission policy and the current round, consult
[`GET /competition/info`](https://api.bringyour.com/competition/info).
Match the round and season as well as the epoch number; staging epochs do not
identify production source.

## Epochs

- [Production epoch 1 — initial baseline](#production-epoch-1--initial-baseline)
- [Production epoch 2 — intake and two-host evaluations open](#production-epoch-2--intake-and-two-host-evaluations-open)

## Production epoch 2 — intake and two-host evaluations open

Published and independently checked against GitHub branch heads on 2026-11-09.
The native transition created the round on 2026-11-10. **Public intake and
two-host evaluation execution are open.** The first two customer jobs were
claimed in submission order on 2026-11-11 at 17:51:52 UTC (Goofe) and
17:52:08 UTC (Sille). Both native agents authenticated the execution image and
shared baseline below; both started candidate simulation replicate 1. This is
execution evidence, not a completed score or an acceptance result.
At 18:04 UTC, the API still reported `grading_ready: false`; deployment of the
separately tested public host-proof projection correction remained unverified.
That discrepancy did not stop these active evaluations. Earlier authenticated
`/competition/v2/score/preflight` returned 200 with the exact round/source
identity and `reward_eligible: true`; preflight itself creates no job.
The epoch-1 winner's reviewed product change and the approved prospective G2/G7
instrument are included. The serving source error cleared after API/config
redeployment. Epoch-1 results remain publicly readable. Epoch-2 public rankings
and winner publication remain held until the round's publication conditions pass.

| Identity | Value |
|---|---|
| Season | `sim-latency-production-20261028-r2` |
| Created round | `01a1c110-585b-ce38-4f29-ca4860a1c058` |
| Recorded submission window (UTC) | `2026-11-10T04:28:28.670738Z` to `2026-11-17T04:28:28.670738Z` |
| Source generation / activation generation | `1` / `2` |
| Source-lock SHA-256 | `4781aac57e052ff048440ac5f0f118d780a76fa1685326fc0b3af1143ae4f0cd` |
| Original policy / baseline evaluator image | `sha256:5e5dfa2715c980d7ec037b6f15208e1d240a6bc80668aabc3335c1f71b2b92d9` |
| Original baseline compiler | Go `1.27.3` |
| Active security-hotfix execution image | `sha256:98284651822ebe55341d6133ac978aed30c8d18dd5e9c4332136fb4ffca7e702` |
| Active execution compiler | Go `1.27.4` |
| Retained shared epoch baseline SHA-256 | `784aef3401631be5a39a73185c0276bb7ded38410f53de48af7ed942dfdd1340` |
| Takeover margin | `0.269` (26.9%) |
| G2 | Requests and bytes each at least 95% of baseline; no upper bounds |
| G7 | Expected pages and bytes each at least 95% of same-epoch baseline coverage |

All ten repositories use `sim-latency`; use these immutable commits rather than
the moving branch. The shared epoch baseline and host admissions are recorded
separately from this product source checkpoint.

| Repository | Exact commit |
|---|---|
| [server](https://github.com/urnetwork/server) | `6c55fc2ca3bf278ae4f94938e4940547844a71f1` |
| [connect](https://github.com/urnetwork/connect) | `388d8a153952fe36af1753908f4b21501967f3a8` |
| [sdk](https://github.com/urnetwork/sdk) | `d3824aa98c02e3c16ff7abcfe7a7088c8a470cdb` |
| [proxy](https://github.com/urnetwork/proxy) | `af1d364fe34b807c31936642bff7d3fd849ab2a4` |
| [glog](https://github.com/urnetwork/glog) | `1c4954b017d9e8140a4e7020f914eb27410a41c6` |
| [goidenticons](https://github.com/urnetwork/goidenticons) | `39ccec1b69db3a6f67e7346d6f44863499e6241d` |
| [userwireguard](https://github.com/urnetwork/userwireguard) | `f9a92f0e5f5577a80d358065c4d39ca21f971a1c` |
| [sn](https://github.com/urfoundation/sn) | `6e686aabd53b7c45b154051fd268138988224eba` |
| [warp](https://github.com/urnetwork/warp) | `8dfa405ec380a60307952778921f05bba1c59a33` |
| [gvisor](https://github.com/urnetwork/gvisor) | `57c9e137f7407ad6cd41dcca1ba304a446e09c79` |

The source manifest is published in config commit
`ff23e28f3a6d0e9067b5024e5127a7bb697d1a0b`; its SHA-256 is
`f40b65cd3d2d05b9948945f3aa11cbeee794d341675d2c9fdfd9e94797f9225b`.
Do not distribute operational config or vault files; this index is the public
source extract.

### Executor-control correction (approved 2026-11-10; active)

The operator approved correcting the private executor's namespace-probe user,
stop/cleanup ordering and trusted-operator directory handling, plus the already
tested throwaway build-layer branch normalization. These control fixes do not
change the ten product commits, evaluator image, scoring instrument, workload,
G2/G7 rules, margin, submission order or clocks above.

Corrected agents must use newly recorded host fingerprints, not impersonate the
old frozen identities. The implementation records an append-only per-round
operational amendment, retaining the original round and intake-policy bytes.
Registration is prohibited after any customer evaluation or official R9 host
admission starts. The amendment and amendment-bound admissions are now recorded.
The separate approved Sille admission exception preserves its failed statistical
admission result rather than relabeling it as a pass.

### Go 1.27.4 security-hotfix exception (active 2026-11-11)

The operator explicitly approved using Go 1.27.4 for epoch-2 execution without
measuring a replacement baseline or repeating statistical calibration. The
ten product commits above, shared baseline, scoring gates, margin and original
submission ordering remain unchanged. The original Go 1.27.3 measurements are
not presented as Go 1.27.4 measurements.

Both hosts independently passed native containment, CPU/memory-bomb cleanup and
an operational one-replicate compatibility smoke on the new image. Those checks
are not a fresh statistical baseline or R9 admission. Execution provenance is
bound by append-only security-toolchain decision event 540, SHA-256
`0d31e1c6258cec5c1eb7d45dd4d4f4dd84a948bf21c3c5dd0416f44c389f5787`.
The [retained exception and execution evidence](launch/EPOCH2-GO1.27.4-SECURITY-EXCEPTION-2026-11-11.json)
distinguishes the frozen policy/baseline image from the actual execution image.
Each submission runs its complete candidate evaluation on one host, using the
same retained epoch baseline; a submission's replicates are not split between hosts.

## Production epoch 1 — initial baseline

Verified against public `/competition/info` and the source ledger on 2026-11-02.

| Identity | Value |
|---|---|
| Competition | `sim-latency-season-1` |
| Season | `sim-latency-production-20261028-r2` |
| Round | `01a1855d-49bd-0250-90db-d0c0885f0e46` |
| Admission epoch | `1` |
| Source generation / activation generation | `0` (baseline) / `1` |
| Evaluation branch | `sim-latency`; use exact commits, not moving branch heads |
| Canonical source-lock SHA-256 | `ec322ed2db023e15ea5dc3eea60a6ca5989661bba737c340fea3477d8d1300cf` |
| Evaluator image | `sha256:98c9061c4a43a25e7b581a5193ca6e7f48e49e7b7fc03b7c1b6190a029befee2` |
| Compiler | Go `1.27.3` |
| Initial takeover margin | `0.269` (26.9%) |

### Repository branches and commits

All ten repositories use the `sim-latency` branch. Check out the exact commits
below: the branch may advance when a later epoch is promoted.

| Mono-root directory / repository | Exact commit |
|---|---|
| [server](https://github.com/urnetwork/server) | `a9e8e1a710b24c749e6499a0606938252c83e136` |
| [connect](https://github.com/urnetwork/connect) | `e90d59eb404f973dba6e9d0689ecd94e2718c10e` |
| [sdk](https://github.com/urnetwork/sdk) | `d3824aa98c02e3c16ff7abcfe7a7088c8a470cdb` |
| [proxy](https://github.com/urnetwork/proxy) | `af1d364fe34b807c31936642bff7d3fd849ab2a4` |
| [glog](https://github.com/urnetwork/glog) | `1c4954b017d9e8140a4e7020f914eb27410a41c6` |
| [goidenticons](https://github.com/urnetwork/goidenticons) | `39ccec1b69db3a6f67e7346d6f44863499e6241d` |
| [userwireguard](https://github.com/urnetwork/userwireguard) | `f9a92f0e5f5577a80d358065c4d39ca21f971a1c` |
| [sn](https://github.com/urfoundation/sn) | `6e686aabd53b7c45b154051fd268138988224eba` |
| [warp](https://github.com/urnetwork/warp) | `8dfa405ec380a60307952778921f05bba1c59a33` |
| [gvisor](https://github.com/urnetwork/gvisor) | `57c9e137f7407ad6cd41dcca1ba304a446e09c79` |

The preserved [machine-readable listing](launch/FROZEN-SOURCE-CHECKPOINT-2026-10-26.json)
contains these same repository URLs and commits. Its remote-branch observation
is historical; the immutable commit identities are what matter.

### Why the two locks appear different

`server/local/source-graph/lock.yml` selects dependencies for the ordinary
service build at that server revision. It is **not** the competition's
multi-repository source authority. Its Connect ref need not equal the explicit
competition ref.

The preserved
[`baseline/prelaunch-mono-root-c2-build-2026-10-26/source-lock.json`](baseline/prelaunch-mono-root-c2-build-2026-10-26/source-lock.json)
**contains the correct epoch-1 repository map**, including Connect `e90d59eb…`.
Its raw file SHA-256 is
`239cea7fe6f77f989b75ba6efa677e8731640ba2fdd3690a35e6fe72103f3d11`.
That is not the API's canonical source-lock digest.

`CompetitionSourceLockDigestForMode` in
`server/model/competition_source_model.go` hashes compact Go JSON containing
exactly `schema`, `development_snapshot`, and the sorted repository map. It
excludes `candidate_path_base`, formatting, and the final LF. From the `server`
checkout, reproduce the identity without building or running the evaluator:

```bash
jq -cj '{schema:1,development_snapshot:false,repositories:(.repositories|to_entries|sort_by(.key)|from_entries)}' \
  connect/sim-latency/baseline/prelaunch-mono-root-c2-build-2026-10-26/source-lock.json \
  | sha256sum
# ec322ed2db023e15ea5dc3eea60a6ca5989661bba737c340fea3477d8d1300cf
```

Different raw and canonical hashes do not indicate different commits.

### What config/main/sim-latency.yml pins

The source ledger uses schema 2, production mode (`staging: false`), and
`evaluation_source.branch: sim-latency`. Generation 0 contains the ten commits
above, canonical digest `ec322ed2…`, and significant-improvement percentage 26.9.
Its epoch-1 entry selects source generation 0 and activation generation 1.
These source-only facts can be shared instead of the full config.

The main API and worker remain independently maintained control programs on
`main`; their moving commits do not replace the frozen product checkpoint.
Per-evaluation control-program provenance, workload commitments, the baseline
manifest, and the patch policy have separate identities.

For later epochs, use that epoch's section and the newly activated checkpoint
matching the advertised `automation_policy.source_lock_sha256`, not this
epoch-1 listing. See [submission format](evaluator/PATCH-SURFACE.md).

## Maintaining this index

As part of each epoch release, append a section and add it to the list above.
Use the activated source ledger, authenticated source manifest, and live API;
do not infer commits from current local checkouts or remote branch heads.
Record:

- competition, season, round ID, and admission epoch;
- source generation, activation generation, and evaluation branch;
- every repository's full GitHub URL and exact commit;
- canonical source-lock digest, evaluator image digest, and compiler version;
- the epoch's initial takeover margin and verification date; and
- a link to a versioned, public-safe machine-readable checkpoint, identifying
  promotion versus unchanged incumbent carry-forward.

Verify that the complete repository map reproduces the advertised canonical
digest before publishing. Keep raw-file hashes distinct from canonical hashes.
Add a new epoch section even if all commits remain unchanged. Do not publish a
planned checkpoint as active, rewrite earlier entries to moving branch heads,
or copy private operational configuration into this page. Staging records, if
included, must have a separate explicitly named season and staging section.

This page is a publication index, not an input that changes evaluation policy.
Updating it does not change a frozen round or authorize a source promotion.
