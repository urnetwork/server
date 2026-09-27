# Provider usage custody

Mainnet migrations 724–726 append to the existing migration catalog:

- 724 separates signed client-key histories by authenticated policy domain.
- 725 refuses new credit-bearing terminal contracts without `provider_usage`
  and a close time. Published snapshots and terminal outcome/time attribution
  cannot be rewritten. An existing NULL snapshot can only become the exact
  row-bound, zero-credit legacy exclusion used by the separately reviewed
  repair procedure; it never becomes reconstructed positive credit.
- 726 copies the current `provider_usage`, contract ID, outcome and close time
  into `st_provider_usage_archive` in the same transaction that deletes a
  settled or adjudicated contract. Failed capture aborts deletion. The archive
  rejects changes, deletion and truncation; truncating the live contract table
  is also refused. Ordinary billing retention continues deleting its working
  rows and dependencies.

The epoch reader uses one SQL snapshot across live and archived rows. Cleanup
cannot expose a gap or count the same contract twice. A reused identity with
both live and archived credit-bearing rows fails the entire window. The
existing decoder and exact legacy debt checks apply equally to archived data.
Missing historical snapshots remain errors after cleanup. The archive adds no
credit and does not change paid/free byte allocation or settlement outcomes.

Apply these migrations during a coordinated fleet cutover with settlement
readers, writers and the billing reaper stopped. Install the compatible reader
and writer on every operator before restarting work. Require the migration
monitor's actual schema/function/trigger checks, not only numeric version 726.
An old writer is refused by the database; an old reader does not understand the
archive and is not a supported rollback target. No live migration or deployment
is performed by these source changes.

Before admission, retain the exact missing-proof census, including terminal
rows with a NULL close time or usage snapshot. Existing NULL rows and explicit
legacy exclusions do not establish complete economic acceptance. Repairs of
retained working rows continue to require the separate whole-row/report
manifest; there is no automatic repair of an immutable archived row.

This archive is prospective. Migration 726 does not reconstruct rows already
deleted before it was installed. The migration receipt and retained source
census must bound the history claimed by a release. Archive storage is durable;
an authenticated export/retention policy must be designed before pruning it.

These changes do not repair the PostgreSQL/Redis NetEscrow ordering race, choose
or implement a native emission remainder mechanism, or prove chain funding,
capture, claims or mainnet acceptance.
