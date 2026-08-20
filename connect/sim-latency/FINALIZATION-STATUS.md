# sim-latency finalization status

Status date: 2026-08-20

Topology decision: production qualification uses one authoritative evaluator
host. A second image-identical host is not a launch gate. All containment,
frozen-host, frontier, same-seed, independent-seed, reference-separability,
artifact, and operational gates remain unchanged.

The repository-local simulator, scorer, runner, competition control plane, and
miner package are implemented and locally verified. The corrected 20-run
same-seed campaign is complete and authenticated as **historical local
directional evidence**. It replaced the pre-drain-fix baseline, but the later
10-evaluation-core/2-management-core safety split means it must not be reused
as the final-boundary baseline. The replacement reserved-boundary campaign has
10 authenticated baseline replicates. A subsequent nine-replicate attempt
completed its baseline and A/A executions but failed baseline scoring because
replicate 7 had a G5 stability finding; the then-current failure cleanup erased
its tmpfs evidence, so the entire attempt is excluded. The scorer now reports
sanitized finding codes and the evaluator retains sanitized failed evidence,
but this lost finding cannot be reconstructed. The pending main-branch changes
have since landed and the repositories were synchronized. A new frontier pass
then exposed a separate measurement-boundary instability: at q4/r6, identical
seed/config pristine and no-op runs produced 95.170% and 98.768% success. Their
setup paths took different amounts of time, so provider churn/degraded schedules
entered measurement at different phases; the HTTP warm-up also retained only
two of six parallel lanes by default. Both root causes are now corrected and
deterministically regression-tested, but the post-fix frontier replay remains
the next calibration gate. Neither historical result substitutes for the
post-fix production-host calibration or reference separability.
The integration is
**not Apex launch-finalized**: this checkout has neither the accepted
production-hardware calibration nor the root-owned authoritative-host evaluator
deployment, published image digests, Macrocosmos staging decision, production
credentials, or on-call organization required by `FINALIZE.md`. Those gates
remain explicit rather than being replaced with workstation evidence.

## Complete in this checkout

- Score contract `score_schema: 1`, including attempted-only crawl semantics,
  failure charging, type-7 quantiles, G1-G6 boundaries, median aggregation,
  normalization, visibility, and typed errors (`APEX-SCORE-SPEC.md`).
- Complete-body validation shared by warm-up and measured requests; truncated
  headers/bodies, bad JSON, page-size or `Content-Length` mismatch, read/close
  errors, and cancellation cannot produce a successful 200 row.
- Schema-2 evaluation identity/manifest fields, clean-build enforcement,
  prompt signal cancellation, joined clients/fleet children/services/stats,
  durable stats errors, incomplete states, and authenticated lifecycle marker.
- Deterministic `sim-latency score` with strict CSV/JSON/protobuf parsing,
  failure-ceiling raw score, median replicate aggregation, G1-G6, normalizer,
  diagnostics, and infrastructure/submission errors.
- Deterministic `sim-latency score-baseline`, which validates every same-round
  baseline replicate through the production scorer parsers and freezes the odd
  replicate count, raw baseline, workload contract, and takeover margin.
- Golden baseline/improvement outputs; genuine regression; every individual
  gate; simultaneous failures; boundaries; empty CSV; legacy log noise;
  malformed samples; duplicate JSON keys; NaN/Inf; deterministic output; and
  CLI/core parity tests.
- Fail-closed `official-run.sh` and `OFFICIAL-RUN.md`, including immutable
  build/scorer/workload checks, cgroup/hardware preflight, per-job artifacts,
  TERM/KILL boundaries, external accounting/resource formats, scoring, bundle
  hashes, and final evaluation marker.
- Current-server simulation compatibility: active persisted JWT identities,
  deterministic shared-network admin signing, acknowledged provider modes, and
  simulated egress-health/location evidence required by fail-closed selection.
- Deterministic warm-client fixture order and exchange-host assignment, bounded
  retries of only missing identities, and fail-closed CSV flush/marker behavior.
- The warm-client measurement boundary now proves and retains every parallel
  HTTP/1 lane, requires a complete usable quality window, keeps simulator-owned
  TCP flows alive until explicit teardown, and revalidates the entire pool after
  construction. Provider churn and degraded-regime schedules begin only at the
  authenticated measurement boundary, so variable pool-build time cannot
  phase-shift identical seeded runs. Deterministic barrier/schedule tests pass
  100 iterations, the complete package passes normally and under `-race`, both
  database-backed provisioning fixtures pass 10 iterations, and `go vet`
  passes. The rejected pre-fix q4/r6 evaluator retained 36 sanitized artifacts;
  every manifest hash reverified and cleanup left zero job objects.
