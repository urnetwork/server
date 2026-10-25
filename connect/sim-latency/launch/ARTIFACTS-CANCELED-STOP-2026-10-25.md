# Canceled-stop evaluator: build in progress

Status observed **2026-10-25T12:44:09Z**: source qualification passed; the
immutable image build is live. This is not image acceptance, calibration,
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

## Live build handle

- Root: `temp/canceled-stop-evaluator-build-20261025.xPrOmOTq/`.
- Exec session: `11747`; manager PID: `456756`.
- Started: 2026-10-25T12:43:38Z; watchdog: 3,600 seconds plus 60-second kill grace.
- Target local tag: `urnetwork/sim-latency-evaluator-base:canceled-stop-c1f9dda6-go1273`.
- Source builder: clean main `9a3b7970ad1ad2b2faa6f06908a95ff0e8255a98`;
  builder script SHA-256
  `2f07c7c8227173559ee4b5f5c122206ca6bf5d2909bc4344c99f5842e57178d4`;
  Dockerfile SHA-256
  `bf964f84926d1bf741bf5c7465cb166e062a1a91f4811fc5cbc2170e8dead3c2`.

The builder uses the canonical singleton slot, clean exact remote checkouts,
the committed public source ledger and the digest-pinned Go 1.27.3 toolchain.
No worktree overlay, scoring, concurrent test workload, main service restart
or main database mutation is authorized by this invocation. Observe the same
process/session; an observation timeout is not a reason to restart it.

## Still required after terminal build

1. Bind the actual local image ID, simulator/scorer digest, embedded config and
   raw source-lock hashes; verify actual compiler, clean revision and complete
   secret-free ten-repository image contents. A local image ID is not an OCI
   registry-manifest digest or a publication record.
2. Qualify this exact image's offline candidate build, short full lifecycle,
   mount/network/accounting containment and adversarial cleanup. Retain failed
   attempts separately.
3. Prospectively publish fresh measurement requests and protocol before any
   full-window scoring. Preserve the previous stopped Stage A; no original
   control or candidate measurement is reused as this release's evidence.
4. Complete exact-image host rebaseline, real review/test-provider proof, live
   MinIO retention/readback, API/worker acknowledgment quorum and monitoring.
   Main remains under the user-confirmed maintenance hold.
