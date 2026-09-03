# PERFVAR regression and repair agent harness

This is the operating contract for an agent that measures the current network
stack, compares it with the active performance baseline, repairs demonstrated
regressions, and promotes measured improvements. It covers the deterministic
PERFVAR environment plus the two attached Android test devices. The research
contracts remain
[`connect/LOWBAR.md`](../../../connect/LOWBAR.md),
[`connect/MEMSTEADY.md`](../../../connect/MEMSTEADY.md), and
[`PERFVAR.md`](./PERFVAR.md).

`sim-latency` is deliberately outside this harness. Its separate continuous
test suite owns its instruments, A/A variance studies, comparisons, and
baseline. A PERFVAR campaign may cite a completed compatible sim-latency result
as supporting evidence, but must not run, wait for, or promote sim-latency as
part of this protocol.

## Execution model assignment

Use separate model roles for reproducibility and review quality:

- **Terra max** owns test execution: preflight, simulator and Android session
  blocks, benchmark collection, privacy-safe ledger entries, and validation of
  record counts, schemas, hashes, and gates. Terra must not tune the candidate
  or rewrite a frozen campaign after observing results.
- **Sol max** owns failure diagnosis and repair: inspect Terra's preserved
  artifacts, establish the causal boundary, add deterministic reproductions,
  implement the smallest fix, and run focused/race/package verification. Sol
  must not discard failures or change acceptance thresholds, workloads, or
  statistics to make a result pass.
- The coordinating agent reconciles the two reports, reruns the affected
  cohort and full gates after any Sol fix, and records both the measured result
  and the model role in the private manifest. A Sol fix is retained only when
  Terra's repeat measurement is non-regressing under the unchanged contract.

All new campaign records go only to
[`tests/PERFVAR-MEASUREMENTS.md`](../../../tests/PERFVAR-MEASUREMENTS.md).
The older measurement sections in the research documents are historical source
material. Do not append a new harness run to those documents. Update them only
when a result changes the research explanation or product contract, and point
back to the campaign ID in the tests ledger.

## Required outcome

Run the complete compatible matrix, preserve every valid attempt, and issue
exactly one campaign verdict:

- `IMPROVEMENT`: at least one primary metric is meaningfully and statistically
  better, no primary or guard is statistically worse, and every hard gate
  passes;
- `REGRESSION`: a primary or guard is statistically worse, or a hard gate that
  passed in the control fails in the candidate;
- `MIXED`: significant improvements and regressions coexist;
- `INDISTINGUISHABLE`: the campaign has enough valid observations to compare,
  but no predeclared meaningful effect is established;
- `INVALID_ENVIRONMENT`: the intended cohort was never measured because a
  predeclared identity or eligibility condition failed; or
- `BLOCKED`: a required external dependency, service, device, or reconstructible
  control is unavailable.

`REGRESSION` and `MIXED` are investigation states, not end states. Establish the
failing boundary, add a deterministic reproduction, implement the smallest
root-cause fix, and repeat the affected cohort and full qualification gates.
Keep only fixes whose final measurements are non-regressing. A run is complete
only when the retained tree is `IMPROVEMENT` or `INDISTINGUISHABLE`, or a real
external blocker is documented with the evidence needed to remove it.

## Non-negotiable boundaries

- Never edit PERFVAR, Android collectors, workloads, eligibility rules,
  statistics, or baselines to make a candidate win. An intentional instrument
  change creates a new schema and requires a fresh A/A noise study.
- Never discard a slow, failed, memory-heavy, thermally affected, or
  inconvenient completed attempt after seeing its result. Only a predeclared
  identity/eligibility failure may invalidate an attempt. Preserve invalid and
  interrupted attempts separately.
- The independent unit is a simulator run/process or an Android session block,
  not a request, packet, telemetry sample, or Go benchmark iteration. Do not
  manufacture significance from autocorrelated observations.
- Keep credentials, device serials, client/provider IDs, IP addresses, carrier
  and cell identifiers, DNS answers, cookies, headers, packet payloads, and
  signed URLs out of logs, commits, and the measurement ledger.
- Do not infer an HTTP status, origin policy, or bot challenge from encrypted
  payloads. Browser-only response metadata is diagnostic and may not be turned
  into a Connect packet-blackhole signal.
- Android `goRuntimeBytes` is the allocation surrogate for this campaign.
  Android whole-app PSS is diagnostic and does not prove the iOS Network
  Extension `phys_footprint` or jetsam boundary.