- The resident-contract lifecycle race found during bring-up is fixed:
  cancellation can no longer turn a synchronous cache miss into a frozen G5
  `Unexpected error: context canceled`. Selective lifecycle-error recovery is
  regression-tested and unexpected errors still re-panic.
- `TestPtDnsEncodeDecode` no longer depends on time-seeded, correlated random
  impairment or mutates QUIC-owned write buffers while simulating corruption.
  The fixture now uses replayable independent read/write schedules, corrupts a
  private wire copy, uses kernel-assigned UDP ports, and joins attempt-owned
  connection workers. Deterministic schedule and caller-buffer-ownership tests
  cover the root causes; focused normal/race tests pass.
- Every measured run now performs one real quality-ranked, authenticated
  `FindProviders2` probe after the measurement window opens. Deterministic
  tests bind its pool identity, forwarded client address, location/exclusion
  spec, quality-window count, and fail-closed empty-pool behavior. This removes
  the short-run case where warm-up discovery occurred before measurement and
  no in-window matchmaking audit row happened by chance.
- Constructed multi-client window candidates now have one cleanup owner.
  Replacement declines, surplus candidates, and failed initial pings cancel
  the channel and let its `RemoveClientWithArgs` path join Client/OOB work;
  they no longer also call `RemoveClientArgs` and revoke the derived JWT while
  contract-close controls are in flight. A held expand-pass regression proves
  the args are returned exactly once through the constructed-client path, the
  source-level ownership anchor permits only the pre-construction error
  cleanup, and focused normal/race suites pass.
- `sn/api/competition.yml` and matching `server/api` routes for liveness,
  readiness, published policy, hidden-seed round generation/reveal, score
  submission, and polling. Spec/schema/route/security conformance is tested
  alongside the existing `connect/api/bringyour.yml` surface.
- Durable encrypted round storage, one-job FIFO ownership, canonical-patch
  cache identity, submitter ACLs, leases/retry/failover, immutable terminal
  results/events, authoritative-host readiness, and same-round re-baseline enforcement.
- A pinned-command competition worker that authenticates evaluator results and
  retained artifacts, plus the strict host self-check and evaluator protocol.
  These deliberately fail closed without the root-provisioned host boundary.
- A fixed Docker/Compose evaluator now derives one authenticated image per
  `(base, patch, policy, builder)` identity with a networkless build, clean
  deterministic Git commit, offline vet/compile gate, and validated cache
  reuse. A live local smoke passed the shared cgroup parent, the 10-core
  evaluation set with management-only orchestration, memory/swap/PID limits,
  non-root read-only runner, internal-only
  PostgreSQL/Redis stage, trusted schema migration (580/580), cleanup, and
  networkless pristine scorer. It also passed the exact authenticated
  `official-run.sh` preflight and completed a real simulator lifecycle with a
  hash-authenticated run manifest and completion marker using only per-stage
  throwaway credentials. The rebuilt-boundary smoke used base image
  `sha256:22547cd4…7b16a296` and candidate image
  `sha256:80fc0960…b954d75e` and left no smoke container or network.
- The evaluator now binds exactly two frozen host leaves directly into migrator
  and candidate containers: `config/local` and `vault/local`, separately and
  read-only. Their sorted-file manifest hashes are frozen in policy and checked
  before and after every run. It never binds `/runtime`, either repository
  parent, `main`, `all`, or `site`. A full live Docker smoke placed forbidden sentinels beside
  both local leaves, proved they were absent in the container, proved both
  leaves rejected writes, then passed migrations, preflight, a complete
  simulator lifecycle, baseline construction, candidate scoring, and cleanup.
  Sanitized container evidence records mount destinations and read/write state
  without retaining host source paths. A dedicated local JWT key removes the
  last dependency on `vault/all`; evaluation-only credential overrides retain
  fresh per-stage PostgreSQL/Redis passwords without changing ordinary local
  or production resolution. A second live smoke then used the actual host
  `/home/by/urnetwork/config/local` and `/home/by/urnetwork/vault/local` leaves
  directly. Migrations, authenticated preflight, the real simulator lifecycle,
  baseline construction, candidate scoring, and cleanup all passed; writes
  were denied and both source manifests remained byte-identical after every
  stage (`3e231693…2edb8` and `f84b7bdd…3fa`). The pre-freeze development base
  and candidate image ids were `sha256:ac9f70eb…c6da5` and
  `sha256:d138df7f…fa72`, and cleanup left zero job objects. The direct-mount
  implementation is also statically and deterministically tested; its full
  production-scale replay must still be rerun from the clean pushed image after
  the pending source freeze.
