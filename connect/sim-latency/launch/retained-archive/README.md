# First-committed retained archive command — synthetic unit qualified

The exact source passed independent maximum-effort review and three consecutive
offline Go 1.27.3 race-enabled synthetic unit runs: 27 tests per pass, zero skips
or failures. See the [qualification record](../RETAINED-ARCHIVE-QUALIFICATION-2026-10-28.md)
for source and log pins. Live operation remains unqualified: the standalone native
command binary has not been built or pinned, and canonical-wrapper/native-storage
integration remains pending. Do not build or invoke this command during timed
scoring. Unit qualification does not authorize preparation, upload, calibration
acceptance, or production activation.

This is a separate first-committed archive context, not a replacement name for the
earlier C2 bridge archive. It adapts the reviewed bridge uploader's streaming
member verification and existing `server/blob` protected-storage client. It does
not reuse the bridge's inventory, fixed member count, receipts, spent claims, or
object namespace. It adds no service, migration, or general backup system.

## Invocation boundary

Before any invocation, the operator must establish that the intended first-seed
campaign is terminal, its native owner and evaluation processes have exited,
temporary evaluation mounts/resources are released, and no writer or active file
descriptor can change any selected source. All three declared jobs must have
terminal outcomes, or a separately recorded terminal campaign failure must explain
the retained partial/unstarted jobs. A favorable outcome is not required.

The command also acquires the exact existing canonical scoring-lock inode,
nonblocking, before any source scan or hash. It never creates or repairs that
legacy lock. That one exact legacy lock ancestry is not an exception for evidence
paths. The lock alone does not certify terminal jobs or absence of residual
writers; the operator's lifecycle checks remain required. No archive operation
restarts a scoring clock, runs a measurement, changes a seed, samples entropy, or
starts the committed second seed.

Runtime is root-only. All evidence/output ancestry must be root-owned,
non-symlink, and not writable by another principal. Selected source directories
must be mode `0700`. Original source files must be root-owned, single-link regular
files; a private source directory protects original `0644` artifacts without
changing them. Plans, inventories, tars, claims, and receipts are private regular
single-link leaves. New directories are `0700` and completed files `0400`. Symlinks,
hard links, special files, traversal, duplicate names, ambiguous JSON keys,
unknown fields, and case-folded schema aliases refuse.

Only exact roots compiled into `defaultArchivePolicy` are accepted: the original
campaign, first-committed selection/recovery evidence, each of its first three
job roots, and the separately approved management-OOM forensics root. There is no
arbitrary source, endpoint, credential, bucket, retention-date, or context override.
The whole campaign includes immutable private plans and unstarted second-seed
inputs. Their private WORM retention is permitted; never publish, decode, curate
publicly, or reveal those inputs. Files are copied and hashed as opaque bytes.

## Modes

Local verification is the default:

```text
retained-archive -plan <protected-archive-plan.json> -plan-sha256 <reviewed-sha256>
```

It verifies an already-created tar, independently hash-pinned private plan and
inventory, exact compiled context/source/namespace/retention, complete current
source census, every tar member's size and SHA-256, and the whole tar SHA-256.
It neither constructs a blob client nor creates a persistent file.

Explicit local preparation creates a new private inventory, tar, and plan:

```text
retained-archive -prepare -source <exact-compiled-source-root> -nonce <new-48-lowercase-hex-storage-nonce> -staging-reserve-bytes <additional-free-bytes>
```

The nonce identifies storage only; it is supplied by the operator, not generated
by the command, and is not calibration entropy. Each source is a complete regular
file census, including failed/partial artifacts. Empty directories are not tar
members. The inventory records sorted relative names, sizes, and full member
hashes; the tar contains those files plus its exact `ARCHIVE-CONTENTS.json` copy.
The extra inventory member is excluded from its own payload listing, so the
archive has `payload_files + 1` members. No 493-member assumption remains.

Preparation creates one exclusive namespace under the compiled protected work
parent and writes `prepare.started.json` before outputs. Any existing or partial
namespace refuses; it is never rolled back, overwritten, or deleted. A final
source-and-tar verification precedes `prepare.complete.json`. Preparation creates
no transport and does not load blob credentials.

After review of the resulting plan and its SHA-256, upload requires two explicit
authorizations:

```text
retained-archive -plan <protected-archive-plan.json> -plan-sha256 <reviewed-sha256> -upload -authorize-upload-plan-sha256 <same-reviewed-sha256>
```

Upload creates an exclusive fsynced `upload.started.json` **before** constructing
the native blob transport. A preexisting claim, protection proof, retained-write
proof, or receipt refuses. The client comes only from ordinary host-scoped native
blob settings; local storage and a changed general blob prefix refuse. The new
object key is derived from the first-committed namespace, source unit, storage
nonce, and archive digest; it cannot be relabeled as the old bridge.

The command requires protected versioning, compliance object lock, and a native
validated replication configuration. It checks the entire fresh archive prefix's
version history, not just current objects. Any version, delete marker, listing
failure, or uncertainty refuses. Source/plan/inventory/tar validation repeats
immediately before the only operator `PutRetained` call. The native client's own
HTTP retry count is not asserted.

A returned write must match the exact key, byte length, nonempty/non-null version,
`COMPLIANCE` mode, and **exactly** `2028-01-01T00:00:00Z` retention. The exact
returned source version is read back and compared to the archive's complete
length and SHA-256; there is no current-version fallback. Only then is a private
receipt written. It does not prove atomic retained-if-absent behavior, replica
delivery, offsite backup, calibration acceptance, launch acceptance, or permission
to delete originals.

## Bounded work and failure handling

Defaults belong to `archiveSettings`: 4 GiB per tar; 4 MiB per administrative
document; 8,192 payload files; 16,384 total examined source entries; source depth
64; directory batches of 128; 64 KiB streaming buffers; 10 minutes for local
preparation/verification and 30 minutes for upload plus readback. Oversize or
unrepresentable USTAR members refuse rather than selecting a subset. Per-job
archives can be prepared separately; no automatic partitioning is performed.
There is no compression, PAX extension, sparse-file mode, or public curation.

Before preparation, available disk must cover the predicted new tar, at least
512 MiB safety space, the explicitly declared staging reserve (0–1 GiB), and two
maximum metadata documents. Verification/upload account for the already-existing
tar via actual available space and do not double-count it as a new allocation.
They still require safety, metadata, and staging headroom. This is a point-in-time
filesystem check, not a reservation against unrelated disk users.

Any cancellation, disk/metadata failure, ambiguous write, incorrect returned
version/retention, readback error, or partial receipt leaves all source and partial
local evidence in place. No automatic retry, claim removal, cleanup, overwrite,
rollback, or delete exists. Even a failure before the retained write permanently
spends the upload claim. Stop and inspect it manually; do not rerun the upload or
create a replacement nonce to work around uncertainty. A fresh operator-reviewed
disposition is needed after any partial or ambiguous attempt.

Synthetic tests use temporary owned trees, a temporary real flock, generated
digests, `.example` destinations, and an injectable deterministic transport.
They never call native blob storage, decode real private requests, use live
identity literals, or read campaign evidence. The package is wired into the
existing `tests.sh` workflow, whose complete integration run remains separate
from the direct synthetic qualification above.
