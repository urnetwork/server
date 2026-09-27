# NetEscrow ordering

Migration 727 adds a durable per-balance reservation revision. Escrow writes,
contract insertion/removal and transitions into a terminal outcome advance the
revision in the same PostgreSQL transaction. Balance deletion retains a revision
tombstone. A rollback rolls back both the reservation and its revision. The
migration does not backfill escrow history, move ownership, change payout
attribution, or alter zero-credit legacy exclusions.

Every cache publisher now reads `(balance_id, revision, reserved_bytes)` in one
PostgreSQL statement. Create, settlement, quarantine, retention and reconciliation
all publish this absolute snapshot through the same Redis script. A delayed
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

This removes reordered-post and stale-page corruption while the fence history
is retained. PostgreSQL and Redis still do not form one transaction: a crash can
leave the approximate mirror behind until a post or reconciliation succeeds.
Admission still uses the existing approximate cache and can over-reserve during
concurrent creation or cache loss. Separate billing/payout posts are unchanged;
these tests do not establish production load capacity or exact financial
transactionality for those paths.
