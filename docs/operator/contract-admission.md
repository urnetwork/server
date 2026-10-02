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
