# Contract partition review

Reviewed with Astra at maximum reasoning on 2026-10-23 against locally available
`origin/main`, merge commit
`a8050a28634ebee4c4a556f63b74ed6c88dec4db`, including its version of
`CONTRACT-PARTITION.md`. The current checkout's local `main` is older; review
validation used an isolated archive of the merge. No production cutover was
performed and no implementation changes were made.

## Recommendation

Hold production cutover until the rollback race and retirement payment guards
below are resolved and covered by deterministic regression tests. The existing
selected tests pass, but neither case is exercised by those tests.

## Findings

### P1 — Rollback can silently discard a concurrent weekly insert

**Location:** `db_contract_partition_cutover.go:987–1004, 1022–1037`.

Rollback checks that weekly partitions are empty before its transaction takes
exclusive locks. An insert that is uncommitted during the emptiness query is
invisible to that query. Rollback then waits for the insert's transaction,
acquires its locks after the writer commits, and drops the now nonempty leaf
without checking again. The rollback returns success and the committed contract
is lost.

A deterministic PostgreSQL 18.4 reproduction calls the actual rollback function:
insert into a weekly leaf in an open transaction, start rollback, wait until
`pg_locks` shows rollback waiting for the parent's exclusive lock, then commit
the writer. Observed result: `error=<nil> contract_exists=false partitioned=false`.

Acquire all required exclusive locks first, refresh the leaf inventory and
check every weekly leaf's emptiness in the same rollback transaction, then
proceed only if it is still safe. Add a regression test with the lock-controlled
ordering above and require refusal with the committed row preserved.

### P1 — Partition retirement bypasses existing payment-retention protections

**Location:** `db_contract_partition_maintenance.go:270–281`, compared with
`model/subscription_model.go:6161–6173, 6313`.

The partition blocker recognizes only payments that are both incomplete and
not canceled. The existing row reaper also protects canceled payments that
carry an external-attempt identity or record (`circle_idempotency_key`,
`payment_record`, or `tx_hash`), and payments whose
`contract_retention_pending` flag is set. Canceled external attempts are
supported by the reconciliation code; the cancellation flag alone does not
establish that their contract and sweep data can be discarded.

A PostgreSQL 18.4 reproduction calls actual partition retirement with two old,
settled contracts: one linked to a canceled, incomplete payment with an external
idempotency key; the other linked to a completed payment with `complete_time`
set and retention stamping still pending. Both contracts and sweeps are deleted,
two archives are written, and no payment blocker is reported.

Preserve the row reaper's payment guards in partition retirement, or obtain an
explicit retention-policy decision before weakening them. In particular,
completed retention-pending payments have already credited account totals; the
pending flag queues bounded retention stamping, so this case demonstrates a
retention-policy mismatch rather than, by itself, an unfinished external
payment. Add deterministic tests for each external-attempt marker and the
pending-retention state, plus eligibility after the protection clears.

## Validation completed

Validation used an isolated PostgreSQL 18.4 and Redis fixture with Go 1.27.3,
not production. The merged source was archived at
`/private/tmp/contract-partition-review.dti5D2`; a temporary modfile resolved
sibling dependencies to the user's existing local checkouts.

- Root package: 15 selected top-level tests passed, covering partition
  maintenance/cutover, index migration, evidence retention, schema audit and
  database-maintenance integration.
- Monitor: 27 selected top-level tests passed, including native partition
  inventory, runway, retention blockers, migration catalog and query plans.
- Model: 10 selected tests passed on the default layout; the same selection
  under `URNETWORK_TEST_CONTRACT_PARTITIONS=1` had 9 passes and one expected
  skip because that test constructs its own partition layout.
- Two temporary regression reproductions failed their preservation assertions
  as described above. They were created only in the isolated review snapshot.
- A separate PostgreSQL control confirmed that `ANALYZE ONLY` on a partitioned
  parent creates and refreshes inherited statistics; it is not a reported bug.

This is 61 passing test executions plus one expected skip, not a full-suite or
race-detector run. The production-scale rehearsal described by the plan was not
repeated. Exact selected tests and logs are recorded in
`/private/tmp/contract-partition-review.dti5D2-fixtures/validation.json`.

Reproduction sources and logs:

- `zz_review_rollback_race_test.go` in the isolated snapshot and
  `rollback-repro.log` in the fixture directory.
- `zz_review_payment_hold_test.go` in the isolated snapshot and
  `payment-hold-repro.log` in the fixture directory.

## Plan and rollout assessment

The principal design is coherent: the six tables share Monday UTC UUID bounds;
the existing heaps attach as legacy leaves without copying live history;
future partitions are pre-created; and retained usage is archived atomically
with contract deletion. The implementation addresses PostgreSQL-specific index
attachment, preserves check validity and leaf truncate guards, and integrates
partition awareness into schema audits, maintenance and monitoring. The selected
integration tests support these mechanisms on PostgreSQL 18.4.

The plan already documents meaningful limitations: dropping the close-report
cascade changes cleanup responsibilities, the attached legacy heap will not
shrink at cutover, and its eventual archive/delete work may take weeks. The
archive itself continues growing and needs a separate capacity plan. Keep the
explicit storage-expansion and owner-go-ahead gates for production.

Correct the operator deadline in `CONTRACT-PARTITION.md:555–557`: maintenance
abandons an unfinished prepare six hours before the cutover week
(`db_contract_partition.go:54`, `db_contract_partition_maintenance.go:88–98`).
The ten-minute finalize margin is an additional last-resort refusal, not a
safe scheduling deadline. An operator following the current runbook could
attempt finalize after the prepared state has already been removed. Schedule
finalize before the six-hour abandonment boundary with time for verification.
Correct the same ten-minute instruction in the prepare success log
(`db_contract_partition_cutover.go:669`). The selected
`TestContractPartitionAbandonsUnfinalizedCutover` passes and confirms the
six-hour behavior.

Also standardize the horizon wording: 13 weeks is 91 days, as the implementation
and evidence guard correctly use, rather than the plan's initial parenthetical
“90 days.” Weekly upper-bound expiry means individual records can remain longer
than 91 days, and blockers can extend that further.
