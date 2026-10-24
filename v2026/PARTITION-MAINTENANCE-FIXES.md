# Partition maintenance implementation review and fixes

Reviewed 2026-10-24 against `origin/main` at
`bf71573c9a9d5ed1bf9441dc1e2c46d94c6040dc`, including the completed legacy
settlement queue retirement and the A6/C2 test follow-ups. Three runtime
defects reproduce on this final merged revision: one P1 and two P2 findings.
Broader retirement validation also found the test assertion error in item 4.

The user authorized fixes after the review. The accompanying changes address
all four items. No new database migration is required. Locations below refer
to the reviewed revision; regression names refer to the accompanying changes.
The separate xops autovacuum playbook and deployment operations are outside
this server-code review.

## 1. P1 — Event retention breaks a long-lived connection's signed session chain

**Locations:** `db_provider_work_session_partition.go:97–109,181–197`;
`model/provider_work_session.go:278–295`.

C4 drops an event week based on age alone, including the admission of a
connection that remains open. On retirement, both network-identity lookups
read the event table. Once that admission's partition has gone, neither
finds it, so retirement appends an event with `network_id = NULL`.

With signing enabled, the signer skips that event. Every subsequent event
requires the previous sequence's receipt, so the missing retirement receipt
permanently prevents this client's chain from advancing. Keeping historical
signed receipts does not help the SQL lookup. Subsequent reservations cannot
establish a complete endpoint chain.

**Reproduced:** seed an admission 14 weeks before the current week, create
the current partition, drop the expired week through the family's guarded
`DropRange`, then retire. The retirement loses its admitted network. A second
reproduction retains valid signed baseline/admit receipts, drops the old
week, then retires and admits again through the production append and signer.
The result is `receipts=2 head=4`.

**Fix implemented:** add a retention precondition that keeps a leaf containing
an admission still needed by a connected row. The registry repeats the check
on the named leaf after detach, under the parent's write fence; refusal rolls
the detach back. A concurrent retirement either commits its event before that
check or remains visible as connected. The check does not use the client's
current directory identity.

An expired partition can remain beyond 13 weeks while a connection needs it.
Once retirement commits, ordinary retention removes it. This prevents future
loss; it does not reconstruct any chain already broken by an earlier drop.

**Regression:** `TestProviderWorkSessionRetentionKeepsOpenAdmission` and
`TestProviderWorkSessionLegacyRetentionKeepsOpenAdmission` seed historical
signed receipts and a live connection. They assert that retention refuses,
change the directory's network identity, retire through the real connection
trigger and signer, drop the expired events, then reconnect. All four receipt
signatures, admitted network identities and previous hashes are verified.
Both weekly and legacy partitions are covered without sleeping.
`TestProviderWorkSessionRetentionAllowsSupersededAdmission` also verifies
that a newer admission for the same connected row releases its old partition.

## 2. P2 — A delayed provide publication restores revoked Redis keys

**Locations:** `model/network_client_model.go:1379–1403,3129–3158`;
mirror TTL at `:2410`.

PostgreSQL locking orders `SetProvide` against deactivation, but their Redis
publications are not ordered. After committing, `SetProvide` unconditionally
writes its caller-supplied keys. Deactivation can delete both database and
Redis keys before that older publication runs. The hold token is checked only
when releasing the hold, after the unconditional writes.

**Reproduced:** pause a real provide in the driver's successful COMMIT
callback. Finish `RemoveNetworkClient`, including mirror deletion; confirm
that PostgreSQL contains no keys. Resume the earlier provide. Both calls
succeed, but `GetProvideModes` returns public mode and `GetProvideSecretKey`
returns the revoked secret with a fresh 72-hour TTL. The delayed provide also
publishes peer modes. This demonstrates stale revocation and advertisement;
it does not demonstrate bypassing the active-client checks on new contracts.

