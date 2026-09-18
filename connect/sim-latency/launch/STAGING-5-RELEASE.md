# Staging epoch 5 release

Prepared on 2026-09-18. The shared-baseline, simulator-lifecycle, and timing-
serialization fixes are built into the installed image below; independent Go,
control-plane compatibility, clean-image offline build gates, and full Docker
smoke passed. This release is ready for staging API/config deployment, not yet
live. Do not announce a new open round from this local release record.

## Frozen evaluator

| Identity | Value |
|---|---|
| Source server commit | `807b473c927d1ae09a03276bb9758afb715fac9e` |
| Reviewed main commit | `543d98378bc96f84bff076f7edf7da4dfdb26f19`; source merge preserves the previous competition branch ancestry |
| Identical main/source tree | `8dea9f12298e2359e3eab8a5c8e8c0812abc760e` |
| Evaluator image | `sha256:b0c07cf45c30adb483ee5c215b7426b2e098c0b35c7cf4c4abcb0166cb43a87e` |
| Local image tag | `urnetwork/sim-latency-evaluator-base:shared-baseline-epoch5-807b473c` |
| Simulator/scorer SHA-256 | `e27929e9f2ef45f9f23c2120b651fafe048f38d82f3ca308b1b200b48ea05cbf` |
| Source ledger SHA-256 | `f123d5332a2d68a387c983de373d5eafacb6d797c04039c087962c410d3172ca` |
| Complete source-lock SHA-256 | `fff5bd221ff13c37f4a0250ced9458c1ceba056bbe91d22d72280c908bd0a58c` |
| Installed release | `/usr/local/libexec/urnetwork/competition-b0c07cf4` (root-owned, read-only) |
| Evaluator command SHA-256 | `a71956232074e7bb09e2014037db4f62e4aac6ed1230f6cc89aeeb62254819f2` |

`config/main/sim-latency.yml` locks server, connect, sdk, proxy, glog,
goidenticons, userwireguard, sn, operator-proxy, and warp. Both `sim-latency`
and `sim-latency-staging` remote branches were published and verified at all
ten exact commits. No long-lived product checkout was moved to those branches.
Staging epoch 5 is an API round number; its pre-production source checkpoint
remains source epoch 0. It is not production epoch 5.

The image was built from clean remote clones, without a development overlay.
Its embedded Go build identity is the source commit above with
`vcs.modified=false`. Only `config/local` and `vault/local` are candidate
configuration mounts; their unchanged hashes remain in `config/main/competition.yml`.

## Scoring change and historical compatibility

The first evaluation measures and freezes one nine-run trusted control before
building the first submission. Every candidate contributes nine independent
runs and uses those same authenticated baseline bytes. New shared-control
rounds rank by absolute median raw latency, then submission time and job id;
normalized score is display-only. The initial improvement margin is still
16.1%, with all correctness/resource gates and the Welch significance test.

Historical per-job-control rounds retain their original rankings. The API
upgrade must include the compatibility migration and controller checks; a
round cannot acquire a shared-control identity after legacy scores or
finalization. This is a control-plane change on `main`, not a change to the
frozen measured product above.

Epoch 4 finalized at `2026-09-17T14:56:00.946063Z` with four leaderboard
entries. Macrocosmos confirmed its downstream path worked. Its source,
stored scores, and published ranking remain historical evidence, not a
measurement of this new evaluator.

## Validation and deployment order

- The full `sim-latency/tests.sh` suite, offline `go vet ./connect/...`,
  compile-only candidate-package checks, targeted controller tests, migration
  and monitor checks, API/OpenAPI conformance, and launch-contract tests passed
  before freezing the source graph.
- The image's offline build gates passed for wrapped database write-timeout
  recovery, unsafe retry rejection, shared-score contract, and explicit
  shutdown classification, plus the three handler-heartbeat/H1-listener
  regressions, and the producer/CSV/baseline/scorer precision round-trip. Its
  Go build identity is the clean source checkpoint above.
- Historical ranking tests cover migration 677 to 678, unchanged legacy
  leaderboard order, shared-control raw-score order, ordered honesty review,
  and rejection of missing or retroactively attached shared controls.