- The Connect mobile ceiling is absolute: `goRuntimeBytes` must never exceed
  25,165,824 bytes (24 MiB) in an acceptance sample. Statistical memory
  improvement, a low p95, or later reclaim cannot override this gate.
- Preserve unrelated working-tree changes. Build controls in temporary sibling
  worktrees; do not reset, switch, or clean an operator checkout. Do not pull,
  rebase, commit, push, deploy, or change public-provider state unless the
  operator explicitly requests it.
- Profilers perturb allocation, scheduling, and GC. Profile runs are
  attribution-only and never enter an acceptance comparison.

## Source and baseline identity

The active baseline registry and every new result live in
`tests/PERFVAR-MEASUREMENTS.md`. A comparison is legal only when its baseline
entry is reconstructible and its compatibility key matches the candidate.
At minimum the key contains:

- harness and result schema, workload, profile, seed family, payload, direction,
  topology, route/transport, resource profile, and all non-default flags;
- server, Connect, SDK, Android, proxy, and tests revisions plus complete dirty
  state hashes where applicable;
- Go/toolchain, OS, architecture, CPU, `GOMAXPROCS`, race/profile mode, and
  PERFVAR simulator config hash;
- Android device role, model, OS/build, app/test build ID, Chrome version,
  underlay, VPN transport, provider mode, and public versus exact pinned exit;
- provider build/security-policy identity when exposed, without provider ID;
- thermal/power eligibility, run order, cold/warm state, UTC interval, and the
  private artifact-bundle hash.

Never compare across a PERFVAR schema/scenario hash, host class, Android
model/OS, underlay, transport, provider role, payload, or memory-policy change.
If no compatible reconstructible baseline exists, first run an unchanged A/A
campaign and append it as a new baseline. Historical dirty revisions without
retained patch content are evidence, not reconstructible controls.

Public Internet and public-provider paths can change between attempts. Their
absolute results are product release gates and longitudinal observations. A
source-level causal percentage claim additionally requires a same-session
control/candidate bracket with an exact pinned provider or a deterministic
simulator reproduction. Do not promote code merely because it drew a better
public exit.

## Private artifact contract

Create one mode-0700 directory outside every product repository, for example:

```sh
export URN_REG_WORKSPACE=/Users/builder/urnetwork
export URN_REG_RUN_DIR="$(mktemp -d /tmp/urnetwork-perf-regression.XXXXXX)"
chmod 700 "$URN_REG_RUN_DIR"
mkdir -m 700 "$URN_REG_RUN_DIR"/{private,manifest,simulator,devices,analysis,diagnosis}
```

Give the campaign an opaque ID such as `pv-20260831-001`. Raw logs, NDJSON,
profiles, control APKs, credentials, client IDs, and browser artifacts stay in
the private bundle. The checked-in ledger stores only privacy-safe aggregates,
the bundle SHA-256, and a private retention locator. It must not store Android
serials or stable client/provider identifiers.

Before measurement, write a manifest containing the compatibility key, exact
commands, planned ordering, planned sample count, primary metrics, guards,
practical-effect thresholds, invalidation rules, and baseline ID. Fingerprint
the manifest before the first candidate result. Append attempts and failures;
do not rewrite the plan after observing data.

## Bootstrap and preflight

1. Read this file, `PERFVAR.md`, the current active portions of `LOWBAR.md` and
   `MEMSTEADY.md`, Android's
   [`PHYSICAL_LOWBAR.md`](../../../android/app/scripts/PHYSICAL_LOWBAR.md), and
   the active entry in the tests measurement ledger.
2. Inspect branch, HEAD, upstream, and `git status --short --branch` in tests,
   server, Connect, SDK, proxy, build, and Android. Record state without
   printing diffs that may contain private material.
3. Confirm no competing test, benchmark, simulator, backup, indexing job, or
   stale campaign process is consuming material CPU, disk, network, PostgreSQL,
   Redis, adb, Chrome DevTools, or emulator resources.
4. Confirm the local PostgreSQL/Redis fixture and environment documented in
   `PERFVAR.md`. A DB outage is `BLOCKED`; it is not permission to skip the
   DB-backed gate or call a partial run complete.
   Terra must execute fixture-backed arms in a network-enabled runner. A
   sandbox-denied or unreachable fixture attempt is preserved as
   `INVALID_ENVIRONMENT`/`BLOCKED` with zero measurements, then retried with
   the identical frozen command when the fixture is available; it must never
   be relabeled as a correctness result.