**Fix implemented:** place the provide token under the PostgreSQL advisory
lock and client row lock, including unchanged key sets. A single Redis script
compares that token and replaces the mirror atomically. Deactivation and
cleanup remove the token together with the mirror. A missing, expired or
superseded token cannot authorize a delayed write.

Publication also confirms the committed keys and conditionally removes an
unconfirmed mirror. This covers a newer commit whose best-effort Redis hold
failed and ambiguous publication replies. It reuses the read-through
confirmation behavior. Peer publication reads canonical keys after acquiring
the peer mutation snapshot and repeats the read when its compare-and-swap
loses. Deactivation clears peer modes too.

**Regressions:** `TestSetProvidePublicationAfterDeactivationCannotRestoreKeys`
and `TestSetProvidePublicationAfterKeyRotationKeepsLatestKeys` force the
COMMIT/publication ordering and check database keys, mirrors and peer modes.
`TestProvidePublicationConfirmsKeysAfterMissedInvalidation` covers a retained
old token after a newer database commit. `TestNetworkPeerProvidePublicationReloadsAfterDeactivation`
forces deactivation between a peer's canonical read and its publication.
Existing tests cover reverse ordering, failed writes, refill and cleanup.

## 3. P2 — A partial partition vacuum suppresses the stale-maintenance warning

**Locations:** `db_maintenance_hot_vacuum.go:285–299,716–750`.

The parser marks a whole invocation vacuumed when any non-TOAST relation
reports completion. Outcome selection prioritizes that flag over skips and
errors. PostgreSQL can also silently omit a locked leaf while expanding a
partitioned target, without sending a skip warning. A successful statement
and one completion notice therefore do not prove complete coverage.

This affects `finished_task` after C3. An available leaf refreshes the parent's
vacuum/analyze timestamps while another leaf remains blocked, suppressing
the one-hour stale-vacuum warning on every pass.

**Reproduced on PostgreSQL 18:** create two leaves and hold
`SHARE UPDATE EXCLUSIVE` on one in a separate session. Initialize the parent
as two hours stale and run real maintenance. Only the available leaf finishes,
but the original implementation reports `vacuumed`, refreshes both timestamps
and returns an empty `staleTables` list. The lock establishes the ordering.

**Fix implemented:** inventory the intended heap or leaves before VACUUM and
require a completion notice for every one. The inventory and VACUUM share the
table's timeout. Reports retain missing relation names and errors. ANALYZE
requires leaf and parent completion. A completed heap vacuum still counts
when its later analyze fails.

**Regressions:** `TestDbHotVacuumPartitionSkipKeepsStaleWarning` exercises the
real silent skip. `TestDbHotVacuumPartitionTimeoutKeepsStaleWarning` supplies
first-leaf completion followed by a timeout through the PostgreSQL wire
fixture. Both require a later complete pass to clear the stale warning.
`TestDbHotVacuumCompletedHeapBeforeAnalyzeTimeout` preserves the distinct
completed-heap behavior. The synthetic clock and explicit lock avoid timing
assumptions.

## 4. P2 — The converted repair test compares a contract with its whole cohort

**Location:** `model/subscription_contract_expiry_repair_test.go:144–145`;
`model/subscription_legacy_reservation_fixture_test.go:138–162`.

`TestContractExpiryRepairPreviewApplyAccounting32` now settles all 32 contracts
inline. Two contribute three bytes each to the same provider. The assertion
helper compares a selected contract's sweep with that provider's entire
account plus all pending contributions. Its first zero-byte contract therefore
fails with `swept=0 provided=0 pending=6 ... want=0`.

This is a test integration error, not evidence of an incorrect production
settlement. The remaining targeted retirement tests passed.

**Fix implemented and regression:** retain the existing deterministic
32-contract test, assert each contract's exact zero- or three-byte payout,
and compare the provider's account plus durable pending contributions with
the six-byte cohort total. Repeat the cohort check after replay. The shared
single-contract helper delegates to the same cohort-aware assertion.

