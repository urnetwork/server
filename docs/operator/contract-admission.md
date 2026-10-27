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

## Reservation before the contract transaction (pending markers)

A creator used to reserve in Redis inside its read committed contract
transaction, holding the request fence (`pg_advisory_xact_lock` on
`redis-contract-v2/<contract>`) so that recovery could not release a token the
transaction was about to publish. Under transaction pooling, every Redis round
trip there (a preflight PING and an EVAL per attempted grant, about 4.5 per
positive contract on Main on 2026-10-22) kept a pooler server idle in
transaction, and a Redis stall stalled every creator's server at once.

A creator now reserves before BEGIN. Grant discovery runs as autocommit
statements on pooled connections, and each reservation marks its token
pending in `{escrow_<balance>}approx_pending_v3`, which recovery never reads.
The transaction takes the request fence first, checks the attempt is still
publishable, and then runs SQL only: custody, shard, endpoint and grant checks
and the escrow/contract inserts. After the transaction joins, the request hands
its pending tokens to recovery's own marker with its post-admission recovery
page (acknowledging the published ones without SQL), or, when it published
nothing, releases them in Redis alone with `release-pending`, with no
PostgreSQL transaction. The companion origin is likewise selected before the
transaction, which only rechecks its deadline after the client fence.

Two bounds keep recovery away from a live request's tokens:

- A transaction publishes only if it takes the request fence within
  `redisPendingPublicationTimeout` (60s) of the attempt's first reservation
  request, measured on the creator's monotonic clock from immediately before
  that request. Pooler waits for the grant reads before it do not count; waits
  for later grant pages and for BEGIN do. A later fence refuses and
  compensates.
- A recovery page moves another request's pending marker to the recovery
  marker only when it is older than `redisPendingRecoveryAge` (10 minutes) by
  Redis time. Only then can the fenced SQL custody check release it.

A pending token can therefore reach recovery only after its request's window
closed. Marker ages use the TIME of the slot's Redis master; the 9-minute
margin absorbs a forward step of that clock, or a failover to a replica whose
clock leads. A request that died with pending tokens leaves them reserved for
up to ten minutes, then any request on the balance recovers them. Attempts of
one request share its tokens: the request's own release or hand-off advances
an in-process epoch first, and an attempt that reserved before it refuses to
publish. Compensation releases a pending token directly only while no attempt
is in flight and none has started publishing; otherwise the fenced SQL custody
check decides.

A refused request's caller waits for one compensation pass, bounded like an
admission round trip (one second). That pass ignores the request's shared
budget, so the compensation after an insufficient-balance rerun that spent the
budget still makes it. Until 2026-10-26 the caller retried once a second for up
to a 300-second cleanup budget, and a Redis pool outage turned that into a
self-sustaining storm of retry loops; that day's interim fix cut the budget to
five seconds. Balances the pass leaves pending go to a process-wide deferred
owner: four workers, on the same no-retry deadline pool, holding up to 1024
balances. It retries each request's balances with backoff (one second doubling
to a 30-second cap, jittered) for the rest of that request's five-second
budget, through the same release, which reads the request's attempts when it
runs. A compensation waits out each backoff on a timer, not on a worker: a
worker is held only while release calls run, so the owner's throughput is
bounded by its four concurrent calls rather than by backoff, and the owner does
not wait for a retry its budget cannot reach. That request's own recovery page
does not run, so its pending tokens stay with the owner rather than moving to
the recovery marker. When the budget leaves no further retry, the owner gives
up and moves each token still pending to the recovery marker in one Redis call,
as the request's own page would have, unless an attempt of the request is in
flight or has started publication (read under the request's pending lock when
the handoff runs). The next page on the balance then releases the token
through the fenced SQL custody check at once. A full owner drops the
compensation without blocking. The proxy's before-exit hook quiesces the owner,
as do the api once its HTTP drain returns and the taskworker once its tasks have
handed back their claims. A dropped token, one the quiesce ended or one the
owner could not hand off stays reserved under its pending marker: any page on
the balance takes it once it is ten minutes old, and its 24-hour lease ends it
at last.
`urnetwork_redis_contract_reservation_total` counts
`operation="compensation"` with the results `completed` and `pending` when a
compensation ends, `deferred` when the owner takes one, `expired` when its
budget left the owner no pass (no retry fit in it, or it ended while the
compensation waited for a worker) and `dropped` when the owner is full. It
counts each given-up token's `operation="handoff"` as `moved`, `absent` (no
pending marker was left), `skipped` (an attempt was in flight or publication
had started) or `error` (left to the ten-minute age). Two gauges show how full
each process's owner is: `urnetwork_redis_contract_compensation_balances_held`
counts the balances it holds, waiting for a retry or in a pass (it drops new
compensations past 1024), and
`urnetwork_redis_contract_compensation_calls_inflight` the release and handoff
calls its workers are making (at most four). The proxy dashboard's
compensation panel draws the busiest process's value of each. A sustained
`dropped` rate, or held balances near 1024, means Redis refuses releases faster
than the owner retries them.