5. Require exactly two authorized arm64 Android test devices. Assign opaque
   roles `device-a` and `device-b` in artifacts. Keep serials only in private
   shell variables. Verify both can use validated Wi-Fi and validated cellular,
   and that both share the same LAN during P2P cells.
6. Confirm the Android SDK tools, acceptance gomobile tools, Chrome DevTools,
   app/test build prerequisites, and dependency-free collector tests.
7. Require `/Users/builder/urnetwork/.tests.yml` to be a non-symlink regular
   file owned by the current user. Tighten it to mode 0600 when necessary. It
   contains `user` and `pass`. Use a secret-aware parser to create a mode-0600
   two-line credential file inside `private`; never interpolate either value
   into a command or output. An ownership/type problem is a blocker.
8. Check device battery and thermal state before each physical block. Start no
   block while a phone is thermally throttled. USB-powered measurements remain
   valid for functionality/performance when labeled, but only wireless-debug,
   unplugged cells may support battery claims.
9. Run parser/unit preflight before trusting the instruments:

```sh
cd "$URN_REG_WORKSPACE/android"
node --test app/scripts/physical_lowbar_capture_test.mjs \
  app/scripts/chrome_video_probe_test.mjs

cd "$URN_REG_WORKSPACE/server"
go test ./connect/perfvar -run '^TestPerfvar' -short -count=1
```

Do not begin a candidate arm until the control source is reconstructible, both
APKs are stamped with distinct immutable build IDs, and the full manifest is
frozen.

## Statistical contract

Predeclare metric direction. Convert paired continuous observations to an
oriented benefit: `log(candidate/control)` for throughput, goodput, and
efficiency; `log(control/candidate)` for time, bytes-on-wire, allocation, CPU,
and memory. Positive is always better. Use raw paired differences for values
that may be zero and an exact paired binary method for pass/fail outcomes.

For PERFVAR and Android paired cohorts, report the paired median effect, a
deterministically seeded 95% bootstrap confidence interval, and an exact paired
randomization or signed-rank p-value. Correct primary-metric p-values with Holm
at family alpha 0.05. Guards use raw alpha 0.05. Record the method, seed, sample
count, excluded preflight attempts, effect, confidence interval, raw p-value,
adjusted p-value, and minimum detectable effect.

Use these minimum independent sample counts:

| Cohort | Minimum comparison unit |
| --- | --- |
| Go microbenchmark | 10 fresh process samples per side, same-session order-balanced, analyzed with a pinned `benchstat` version |
| PERFVAR | 5 fresh complete processes per side; use the documented five internal repetitions and process-level scenario aggregate |
| Android | 7 valid paired session blocks per device/build/underlay cell; use the session summary as the unit |
| Video pass/fail | 7 session opportunities per required cell; do not count multiple requests on one H2/TLS connection as retries |

Choose 7 or 11 Android blocks from the baseline noise/MDE before starting; do
not stop early because the current point estimate looks favorable. If a cohort
is underpowered, the verdict is `INDISTINGUISHABLE`, not “no regression.” The
default minimum practical effect is 5% for performance and 0.5 MiB for
session-level Go-runtime memory unless the active baseline predeclares a more
specific product threshold.

The 24-MiB absolute ceiling is evaluated before statistics. Memory statistics
can choose between two passing candidates; they can never make an over-ceiling
candidate pass.

An effect is statistically better when its oriented confidence interval
excludes zero in the beneficial direction and its Holm-adjusted p-value is
below 0.05. It is baseline-promotable only when the median improvement also
meets the predeclared practical effect. Any statistically worse primary or
guard starts regression diagnosis even when its magnitude is small.

## Deterministic simulator campaign

Run all commands from the server repository with the documented local fixture.
Correctness runs must explicitly clear measurement/failure-probe variables.

```sh
cd "$URN_REG_WORKSPACE/server"
unset CONNECT_PERFVAR_MEASURE CONNECT_PERFVAR_FAILURE_PROBE
unset CONNECT_PERFVAR_RESOURCE_HELPER CONNECT_PERFVAR_RESOURCE_HELPER_NAME
go test -p=1 ./connect/perfvar -parallel=1 -count=1 -timeout=0
go test -race -p=1 ./connect/perfvar -short -parallel=1 -count=1 -timeout=30m
```

