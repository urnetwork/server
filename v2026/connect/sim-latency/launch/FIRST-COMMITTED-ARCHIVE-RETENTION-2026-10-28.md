# First committed screen: private retained source versions

All three separately authorized archives completed native COMPLIANCE retention
through **2028-01-01T00:00:00Z**, with exact source-version length and SHA-256
readback equality. An independent checkpoint reader rehashed and inspected the
three root-private receipts. This preserves the [failed screen](FIRST-COMMITTED-SCREEN-2026-10-28.md);
it does not change its `accepted: false` result or grant launch acceptance.

## Exact retained objects

The bucket is `competition`. Every receipt records protected versioning, a
validated replication configuration, complete pre-upload source/member hashes,
one operator retained-write call, and exact-version content readback equality.
SDK HTTP retry counts and atomic retained-if-absent behavior are not claimed.

| Unit | Payload entries | Tar members | Tar bytes | Exact source version |
| --- | ---: | ---: | ---: | --- |
| Management OOM forensics | 31 | 32 | 89,600 | `ed5dd7f9-47ca-4dd2-817a-6672576bc3c2` |
| Whole campaign | 1,026 | 1,027 | 778,926,080 | `0eebfba7-ab33-47f3-9f8f-e93e1b51cccf` |
| Selection and recovery | 37 | 38 | 182,252,544 | `23d9ce25-6a46-41b4-9598-3cd719e69350` |

There are **1,094 payload entries plus three embedded inventories**, totaling
**961,268,224 tar bytes**. These are archive-entry counts, not a deduplicated
payload-byte claim. The last receipt completed at `2026-10-28T05:38:13.903813726Z`.

Management OOM forensics:

- Tar SHA-256: `3f2e47482d6868d31feff9cdde762f5f8508ee46523580ed789a15aeee015857`.
- Private receipt SHA-256: `595606d14042643af84ea5e5564bcade01f3b08e543e0f3c2103187850d558d8`.
- Key: `blob/competition/calibration/first-committed-2026-10-28/management-oom-forensics/be9260b63d9b67bececf79347f625254a3564e79aab46f64/3f2e47482d6868d31feff9cdde762f5f8508ee46523580ed789a15aeee015857.tar`.

Whole campaign:

- Tar SHA-256: `868ef62d67869d5719e8e1f1249d412d91fe5dd895dc2b3b938d8f3d94499604`.
- Private receipt SHA-256: `cfd45d4b09f9036ebd8096e41496a8892f63c2ab400adffd33586718d36fde16`.
- Key: `blob/competition/calibration/first-committed-2026-10-28/campaign/6a8b4c1c0d025f2393ff5ca3809ab0d5335caabf1776419c/868ef62d67869d5719e8e1f1249d412d91fe5dd895dc2b3b938d8f3d94499604.tar`.

Selection and recovery:

- Tar SHA-256: `ba8819279254ea7e4d0ca2fa878d417a095f29cece6ed9441e85131b9b6823b4`.
- Private receipt SHA-256: `258e502387b906ad2bd0343fb7a93d384a3d52736daa6423b12bb5d7ef97d2c5`.
- Key: `blob/competition/calibration/first-committed-2026-10-28/selection-and-recovery/0f88e8b9983c7a4450206cb87f6dc3868c5604cc3f776d6d/ba8819279254ea7e4d0ca2fa878d417a095f29cece6ed9441e85131b9b6823b4.tar`.

## Binary and evidence boundaries

The installed root-owned `0500` command is
`/usr/local/libexec/urnetwork/competition-retained-archive-d820d5b3-20261028-v1/retained-archive`:
40,180,784 bytes, SHA-256
`6132191ee9ae1bf9c410fa16fee508c0f02cdcb2c60e94df069cadd5e969964c`.
Independent installed-byte/ownership/build-metadata readback verified Go 1.27.3,
CGO enabled, trimpath, server revision
`d820d5b301850dffc0e0ab1d6c94c9964296a253` and `vcs.modified=false`.
The new release directory is root-owned `0700`; its existing parent stayed
`0555`. [Source-unit and build evidence](retained-archive-qualification-2026-10-28/README.md)
retains the separate phases and their original observations. No full transitive
source census or complete integration-wrapper pass is claimed by the build.

The raw receipts, tars, inventories, private plans and provider snapshots remain
private. The whole-campaign tar includes the private **unstarted second seed**;
retention did not measure it or authorize disclosure. This public record contains
only safe object/version/count/hash metadata. Do not copy private payloads or
seed values into Git. All three receipts record zero source deletions and zero
configuration changes; originals and partial/failed evidence remain preserved.

At approximately 06:37 UTC on October 28, the three redundant local
`full-evidence.tar` files were removed after rechecking each root-owned,
single-link regular file's exact size and SHA-256 against its retained receipt.
This reclaimed 961,268,224 bytes for qualification. No original source,
measurement, private input, receipt, inventory or MinIO object was removed.
Restore a tar by its exact bucket/key/version above before attempting local
tar verification; an absent staging copy is not a missing retained version.

Replication configuration validation is **not replica delivery proof**. Delivery
of these three versions, offsite backup, and restoration from a replica remain
unverified. No deletion authority, successful calibration, new seed draw,
measurement, late opening or production activation follows from these receipts.

## Separate retained-baseline check

The owning LOW runner reported the repository baseline verification exited zero
in six seconds. Independent log readback counted 3,677 manifest `OK` rows/files,
zero failures, the final `sim-latency baseline snapshot: verified` line and empty
stderr. Stdout SHA-256 is
`bd311447c251d95f2d367adce991ba255a2dbaab97441132828f1307903b9f38`;
stderr SHA-256 is
`e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`.
The private logs remain at `/tmp/baseline-verify.RAaf2sE5`. This checks its own
repository snapshot manifest, not new measurements or a different 3,677-entry
inventory for the three tars above.
