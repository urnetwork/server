# Production epoch 1: exact source checkpoint

Verified against public `/competition/info` and the source ledger on 2026-11-02.
These source identities may be shared; operational config, vault material,
hidden seeds, and runtime credentials are not part of this public extract.

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

## All ten repositories

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

## Why the two locks appear different

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

## What config/main/sim-latency.yml pins

The source ledger uses schema 2, production mode (`staging: false`), and
`evaluation_source.branch: sim-latency`. Generation 0 contains the ten commits
above, canonical digest `ec322ed2…`, and significant-improvement percentage 26.9.
Its epoch-1 entry selects source generation 0 and activation generation 1.
These source-only facts can be shared instead of the full config.

The main API and worker remain independently maintained control programs on
`main`; their moving commits do not replace the frozen product checkpoint.
Per-evaluation control-program provenance, workload commitments, the baseline
manifest, and the patch policy have separate identities.

For later epochs, use the newly activated checkpoint and advertised
`automation_policy.source_lock_sha256`, not this epoch-1 listing.
See [submission format](evaluator/PATCH-SURFACE.md).