For each control/candidate process block, alternate
`control,candidate,candidate,control` and reverse the order on the next block.
Hold seeds and scenario inputs fixed. Capture every `[perfvar]` JSON record,
the command status, stderr, and aggregate. Reject numeric throughput evidence
from incorrect, queue-refused, or calibration-invalid runs, but retain those
runs in completion/failure statistics.

Run the canonical static low-bar matrix exactly as defined by `PERFVAR.md`:

```sh
CONNECT_PERFVAR_MEASURE=1 \
CONNECT_PERFVAR_SEED=20260810 \
CONNECT_PERFVAR_ROUTE=exchange-h1,exchange-h3,p2p-fast,p2p-legacy \
CONNECT_PERFVAR_PROFILE=cell-edge-5m-down-1m-up,cell-edge-1m-down-250k-up,cell-edge-256k-down-64k-up \
CONNECT_PERFVAR_WORKLOAD=tcp,udp,latency-under-load \
CONNECT_PERFVAR_DIRECTION=upload,download \
CONNECT_PERFVAR_TOPOLOGY=one-hop \
CONNECT_PERFVAR_EXTENDERS=0 \
CONNECT_PERFVAR_RESOURCE=mobile-surrogate \
CONNECT_PERFVAR_RUN_COUNT=5 \
go test ./connect/perfvar -run '^TestPerformanceVariations$' \
  -count=1 -timeout=0 -v
```

Run its dynamic capacity/outage/MTU matrix:

```sh
CONNECT_PERFVAR_MEASURE=1 \
CONNECT_PERFVAR_SEED=20260810 \
CONNECT_PERFVAR_ROUTE=exchange-h1,exchange-h3,p2p-fast,p2p-legacy \
CONNECT_PERFVAR_PROFILE=cell-edge-rate-collapse-recover,cell-edge-outage-1s-recover,cell-edge-mtu-reduction-recover \
CONNECT_PERFVAR_WORKLOAD=tcp,tcp-warmed \
CONNECT_PERFVAR_DIRECTION=upload,download \
CONNECT_PERFVAR_TOPOLOGY=one-hop \
CONNECT_PERFVAR_EXTENDERS=0 \
CONNECT_PERFVAR_RESOURCE=mobile-surrogate \
CONNECT_PERFVAR_RUN_COUNT=5 \
go test ./connect/perfvar -run '^TestPerformanceVariations$' \
  -count=1 -timeout=0 -v
```

Also run the clean 32-MiB all-route TCP matrix, both directions, one hop, with
both default and mobile-surrogate resources:

```sh
CONNECT_PERFVAR_MEASURE=1 \
CONNECT_PERFVAR_SEED=20260810 \
CONNECT_PERFVAR_ROUTE=exchange-h1,exchange-h3,p2p-fast,p2p-legacy \
CONNECT_PERFVAR_PROFILE=clean-lan \
CONNECT_PERFVAR_WORKLOAD=tcp \
CONNECT_PERFVAR_DIRECTION=upload,download \
CONNECT_PERFVAR_TOPOLOGY=one-hop \
CONNECT_PERFVAR_EXTENDERS=0 \
CONNECT_PERFVAR_RESOURCE=default,mobile-surrogate \
CONNECT_PERFVAR_RUN_COUNT=5 \
CONNECT_PERFVAR_BYTE_COUNT=33554432 \
go test ./connect/perfvar -run '^TestPerformanceVariations$' \
  -count=1 -timeout=0 -v
```

H1 is the performance primary; H3, DNS-H3,
and Auto remain compatibility/correctness observations until their planned
future iteration. P2P fast and legacy remain required adjacent paths. Run the
focused H1 lane/backpressure, exact no-hot-path-retransmit, route-health,
poison-recovery, dynamic-transition, and same-LAN P2P deterministic tests named
by the current source; never replace the complete package gate with that
focused set.

For changes shared by servers, run every benchmark in `server/connect`,
`server/connect/perfvar`, and `server/proxy` in separate serial processes with
`GOMAXPROCS=10`, `-benchmem`, `-benchtime=500ms`, and `-count=10`. Compare exact
control and candidate trees in balanced order. `B/op` and `allocs/op` are
guards; a throughput win that adds an unexplained hot-path allocation is not a
promotion.

## Physical Android campaign

