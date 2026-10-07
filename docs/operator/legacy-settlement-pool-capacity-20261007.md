# Legacy settlement pool and transaction capacity

The candidate removes one sequential database request from every legacy
financial attempt by sending the existing `BEGIN` and two `SET LOCAL` commands
together. It preserves a separate transaction, commit, rollback and financial
guard for every contract. It changes the automatic owner and the explicit
applying drain; read-only previews keep their existing transaction modes.
This is a bounded source optimization. Its production capacity impact remains
unmeasured.

The investigation starts from server `1193f72dffc1427a7e63d4f1d23cd3bb71c9eaef`.
No Main contact, deployment, cancellation, limit change, or packet-lock database
work is part of this change. Independent native execution owns qualification.

## Source evidence and capacity bounds

`flushLegacySettlementsPage` discovers one contract at a time through `server.Db`.
`flushLegacySettlementWithGrantWait` may also read admission grant membership,
then acquires a connection through `server.Tx`. Each target previously required
a distinct begin response before sending its transaction-local limits. The
transaction's body and commit are still required. The outer page waits for the
post callbacks before its next visit. Pool checkout reuse cannot remove those
SQL or Redis dependency waits.

The historical supplied target trace had 28.658ms application acquire time,
27.386ms begin time, 1.125ms commit time, 204.288ms transaction return and 90.062ms
financial body, of which 66.803ms was accounting. Joined-post time was 167.708ms
with overlapping inner families. That is a single historical sample; the
intervals must not be added as disjoint costs or converted into a fleet rate.
The narrow saving here is the removed setup request, not the entire acquire or
begin duration. `DbTimingBegin` now includes the absorbed setup work.

Sixteen `RunOnce` shard identities bound concurrent legacy pages across workers.
The 256-visit ceiling and fifteen-second page context are ceilings, not measured
throughput. Busy/gone entries consume visits; head and payer visits are subsets,
not extra completed contracts. Shared grants cross shard boundaries, so sixteen
active pages do not imply sixteen independent financial owners. More pollers
cannot remove a shared grant's lock residence.

`TaskWorker.Run` waits for its current `EvalTasks` batch to finish, but current
`finalizeTask` commits each returned result and its successor independently.
The existing native `TestTaskBatchCompletionPublishesBeforeUnrelatedWorkReturns`
holds one sibling and proves another worker can claim the completed task's
successor. Eight loops with default batch size four are generic execution
capacity per ordinary Taskworker process, not sixteen reserved settlement
workers. Ready-to-claim lag, available loops, actual task placement and current
source identity are needed before attributing capacity to dispatch. No scheduler
policy is changed by this candidate.

Holding a page-wide pooled connection would retain a scarce resource during
Redis and other projection work and could reintroduce nested checkout
starvation. Grouping contracts into one financial transaction would lengthen
grant ownership and alter rollback and committed-prefix behavior. Neither is
required to remove the redundant setup round trip.

## Protocol and plan research

PostgreSQL's simple-query message may contain several statements and emits one
`ReadyForQuery` after the message. An explicit `BEGIN` keeps subsequent commands
inside that transaction. The first error stops the remaining statements.
`SET LOCAL` lasts only through that transaction, including rollback.
These are the protocol and scope properties used here.
[PostgreSQL message flow](https://www.postgresql.org/docs/current/protocol-flow.html),
[PostgreSQL SET](https://www.postgresql.org/docs/current/sql-set.html).

The pinned pgx v5.10.0 source sends argument-free `Exec` through simple protocol.
`BeginTx` executes `TxOptions.BeginQuery` when supplied, overriding the other
mode fields; therefore the fixed query spells out all three existing modes.
It returns a normal pgx transaction, and the existing server wrapper still owns
rollback, detached commit, no-retry policy, connection release and posts. A begin
error causes pgx to close the failed connection. The financial body continues
using its existing extended/prepared protocol.
[pgx transaction implementation](https://github.com/jackc/pgx/blob/v5.10.0/tx.go),
[pgx query implementation](https://github.com/jackc/pgx/blob/v5.10.0/conn.go).

With transaction pooling, a backend is assigned for the transaction and released
at its end. Thus an application's successful pool checkout can precede waiting
for a PostgreSQL backend at `BEGIN`; increasing the application's pool does not
create backend capacity. PgBouncer protocol-level prepared statement tracking
depends on a nonzero `max_prepared_statements`; it prepares a missing statement
on the selected backend and shares identical SQL strings under its internal
names. SQL `PREPARE` is a different session feature. Current effective Main
settings and backend reuse were not collected by this investigation.
[PgBouncer pooling modes](https://www.pgbouncer.org/features.html),
[PgBouncer prepared statements](https://www.pgbouncer.org/config.html#max_prepared_statements).

Prepared reuse also does not guarantee an efficient plan. PostgreSQL normally
compares a generic plan with the average estimated cost of the first five
custom executions. Backend reuse, cardinality and statistics can therefore
matter independently of application request count. The candidate's argument-free
setup has no parameter-dependent scan plan; it does not alter plan-cache mode,
prepared-statement settings or the grant/body queries. Those queries require
separate custom/generic plan controls.
[PostgreSQL PREPARE](https://www.postgresql.org/docs/current/sql-prepare.html).

Redis Lua executes atomically and blocks other server activity during execution;
an advisory Redis admission lease does not replace the PostgreSQL financial
owner. Combining database setup neither shortens a Redis script nor makes
separate scripts or projection requests disappear. End-to-end gains require
the separately owned Redis and post-path controls.
[Redis Lua execution](https://redis.io/docs/latest/develop/programmability/eval-intro/).

## Native qualification contract

`TestLegacySettlementBeginLoadedPoolRequestControl` uses real PostgreSQL and
pgx, the attested disposable database, sixteen concurrent contenders, two warmed
pool slots and four transactions per contender. All contenders reach acquisition
while the fixture holds both slots. Both arms run the same settings read and
commit; only transaction setup differs. It counts complete frontend Query/Sync
and backend ReadyForQuery frames without retaining SQL or payloads. Prepared
descriptions are warmed on both connections before counting. The expected
baseline is 256 cycles for 64 transactions; the candidate is 192, exactly one
fewer per transaction. Zero and 5ms request-delay arms report elapsed time and
actual pool acquisition counters without a wall-time assertion. These are
setup controls, not financial-body or fleet throughput measurements.

`TestLegacySettlementBeginFinancialSettingsAndSessionRelease` uses the real
automatic and explicit applying paths. A transaction trigger checks isolation,
access, deferrability and both timeouts while the intent is owned. The one-slot
fixture starts with different session defaults. Two independent contracts commit
exact payer debit and provider payout; a third insufficiently funded contract
rolls back without affecting that prefix. Session settings restore after both
commit and rollback, and replay cannot repeat payment.

Run the existing legacy target trace body/commit refusal, grant-busy/release,
parent cancellation, admission release, and task independent-finalization
controls on the same frozen source. In particular, fewer requests cannot turn
an unknown commit into success, release unresolved reservation, advance an
interrupted cursor, or make a callback hold the pool. Native results and timings
are pending in the initial source handoff.

False-positive qualifiers: application acquire and begin are client intervals,
not PostgreSQL execution timers; workload completeness and phase nesting must
be preserved. Setup-only acceleration and completed-page counts do not establish
unique financial completion or customer recovery.

False-negative qualifiers: reduced request count can coexist with a growing
backlog when arrivals exceed completed work, busy visits dominate, backend
queues wait, shared grants serialize, or posts stall. Missing source-qualified
arrival/completion pairs and partial worker coverage remain unknown.