## Review completion and validation

The previously outstanding merges were reviewed and exercised:

- A6 now opens fresh pool sessions after the role-default migration and
  verifies the exact timeout before and after refusal. Its previously failing
  DB test passes.
- C2's FP2 catalog test now scans the additional v723 successor column and
  compares it across the history cutover. The previously failing test passes.
- Legacy retirement retains the empty table for rolling readers, refuses a
  nonempty queue under bounded table locks, removes its guard and foreign key,
  and removes current readers, writers and task targets. Remaining legacy
  reservations settle through the inline financial owner. The monitor follows
  the retirement migration by identity. Tests cover migration refusal, catalog
  state, removed targets, ownership, expiry/repair and scheduled-close paths.

The review and tests use disposable PostgreSQL 18 and Redis fixtures. The
initial review's registry, cutover, egress-history, finished-task, vacuum,
identity-retention and bloat tests passed, apart from the since-fixed A6 test.
The final snapshot reproduced all three runtime findings and passed the A6,
C2 and retirement integration selection across the root, model, monitor,
taskworker and taskworker/work packages. The broader retirement selection
exposed only item 4 (model); its taskworker/work and monitor checks passed.

Post-fix validation:

| Checks | Result |
|---|---|
| All `TestDbHotVacuum*` tests, including the real locked-leaf regression | Passed, 15.4 s |
| Session partition lifecycle and registry tests | Passed, 231.3 s |
| Partition/session monitor selection | Passed, 35.6 s |
| Session/provide regression selection and corrected 32-contract repair | Completed, 758.0 s; only the new helper mismatch described below failed |
| Final weekly, legacy and superseded-admission retention regressions | Passed, 33.7 s |
| Corrected missed-invalidation regression | Passed, 16.8 s |
| Post-merge 32-contract repair and legacy settlement post-batch tests | Passed, 21.9 s |

The broad model run's sole failure was in the new missed-invalidation test:
its reused deletion helper incorrectly expected a rotated client's canonical
keys to be absent. The mirror had been removed and read-through correctly
returned the new key. The assertion now checks raw Redis absence first and
then requires the new canonical key. That test passed on rerun; no production
change was needed. All selected existing checks and the other new regressions
passed. Formatting and whitespace checks also passed.

These are targeted checks, not a full repository test run.

Evidence and fixture runners are under
`/private/tmp/partition-maintenance-review.YdARQf/`:

- `final-repros.log`: all four original reproductions fail on `bf71573c9`.
- `final-integration.log`: completed A6, C2 and retirement checks pass.
- `final-retirement.log`: wider retirement tests expose the assertion error.
- `fix-regressions.log`, `fix-vacuum.log`, `fix-model.log`, and
  `fix-partitions.log`: regression and compatibility runs during the fixes.
- `fix-retention-final.log`: the final three admission-retention cases.
- `fix-publication-final.log`: the corrected assertion's passing rerun.
- `merge-retirement-tests.log`: the affected settlement tests after integration.
- `review-repros.patch`: the original four reproduction tests, retained with
  the detached baseline worktree for comparison.

Local main was fast-forwarded to the reviewed revision before applying fixes.
At review completion, fetching over SSH failed with `No user exists for uid
501`; the exact cached revision above is the review boundary. No deployment
was performed as part of this review.

At handoff, local main was also fast-forwarded to
`263c96ec74759da565f2a42f1d2dcaef91233b89`. Its five additional commits change
only simulation documentation and recorded artifacts under `connect/sim-latency`;
the reviewed maintenance implementation and tested source are unchanged.

Subsequent integration merged cached upstream main at
`822902437d8614b18ed89a850b5c1646a47c55a3`. Its settlement test follow-up
independently makes the same cohort correction as item 4. The final merge
retains upstream's per-contract helper and its absent-clock assertion, and
the affected 32-contract repair and post-batch tests pass together.
