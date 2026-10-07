# Contract admission without shared financial row queues

Contract creation must stay independent when many clients in one network use
one funded balance. The approved policy trades absolute reservation consistency
for availability. Healthy admission atomically reserves bytes in Redis; crashes,
Redis eviction and concurrent durable debits can create temporary accounting
edges. PostgreSQL continues to own each contract, close reports, usage and
settlement debit. This policy does not authorize inventing provider usage,
settling both parties at zero, or crediting an unmeasured probe.

The old path serialized each payer inside a process, locked its balance rows,
and updated common reservation revision and snapshot rows. Holding any one of
those three database rows caused all 64 independent-process requests to time
out in the deterministic root-cause test. A Redis mutex around the same database
work would only move that queue.

The new path uses an unlocked grant read and one atomic Redis reservation per
selected balance. Each request retains its contract token across transaction
retries. New escrow rows carry `redis_reserved=true`; compatibility triggers,
legacy snapshots and exact legacy censuses exclude them. Creators write only
per-contract escrow/contract rows and retain endpoint and private-shard lifecycle
checks. They neither take the payer permit nor update shared financial rows.
Settlement still claims the contract and debits its actual usage exactly once;
a post releases only that contract's Redis token. A lost or replayed release
cannot subtract a different contract's reservation.

Each balance's counter, tokens and expiry index use the same Redis hash slot.
Redis executes signed 64-bit reservation arithmetic without floating-point
money. Each operation cleans at most 32 expired tokens. A reservation lease is
24 hours from its first acceptance; retries do not renew it. Idle keys expire
25 hours after their last operation. Corrupt values and Redis transport errors
are errors, while missing/partially evicted reservation state starts a new
approximate generation at zero. A late release from a lost generation finds no
token and does nothing.

Accepted failure windows:

- A PostgreSQL rollback, pre-write failure or lost commit result can leave a
  Redis reservation until its lease expires. This temporarily reduces usable
  credit. Known insufficient-credit refusals release their partial allocations.
- Redis loss/eviction, a still-open contract older than 24 hours, or an unlocked
  grant read overtaken by a settlement debit can over-admit credit. Actual
  contract usage and debit remain durable; Redis is admission coordination,
  not a second payment ledger.
- A lost settlement post retains reservation debt until a repeated close,
  targeted reconciliation or expiry. `ReconcileRedisContractReservation` repairs
  one known contract with primary-key reads. Repair after an open/closed race can
  temporarily resurrect debt, but never extends its original 24-hour horizon.
  It is an explicit exception tool, not a history scan on the creation path.
- Compatible legacy readers continue subtracting marked rows' Redis debt. Keep
  upgraded settlement and maintenance readers until marked rows are drained.
  This release has no admission-off mode; rolling back to an incompatible
  binary after activation is unsupported.

Migration 755 installed compatibility and the initial `enabled=false` singleton.
The initial rollout required upgrading and retiring every incompatible financial
reader before explicitly enabling Redis admission. This bridge release follows
that activation: contract creation now always uses Redis admission and never
reads `redis_contract_admission_policy`. Changing its old `enabled` value no
longer controls this release. The schema remains compatible with migration 755.

Keep the initial switch true while any prior release still reads it. Deploy this
bridge to every creator and companion creator, including API, Connect,
Taskworker and actual maintenance jobs, and establish predecessor retirement.
Only then apply the separate migration that drops the `enabled` column. Dropping
it before an older reader retires would make that reader's contract creation
fail. The table's singleton and all reservation/accounting columns remain.

The compatibility inventory includes every API process serving contract create,
close or balance reads; every Connect process able to dispatch those operations;
every Taskworker process (including its local probe controller, expired-contract
closer, shard cleanup and reservation reconciliation); and manual/periodic
`bringyourctl` or other programs linked to these model functions. External SDK
clients do not read the reservation tables and need no update for this switch.
Read-only database catalog monitoring does not participate in reservation
accounting. Use the enabled deployment inventory, including draining replicas,
rather than a fixed count of latest metric series.

Before the initial enable, establish compatible current binaries and retirement
of incompatible predecessors. Before dropping `enabled`, establish the same
boundary for older binaries that still read the column. Current metrics,
selected tags and a completed deployment command alone do not establish exit.
Use actual host/container process and restart state, with compatible next-start
selection and the actual known manual-job inventory. Edge5 is excluded under the
operator's standing offline and stopped-worker instruction. A null observation
remains unknown. Do not add a hypothetical future manual-resumption condition.

Old settlers are incompatible with marked rows: their snapshot code treats the
marked reservation as legacy even though migration 755 no longer advances its
legacy revision. It can subtract an amount absent from a current legacy cache
and fail with snapshot underflow; it also lacks the owned Redis-token release.
Old cold-census/reconciliation readers can include marked rows in the legacy
mirror, double-counting them alongside the approximate counter. Old creators
can instead reuse a legacy-only cache without subtracting approximate debt.
These are source-defined compatibility risks, not a claim that an old process
has already performed one of them on Main. Accepted Redis crash/eviction edges
do not make a mixed incompatible-binary rollout supported.