- Container bind ownership no longer assumes an identity UID/GID mapping. A
  short labeled probe resolves UID/GID 65532 through the live container
  `/proc/*/{uid,gid}_map`; the evaluator uses those translated host ids and
  retains both mapping digests. Deterministic fixtures cover identity, daemon
  remap, rootless split-root, gaps, overlaps, and malformed maps. The real-local
  smoke passed through this path on the current identity-mapped development
  daemon. Production promotion and host readiness require a real remap plus a
  matching daemon security option, so the current daemon is correctly rejected.
- At 2026-08-17 19:56 UTC, `install-authoritative-host-controls.sh --check`
  authenticated both installed root-owned executables, the shared resource
  boundary, and both systemd units as enabled, active, byte-identical, and
  passing. The CPU service enforces 12 physical CPUs, SMT/turbo off,
  `performance`, Redis-safe overcommit, and the 10+2 split before Docker. Its
  dependent IRQ service discovered 49 movable device IRQs and verified all 49
  exactly on management CPUs `20,22`, with zero failures and affinity SHA-256
  `487d80c7…37cbf`. Repository tests freeze the dependency ordering, complete
  install set, exact IRQ behavior, and hardened Docker daemon semantics. A boot
  cycle has not yet proved persistence.
- Infrastructure failure evidence is no longer discarded with the bounded
  tmpfs. Before unmount, the evaluator stops every job object, removes hidden
  input/scorer copies, all runtime and env credential paths, and every
  non-regular attacker entry, then copies and hash-manifests the remaining
  diagnostics and seals them read-only. A fixture containing credentials,
  hidden inputs, a symlink, and a FIFO passed this sanitizer. A separate real
  evaluator test deliberately failed after mounting the tmpfs and proved
  sanitized retention, unmount, and zero residual containers/networks. Baseline
  scorer errors now list deterministic finding codes without copying raw stderr.
- The production evaluator command then completed the first full-scale local
  protocol request on that reserved boundary against the corrected
  identity-binding and Connect-lifecycle code:
  one 1,800-provider, 200-client, 80-arrival/minute, three-minute baseline and
  comment-only candidate; offline candidate
  derivation, strict build-record validation, independent authentication of
  base/patch/policy/builder/image-key identities, final labels and runtime
  identity, separate fresh baseline/candidate stacks, live cgroup counter
  sampling, pristine networkless baseline builder/scorer, authenticated
  accounting/resources, and cleanup. Independent replay authenticated all 54
  retained evidence files, all six published artifacts, and the completion
  chain; all 15 security booleans were true and no job container, network, or
  evidence mount remained. The development base/candidate image ids are
  `sha256:22547cd4…7b16a296` and `sha256:09b219eb…70c189d`. All G1-G6 gates
  passed and the comment-only candidate was placeable. The single baseline run
  was 27,174.12 ms with its provisional 26.555828% significant-better line at
  19,957.81 ms; candidate raw score was 27,051.04 ms. Success was 99.609% and
  99.430%, and both matchmaking pools had p05 1,165 with no empty sample.
  Evidence-manifest SHA-256 is `65cc0d47…3f4dca4f`, completion SHA-256 is
  `35a9aa53…458d0c5b`, and worker-result SHA-256 is
  `db67aa94…34b08398`. The complete ignored local bundle is retained at
  `eval-12c/container-evaluator-p1800-attempt-011/`. This is one local
  directional pair, not a stable baseline or a season release digest.
- The evaluator now freezes a hostile-job survival boundary before any new
  measurements: 10 physical cores for builds and evaluation, 2 physical cores
  for the worker/cleanup plane, 72 GiB runner + 16 GiB PostgreSQL + 8 GiB Redis
  hard ceilings, and at least 24 GiB unavailable to the active job stack. The
  offline BuildKit job is separately capped at 12 GiB with no swap, the same
  10-core CPU set, a dedicated cgroup parent, a 600-second TERM/KILL deadline,
  and bounded/drained build logs. A live cache-miss build exposed exactly those
  kernel cgroup values. Live attempt 11 independently showed the runner at
  exactly those 10 CPUs and 72 GiB/no-swap while the worker remained on the
  management set. Baseline/candidate stack peak RSS was 14.31/14.54 GB and
  neither run was OOM-killed, hard-killed, or outside its limit.
