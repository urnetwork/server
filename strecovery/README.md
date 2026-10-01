# Operator signature custody census

`go run ./cli/strecovery` provides bounded MG03/PF03 discovery, local
restoration and offline receipt-observation reconciliation. It reads every
intent generation and every signed attempt from each explicitly selected
operator database, without filtering status or time, and joins them with
selected retained transaction stores. It never signs, estimates new transaction
fees, reserves a nonce, broadcasts or changes database status. The custody
commands do not contact chain RPC. The separate `reconcile` command computes conditional gas
fees from pinned observation inputs; it cannot authenticate their finality.
The [`verify-receipts` command](RECEIPT-COMMITMENTS.md) checks retained raw
headers and transaction/receipt proofs, including gas derived from committed
cumulative values. Actual runtime fees and independent finality remain unknown.

The separate [`collect-receipts` command](RECEIPT-COLLECTION.md) performs bounded
owned-RPC reads to produce those observation/proof inputs in one private,
create-only file after full offline verification. `verify-collection` replays
that file offline. Both retain unapproved node/mapping status and unknown actual
fees; these added commands do not change original custody or operator databases.

The offline [`verify-finality` command](NATIVE-FINALITY.md) joins that exact
collection to GRANDPA weighted certificates, scheduled authority handoffs and
the native Frontier digest relative to a separately pinned checkpoint. It keeps
checkpoint, genesis and runtime approval explicitly absent and actual fees null.
It adds no signing, RPC, database or custody mutation to verification.

[`capture-finality`](NATIVE-FINALITY-CAPTURE.md) reads an explicit owned archive
route into a bounded private journal, preserves partial evidence across restart,
and publishes a pinned proof only after offline verification. A bounded certified
descendant can cover a boundary with no stored justification while preserving
the original collection. Independent authority approval remains absent.

The incident motivating this path was a continuation collector that had 226 of
230 database signatures. Four original signatures needed manual restoration.
The live account reconciler is deliberately status-filtered and cannot serve
as a complete historical custody exporter. This separate path also covers
originals, fee replacements, cancellations, reverted generations, store-only
signatures and unsigned reservations. The prior incomplete-receipt recovery fix
remains unchanged.

## Inputs and authority

Prepare an owner-private physical directory on Linux, a private config file and
separate private connection URL files. All paths must be absolute, canonical
and free of symlinks. Selected directories/files must belong to the effective
user; files must be regular, singly linked and inaccessible to other users.
There is no vault lookup, environment-selected database, password prompt, chain
client or signer interface. Use database credentials restricted to read access.

The config names at least two databases and one evidence store. A connection
file contains a PostgreSQL URL with explicit host, user, database and `sslmode`;
its `sha256:` pin covers exact file bytes, including any newline. Credentials
are never copied into the archive or printed in diagnostic errors. Passwords
are taken only from the URL; pgx fallback hosts/password-file values are not
used. Connection TLS configuration follows the explicit URL and pgx's supported
TLS settings; review the connection environment as part of source selection.

Supply the network, genesis, role addresses, independently expected half-open
nonce intervals and source topology from approved operator evidence. A digest
only detects changed bytes; it does not attest where they came from. Distinct
source labels/paths do not prove independent databases: review their topology
and complete role assignments. A signature encodes a chain id, not a genesis
hash. Genesis identity is supplied configuration/database evidence, not an
offline chain attestation. Empty expected intervals are allowed only when the
independent expectation is that the role has no signed history.

Example config shape, using synthetic identities and paths:

```json
{
  "schema": "urnetwork-operator-recovery-config-v1",
  "chain_id": 31337,
  "genesis_hash": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "roles": [
    {"id": "operator-a", "address": "0x1111111111111111111111111111111111111111", "first_nonce": 7, "next_nonce": 9},
    {"id": "operator-b", "address": "0x2222222222222222222222222222222222222222", "first_nonce": 11, "next_nonce": 13}
  ],
  "databases": [
    {"id": "database-a", "connection": {"path": "/private/recovery/database-a.url", "sha256": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, "roles": ["operator-a"]},
    {"id": "database-b", "connection": {"path": "/private/recovery/database-b.url", "sha256": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, "roles": ["operator-b"]}
  ],
  "stores": [
    {"id": "retained-store", "directory": "/private/recovery/transactions", "roles": ["operator-a", "operator-b"]}
  ],
  "limits": {
    "maximum_intents": 10000,
    "maximum_attempts": 20000,
    "maximum_transaction_bytes": 131072,
    "maximum_total_bytes": 33554432
  }
}
```

Replace every example identity, interval, path and connection pin with reviewed
inputs. Config JSON rejects unknown fields, duplicate/case-folded keys and
trailing values. Connection pins do not grant signing or spending authority.

## Collection and review

```sh
go run ./cli/strecovery collect \
  --config /private/recovery/config.json \
  --archive /private/recovery/census.json --timeout 5m

go run ./cli/strecovery inspect --archive /private/recovery/census.json
```

Each database uses one read-only repeatable-read transaction. Counts and raw
byte bounds are checked before full row reads; row errors, count mismatches,
commit errors and source failures refuse a successful census. Global limits
also apply across all sources, counting duplicate raw bytes. Different
databases are separate snapshots, so quiesce the selected producers during
collection. This command is not a cross-host ownership fence.

Stores use the existing canonical `<64 lowercase hex digits>.rlp` filename
format, without `0x`. Every file is enumerated, bounded and read through a
pinned directory descriptor. Two full byte passes and repeated name censuses
refuse observed changes. Quiesce store writers before collection. Unknown files
require explicit source separation instead of silently disappearing behind a
glob. Hash-named `.scale` files are retained byte for byte as opaque native
evidence; they do not supply EVM nonce coverage and are not restored by this
command.

