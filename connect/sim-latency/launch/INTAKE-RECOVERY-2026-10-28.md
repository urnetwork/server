# Production intake recovery — October 28

## Executive summary

The competition is **not open**. The original October 28 00:00 UTC launch was
missed. The subsequently reserved 23:25 UTC intake opening was also missed;
it must not be backdated or reported as a successful launch.

The deployed API acknowledged the exact production source manifest. Host
preparation then failed before timed R1 simulations because the pinned evaluator
image had been removed from Docker's image store. The missed reservation is
protected by a source-activation hold, in addition to the evaluation and
reward-delivery holds. Calibration and genuine Qwen/Gemma qualification remain
deferred, not passed.

This note records observed state through 23:41 UTC. It does not authorize a
replacement season, release a hold, or qualify recovered images.

## Verified deployment and reservation

- Published control release: server `72fcd28b2108b3717b966d80d25adde0fec8b6de`,
  SN `a108f14381abc8062ae5952bbf24456754deb5cb`.
- Published intake config: `6fd49bc07d572166832223b8aa11b8600aa96430`.
- Production namespace: environment `main`, competition `sim-latency-season-1`,
  season `sim-latency-production-20261028`.
- Pending baseline activation generation: `1`; reserved round:
  `01a184cc-96cf-9aad-3342-3af87251c41b`.
- Its immutable opening is `2026-10-28T23:25:00Z`. The source was not activated
  and the round was not created.
- The native bootstrap completed at 23:22:18 UTC. Publication verification
  produced all eleven receipts: ten frozen evaluation repositories and config.
  The evaluation checkpoint was not replaced with the moving control release.
- A real API acknowledgment returned HTTP 200 at 23:25:49 UTC, role `api`,
  generation `1`, manifest commit `6fd49bc…`, manifest SHA-256
  `90097d6d0bbe96ca93a48364731b35e1b4da97c0e18c099ce299041466201eae`,
  runtime image `sha256:adaf4660eb23d6372c8a7325cc6b087941c0a19c7002d386f3ea9752ac3ffd4c`,
  and capability `competition.reward.v2.runtime-blob.v1`.
  This is an API acknowledgment, not worker quorum or successful activation.

The intake implementation's independent test execution completed with 558
top-level passes, zero failures and zero skips. Its sealed receipt is
`/mnt/data/competition-intake-runtime-VBJKBV72/TERMINAL-RECEIPT.txt`, SHA-256
`02cf5012d2a3ae42eec43c4fdd976336706da3afae1ad63a9dd589b45cc13b00`.
Unit tests do not establish calibration, provider qualification, or runtime
readiness.

## Image-loss incident and retained evidence

The host audit records an interactive `docker image prune -a` at 23:19:14 UTC.
The evaluator image
`sha256:98c9061c4a43a25e7b581a5193ca6e7f48e49e7b7fc03b7c1b6190a029befee2`
and newly built control image were subsequently absent. The exact read-only
Docker source-tree probe returned exit 125, "No such image". No timed R1
simulation or worker readiness acknowledgment resulted from that preparation.

Private incident evidence is retained under
`/var/lib/urnetwork/competition-production-intake-20261028-v1`:

| Evidence | SHA-256 |
| --- | --- |
| `launch-incident.json` | `1cdef1563d4bb45dbde301788565a26da47cc98fa57e448376b0f39662ef8dfa` |
| `docker-prune.audit.log` | `08a96d1eafe851d490357bb8c013273e64f58413e8b43fdac79b3935559a6e69` |
| `api-readiness.receipt.json` | `d31f7f081c302a4e3aa671576c88451b9e34cf11a955ab85f02ac45ac77a343a` |
| `source-activation-hold.receipt.json` | `9c63532f53787cf2bdc3b05149ad5337a4d97e2eb464beadbde49c61aada0ac6` |

The activation hold was accepted with HTTP 200, revision `1`, hold ID
`01a184d8-0961-8c35-e65a-ab592fc8480f`. Do not release it merely because image
recovery succeeds.

The control image was restored by an unchanged, all-cache-hit build and its
actual image ID matched the original
`sha256:dd6bda462c032b7529a95439bcdc04dfb9aa63fdd1b72bc9a04302c4d826ba0a`.
Its protected recovery archive is
`/mnt/data/competition-image-recovery-xF0kuLaU/control-dd6bda46.tar`,
377,424,384 bytes, root-owned mode 0400, SHA-256
`409f57ca34e3ebe12fad9c490506c51ca00cb14650e081b392a548aea1201090`.
The unchanged config and all five layer content digests were independently
verified. The recovery receipt SHA-256 is
`c8241e5a7eb29b5c28a6660ffe43af8fb30086ce947e7e2537c42cbca5577256`.
This does not restore or qualify the evaluator image.

The original evaluator config and backing layer metadata/data were found in
retained Docker/BuildKit storage. Exact archive reconstruction is in preparation;
it is not yet accepted. Recovery must verify every complete uncompressed layer
against its original diff ID and require the original final image ID. Do not
substitute a newly built image or use `docker import` as an equivalent identity.
Do not mutate daemon metadata or prune retained caches during recovery.

## Safe continuation

The existing native code and database guards do not support rescheduling this
acknowledged baseline reservation. Omitting `--opens-at` when retrying bootstrap
preserves its persisted opening; it does not reset it. Activating after that
opening would leave a source whose round creation is rejected. No manual
timestamp, intent, pointer, or historical-data edits are authorized.

Owner approval has been requested for a replacement production season scope.
If approved, preserve this failed reservation and all evidence, provision the
replacement scope and scoped holds/config/credentials, and bootstrap with a
dynamic opening rather than another expiring reservation. The frozen source,
six full epochs, economics, significance rules and deferred grading gates must
remain unchanged.

Before replacement intake, require genuine current-boot host qualification,
exact-new-target R1, independent durable API/worker acknowledgments, current
hold verification and native fenced activation. Then verify an actual submission
is accepted and remains queued with grading and publication held. Full launch
readiness still requires the actual deferred calibration and two-model reviewer
qualification; none is inferred from image recovery or successful API deployment.