- The adversarial cleanup gate ran simultaneous CPU and memory bombs. The CPU
  bomb executed on every evaluation CPU—and no CPU outside that set—and
  remained live until cleanup. The fast deterministic OOM probe passed at
  128 MiB. The production-pressure mode then hit the actual 72 GiB runner
  no-swap ceiling, exited 137, and reported `OOMKilled=true`; while it grew, a
  live snapshot showed 41.4 GiB resident and ~902% CPU with the management plane
  still responsive. From management-only affinity the gate removed every
  labeled container and network in 1,019 ms (10,000 ms limit), leaving zero
  residual objects. Durable local evidence is
  `eval-12c/resource-bomb-cleanup-production.json`; deterministic
  source-contract tests cover the boundary and both gate modes. The full evaluator replay now passes; official
  replacement-baseline collection still awaits qualification of the
  authoritative host and selection of the frozen frontier point.
- The evaluator gives the complete untrusted work/evidence tree a root-owned
  32 GiB `tmpfs` ENOSPC boundary with `nosuid,nodev,noexec`, records the quota
  in retained evidence, and copies only the bounded sanitized tree to durable
  storage. A deliberately non-compiling allowed patch produced an authenticated
  terminal `candidate_build_failed` result with no containers, networks, or
  mounted work tree left behind; it did not misreport runtime gates as passed.
  A fresh success-path rerun against the same hardened evaluator authenticated
  all 49 evidence files and correctly left the comment-only patch unplaceable
  when its noisy R=1 sample failed G1, G2, and G4.
- A post-campaign passwordless-`sudo` smoke passed again at 2026-08-17 08:00
  UTC with development base image `sha256:20e5700d…00c65` and newly derived
  candidate image `sha256:c0e390ec…101f7d`. It authenticated offline patch
  derivation and cache reuse, the clean candidate commit, non-root read-only
  execution, shared cgroup/CPU/memory/PID limits, the internal run network,
  fresh PostgreSQL/Redis and trusted migrations, the exact official preflight,
  a complete simulator lifecycle, provider-egress accounting, external
  resource evidence, the real baseline manifest/candidate score, and the
  pristine networkless scorer. Cleanup left zero smoke containers or networks;
  the content-keyed candidate image remains intentionally cached.
- Provisional no-op, deliberately worse, and plausibly better reference patches
  now exist for the exact development base in `competition/references/`. All
  three passed structural authentication, deterministic clean-commit creation,
  networkless vet/compile, embedded simulator provenance, live Docker identity
  verification, and authenticated cache reuse. Their hardened development image
  IDs are `sha256:c3cdba0c…aba931`, `sha256:d9ad4a6d…ac4e2e`, and
  `sha256:25ba429f…c76571`. The better reference's bulk contract lookup now uses
  the unordered key returned by that lookup; a deterministic reverse-ID
  regression test prevents a false-inactive directional-key mismatch. No
  performance ordering has been run, so official separability remains
  `not_run` and these are not season release images.
- The submission builder no longer executes candidate package initialization as
  root in a layer inherited by the final image. Offline vet/tests now run as
  UID/GID 65532 in a discarded stage, the candidate binary is compiled from the
  authenticated source in a separate fresh stage, and the final stage imports
  only that binary. A deterministic Dockerfile contract test enforces the stage
  graph and users. A live adversarial patch then executed `init()` attempts to
  overwrite the entrypoint, official runner, patch validator, database
  migrator, source lock, and `go.mod`; all six final-image files remained
  byte-identical to the pristine base, no build/check directory crossed into
  the image, and the probe image/container cleanup completed.
- The complete hardened-builder Docker/Compose smoke now passes reproducibly.
  Its original 8-provider fixture combined forced 10-second churn, 10 ms
  platform timeouts, and 120 arrivals/minute; this overloaded tiny run crossed
  the unchanged 97% success floor according to runtime ordering. The fixed
  plumbing profile retains the seeded multi-minute uptimes, restores the
  simulator's 3 s/2 s test and announce timeouts, and uses 30 arrivals/minute.
  A source regression test freezes those settings. Two consecutive live runs
  shared config SHA-256
  `857b6e1b03b694987a31af21a1c2b9b6dc8dac6b487ac8fcd0dba051ec5e5045`,
  produced exactly 910 measured rows apiece with zero failures and non-empty
  matchmaking pools, and passed baseline creation plus candidate scoring. They
  used development base/candidate image IDs `sha256:20e5700d…00c65` and
  `sha256:1bcf29ec…c25a7`; these remain local containment evidence, not season
  performance measurements. Cleanup left zero smoke containers or networks.
