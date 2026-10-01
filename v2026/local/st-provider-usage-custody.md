# Provider usage custody

Mainnet migrations 744–746 follow main's published catalog through 741:

- 744 separates signed client-key histories by authenticated policy domain.
- 745 refuses new credit-bearing terminal contracts without `provider_usage`
  and a close time. Published snapshots and terminal outcome/time attribution
  cannot be rewritten. An existing NULL snapshot can only become the exact
  row-bound, zero-credit legacy exclusion used by the separately reviewed
  repair procedure; it never becomes reconstructed positive credit.
- 746 copies the current `provider_usage`, contract ID, outcome and close time
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

Migration 748 appends a partial index for credit-bearing terminal rows with a
NULL close time. The reader now probes one such identity per live/archive store
in the same statement snapshot as the requested epoch. If either exists, the
entire new payout is blocked: an unknown timestamp cannot prove that work lies
outside any requested epoch. No partial usage, guessed timestamp, positive
backfill or implicit zero-credit exclusion is produced. Canceled/open rows do
not own credited work and remain excluded. The archive's existing close-time
index covers its corresponding bounded probe. The migration monitor checks
the exact live index definition and valid/ready/live state. Interrupted online
index creation is repaired by separate concurrent drop/create steps; the
published main catalog through 741 is unchanged. The eight hardening SQL
entries retain their exact identities and order at versions 742–749. Retained
databases carrying the old branch positions 722–729 fail catalog verification;
the merge does not rewrite or reinterpret their history.

Apply these migrations during a coordinated fleet cutover with settlement
readers, writers and the billing reaper stopped. Install the compatible reader
and writer on every operator before restarting work. Require the migration
monitor's actual schema/function/trigger/index checks through 748, not only a
numeric version.
An old writer is refused by the database; an old reader does not understand the
archive and is not a supported rollback target. No live migration or deployment
is performed by these source changes.

Before admission, retain the exact missing-proof census, including terminal
rows with a NULL close time or usage snapshot. Existing NULL rows and explicit
legacy exclusions do not establish complete economic acceptance. Repairs of
retained working rows continue to require the separate whole-row/report
manifest; there is no automatic repair of an immutable archived row.

The new read barrier reports a retained contract identity, not a complete debt
census. Preserve the complete exact NULL census before cutover. Historical
unknown close times remain an unresolved admission blocker until a separately
reviewed policy can establish their treatment without inventing epoch ownership.
Previously retained payout artifacts are replayed as before; this change neither
recalculates them nor certifies that their original source history was complete.

This archive is prospective. Migration 746 does not reconstruct rows already
deleted before it was installed. The migration receipt and retained source
census must bound the history claimed by a release. Archive storage is durable;
an authenticated export/retention policy must be designed before pruning it.

Migration 747 separately fences the PostgreSQL/Redis NetEscrow ordering race;
see [NetEscrow ordering](net-escrow-ordering.md) for its coordinated writer
cutover and cache recovery requirements. These changes do not choose or
implement a native emission remainder mechanism or prove chain funding,
capture, claims or mainnet acceptance.
