# NetEscrow ordering

Migration 747 adds a durable per-balance reservation revision. Escrow writes,
contract insertion/removal and transitions into a terminal outcome advance the
revision in the same PostgreSQL transaction. Balance deletion retains a revision
tombstone. A rollback rolls back both the reservation and its revision. The
migration does not backfill escrow history, move ownership, change payout
attribution, or alter zero-credit legacy exclusions.

The main integration preserves main's published versions 1–741 and relocates
the eight hardening additions from 722–729 to 742–749 without changing their
SQL identities. A database carrying the former branch catalog is refused at
the conflicting index; migration history is never relabeled automatically.

Every cache publisher obtains `(balance_id, revision, reserved_bytes)` from
PostgreSQL. Create, settlement, quarantine, retention and reconciliation all
publish this absolute snapshot through the same Redis script. A delayed
create cannot double a repaired reservation; a delayed settlement cannot release
a neighboring contract's reservation. A newer cached revision rejects an older
page. Different authoritative amounts at the same revision are an integrity
error, while a corrupted or missing counter remains repairable from the same
authenticated amount. Ambiguous Redis responses can retry the whole pipeline.

The amount remains the existing per-balance string key. A second key in the same
Redis cluster slot retains `revision:reserved_bytes`. The revision comparison
uses decimal strings, so revisions and byte counts above 2^53 remain exact.
The counter has a bounded expiry; its fence has no expiry and survives zero
reservations. Correct counters keep their shorter expiry, and lost or overly
long expiries are capped. Empty, never-versioned balances need no cache write.

## Required deployment gates

This source change is not a live deployment. Stop all contract creators,
settlers, quarantine/expiry workers, billing retention, and reconciliation
writers. Drain existing posts, apply the append-only migration to the matching
catalog, deploy the compatible publisher everywhere, and require the migration
monitor's actual function/trigger checks. Reconcile affected balances before
admitting traffic. **Mixed additive and fenced writers are unsupported and
block deployment.** An older executable is not a supported rollback target.

Non-expiring fence keys require an eviction policy that preserves them
(`noeviction` or a `volatile-*` policy). An all-keys eviction policy, manual
fence deletion, Redis flush, or restoration from an older cache snapshot can
erase ordering history. Such recovery requires stopping and draining writers,
then rebuilding from PostgreSQL before resuming. A PostgreSQL restore also
requires a coordinated cache reset; old fences must not outrank restored
database revisions. Cache loss is not proof that financial state is lost.

Capacity qualification must include the new revision row and persistent fence
for every used or retired balance, plus the indexed source read and revision
write on the hot creation path. Revision tombstones have no automatic pruning;
design a bounded export/retirement policy before deleting them. The migration
guards ordinary deletion, revision rollback, and source-table truncation.

Positive-byte admission locks eligible payer balances in ascending ID order and reads
durable open reservations in a separate statement after the lock completes.
Both origin and companion creation use read-committed transactions. Reading
reservations in the locking statement would retain its pre-wait snapshot and
reintroduce double admission. Client lifecycle locks precede balance locks;
settlement takes its contract lock, then the same ordered balance locks, before
claiming the terminal outcome and updating reservation revisions.

The internal prober retains main's newest-16 and next-48 candidate windows.
Each window takes ordered balance locks before the durable reservation read,
then uses the payer client's hash in the original discovery order. A successful
window retains its locks through commit. A rejected window rolls back its
savepoint before the next window or complete fallback, so speculative locks
cannot invert the next lock order. Ordinary payers and full fallback lock all
eligible grants for positive-byte requests. Zero-byte anchors read only the
earliest-expiring eligible grant without balance locks or a reservation census.
Their priority comes from that committed statement snapshot, including while
another transaction changes a grant's expiry, paid status or existence. The
endpoint lifecycle locks remain in place, so concurrent client deactivation
still blocks admission and rejects it after committing. The bounded discovery
path adds a locked reread; it does not
establish production contention or capacity limits.

Creation mirror posts reuse the exact admission census when one subsequent
committed statement confirms the created contract is still open and each
balance's revision equals its admission revision plus two: one advance for the
escrow insertion and one for the open-contract insertion. The corresponding
absolute amount is the admission reservation plus this contract's allocated
bytes. This check adds no statement under the grant locks. Contract visibility
guards a rolled-back transaction whose predicted numeric revision is later
reused. A changed revision or missing/closed contract triggers the existing
exact census for that balance; concurrent cancellation between the census and
inserts is therefore also detected. The same Redis script still rejects older
publication after this read. Other publishers retain their authoritative census.

The reservation query excludes zero-byte anchors before joining contract
outcomes. It retains disputed unresolved reservations, the unsettled prefilter,
and the existing per-balance optimization boundary. In a disposable PostgreSQL
18 fixture with 8,192 zero anchors, 64 positive open reservations and 64 closed
unsettled rows, custom and generic plans reduced contract-join inputs from
8,320 to 128 and returned the same 64 reserved bytes. Buffers were unchanged
in that small hash-join fixture; this is not a forecast of Main CPU reduction.

These reductions require no new migration or index. The existing reservation
revision triggers and guards must pass their actual-definition monitor checks.
Rolling deployment among the current fenced writers is compatible. Observe
`urnetwork_net_escrow_creation_snapshot_total{result="reused"|"reloaded"}`,
the changed census SQL fingerprint, grant lock queues, and latency/CPU under
comparable traffic. Positive allocations still serialize on shared grants;
benefit depends on zero-anchor volume and how often committed revisions remain
unchanged until the post. This patch alone does not establish Main recovery.

The successful terminal claimant debits consumed payer bytes in that same
PostgreSQL transaction. A missing post can no longer release a reservation while
leaving its spent credit available for a new contract. Rollback rolls back the
outcome and debit together; a duplicate settlement cannot debit again. Existing
zero-byte anchors, positive-grant allocation, signed provider usage and explicit
zero-credit legacy exclusions retain their semantics. No new migration is
required for atomic admission; the relocated hardening catalog through 748
retains the same SQL identities.

The bilateral settlement mean is computed without adding two signed byte
counts. Two valid maximum-sized reports must not wrap negative and bypass the
debit; excessive usage still rejects settlement without releasing its reserved
credit. Negative reports consumed by ordinary or adjudicated settlement also
fail before the terminal claim, including legacy rows without usage snapshots.
Malformed negative escrow grants are rejected before cumulative settlement
arithmetic; they cannot wrap an unpaid contract into a completed settlement.

This removes reordered-post and stale-page corruption while the fence history
is retained. PostgreSQL and Redis still do not form one transaction: a crash can
leave the approximate display mirror behind until publication or reconciliation
succeeds, but that mirror no longer authorizes admission. The coordinated drain
must also exclude every old creator and old asynchronous debit writer before
traffic resumes. A rolling mixed-version deployment is unsupported.

Historical terminal contracts may already have lost a debit post. The old
schema has no per-debit receipt that distinguishes applied from unapplied
debits, so neither a terminal outcome nor a settled escrow marker is evidence
that the payer was charged. Retain and independently reconcile the exact
historical balance/debit evidence before activation; do not guess a debit or
repeat an old post to repair it. Restoring PostgreSQL must likewise preserve
that evidence and its financial cutover boundary.

Participant sweep publication, account payout increments and clock/statistics
posts remain separate asynchronous work. This bounded custody fix does not
make those posts atomic, durable or idempotent and does not prove complete
financial settlement or production load capacity. Qualify the extra indexed
reservation read and per-payer lock contention under the intended workload
before production admission.