- Two pre-existing broad-package `go vet` findings found during the final audit
  are fixed without changing the measured simulator binary: key-manager context
  creation now occurs only after seed/key validation succeeds, and a lifecycle
  test no longer copies protobuf lock state while formatting a failure. Focused
  normal/race tests and broad `go vet` pass.
- The dependency-free Python miner client/generator/runner/screener/normalizer
  and reveal verifier, with contract tests and reproducibility documentation.

## Historical local directional baseline: authenticated, superseded

The pre-boundary-fix **eval-48d** campaign completed on 2026-08-16 with 20 consecutive
authenticated 30-minute runs (`r001`–`r020`), 20 total attempts, and zero
exclusions. Every run established 200/200 warm clients, used the same seed-48
fixture and frozen binary, retained a clean G5 log, and produced a matching
CSV/manifest/final-marker SHA-256 chain. Pre-fix and bring-up attempts are
archived separately and were not mixed into this baseline.

| Identity field | Recorded value |
|---|---|
| host | `sille`, linux/amd64, 24 logical CPUs |
| build revision | `05c745050657fdd31f908d7a0e06ef4e26f636d8` (`modified: true`) |
| simulator SHA-256 | `62665d25290c9ee9e81434a542e1ccd959709eb35ccdc82fef511e33204c5b29` |
| fixture SHA-256 | `549ec41c033f344d6e0a6b1de82b404bb63d5a8dfb5861b6c4b6d55886cdace4` |
| measured duration | 1,800 seconds per run |
| request-timeout ceiling | 120,000 ms |

Selected run-to-run results:

| Metric | Mean | SD | CV |
|---|---:|---:|---:|
| Apex raw score | 23,823.31 ms | 696.30 ms | 2.923% |
| success rate | 98.846% | 0.114 percentage points | 0.116% |
| total latency p95, successful rows | 20,902.62 ms | 435.72 ms | 2.085% |
| TTFB p95 | 1,025.40 ms | 47.82 ms | 4.663% |
| throughput p50 | 290.75 kB/s | 8.20 kB/s | 2.819% |
| throughput p05 | 84.99 kB/s | 2.35 kB/s | 2.765% |
| goodput | 41.151 MB/s | 0.299 MB/s | 0.727% |
| throughput p95 | 11.258 MB/s | 2.401 MB/s | 21.330% |

No scored metric had an absolute time-drift t statistic of 2 or greater. The
audit reported `r004` and `r011` as robust TTFB-p95 investigation candidates;
both remain in the baseline because neither met a predeclared exclusion rule.
The primary diagnostic SD sequences converged within the local 10% heuristic,
but the Apex raw-score SD sequence did not. Some between-run SD estimates are
also below within-run block SE, so the report conservatively warns that the
noise floor may be understated.

The scalar result therefore **does not support a single-run 1% takeover**. Its
quarter-margin rule implies a smallest locally supported margin of 11.691%, and
deterministic median-of-R estimates for R=1,3,5,7 all remain above 0.25% CV.
This is evidence against freezing a cheap local policy, not permission to set
an official margin.

Resource and matchmaking audit highlights:

- peak simulator RSS 14.97 GiB; mean simulator CPU 10.23 logical-core
  equivalents; peak established TCP sockets 26,051; zero swap in every run;
- complete host and native PostgreSQL/Redis telemetry coverage for all 20 runs;
- 20,947 in-window FindProviders2 samples, zero empty pools, and at least
  99.46% measured-window sample span for every run;
- no panic, fatal, scorer G5, or measured-window `ENOBUFS` signature.

Two separately collected held-out runs (`h001`, `h002`) also authenticated.
Their A/A result is **indistinguishable** at alpha 0.05. The raw-score delta was
-0.653% (B versus A); median-throughput and TTFB-p95 deltas were 0.625% and
2.075%, respectively, and neither primary was significant after Holm
correction.

Authoritative local artifacts:

- `eval-48g/baseline.json`
- `eval-48g/baseline-summary.json` and `baseline-summary.md`
- `eval-48g/baseline-stability.svg`
- `eval-48g/heldout-aa-compare.json`
- `eval-48g/postcampaign-verification.log`

These remain authenticated historical noise-study artifacts, not a signed
`sim-latency-score-baseline` manifest. They are not eligible to become the
final baseline: the old duration path canceled every in-flight crawl at
`measure_end_ms`, emitted those partial requests as status-0 failures, and then
charged them at the timeout ceiling even though their arrivals were in-window.