`urnetwork_redis_contract_publication_refusals_total{reason}` counts the
transactions that refused to publish: `window_expired` and `released`. The
connect controller reports them as contract failure and rejection causes
`reservation_window_expired` and `reservation_released`, and answers clients
with Setup, which they retry. Contract creation timing no longer nests
`grant_selection` inside `transaction` for a Redis-admitted positive request:
grant selection and the reservations precede it.

The previous release's scripts run beside this one during a rolling deploy.
Its recovery page reads only the recovery marker and its owned release
requires one, so it can neither discover nor release a pending token. Its
token-keyed close-path releases and settlements work unchanged and leave at
most a stale pending marker, which the next current page removes. A rollback
to the previous release leaves pending tokens of interrupted requests reserved
until their 24-hour lease. `urnetwork_redis_contract_reservation_total` gains
the operations `reserve-pending`, `reserve-pending-full` and
`release-pending`; `reserve-owned` no longer counts creation.

## Refusal probes for reserved grants

A refused reservation costs a preflight PING and the whole reservation script
(a 5.8 KB EVAL with up to five replicated writes), and its grant joins the
request's recovery page. A payer whose grants are all held by open
reservations pays that for every grant on every request before it is refused
or, while the degradation valve is open, given a zero escrow contract.

After a request's first refused reservation, selection reads the remaining
candidates of each spread window or keyset page with one read-only script per
grant, pipelined and without a preflight PING. The probe proves a refusal only
in the reservation script's plain state: a valid counter generation, no
expired lease and no token of this request. There, credit minus legacy and
reserved bytes is the reservation's own availability, so selection skips a
partial reservation with nothing left and a whole one with less than the
request. Any other state, or a failed probe, leaves the decision to the
reservation. Attempt order and the grants considered do not change. A skipped
grant holding a recovery or pending marker stays in the request's recovery
page, as a refused one does; a grant with neither leaves it, since its page
would find nothing.

`urnetwork_redis_contract_reservation_total` gains `result=skipped` on
`reserve-pending` and `reserve-pending-full` (`refused` plus `skipped` is the
former `refused`), and `operation=reserve-probe` with
`result=empty|available|undecided|error`.

## Exhausted payers

Probes still leave an exhausted payer's census its other costs on every
request: up to three discovery queries, one real refused reservation and a
probe pipeline per spread window or doubling batch, the reservation of
whatever room a grant has left, that room's release in the refusal's
compensation, and a recovery page call for each grant the census attempted.
On 2026-10-26 the platform's own proxy network, with thirty-two grants, paid
this on hundreds of refused requests a second, and the compensations of those
refusals failed on the deadline pool every reservation shares.

After a complete census refuses an ordinary payer as exhausted, its process
lists the payer for five seconds; a funded census removes it. A listed
payer's request first reads its grants with the census's keyset query and
probes every grant with credit in one pipeline per page of 64. When every
probe is exact, no grant has room for the rest of the request, the total room
is below the shrink floor, fewer grants have room than the selected limit and
the grants fit the row limit after the spread windows, the request is refused
with the census's own error and total, and the payer stays listed. Nothing is
reserved, so nothing is compensated, and only grants holding a recovery or
pending marker join the recovery page. Any other result runs the census
unchanged and keeps the payer with the census for five seconds, through
refusals and fundings alike, so a state the proof cannot decide costs at most
one proof per process every five seconds. A refused request whose recovery page released bytes reruns the
census directly.

The proof is the census at one instant: in the reservation script's plain
state a whole reservation reserves only a grant with room for the whole
request and a partial one reserves the room left, so a census reading each
grant when the proof read it refuses with the same total, then releases that
room. The list only chooses the proof, per process and in memory: a stale or
missing entry costs at most one extra read and probe pipeline, or the census.
The internal prober is never listed, and its census keeps its selection
outcomes.

`urnetwork_redis_contract_reservation_total` gains
`operation=exhaustion-proof` with `result=proven|unproven`. A proven refusal
counts its probes as `reserve-probe` results and no `reserve-pending`,
`reserve-pending-full`, `release-pending` or `compensation` result.

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
