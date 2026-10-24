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
