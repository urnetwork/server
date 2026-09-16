# Staging epoch 4: comment-only submission and G5 recovery

Investigation date: 2026-09-16. Repair target: staging epoch 5. Epoch 4's
source, evaluator image, submitted patches, and results remain immutable.

## What failed

Job `01a0a6af-cb0e-8595-94ae-e3182c368001` submitted only three ordinary Go
comments in `connect/resident_contract_manager.go`. The authenticated patch
contains no executable change or compiler directive.

The job completed all nine baseline and nine candidate replicates in
2 hours 35 minutes 18 seconds. Each replicate completed its full warm-up and
measurement window, exited zero, and had no resource-limit finding. It did
not hit the three-hour evaluation ceiling.

Candidate replicate 7 logged one `Unexpected error:` recovery during startup:
`*pgproto3.writeError`, wrapping a TCP write `i/o timeout` to the disposable
PostgreSQL service. The stack runs through `WithPgResult`,
`GetNetworkPeerProfile`, and `ConnectionAnnounce.run`. The event occurred at
22:19:01.155791 UTC on September 15, approximately 190.5 seconds before the
measurement window began.

The frozen G5 rule covers the entire replicate lifecycle, not just the
measurement window. It correctly recorded `unexpected_recovery`, made
`all_replicates_clean` false, and rejected the submission as `run_unstable`.
G6 passed. This was not a variance/significance rejection. No score or ranking
is published here before the round's reveal.

## Root cause

The evaluator's frozen source omitted an existing database error-unwrapping
fix. The qualified August baseline used server commit
`46515d82fe98ff666c61b2b5bb1d34a89cf4dad8` (`fix(db): retry wrapped connection
failures`). The subsequent source-epoch branch was instead cut from that
commit's parent:

```text
5ca3d524  full-window matchmaking coverage
├── 46515d82  wrapped database connection fix; qualified baseline
└── 859be811  source-epoch branch
    └── 8f4cde37  complete dependency graph
        └── 759b7462  significance-capable evaluator; staging epoch 4
```

Current `main` retains the fix. Frozen `759b7462` does not contain it or its
`db_error_test.go` regressions. Its database classifier fails to unwrap the
pgx protocol error to the underlying network error, so the failure bypasses
the connection-recovery path and reaches the unexpected-error logger. A
control-plane API/worker update cannot repair code inside an immutable
evaluator image.

The historical log does not record `ctx.Err()` at the failure. It cannot
distinguish cancellation-triggered socket expiry from a live socket stall.
The missing classification fix is proven; the original socket timeout's
trigger is not. It also lacks the protocol's write-byte count and
`SafeToRetry` state, so zero-byte versus partial-write failure is unknown.
Do not claim that the comments caused it, or suppress all timeouts or all
startup recoveries to make this job pass.

The adjacent `main` audit deterministically reproduced three additional
wrapper defects: cancellation with retries disabled could escape the done
path; cleanup ran before classification and could release a failed connection
back into the pool; and an explicit unsafe partial-write error could replay a
callback. These are separately tested repair targets, not claims that all
three happened in this historical replicate. In particular, pgx itself can
discard a connection already marked closed; the wrapper must still honor its
own disposal contract when pgx has not done so.

## Repair and release boundary

The operator approved pulling current `main` for staging epoch 5. Before
activating that evaluator:

1. Finish deterministic regression tests using the actual pgx protocol error
   and database callback lifecycle, including cancellation, connection
   disposal, and negative controls for genuine errors and unsafe retries.
2. Have Terra max verify that the tests reproduce the pre-fix behavior and
   pass on the repaired source; Astra max reviews the repair and merges.
3. Require the relevant regressions to exist and pass against the exact
   source being built into the image. A passing test on a different `main`
   checkout is not qualification of the frozen evaluator.
4. Commit and synchronize the reviewed epoch-5 source graph, build a new
   immutable evaluator, and retain its source lock, test evidence, and smoke
   results. Do not activate new pins while epoch 4 is open or draining.
5. After epoch 4 drains and finalizes, deploy and verify the new source/image
   configuration before creating epoch 5. Keep historical round policies and
   results unchanged. Observe an accepted live score and finalized staging
   leaderboard entry, then perform the required production rebaseline before
   public launch.

The runtime repair and fail-closed image-build gate are implemented and passed
independent post-fix validation. The epoch-5 image has not been built or
activated. Do not describe epoch 5 as ready until its immutable release and
live round have both been verified.