The corrected runner now closes only the arrival gate, drains admitted crawls
for up to their own deadline, and fails closed if that drain cannot finish. A
five-minute 1,800-provider/200-client/80-arrival candidate run on the 12-core
affinity completed all frontier gates with 99.192% success and a 30,608.37 ms
raw score. The comparable pre-fix run had 96.985% success and a 34,794.46 ms
raw score, so removing the artificial boundary failures improved the scalar by
12.031%. This is corrected frontier evidence only; the same-seed baseline,
independent-seed campaign, and reference patches still require the qualified
authoritative-host environment.

The first post-probe prequalification binary was pinned at
`eed07dbbc495922242a7a996c136f9c5eb60f13277dbd07480f390107734211b`.
Its four completed three-minute 1,800-provider quality-window points passed
their frontier gates but are now archived rather than mixed forward:

| quality window | mode | raw score | success | mean CPU cores | peak RSS | cleanup 401s |
|---:|---|---:|---:|---:|---:|---:|
| 2 | impaired | 26,045.63 ms | 99.451% | 5.21 | 10.93 GiB | 21 |
| 2 | no impairment | 27,767.23 ms | 99.315% | 5.03 | 10.98 GiB | 10 |
| adaptive | no impairment | 24,955.71 ms | 99.307% | 5.31 | 12.43 GiB | 31 |
| adaptive | impaired | 25,659.11 ms | 99.703% | 5.67 | 12.76 GiB | 31 |

The q=2 pair reported impairment 6.200% faster while the reversed adaptive
pair reported impairment 2.819% slower. Both profiles' first observation was
faster, so the two pairs identify order/noise, not an impairment effect. More
importantly, the remaining `401` signatures exposed three constructed-candidate
paths that canceled their channel and directly removed its args even though
the channel cleanup already owned them. The direct removal revoked auth before
the owner could send final controls.

That double-cleanup root cause is fixed and the replacement binary is pinned at
`4db9cea1be48ec91bc5ef6965912b6b7f221a7a53d50c0d6b794637733a4a4e8`.
Its adaptive/impaired campaign completed 20/20 consecutive, authenticated,
frontier-eligible runs (`verify-r001`, then `baseline-r002` through
`baseline-r020`) with the pinned config
`090b3931275d835e7d3166bb1833d221ba41751eec6a682d68aae805b951f138`,
seed 48, 1,800 providers, 200 warm clients, 80 arrivals/minute, four exchange
hosts, four fleet shards, a three-minute measure window, and CPU affinity
`0,2,...,22` with `GOMAXPROCS=12`.

This 20-run series predates the hostile-job survival split. Because the final
evaluator now exposes 10 physical cores to the workload and reserves 2 for
management, the series is historical planning evidence only. Its exact
baseline and threshold remain useful in previews, but the final-boundary
same-seed campaign must start over after the 10-core evaluator replay passes.

The raw-score mean is 27,175.78 ms, sample SD 1,804.19 ms, CV 6.639%, median
26,884.88 ms, p05 24,916.07 ms, p95 30,430.63 ms, and range
23,903.49–30,590.22 ms. The local quarter-margin rule therefore pairs that
27,175.78 ms directional baseline with a significant-better threshold of
19,959.03 ms, 26.556% lower. This is a planning bound, not a frozen takeover
policy. The chronological trend is not persuasive (`t=-0.373`), and the
last-five SD-prefix relative span is 7.260%, which passes the local 10%
convergence heuristic; the sample-SD relative standard-error estimate remains
16.222%.

All runs completed with 200/200 warm clients. Six runs needed exactly one
bounded retry of a missing client. Success ranged from 99.107% to 99.815%
(mean 99.498%); all 20 runs had zero empty pools, cleanup `401`s, `ENOBUFS`,
swap, or failed gates. Maximum per-run mean simulator CPU was 5.73 of the
12-core budget and maximum peak simulator RSS was 12.97 GiB. Every completion
marker authenticates its final run manifest, every manifest authenticates its
CSV, diagnostic telemetry is complete, the campaign has one identity, and two
fresh aggregate generations matched the authoritative JSON and Markdown
byte-for-byte.

Run 8 completed and sealed before a live edit to post-run diagnostic summary
fields caused its already-running shell wrapper to stop after writing the
summary and before starting run 9. Its completion-marker hash matches the
immutable run manifest and the stable wrapper re-summarized the retained CSV,
manifest, samples, and telemetry with every gate passing. No simulator attempt
was lost or repeated. Runs 9–20 restarted fail-fast and completed with the
unchanged binary, fixture, flags, affinity, and runtime protocol; the stable
wrapper SHA-256 is
`345f926425288533f3a057f6ef32de9c797ee22d4615f085be3e9ef543c5dbe7`.
These measurements remain directional: the official same-seed clock starts
only on the qualified authoritative-host boundary.