Use the long-lived `PhysicalLowbarSessionTest` and privacy-safe collectors from
Android's `PHYSICAL_LOWBAR.md`. Build both the exact control and candidate AAR,
app APK, and test APK with distinct `urnetworkAcceptanceBuildId` values. Install
credentials and exact P2P peer IDs only through private mode-0600 app files.
Use `finish` on every session so sampling joins, summaries are written, both
roles disconnect, and logout completes.

Counterbalance source and underlay with this four-round schedule, then repeat
the whole cycle until the predeclared session count is reached:

| Round | `device-a` | `device-b` |
| ---: | --- | --- |
| 1 | control on Wi-Fi | candidate on cellular |
| 2 | candidate on cellular | control on Wi-Fi |
| 3 | candidate on Wi-Fi | control on cellular |
| 4 | control on cellular | candidate on Wi-Fi |

This makes each device alternate Wi-Fi/cellular and gives each source arm both
device/underlay positions. Reverse the first source arm on alternate cycles.
Do not run both phones' public speed tests simultaneously; they may share the
same Wi-Fi, host, provider pool, or radio environment.

Each public-path session block must include:

1. a fresh Chrome process and two DevTools readiness probes at least five
   seconds apart;
2. a Direct control with the no-VPN collector gate;
3. explicit H1 to the United States pool with the VPN/underlay collector gate;
4. five cache-disabled Wikipedia page loads and three canonical fast.com runs
   in each Direct/H1 bracket, preserving failures;
5. exact H1 ingress bytes and all queue/backpressure/drop/recovery counters;
6. the current CNN and Bloomberg video probes, with instrumentation installed
   before navigation and a 45-second playback deadline;
7. an active memory window covering traffic and a five-minute quiet connected
   recovery window after ownership and queues drain; and
8. a final snapshot and orderly disconnect/finish.

Use `chrome_page_benchmark.mjs`, `chrome_fast_benchmark.mjs`, and
`chrome_video_probe.mjs`; never save request paths, headers, cookies, signed
tokens, or packet content. Production Chrome is restarted between candidates;
do not claim an unsupported fresh browser context. A displayed fast.com value
is the external product metric. Correlate it with exact H1 ingress bytes because
Chrome may run bulk workers outside the inspected target.

After each four-round public cycle, place both devices on the same validated
Wi-Fi and run exact-ID P2P in both directions. Measure the control build and
candidate build in alternating order. Each direction must include the same
page, fast.com, video, active-memory, and quiet-memory workloads. Record client
and provider memory separately. A connected label is insufficient: require
bidirectional client/provider byte proof, exact peer selection, zero security
blocks for the tested flow, and complete payload/playback evidence.

Auto and explicit H3 receive one smoke/correctness cell per device/underlay so
adjacent breakage is visible, but H1 remains the default performance decision
path. Do not spend the H1 memory budget or change H1 promotion based on H3/DNS
speed until their planned rework has its own compatible baseline.

### Physical memory gates

Evaluate memory per session and role, not per 15-second sample as independent
data:

- **25,165,824 bytes (24 MiB) is an absolute `goRuntimeBytes` ceiling. Every clean sample during
  startup, active traffic, burst recovery, and the quiet connected window must
  be at or below 24 MiB; one sample above the ceiling fails the cell.**
- Report p50, p95, maximum, time to return below 24 MiB, and counts above 24
  and 28 MiB, but no percentile or later reclaim can excuse an over-ceiling
  sample. The old 28-MiB threshold remains a severity diagnostic, not the
  acceptance barrier;
- client and provider roles must pass independently;
- packet roots must stay within the active policy bound, returned-pool storage
  must reconcile, reliable H1 carrier/Pack drops must remain zero, Pack waits
  must equal successes, and final receive reorder use must be zero; and
- controlled H1 fast.com median must remain at least 40 Mbit/s while Wikipedia
  TTFB/load and failure rate are non-inferior to the active baseline.

When runtime rises, classify the allocation before changing a budget. Record
live/allocated/in-use/idle/released heap, retained spans, stacks, goroutines,
GC count/pause/forced-GC, pool outstanding and returned bytes, packet roots,
flow topology, descriptors, and queue occupancy. High returned-pool bytes
suggest reclaim; high live heap/goroutines with low pool retention suggests
flow/topology; low live heap with high runtime suggests stacks, fragmentation,
or allocator spans. Forced GC or trim is diagnostic, not a steady-memory fix.

If a sample exceeds 24 MiB, preserve the clean run first. Then use a separately
stamped `urnetworkMemoryProfileRateBytes=65536` build, take before/peak/after
snapshots and a private heap profile, and reproduce the same bounded load in a
simulator or host test. Never compare the profiled run's peak or forced-GC
recovery with the clean acceptance distribution.

