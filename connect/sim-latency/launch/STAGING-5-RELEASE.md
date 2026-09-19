# Staging epoch 5 release

Prepared on 2026-09-18; staging deployment verified on 2026-09-19. The
shared-baseline, simulator-lifecycle, and timing-
serialization fixes are built into the installed image below; independent Go,
control-plane compatibility, clean-image offline build gates, and full Docker
smoke passed. The API/config release is deployed, staging epoch 5 is open,
and its singleton worker is running after staging preflight with authenticated
host refreshes. Live shared-control scoring and exact-image production
qualification remain unproven.

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
Its public `providers.yml` reveal succeeded under the deployed API on
2026-09-19, with SHA-256
`3f3829a588e4c024459e2c4c653be8244e2e515c56da645e3c6447b9c1d99fae`.

## Staging deployment record

The public API reports version `2026.9.18+1049819730`; repeated
`/competition/info` checks match the frozen source and evaluator image above.
Read-only database verification found migration audit maximum 683, the
`competition_round_baseline` table, and both legacy/shared-ranking guards.
No earlier staging jobs were queued or running before the new round was created.

The API returned HTTP 201 for staging epoch 5, round
`01a0b913-c344-27f3-cb64-338dbddc8b07`, initially `scheduled` with opening
`2026-09-19T09:57:00Z`, close `2026-09-21T09:57:00Z`, and the same reveal
timestamp. Results remain embargoed until close and backlog drain. This
48-hour staging window does not alter the seven-day production contract.
No production round was created.

| Worker identity | Verified value |
|---|---|
| Server main commit | `07b210bc76f15c714d2de8484153e407bbf65808` |
| Worker image | `sha256:07d6caa778715a1e104fdf661eaaf68f3c13b1ca2b0e4dee2cb0246d45c6dcae` |
| Local image tag | `urnetwork/competitionworker:staging-07b210bc76f1` |
| Installed binary | `/usr/local/libexec/urnetwork/competition-worker-07d6caa7/competitionworker`, root:root mode 0555 |
| Binary SHA-256 | `f5fed5489bc49a37e60300efcfdf0541fb2c8b6f5c0f7d0feb5bd0592ef59ecd` |
| Singleton service | `urnetwork-sim-latency-staging-epoch-5.service` |
| Start time | `2026-09-19T09:53:30Z` |
| Invocation | `ae75d48be35948189dea302ab747d6f8` |
| Initial process | PID 3449082, management CPUs `20,22`, `OOMScoreAdjust=-900`, restart-on-failure, zero observed restarts |

Independent worker version/image proof and staging preflight passed; preflight
took two seconds. Evidence log SHA-256 values are
`e915831831edada6a1b111f564d3b8d1d16693450f7bde71a9336a3939e6d4e2`
and `be0021cca91d5bbe68d40b61bb4c618995d6dfeddf48692eb41b77fd1be12431`.
Creation and source-check artifacts are retained under
`/tmp/urnetwork-staging5-launch.tNf1cYJO`; the creation response SHA-256 is
`374edcae7f66581d6aced00196e959793a4f61bc1ee45eadfe7e2a17006b32a9`.
At `2026-09-19T09:57:30Z`, `/competition/info` reported this round `open`
with unchanged source/image and admission timestamps. The worker remained
active/running with the same PID/invocation and zero restarts; authenticated
staging host refreshes continued through `2026-09-19T09:57:19Z`.
Three further API checks at `2026-09-19T09:59:31Z` matched the open round,
schedule, and source/image. The shared baseline count was zero while awaiting
the first submission; a successful shared-control score is not claimed.
Staging retains the existing null-winner policy after close and drain; shared
baseline scoring does not name or promote a staging winner.

Redis remote cluster ports again refused connections on September 19. The
worker used the supported authoritative PostgreSQL FIFO fallback; this degraded
dispatch path does not block staging admission. At `2026-09-19T09:58:17Z`,
an authenticated public-TLS Grafana/Prometheus query returned HTTP 200 with
epoch 5, staging 1, queue/backlog zero, and heartbeat age 25.955 seconds
(below the 30-second stale threshold), for `host=sille`, `env=main`,
`service=sim`. Source-info metrics matched the exact worker image/revision.
Response SHA-256:
`9eeab8f1dd3975d96d1c63be412bc79e014f6cb9c70289401ea557ac1572378a`;
verification metadata SHA-256:
`d2df1abfa95e22f4d2d0d5f3e5689029b080c0ce852a95244f6f4c36e13ec3d8`.
A read-only database check at `2026-09-19T09:58:11Z` found the host heartbeat
at `2026-09-19T09:57:59Z`, age 11.897 seconds. The metrics forwarder is active
but transient; no reboot persistence or endpoint failover is claimed.
The retained containment checker authorizes only the staging exception;
strict exact-image production qualification remains pending.

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

Deployment sequence (steps 1–3 completed on September 19):

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
