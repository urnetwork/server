# Preparation identity and impairment corrections — October 24

## Findings and corrections

Preparation no longer needs to freeze one main API release for a season. An
empty `api_image_digest` in trusted adapter/qualification settings now means
"authenticate this intent's actual durable API acknowledgment," not "ignore
identity." An explicit configured image remains mandatory when supplied.
The HTTPS response must match the independently read durable role row before
qualification starts. The adapter retains that observed identity through final
quorum validation; qualification records it in the retained profile/result.
An existing intent replays its immutable acknowledgment without contacting a
later main rollout. Worker identity, exact-source binding, fencing and both-role
readiness remain enforced. There is no new public API or database migration.

The frontier audit found a separate evaluator defect: `run --no-impair` changed
a parent-process global, but `spawnFleetShards` did not pass it to children,
and `fleet` did not accept the switch. Every new child defaulted to enabling
provider-network impairment. A run could therefore record `flags.impair=false`
while its sharded provider connections still used impairment.

The fix uses owner-scoped `FleetSettings`, forwards the mode in the actual child
argv, accepts it in the real CLI grammar, and uses the same settings for fleet
construction and run metadata. Nil run settings retain the historical enabled
default. Disabling or retiring one fleet cannot change a healthy sibling.
The baseline → run → fleet path is also covered. No scoring formula, metric,
timeout, replicate count, workload or product networking algorithm is relaxed.

## Evidence and implications

- The pre-fix CLI test rejected the switch. The actual frozen image
  `sha256:826569fdeb02f72ec4f24ace7c93b16b69a60e1875c31799c5a0217b402e0d6c`
  independently reproduced that rejection, offline and non-root. Its log SHA-256
  is `7aa7ca3af3095a16cbc2b5543a7051c70a0732fdddbb3009725a83e57a643804`.
- The fix's mode regressions passed three race repetitions. They exercise every
  shard in fresh child test processes, the real parser/settings constructor,
  independent fleet lifetimes and baseline-command forwarding.
- All requested-off attempts in the old frontier archive now have
  `impairment_mode_valid: false` and are ineligible under that condition, even
  when teardown succeeded. Raw CSVs, sidecars and logs are unchanged.
- The final 15,000-provider requested-off trial warmed all 200 clients, failed
  matchmaking probe 6 and cleaned up. Its artifacts are retained as `p15000-off`.
  Similarity to the prior on failure **does not rule out impairment**; the off
  condition was not implemented. No diagnostic signal was delivered.
- The explicitly impairment-on R9 data remains exploratory evidence for its
  recorded source/image. It is not an official or replacement hidden baseline.
  Historical v1 artifact bytes and checksums remain unchanged.

## Final validation gate

Private log root, relative to the mono root:
`temp/fleet-mode-final-validation-20261024.nhNrHSdy`.
The controller gate passed 102 test executions across three race repetitions,
including disposable PostgreSQL/Redis integration tests (35.825 seconds).
Three consecutive complete `tests.sh -count=1` invocations then passed; each
included the six-epoch lifecycle gate and full race suite, with 428 reported
top-level passes across the script's invocations.

| Log | SHA-256 |
| --- | --- |
| `controller-runtime-identity-race.jsonl` | `7d34e64513f5fed2acd30354d41508bc683b8dfed5464d1b775b17f8a39df706` |
| `full-suite-1.log` | `f279773b6ec09c0a258c50fd5b8123404c364773998f563aaa6f6579ab779065` |
| `full-suite-2.log` | `5648de12c49be9579d6d20ad03dc36c8c75c2e75f4c69f0d845e3662bbefab30` |
| `full-suite-3.log` | `3207a7ac5a11c7e56b0854bc3cfb3ce1ee2f4b1103728a1711c58c704e82a47c` |

Their outer script durations were 177, 140 and 139 seconds; race-package
durations were 136.964, 131.856 and 131.830 seconds. Owned fixture containers
were removed after the gate. No timed measurement overlapped this validation.

Earlier diagnostic attempts are preserved, not counted as this gate:
`temp/preparation-runtime-validation-20261024.qwt69Zn8` contains a direct
controller invocation whose local-DB guard refused the missing launcher
attestation. `temp/preparation-runtime-services-20261024.6OH3Jpj8` contains two
successful full invocations followed by an automatic invocation that compiled
during the new impairment edit and found its not-yet-added command helper.
The fresh three-pass gate above started only after all edits were complete.

## Remaining deployment boundaries

Do not run true off-condition trials with the old image. The harness correction
needs an authenticated evaluator build/source identity and exact-image
qualification before those trials resume. Preserve the old checkpoint/image
records; do not relabel old measurements or pull unrelated product changes into
a minimal harness backport. The new main control programs also need their own
installed release before unpinned API preparation can be used.

The separately staged production-image host configuration is prospective only.
Its initial probe observed hardware fingerprint
`60043b9e85d16a551175c72932848a1624e5777db92be489620ae5c7aab7951a`
with `qualification_match: false` and false new-image template/reset/cleanup/
immutable-report checks. Its inherited rebaseline flag names an old staging
round, not a qualified production round. Neither that probe nor a future image
build proves launch readiness.