## Local verification: complete

`verify-local-baseline.sh` passed at 2026-08-16 18:01:01 UTC. It replayed the
baseline and summary twice byte-for-byte; authenticated every run, sample,
telemetry file, held-out run, and pinned identity; then passed:

```text
go test -count=1 ./connect/sim-latency ./stats
go test -race -count=1 ./connect/sim-latency ./stats
go vet ./connect/sim-latency ./stats
go test -run '^$' ./...
go test -count=1 ./connect -run '^(TestExchangeWaitForIdleJoinsAcceptedConnectionOwnership|TestExchangeWaitForIdleJoinsResidentInternalClientOwnership|TestConnectHandlerCloseJoinsAndClosesPreboundPacketConns|TestConnectionAnnounceChildWorkersCloseAdmissionAndJoin)$'
```

A follow-up audit completed at 2026-08-16 18:18 UTC. It reran the normal and
race-enabled `connect/sim-latency` and `stats` suites, the three
resident-contract cancellation regression tests in normal and race modes, a
broadened `go vet ./connect ./connect/sim-latency ./stats`, and the repository
compile gate. All passed. Those resident-contract tests are now included in
the verifier's permanent focused `connect` gate.

The post-campaign audit completed at 2026-08-17 08:00 UTC. With the exact
`test.sh` local environment variables it passed normal and race-enabled
`connect/sim-latency` and `stats`, focused normal/race DNS, impairment,
buffer-ownership, joined-cleanup, and single-owner constructed-client tests,
broad affected-package `go vet`, and compile-only gates for every package in
both the server and connect repositories. The competition API/handler,
  evaluator, and patch packages passed normal and race suites; all 17 Python
  miner contract tests passed. An initial direct server test command omitted the
required `WARP_ENV`/vault settings and failed only that harness precondition;
the harness-correct invocation passed and is the recorded result.

The pre-reserve identity-bound evaluator audit completed at 2026-08-17 09:48 UTC.
With the same `test.sh` environment, the complete `competition` package passes
normally and under `-race`; `go vet`, shell syntax, diff checks, and the focused
builder/evaluator identity regressions pass. The live malicious-initialization
gate also passed again with all six protected files unchanged and its exact
probe image removed. A first manual isolation invocation supplied a raw image
id where BuildKit requires a local alias and failed before candidate execution;
the authenticated alias resolved to that exact id and is the recorded passing
invocation.

The management-reserve audit completed at 2026-08-17 10:53 UTC. The focused
and complete `competition` suites, race suite, `go vet`, shell syntax, and diff
checks pass. The resource-boundary contract deterministically checks the 10+2
CPU split, 72/16/8 GiB active ceilings, >=24 GiB reserve, 12 GiB offline-build
limit, deadline, and bounded log capture. Live Docker verified a cache-miss
BuildKit cgroup with the exact CPU/memory/no-swap values, repeatedly passed the
fast CPU/OOM gate, and passed the production 72 GiB pressure gate. The latter
completed management-only cleanup in 1,019 ms with zero residual containers or
networks.

The rebuilt-boundary protocol audit completed at 2026-08-17 11:35 UTC. The
development base is `sha256:22547cd4…7b16a296`, source-lock SHA-256
`40bb4579…313896e`, and simulator SHA-256 `216f6985…ad244176`. Its live smoke
passed before full-scale attempt 11. Independent replay then authenticated
54/54 retained files by hash and size, 6/6 published files, the completion
chain, all 15 security booleans, and zero residual containers, networks, or
evidence mount. Both runs established 200/200 clients, used `num_cpu: 10`,
completed without OOM/hard kill/limit escape, and passed G1-G6. The one-pair
result is directional protocol evidence, not the required 20-run baseline.

The reserved-boundary same-seed campaign then authenticated attempt 11's one
baseline replicate and attempt 12's nine baseline replicates, for 10 usable
replicates. Attempt 13 completed nine baseline and nine comment-only A/A runtime
executions, but its pristine baseline scorer rejected replicate 7 as unstable.
The surviving outer log contains only the replicate number, not the finding;
the old cleanup path then unmounted and removed the tmpfs. Attempt 13 therefore
contributes zero baseline replicates. At 2026-08-17 18:30 UTC, exact local-only
mount tests, sanitized failure-retention fixtures, a real post-mount evaluator
failure, deterministic stability-code tests, and the full Docker smoke all
passed. The final campaign will restart from the clean pushed source identity
after the explicit pre-freeze pause is cleared.

