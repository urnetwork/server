# Required private full-archive retention

No upload was performed during this curation. The local original remains the
only complete retained bridge tree represented by this dataset. Do not delete
it on the strength of the smaller Git copy or this plan.

The full artifact root is
`/home/by/urnetwork/temp/c2-metadata-bridge-preparation-20261026.KNG6TfgM/artifacts`:
**479 regular files, 380,987,722 bytes**, all enumerated and byte-authenticated in
[ORIGINAL-ARTIFACTS.json](ORIGINAL-ARTIFACTS.json). The public subset preserves
279 originals and explicitly omits 200. Full retention must include all 479,
including both original run/scorer-input copies and every verbose log. The small
manager logs, exact native acceptance/input/verification/assembly receipts,
precommit/publication, margin freeze, instrument descriptor and source lock must
be retained alongside it with their original byte identities.

The separately authorized archival step should:

1. Recheck the complete original inventory, refuse links/special files, duplicate
   archive entries and unsafe paths, then package only those known files under
   relative paths. Do not recurse into the workspace, source checkouts, Docker
   storage, live main configuration, keys or vaults. Preserve every original
   file's content; compression is allowed only as a lossless wrapper with a
   declared archive SHA and uncompressed file inventory.
2. Use the repository's existing [protected blob interface](../../../../blob.go):
   require `ProtectedBlobStore.CheckProtection`, versioning, compliance-mode
   retention and the required server-validated replication destination. Choose
   the private calibration-evidence key/retention policy through the authorized
   application configuration, never a public bucket or ad-hoc endpoint.
3. Upload with `PutRetained`; preserve the returned key, immutable version id,
   size, retention mode and retain-until evidence. Record authority from the
   protection proof and the archive SHA computed by the caller separately:
   `BlobRetention` does not itself return a SHA. Do not substitute an ETag for
   content authentication or fabricate a receipt from a successful command exit.
4. Read that exact version with `GetVersion`, stream-check the archive digest,
   and validate every original relative path, byte count and SHA against the
   479-file inventory. Preserve the readback/protection/replication receipts.
   Only then can an actual private archive receipt be linked from a later
   append-only publication. Do not overwrite the current false upload flag.

This plan does not authorize an upload, retention-policy change, rescore,
measurement, deletion or another calibration/model attempt. It does not make
historical bridge/A controls into a future production epoch baseline.
