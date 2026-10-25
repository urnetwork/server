# Canceled-stop evaluator: built; local qualification passed

Status observed **2026-10-25T13:19:53Z**: source qualification, the immutable
image build and all four independent local qualification phases passed.
This is not production acceptance, calibration,
runtime approval or production activation.

## Qualified inputs

| Input | Exact identity |
| --- | --- |
| Evaluated server | `c1f9dda603d6279666fca5d92b99c5663306c5f7` |
| Parent release | `0d58d0d91b069c62bcfb39cd72166c26925b786a` |
| Ten-repository canonical source digest | `5e00bf5ee7b4708be94ac1ebc6daf5c024f4afe467ca30c35024f65f96b6c90e` |
| Config main commit | `0c6dcaa1da2a680696f53c043291df7860c8136f` |
| `config/main/sim-latency.yml` SHA-256 | `2da3e012f2ac775514a8ee405843e26fb8b6e5c40476f9b07c282cc155c8a16c` |
| Source-choice preparation SHA-256 | `ce7abe90ae352ba6d6060af749c1066927986225d982d9e44d343ea75cabd3d7` |
| Required actual compiler | Go 1.27.3 |
| Built local image ID | `sha256:002c39040ce23dee27b171c7e657f749c6a6d05b1e9b97d90ac87336470dbaf3` |
| Simulator/scorer SHA-256 | `6c8587be62b8292c5d4ec27a8b5c3353e84a8121a6054ba3755d022d2372e9a3` |
| Embedded raw lock-file SHA-256 | `a7208c582b8e7b559a0d8ab741b41420be4c313a4cd75282cb0fb4902cc9f392` |

The server successor contains only the three-file cancellation correction;
the other nine pins are unchanged. Its 17-test targeted race smoke and three
consecutive 340-test full simulator suites passed. See
[root-cause and qualification evidence](DB-CANCELLATION-RECOVERY-2026-10-25.md).
The config merge from origin added unrelated Geo databases without changing
the source ledger. Nothing copies those databases or any config/main/all or
vault/main/all tree into the evaluator image.

## Independent remote-source preflight

The existing root-owned native source tool was read back at SHA-256
`e27b5964d25a166d2487e7ab35a62634b1a693f2fe381e9c0dee4c04dedd0a5c`.
Independent execution passed in 24 seconds, exit 0: epoch 0, production source
branch `sim-latency`, all ten exact published commits, server `c1f9dda60`, and
independently reconstructed canonical digest `5e00bf5e…`. Operator and discovery
checkout state receipts were byte-identical before and after. No new native/Git
child or preflight temporary directory remained.

Evidence: `temp/canceled-stop-source-record-20261025.TdEQCMWP/`.
Native JSON stdout SHA-256:
`1c9dadb0904ba71bc4ba098ec738cd974eebea22e3be9d2d1d5a0e6055e65e65`.
Stderr SHA-256:
`e691e48a13658a0777d271bec95691a99a8a4cbb47199c2b51c9230b086e4ce0`.
Discovery repositories intentionally without a local HEAD were recorded as
unborn; their operator working branches were not changed or required to match.

## Completed build

- Root: `temp/canceled-stop-evaluator-build-20261025.xPrOmOTq/`.
- Exec session: `11747`; manager PID: `456756`.
- Started: 2026-10-25T12:43:38Z; watchdog: 3,600 seconds plus 60-second kill grace.
- Ended: 2026-10-25T12:58:07Z; exit 0 in 869 seconds (14m29s).
- Target local tag: `urnetwork/sim-latency-evaluator-base:canceled-stop-c1f9dda6-go1273`.
- Source builder: clean main `9a3b7970ad1ad2b2faa6f06908a95ff0e8255a98`;
  builder script SHA-256
  `2f07c7c8227173559ee4b5f5c122206ca6bf5d2909bc4344c99f5842e57178d4`;
  Dockerfile SHA-256
  `bf964f84926d1bf741bf5c7465cb166e062a1a91f4811fc5cbc2170e8dead3c2`.

Build log SHA-256:
`e0d61d906804702e4bd4819cbe89ce72d8f2ebae1e63c5d941cdea1c29e66e6d`.
Image record SHA-256:
`f7663f4c01fc8df1ee2804c78d422528a6a7246d4878256b8c58e29b0546a36b`.
The build handle is terminal; it must not be restarted.

The builder used the canonical singleton slot, clean exact remote checkouts,
the committed public source ledger and the digest-pinned Go 1.27.3 toolchain.
No worktree overlay, scoring, concurrent test workload, main service restart
or main database mutation is authorized by this invocation. Observe the same
process/session for any live qualification; an observation timeout is not a
reason to restart it.

## Independent qualification handle

Session `80907`, manager PID `519199`, evidence root
`temp/canceled-stop-image-qualification-20261025.7NyTFOdF/`.
Basic readback passed at 13:01:42 UTC: non-root UID 65532, network none,
read-only root, capability drop and no-new-privileges verified actual Go 1.27.3,
clean `c1f9dda60`, binary/config/lock hashes, exactly ten source repositories
and absence of main/all configuration or vault trees. Original-only resources
were restored. This is not live host attestation or registry publication.

The short synthetic lifecycle passed in 391 seconds, exit 0: offline candidate
build, real 32-provider/8-client simulation, trusted accounting/resource report,
baseline/candidate scoring and the networkless pristine scorer were verified.
Original resources and the exact image/tag were checked after cleanup. Full
smoke log SHA-256:
`d7fe31745dc422ee8e02375616e57c8fcbbea8102c8ba39a1837a207f1dd37a5`.

Malicious-build isolation passed in 251 seconds, exit 0: the compile-only
candidate's package initialization could not mutate the simulator or protected
source. Log SHA-256:
`9a7d40389dc578740386b19aef528997f91e2abcb9ffbbd8006b9e3f902b1ebb`.

The simultaneous full host-boundary bomb test then passed in 64 seconds,
exit 0: ten evaluation CPUs saturated, memory constrained to 72 GiB, OOM exit
137 and cleanup in **958 ms**, below the 10-second test bound. No residual
owned container/network remained. Log SHA-256:
`610ccfc19a69eee8c662abb6ef509e42b7da1ce74c05bced10348309f74d7fee`.
The bomb fixture qualifies host cleanup, not image-specific host attestation.
Source remained clean, the exact base tag/image was unchanged, original three
containers/four networks remained, and the singleton slot was free at terminal.
Session `80907` is terminal, exit 0; do not restart it.

Full-window calibration remains unauthorized until a new prospective protocol
and its actual ordered requests are published.

## Qualification boundary and remaining work

1. Completed: bound the actual local image ID, simulator/scorer digest, embedded
   config and raw source-lock hashes; verified actual compiler, clean revision
   and complete secret-free ten-repository image contents. A local image ID is not an OCI
   registry-manifest digest or a publication record.
2. Completed: qualified this exact image's offline candidate build, short full
   lifecycle, inspected mounts/internal network/accounting and adversarial
   cleanup. This is not proof of every possible host-network egress path;
   default-deny negative probes remain part of production containment signoff.
3. Remaining: prospectively publish fresh measurement requests and protocol before any
   full-window scoring. Preserve the previous stopped Stage A; no original
   control or candidate measurement is reused as this release's evidence.
4. Remaining: complete exact-image host rebaseline, real review/test-provider proof, live
   MinIO retention/readback, API/worker acknowledgment quorum and monitoring.
   Main remains under the user-confirmed maintenance hold.