The census authenticates transaction encoding, recovered sender, chain id,
nonce, recipient, value, calldata, gas and fee fields against durable attempt
and intent columns. Cancellation uses its signed zero-value, empty-calldata
self-transfer envelope rather than the logical execution recipient. Only the
operator's legacy and dynamic-fee EVM formats are supported. Same-hash entries
merge only when the original bytes agree. Every database/source/status/attempt
and every original store filename remains distinct provenance. Same-nonce
alternatives remain separate signatures.

Unselected networks/roles remain in full raw source images and an explicit
exclusion inventory; they never enter the restoration union. Malformed source
records still refuse, including records whose metadata might otherwise hide a
selected signature. Unsigned intents remain separate. Missing store membership
is listed per signature and store. Missing expected signed nonces, absent
attempts, orphan attempts, malformed records and conflicting columns return a
source/record-specific refusal and no usable archive. Original source records
remain unchanged; retain the refusal alongside those sources and repair or
review the source selection before collecting again. There is no partial-green
archive format.

The archive includes all source images, the exact selected inputs (including
credential paths/pins but no credential bytes), derived transactions, fee
envelopes and a census digest. It is private and contains usable signatures:
retain it under operator custody. Writing is atomic and create-only; a different
archive cannot overwrite previous evidence. Inspection is offline and rebuilds
the derived census from retained sources. A changed summary with a recomputed
outer digest still fails when it disagrees with those source images.

Fee fields are maximum gas-fee envelopes in the EVM unit. The all-signature sum
counts unique signatures once; the distinct-nonce bound sums the largest fee
alternative for each nonce. These are neither actual charges nor approvals to
spend. Value transfers are retained as signed transaction values; the fee
envelope is not a total account balance or solvency report.

## Create-only local restoration

After reviewing the archive and its census hash, select an existing private
destination store and pass the exact reviewed hash:

```sh
go run ./cli/strecovery restore \
  --archive /private/recovery/census.json \
  --store /private/recovery/transactions \
  --accept-census-hash sha256:REVIEWED_CENSUS_DIGEST
```

This operation writes only selected original `.rlp` bytes. It has no database,
network, signing or send port. It preflights every existing destination file
before adding any signature; a conflict refuses with the old bytes intact.
Other recovery owners are excluded by a nonblocking directory lock. Quiesce
other writers, which may not honor this lock. Existing matching signatures and
native evidence are not rewritten. Count/byte bounds apply to the resulting
destination too.

Each addition is written to a private temporary inode, synced, sealed read-only,
published with Linux `RENAME_NOREPLACE` and followed by a directory sync. A
restart after a durable prefix adds only the missing files. A crash before
publication may leave `.strecovery-*.tmp`; retain that interruption evidence and
move it out of the selected store after review before retrying. The tool never
silently deletes unknown files. Platforms/filesystems lacking these custody
operations refuse; there is no weaker overwrite fallback.

The inspection output deliberately keeps `spending_authorized`,
`canonical_receipts_reconciled` and `actual_fees_reconciled` false. The restore
result itself reports only created/already-present hashes. Restarting the
operator service, rebroadcasting a retained signature or advancing a database
status requires separate existing authority and canonical recovery checks.

## Remaining PF03 work and qualification

This completes a source candidate for status-independent cross-database/store
discovery and create-only original-byte restoration, with an additive offline
receipt-observation adapter described in [RECEIPT-OBSERVATIONS.md](RECEIPT-OBSERVATIONS.md).
The adapter joins every retained signature and calculates exact conditional gas
fees for unambiguous observed nonce winners. Its output explicitly leaves
native-to-EVM finality authentication, canonical reconciliation and actual chain
accounting false. It does not close MG03 or PF03. An independently approved
observation producer and authenticated mapping verifier, native receipt
reconciliation, synchronized source ownership and final production restart
qualification remain open. No live
database, signing, RPC mutation or deployment was exercised during development.

Deterministic tests live in `census_test.go`, `census_database_test.go`,
`census_files_test.go`, `receipt_reconcile_test.go` and
`../cli/strecovery/main_test.go`. The old active-only query is retained as a
causal negative control. A snapshot barrier forces a
committed concurrent insert between counts and reads. A restoration hook forces
interruption immediately after one durable file, then a fresh owner resumes
without changing the first inode. Other roots exercise source failures,
generation/status variation, original/replacement/cancellation identity,
store-only custody, missing nonce/attempt coverage, duplicates, fee envelopes,
exclusions, resealed-summary tampering, path links, ambiguous JSON, conflicts,
owner locks, bounds and cancellation.

Behavioral qualification is delegated to Sol on a frozen commit. Implementation
did not run test bodies. Qualification must use an isolated PostgreSQL/Redis
harness for the three `TestCensusDatabase*` roots, plus focused normal/race gates
for `./strecovery ./cli/strecovery`, adjacent existing receipt/account recovery
and transaction-model roots, `go vet`, and before/after source/module fences.
Retain failures and environmental mismatches; do not count compile-only checks
or prior receipt-reconciler tests as this candidate's qualification.

The receipt-observation candidate has its own frozen-source gate in
[RECEIPT-QUALIFICATION-HANDOFF.md](RECEIPT-QUALIFICATION-HANDOFF.md). Its new tests
use synthetic signatures and private temporary files only; they require no
chain or database service. The earlier census and adjacent controller/model
gates still require the isolated fixture harness specified in their handoff.

The additive commitment verifier has its exact root selectors, dependency pins
and causal controls in
[COMMITMENT-QUALIFICATION-HANDOFF.md](COMMITMENT-QUALIFICATION-HANDOFF.md).