The former `contract-admission-mode.sql` operator program is removed from this
release. It cannot stop new admission after the switch has been retired. Keep
compatible settlement and maintenance readers: actual usage and debits stay
durable, and existing marked reservations still need their owned releases.
Rolling back to an incompatible binary, clearing Redis keys, or restoring old
trigger bodies is unsupported. Use targeted reconciliation for known exceptions.

The mandatory release gate is part of `test.sh`, before filtered or expensive
package tests. It always runs the actual PostgreSQL/Redis held-row and held-payer
permit regressions, mixed legacy/new settlement/replay/expiry controls, and a
512-client, two-process same-network workload with varied and shared
destinations. Missing fixtures fail the gate. Run it directly after sourcing
`test-env.sh` with:

```sh
WARP_TEST_ENV_FAIL_FAST=1 go test -p=1 -race -count=1 -timeout=5m ./model ./taskworker/work \
  -run '^(TestRedisAdmission.*|TestContractCreationSameNetworkLargeNContentionFree|TestPrivateProviderCreationDoesNotQueueOnSharedFinancialRows)$'
```

The standalone larger workload uses the same owning test, with
`URNETWORK_CONTRACT_CONTENTION_CLIENTS=1024` (accepted range 256–4096, even).
Keep enough funded credit for at least ten times the complete workload. Record
actual interprocess overlap, completed requests, latency, bounded database wait
families and final Redis/ledger totals. A healthy independent-payer test alone
cannot prevent this regression. Source string checks and a locally serialized
workload are not substitutes for the held-row and held-permit controls.

After activation, compare source-qualified contract timing, returned successes,
cancellations, DNS no-provider-write evidence, continuous database samples and
independent CPU observations. `urnetwork_redis_contract_reservation_total` has
finite operation/result labels; it includes token replay and does not count
committed contracts. Legacy cache counters cover legacy rows only. Missing or
capped query families remain unknown, and faster local tests do not establish
Main recovery or the FP2 coverage target.

## Asynchronous settlement writeback (migration 758)

Current Redis-reserved contracts commit their terminal outcome and exact payer
consumption to `transfer_debit_journal` without locking/updating their shared
grant, revision or reservation snapshot. A journal row belongs to one
contract/balance and does not depend on provider payout eligibility. Unmarked
legacy contracts keep the existing synchronous debit path.

Redis keeps the consumed portion reserved until the journal worker commits a
batch. The worker takes one grant with `FOR NO KEY UPDATE SKIP LOCKED`, applies
at most 512 records in one debit, marks those records applied in the same
transaction, and releases all PG locks before bounded Redis I/O. It deletes the
applied records only after Redis acknowledges release. Lost callbacks, rollback,
partial pipelines, lost acknowledgment and replay cannot erase or repeat the
durable debit. Lost metadata callbacks are reconstructed from the journal.

This exchanges immediate PG balance consistency for contention-free current
settlement and eventual writeback. During a healthy delay, PG credit is high by
pending consumption and Redis retains that consumption. Missing/expired Redis
state or overlapping old settlement callbacks may temporarily under-reserve;
there is no claim of absolute cross-store consistency. The durable journal
remains the recovery source. Do not delete it, synthesize provider payout rows,
or remove its balance-retention guard to make balances appear settled.

Sixteen independent recurring tasks keep durable balance cursors. Each call
visits at most 64 balances within 15s, with a 2s SQL/250ms lock bound per batch and
1s Redis deadline. Full successful pages continue immediately; a busy or failed
key advances the cursor on the regular 2s cadence. A failure returns its cursor
before another slow key can consume the owner deadline. A permanently locked
first grant cannot starve later grants. Sixteen scheduled partitions do not
establish the number of actual executing workers. Use SIGNALS §2.5a to observe
both pending and applied-but-unreleased age, missing schedules and actual
source-qualified owner/completion evidence.

Migrate the canonical 756/757/758 sequence before selecting this code. Deploy
Taskworker recovery owners promptly with API/Connect/Proxy settlement writers.
756/757 preserve upstream payout provenance and immutable earning authority;
old manual bonus amount edits are intentionally refused by 756 and must use the
current provenance-preserving operation. The journal is backward-compatible
with old settlement replay (old outcomes have no journal); new pending debt
prevents both new cleanup and old balance-delete paths from removing its grant.
Retire old writers to close their early-Redis-release overlap. Never roll back
by dropping the journal: drain it with a compatible worker first. Old legacy
settlement still has shared-row contention and needs separate measured closure.

`./test.sh` includes the mandatory real-PG/Redis contention gates. Focused gate:

```sh
go test -p=1 -race -count=1 -timeout=5m ./model ./taskworker/work \
  -run '^(TestAsyncDebit.*|TestRedisSettlementCompletesWithSharedFinancialRowsHeld|TestContractSettlementSameNetworkLargeNContentionFree|TestTransferDebitTaskPartitionsAndBoundedContinuation)$'
```

The large settlement test uses 512 clients and two independent processes with a
held shared grant and actual public/controller closes. Set
`URNETWORK_CONTRACT_CONTENTION_CLIENTS=1024` (or an even 256–4096) for the standalone
loaded run. Held-row completion and simultaneous process overlap prevent a
hidden local serializer from turning a queue regression into a false pass.
The old 35274673 ordering times out all 64 deterministic public held-row closes;
the candidate completes them before the holder releases. Local results are not
a Main capacity or CPU-share measurement.