## Regression deep dive and repair loop

For every `REGRESSION`, `MIXED`, hard failure, corruption, timeout, video stall,
or >24-MiB sample:

1. Freeze the first failing artifacts and add a `diagnosing` entry to the tests
   ledger. Reproduce with the exact identity at least once; a disappearing
   symptom remains an observed failure and is not erased.
2. Establish the last healthy and first bad source boundary. Use detached
   worktrees and a bounded commit bisect when needed; never move the operator's
   branches.
3. Locate the limiting hop: browser/origin, Android TUN/gVisor/NAT, receive
   admission, Transfer sequence/ACK/recovery, H1 TLS/WebSocket, exchange/server,
   provider socket/flow scheduler, public-exit reputation, radio, or host.
4. Use the existing counters to prove where useful bytes stop advancing. Add
   temporary sampled/test-only instrumentation only when the current boundary
   is ambiguous. Do not add an atomic or timer to every ACK/packet without a
   measured cost gate.
5. Inspect similar and adjacent paths: upload/download, client/provider,
   H1/H3/DNS-H3, reliable/unreliable, public/P2P, connect/server/proxy, route
   creation/retirement, cancellation, and memory-pressure/reclaim transitions.
6. Add a deterministic test that fails for the demonstrated mechanism before
   the fix and passes after it. Model the actual boundary—queue saturation,
   loss/reorder/outage, stale route, short-lived flow, poisoned exit, flow
   fanout, or allocation retention—not merely the observed final timeout.
7. Implement the smallest causal fix. Do not accept a larger queue, timeout,
   retry, forced GC, disabled check, reduced workload, or suppressed failure
   unless evidence proves it is the correct bounded contract.
8. Run the new test three consecutive times, the adjacent focused tests, race
   coverage for changed concurrency, package tests, vet, and affected
   cross-builds. Any failure resets the focused count.
9. Repeat the exact failed measurement with the same planned ordering and
   sample count. Then repeat the full simulator, server benchmark, two-device,
   Wi-Fi/cellular, and bidirectional P2P guards affected by shared code.
10. Keep a candidate only when the measured regression is removed and no
    primary/guard/hard gate regresses. Revert rejected experiments without
    disturbing unrelated user work, and record their measurements and reason.

For a video failure, distinguish no new TLS opportunity from a fresh
connection. A reused Chrome `connectionId` cannot be rerouted. A genuine new
connection is placement evidence; an encrypted 403 is returned traffic, not a
network blackhole. For a throughput failure, correlate application bytes,
H1 ingress, Transfer ACK progress, resend reason, carrier/Pack waits, receive
reorder, socket batch size, CPU, and provider-origin progress before changing
sequence depth or buffer size.

## Baseline promotion

Never move a baseline downward. `REGRESSION`, `MIXED`, invalid, blocked,
profiled, incompatible, and underpowered results remain append-only evidence
and cannot become the control.

Promote an `IMPROVEMENT` only when:

- its complete compatibility key and private artifact hash are present;
- at least one primary is statistically and practically better;
- all other primaries and guards are statistically non-inferior;
- exact payload, security, route lifecycle, memory, video, queue/drop, cleanup,
  race, and build gates pass;
- the improvement reproduces in the owning deterministic environment and on
  the relevant physical/real-workload gate;
- the source and baseline are reconstructible; and
- an independent confirmation block not used to tune the candidate agrees.

Append the complete result to `tests/PERFVAR-MEASUREMENTS.md`, then append a new
active-baseline registry row pointing to that campaign. Preserve the previous
row as superseded; do not edit away its values.

## Cleanup and completion

Always issue `finish`, pull the privacy-safe summaries, release every temporary
client with `build/all/acceptance/client-cleanup.mjs`, remove on-device
credentials/commands/peer pins/profiles, close adb forwards and Chrome targets,
restore Wi-Fi/cellular state, stop campaign-owned processes, and audit that
PostgreSQL/Redis/test resources returned to their prior state. Cleanup failure
is a campaign failure.

The final ledger entry must include all attempted cells, invalidations,
statistics, failures, root causes, deterministic tests, retained/rejected
fixes, cleanup result, and baseline decision. A concise agent report should
name the verdict, important effects, memory peaks/p95s, fast.com and page
results, P2P result, remaining blocker, and the two changed Markdown files.