The pre-freeze import review now records a single literal editable file,
`connect/resident_contract_manager.go`, in `competition/PATCH-SURFACE.md`.
`TestExamplePatchPolicyMatchesReviewedSurface` rejects glob expansion and
explicitly proves that competition, simulator, stats, migrations,
`config/{local,all}`, `vault/{local,all}`, site, and module metadata remain
outside the patch surface. The final blob identity and policy digest still
depend on the clean pushed season base.

Host-containment promotion now replays the evaluator's worker result,
completion marker, evidence manifest, every declared evidence hash/size, exact
runner mounts, and the production CPU/OOM cleanup report before it can update
the root-owned qualification marker. The self-check no longer mistakes the
host route table for proof of candidate isolation. A deterministic root-owned
fixture passes a valid chain and rejects a hash-consistent adversarial chain
that mounts `/runtime/config`, leaving no failed marker behind.

The artifact verifier additionally requires 20 consecutive authenticated tags,
complete warm pools, success >=97% in every run, non-empty matchmaking pools,
full resource coverage, zero swap, and the held-out `indistinguishable` verdict.

The 2026-08-17 20:07 UTC pre-freeze regression passed the exact `server/test.sh`
environment for race-enabled `competition`, `api`, `connect/sim-latency`, and
`stats`; affected-package vet; all 17 Python miner-contract tests; host-control
and 49/49 IRQ live checks; valid/adversarial containment promotion; and the fast
hostile CPU/OOM cleanup gate (683 ms, zero residuals). The DNS encode/decode
test plus its deterministic schedule, caller-buffer, and joined-cleanup
regressions passed five consecutive normal executions and one race execution.
The direct local-leaf hashes remained `3e231693…2edb8` and
`f84b7bdd…3fa`, and no labeled job/probe/bomb object remained. An initial Go
invocation without the `test.sh` environment was rejected at the documented
`WARP_ENV` harness precondition; the correctly configured runs above are the
recorded results.

## Deliberately still open

- UR engineering review/signature and Macrocosmos confirmation of the score
  contract, submit/poll timeout, hidden-seed flow, and reveal policy.
- Official-box frontier sweep and 20-run reproduction, at least 20 independent
  seeds, execution of the provisional no-op/worse/better references after
  promotion to the frozen season base, 19/20 separability, accepted noise
  policy, and a signed production `APEX-CALIBRATION.md`.
- Clear the explicit source-freeze pause; review all pending main fixes, commit
  every participating repository, pull/merge/push, verify the clean pushed
  source lock, rebuild the evaluator image, and restart the 20-run campaign.
- Frozen production scale, duration, replicate count, takeover margin, tuning,
  base tag, exact patch allowlist, and immutable evaluator image.
- Root-owned deployment on one authoritative machine: exactly 12 exposed
  physical CPUs split into 10 evaluation CPUs and 2 management CPUs, frozen
  SMT/governor/turbo/NUMA/IRQ/kernel/microcode, one parent boundary for the
  runner/PostgreSQL/Redis, at least 24 GiB management memory reserve, template
  DB/Redis resets, default-deny networking, offline build cache, immutable
  artifact retention, cleanup, and monitoring. At 2026-08-17 19:56 UTC the
  enabled root-owned CPU/IRQ units put exactly 12 physical CPUs online, disabled
  SMT/turbo, selected `performance`, set `vm.overcommit_memory=1`, produced the
  live 10+2 split, and pinned all 49 movable IRQs to CPUs `20,22`. The local
  Compose boundary and adversarial CPU/OOM cleanup gate pass. Still open: prove
  both units after reboot; freeze NUMA/kernel/microcode and the final host
  digest; enable Docker user-namespace remapping or rootless mode; verify the
  firewall; and restart Docker in a controlled window (its daemon still reports
  the pre-control 24-CPU view and `/etc/docker/daemon.json` is not yet installed).
- Final source-bound patch-policy digest/base tag and promotion or regeneration
  of the provisional reference patches against them; the literal pre-freeze
  file review is complete, but must be re-authenticated after the pending main
  fixes. Root-owned worker deployment, the adversarial probe day, published
  API/worker/evaluator image digests, and production credentials also remain.
- A Macrocosmos decision between an external scoring-service adapter and a
  dedicated-host referee profile. The current public Apex sandbox is limited
  to 4 CPUs/4 GiB and has no external generate/submit/poll adapter, so its stock
  contract cannot contain this evaluator. Real contract harness/staging,
  HANDOFF acceptance, registry activation, and on-call ownership follow that
  decision.

The launch gate remains closed until that external evidence exists. In
particular, this local 24-logical-CPU campaign must not be copied into
an official 12-core signed round baseline.