At 18:22 UTC main `/competition/info` still returned 503, the configured sille
MinIO authority refused connections, and the dedicated model credential was
absent. Main control-plane writes remain on the maintenance hold. No production
round was opened or main database migrated by this work.

## Corrected source publication and build preparation

The fixes and corrected evidence were committed and pushed to main; the tested
runtime changes are present in main merge `822902437d8614b18ed89a850b5c1646a47c55a3`.
Only the four-file impairment correction was backported onto the original frozen
server in a fresh temporary checkout. The published `sim-latency` server head is
now `3d96d16ddb4fc2fb08d3f9003c52e30b9a47241d`. A whole-tree diff excluding
`connect/sim-latency/` is empty against the original `67c87445...` checkpoint;
the other nine repository commits are unchanged. The operator checkout remains
on main, not the competition branch.

The backport built offline with clean VCS metadata and passed its mode tests
three race repetitions (37.582 seconds). This is a smoke binary, not an evaluator
image: SHA-256 `db008bd2206647ef4f74f6e9e6cc5b46b57f004787cbe1c443b57a4b8d2f37e0`.
The test log digest is
`0815ba51411d5db668cc1c26e0613deef030e43156cfa3ed39fe9bce8de0411d`.
Logs and the exact temporary checkout are under
`temp/production-impairment-backport-20261024.Lp0rommJ`.

Config main `c93a289d404ed23cfee11fb9087e8d72689a73ce` publishes the corresponding
preparation ledger and explicitly unactivated source-choice descriptor:

- canonical ten-repository source lock:
  `ba3971eeffae3469bab9a07cfeebb95a1986b3222a8420b7c1a6367756a7f35e`;
- source-manifest file SHA-256:
  `6fca07712cc22b9bb3f9c30ef974392c8e49eff808cab491a8a2ffca08eeeb81`;
- source-choice preparation file SHA-256:
  `ce6e7b23447a180150cc7179518f3ff12150e18d9de05665f7fe0f5dfc16beb5`.

The prior manifest is preserved byte-for-byte in
`config/history/sim-latency-2026-10-24-074239.yml`, SHA-256
`db7d7da3158c5525e1eb74b05a986997dd3bfbb5c4236e5ca10903f01c5bd82f`.
All ten remote refs and frozen module coverage were verified against the new
ledger. This does not activate a production policy or approve its provisional
16.1% floor as newly calibrated.

The corrected evaluator build completed under
`temp/corrected-evaluator-build-20261024.7nUOrNCn`, using a separate frozen
controller workspace and a sparse config checkout containing only the source
ledger. The Docker context includes only the ten product repositories, that
public ledger and trusted evaluator scripts, not the surrounding config checkout.
An initial argument-parsing failure and an unnecessary config LFS checkout are
retained separately; neither started an evaluation. The actual new image ID is
`sha256:79400f3375c64767f5021e43ef852e9ff9da3ca5a4b79dc03e8e9ed381247eb9`;
the embedded simulator/scorer SHA-256 is
`09bfec0e6611c0c8dcca8b55706fda3914fb8ae57a4917ed1d71c56410928402`.
Its raw pretty-printed source-lock file SHA-256 is
`239607aeeae792c009e43367e4e88d00b72d6413d684bbbc10df24658c594937`,
distinct from the canonical model source lock above. The completed image-record
file SHA-256 is
`7bff59a3e12813d18d7a2e7ce00f46c04e6e577ead6585b46db23777bb66aaee`.

The image's own source preflight passed offline, readonly and non-root on the
management CPUs. A separate filesystem census found exactly the ten product
repository directories, no `/workspace/config` or `/workspace/vault`, and no
runtime config/vault `main` or `all` directories. Smoke-artifact SHA-256 values:

- `identity.json`: `9aa81584b97d15ff3c0acecef27e47d27fd63b6456a2d26a4e41a3b3904989ff`;
- `source-check.json`: `c280e11e68b1cef1df7ceab33c27ef77c618795dc403138aa88375239fd086db`;
- `workspace-census.txt`: `83318b263d836ddf24d904313b6d0dd9fb1841766922053dd982e23548f40f30`.

The same image is locally available under the immutable-ID alias
`urnetwork/sim-latency-evaluator-base-id:79400f3375c64767f5021e43ef852e9ff9da3ca5a4b79dc03e8e9ed381247eb9`.
This records local image availability, not registry publication, full resource/
network/host qualification, provider-backed review or an official baseline.

Fresh native main controls also built from a clean private snapshot under
`temp/main-control-native-20261024.L1VK9M5A`; they are **not yet installed**:

| Program | SHA-256 |
| --- | --- |
| `competitionprepare` | `b6d9e2eba649e8d4530e495d09be7504397b156b0285fb44d6497d8eae7c2433` |
| `competitionqualify` | `c069d27e78ccdbe4765fc13ae0de02c3eabadcf204ed2761e5ab3815de489e96` |
| `competitionworker` | `1003f4dbb1cc2a68bd5bc8278f7110cc7a9314600b5da20b9f6c7a6d26d1d6e7` |
| `sim-latency` | `61f7a62206227d03b76f64fa558731df09bf7bef57f241954b3b050a91671143` |

These are main control programs, not the newly evaluated product. Their build
identity is provenance, not a season-wide restriction on future main fixes.
