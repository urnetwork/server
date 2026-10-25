# Immutable positive-cache snapshot: local screen passed, pilot pending

A draft patch passed its isolated correctness/supporting benchmark screen; it is **not a
qualified faster reference or a measured end-to-end improvement**. This
is a separate hypothesis against the corrected frozen source, not a revision
to the failed warm-reader pilot or authorization to change the baseline.
The existing owner-admission-map draft is also unqualified.

The independent screen uses fresh source from exact image `002c3904…` and
Go 1.27.3. The baseline passed 52 race-enabled tests three times; the candidate
passed 60 tests three times, including all eight new tests below. Removing
only the final expiry check and, separately, only the final closed-owner check
made their respective barrier tests fail at the intended assertions. The
candidate was restored byte-for-byte and passed three more 60-test suites.
The warm-hit benchmark improved, but publication became more expensive; the
[complete screen evidence](SNAPSHOT-QUALIFICATION.md) retains every observation,
allocation cost, negative control and cleanup receipt. No scoring pilot has run
for this hypothesis and the frozen baseline source is unchanged.

[`warm-snapshot.patch`](warm-snapshot.patch), SHA-256
`46bc95e3f2719f26f939be7a86c274d0b98bb2b1027f1ef4537d61de4c4ec924`,
changes only `connect/resident_contract_manager.go` against frozen blob
`ba3c09f7960f789b37a262b1542ee4dbdca93996`. Eight candidate-only deterministic
test drafts are preserved as
[`snapshot-qualification.go.txt`](snapshot-qualification.go.txt), SHA-256
`26c459d5bf580a94de34b45636847a6b6880d190681d66bd5df52d2ca36bd40f`.
The `.txt` fixture is not compiled by the simulator suite or submitted patch.
Preparation workspace: `temp/reference-snapshot-draft-20261025.BFH83oNJ/`.

The supporting
[`snapshot-benchmark.go.txt`](snapshot-benchmark.go.txt), SHA-256
`f7a84b34cdcacf09e57004259410d43ef7b48f4d0663503b51add584aab28060`,
uses the actual constructor/default freshness policy and identical code on
baseline/candidate. The completed idle-slot screen used five observations
per source at `-benchtime=100ms`, management CPUs 20,22 and GOMAXPROCS=2.
Its 32 parallel callers exercise one warm pair; source reads other than the
single initial read or any refused hit invalidate that observation. Keep every
output, ns/op and allocation result. Do not reroll an unfavorable result.
Median candidate time must improve before spending a full directional pilot.
These deliberately narrow two-CPU mechanism observations are not the real
ten-core workload, confidence proof, calibrated margin or end-to-end acceptance.
The same fixture separately benchmarks publication with 128 live pairs using
the actual registered read-worker seam. Retain its allocation and owner-lock
cost beside the warm-hit result; that direct seam deliberately excludes Redis
and admission latency. A faster warm lookup alone does not establish that the
tradeoff pays at the real refresh rate. Full-window memory/resource and score
qualification remain mandatory, even if the supporting hit benchmark improves.

## Mechanism to investigate

Warm first-half positive cache hits currently acquire the manager's mutex.
The prior reader-lock pilot did not improve its measured workload; another
reader lock is not assumed sufficient. An immutable, owner-published snapshot
could let that specific path read without a shared lock, while cold admission,
refresh, map sweeping and read-worker registration keep their existing owner
lock. This is a mechanism hypothesis, not evidence that contention dominates
the measured score or that the improvement exceeds noise.

The uncompiled draft uses one `atomic.Pointer` to an immutable snapshot
containing copied `activeContractEntry` values. Publication happens under the
existing owner lock after authoritative updates/removals. No mutable map or
entry pointer may be modified after publication. Cold and second-half requests
fall back to the existing admission path; they do not acquire new queues or
wait for a refresh owned by another flow.

The draft changes the existing close flag to one atomic authority, clears the
snapshot on close, refuses late publication, avoids copying for an absent
negative result and moves the touched admission/sweep interval to private
per-owner settings with the same one-second default. Actual equivalence is not
established by this description; the correctness screen remains mandatory.

## Correctness constraints from the static review

- **No extended authority:** both local freshness and returned Redis source
  lease deadlines remain ceilings. A fast-path hit is limited to the first
  half of their effective intersection, with current time/deadline checks and
  no stale timestamp carried across a potentially blocking boundary.
- **Close visibility:** snapshot readers cannot read the lock-owned plain
  `readsClosed` field concurrently. A prospective atomic closed flag must be
  the same authoritative state used by slow admission and Close, not a second
  eventually synchronized shutdown decision. Closing invalidates the snapshot;
  late completing reads cannot republish a usable snapshot after close.
- **Registration and joins:** fast cache hits register no read worker. All new
  cold/refresh reads still register under the owner lock before shutdown can
  start its join. Retain the existing cancellation and panic handling.
- **Invalidation:** negative refresh, source expiry, freshness expiry and sweep
  removal must not leave a usable old snapshot. A read that linearizes before
  an update still needs its own immutable deadline; no latest-result promise
  should be invented that the existing cache never supplied.
- **Bounded retention:** snapshots must not create unbounded histories. Do not
  retain whole old maps across slow Redis operations or blocked callbacks.
  Include map-copy allocation, retired snapshots and peak overlap in actual
  memory accounting, rather than assuming lock removal is free.
- **Owner settings:** touched legacy admission/sweep timing tunables must move
  to per-owner settings with an explicit defaults constructor, preserving
  current defaults and bounds. Do not repurpose the positive-cache freshness
  setting as an unrelated admission interval or add process-global budgets.
- **Same product behavior:** no measurement-aware branches, bypassed
  authorization, relaxed limits, changed workload or test detection. The
  candidate remains subject to protected-source and honesty checks.

## Evidence needed before advancement

Use fresh private checkouts of exact frozen source. First run the existing
resident-contract and composed packet-path correctness screen on baseline and
candidate. Add deterministic candidate tests for the actual lock-free entry
path: valid warm progress while the slow lock is held; expired/negative results;
half-window refresh ownership; independent pairs; close and in-flight read
join ordering; bounded snapshot retention; and PostgreSQL acquisition tripwires.
Use explicit barriers or state changes, not sleeps as the primary proof.
The eight drafted tests cover a held-lock/slow-path tripwire warm hit, exact
virtual-clock source expiry and immutable old contents, negative refresh,
barrier-controlled late refresh after close, bounded recent provider cohorts
and independent owner shutdown. Two additional explicit-barrier cases pause
an already selected warm hit across source expiry and across Close before
context cancellation. A private nil-in-production hook enables these orderings
without wall-clock sleeps. Before advancement, removing the final deadline
check or closed-flag check must deterministically fail its respective test;
retain those behavioral controls and the restored pass. These controls and
the three restored suites have now passed as recorded in the qualification
record; the existing composed PostgreSQL-tripwire tests passed alongside them.

Only after correctness succeeds, freeze a separate full R9 fresh-control pilot
with exact patch/build identities before observing performance. Keep every
outcome. Do not reuse formal calibration controls, retry an unfavorable result
or call this a qualified better reference before an actual directional pass
and the independently committed held-out screen. No builds, tests or probes
may overlap the running calibration cohort.