- The simulator lifecycle database selector passed in 50 seconds; the full
  race suite passed in 123 seconds, the actual `tests.sh` entrypoint in 122
  seconds, and offline `go vet ./connect/...` in 70 seconds. Log hashes are in
  [the machine-readable release record](../playbook.yml).
- The final precision revision passed its focused tests in 6 seconds, actual
  `tests.sh` entrypoint in 130 seconds, separate full race suite in 113 seconds,
  and isolated offline `go vet ./connect/...` in 71 seconds. Tests cover rounding
  boundaries, byte-for-byte CSV compatibility, all nine summary metrics,
  nonzero bootstrap uncertainty, real baseline/scorer round-trip, and rejection
  of one-floating-point-step or count tampering. Log hashes are in `playbook.yml`.
- Full Docker smoke passed in 351 seconds: 17 checks including offline
  candidate build/cache reuse, direct read-only local-only configuration,
  nonroot containment, trusted migrations/preflight, completed simulation,
  provider accounting/resource reports, a real baseline manifest and candidate
  score, and a pristine networkless scorer. No labeled containers or networks
  remained. Log SHA-256:
  `2d85794ee3122be18638f0c8d5054220981d295528756bf6eae762cf54b75964`.
  This uses a small development fixture, not the nine-run production baseline.
- The prior authenticated host checker separately passed every common
  containment check. It exited 1 with `qualification_match: false`, so exact-
  image production qualification is still absent. The reported historical
  image and qualification identity, and the complete log hash (including its
  retained stderr diagnostics), are recorded in `playbook.yml`. No `/etc`
  manifest was rewritten. Only the explicit staging exception permits this
  prior containment report after a staging round has been scheduled.

The previous image (`51252f63…`) completed simulation but correctly failed
baseline scoring: `ttfb_p50_ms` was `2.678519` in the live manifest versus
`2.679` in persisted CSV. That 349-second run exited 1 and left no residual
labeled resources; log SHA-256
`78f7d924d716bdc0d1802d1898ce4d2c58884c9d08bf87b138e1d27f38cb9205`.
The current producer retains the exact timing values it writes to CSV.
CSV bytes, scorer tolerances, formulas, and historical evidence are unchanged.

An earlier candidate image (`a13bc0da…`) failed runtime warmup: all 32 providers
remained connected, but the simulator never registered or heartbeated their
connection handlers in `network_client_handler`. Current production reliability
selection therefore excluded every provider before measurement. That
335-second attempt retained its artifacts and left no containers or networks;
log SHA-256 `eb9eb6793851b05ab3ce95c9a1801add0d299ec0c203fc6965b2e7f5894f2f55`.
The current release implements owned registration, heartbeats, joined cleanup,
H1-only listener setup, and synchronous HTTP bind ownership. Deterministic
tests prove missing-handler recovery, heartbeat freshness, preserved foreign
registrations/history, failed-startup cleanup, and join-before-delete ordering.
Production eligibility, cache behavior, thresholds, and workload are unchanged.

The first candidate image (`b9f86e6d…`) was rejected, not deployed: its offline
candidate vet exposed a server fixture using a newer Connect API. Connect is
now pinned at `e6637e83b299264b3c16b0c4a1f85dc856bfcaea`, and base-image builds
run the same offline vet surface as candidate builds. A deterministic contract
test protects that gate. An earlier bare-image-ID smoke invocation failed
before evaluation; smoke now uses the ID-verified named tag.

Deployment sequence:

1. Deploy the final server `main` API and run all migrations, including the
   append-only round-baseline and historical-ranking compatibility migrations
   through migration 678.
2. Deploy the matching config commit through config-updater. Confirm public
   `/competition/info` advertises the source and evaluator digests above.
3. Explicitly create staging epoch 5 with a future opening time, then run its
   staging-aware worker preflight and singleton worker from current `main`
   before admission starts. Admission timestamps belong to that newly created
   round. The old finalized round cannot authorize staging worker preflight;
   strict production readiness still requires exact-image qualification.
4. Verify one shared baseline identity across actual successful jobs and the
   finalized staging leaderboard before claiming the new scoring path ready.

Staging retains the prior authenticated containment checker and qualification
record. This is deliberately not exact-image production qualification. Do not
promote it to a public production round without the production host/baseline
qualification and remaining launch approvals in `PLAYBOOK.md`.