### Deterministic pre-fix evidence

Terra max independently captured both failure layers:

| Scope | Result | Log SHA-256 |
|---|---|---|
| Exact `759b7462` connection-classifier body replayed in an isolated current-main harness, with a real `pgproto3.Frontend.Flush` error | Failed as expected: wrapped protocol write timeout not classified as a connection error | `213c7d26b87eec04136f5f6a2086f8bf3461de487c254baf02b0dda0ad352aad` |
| Unmodified `90b64ae` database implementation with test-only regressions and local disposable PostgreSQL | Failed as expected: canceled no-retry error, failed-connection reuse, and unsafe partial-write replay | `bda1b1ed6148e76d4e2b739b7ab6b58028cdf229a588bba1f6b182ff46ee164a` |

The first check reproduces the exact frozen classifier, not a complete
eight-repository image validation. The second separately tests defects in
current `main`; it is not a test run inside epoch 4's evaluator. All fixtures
are synthetic and no live database, evaluator, or retained job was modified.
Full logs are private validation artifacts; only their scope and digests are
recorded here.

### Post-fix validation

Terra max completed validation on 2026-09-16 against the reviewed four-file
code/build-gate diff, SHA-256
`1eece5fc9e0819b877f8ee0381bc7e1bc3f608ea73f26b2f8426c2651d0a6200`,
over base `90b64ae86c0640620d89dcf9ecc8f7525a295ce6`. Sol max reviewed the
runtime changes and release documentation.

| Check | Result |
|---|---|
| Nine targeted database regressions | Passed normally, under `-race`, and three serial repetitions |
| Nineteen adjacent database/transaction/rollback/liveness tests | Passed |
| Offline source/image gate, literal database build gate, and G5 rejection tests | Passed; unexpected recoveries still fail G5 |
| `connect/sim-latency/tests.sh` | Three consecutive passes; each ran the six-epoch prerequisite and 154 top-level race tests |
| `go vet . ./connect/sim-latency` | Passed |
| Live-state check | One matching epoch-4 worker, zero restarts, zero active evaluator containers; no runtime or pin changes |

The three full-suite logs have SHA-256 digests:

1. `42d642e20b35533d84c1d4621f5c1153fe7685863fc0b0d185fe7f8c32bdf923`
2. `1b022494d95e0fc7dfb63cad16aee15faffd0fd68105694391cc68451b0bfc57`
3. `a00dceb7b9ba76b82110a81ee0e5de50d9bad1242914f913bdf6ba42b4495cd7`

The complete private validation manifest has SHA-256
`6d96b30698c6dc93e7ed48611dde306907967c31590e2ec8cb71cd0242371b36`.
It and its mode-0600 logs are retained in the mode-0700 directory
`/home/by/urnetwork/.sim-latency-state/validation/staging-4-db-regression-20260916`.
These results validate the code repair, not a new frozen image or accepted
live submission; those remain the epoch-5 release gates.

## Evidence custody

Raw evidence remains under the authenticated attempt directory and the
versioned MinIO archive; production logs are not copied into test fixtures.
The local attempt is
`/var/lib/urnetwork/competition/01a0a6af-cb0e-8595-94ae-e3182c368001/attempt-01`.
The failing log is `evidence/scorer-input/candidate-07-01a0a6af/stderr.log`.

| Artifact | SHA-256 |
|---|---|
| Canonical patch | `4ba683a9b252bc2675ab2613520285d9094719d09944fe8ad9394922ed5ce490` |
| Score document | `67e66f6e70249f77dc64c2d3b45be8f0c7645c977327dbe4e1aa81ff0d676b14` |
| Evidence manifest | `9bd58c57a92e0f2e96755ef78ff7ede7777395e197479afff6edc46f48961012` |
| Candidate 7 stderr | `1f0d40556d52d7d04ca2f9c41bdb95199657c0520228057f1aa2507403b02f11` |
| Candidate 7 run record | `aa9888ad2da6b45b20a2d980d2ae86a275a93cac114d0700e5a851c590b20bd1` |
| Candidate 7 resources | `9dd036c8bda041236189cf61775b28e5e406b01cd104fc96a5ad5d8c7c9336ca` |

The local artifacts match the immutable evidence manifest. The authoritative
job archive records the same score and evidence-manifest hashes with
post-upload authentication and compliance object locking. No historical
artifact or result was rewritten during this investigation.
