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
- Switching admission off does not make existing marked rows strict: compatible
  legacy readers continue subtracting their Redis debt. Keep the upgraded
  settlement and maintenance readers until all marked rows are drained. Rolling
  back to an older binary after activation is unsupported.

Migration 755 only installs compatibility and an `enabled=false` singleton in
`redis_contract_admission_policy`. Apply the reviewed append-only migration,
then deploy and qualify all API, Connect, Taskworker and maintenance readers
before explicitly enabling that singleton. Each public creation reads the
current switch; there is no process-local policy cache. Do not treat applying
the migration or selecting image tags as activation or runtime convergence.

The compatibility inventory includes every API process serving contract create,
close or balance reads; every Connect process able to dispatch those operations;
every Taskworker process (including its local probe controller, expired-contract
closer, shard cleanup and reservation reconciliation); and manual/periodic
`bringyourctl` or other programs linked to these model functions. External SDK
clients do not read the reservation tables and need no update for this switch.
Read-only database catalog monitoring does not participate in reservation
accounting. Use the enabled deployment inventory, including draining replicas,
rather than a fixed count of latest metric series.

Before enabling, establish both the compatible current binary and retirement
of each incompatible predecessor. A current process metric, selected image tag,
successful deployment command, missing old metric, or elapsed nominal drain
timeout establishes neither old-process exit nor completion of its background
work. Use the owning host/worker process-generation and drain/exit evidence, or
an explicit stop-and-join of the predecessor. Include in-flight requests,
settlement posts, leased workers and suspended/restartable old jobs. Their next
start must also select compatible code. Keep manual old maintenance commands
disabled during and after activation. A null or unobserved slot leaves this
prerequisite unresolved.

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

After independently verifying migration 755's exact artifacts and the above
process inventory, use the existing approved primary PostgreSQL transport with
the reviewed `psql` program below. Its session must be bound to the expected
inventory database; do not guess a port, create a new tunnel or put credentials
in argv. It changes only the singleton, checks primary/database/migration and
the expected old mode, and bounds statement/lock time. This SQL cannot prove
application predecessor retirement. Preserve its terminal receipt; after a
connection loss inspect the singleton once under a fresh observation admission
instead of blindly repeating an ambiguous update.

```sh
# PG connection authority/credentials come from the already-reviewed transport.
PGCONNECT_TIMEOUT=3 timeout --signal=TERM 15s \
  psql -X --no-password --set=ON_ERROR_STOP=1 \
  --set=expected_database="$ADMISSION_DATABASE" \
  --set=expected_enabled=false --set=desired_enabled=true \
  --file=docs/operator/contract-admission-mode.sql
```

Read back `SELECT enabled FROM redis_contract_admission_policy WHERE singleton`
through that same authority and retain the UTC boundary. To stop new Redis
admission, run the same program with `expected_enabled=true` and
`desired_enabled=false`. Calls which already read `true` may still finish and
write marked rows after the disabling transaction commits. Disabling preserves
all counters, tokens and contracts; it neither refunds nor reconstructs them.
Continue running compatible code and target reconciliation at known exceptions.
Do not restore old binaries, clear Redis keys, drop the compatibility column or
restore old trigger bodies. After the first marked contract, a binary/schema
rollback needs a separately reviewed drain, reconciliation and data-conversion
plan; neither a false switch nor waiting 24 hours proves it safe.

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
