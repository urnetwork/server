# Corrected Go 1.27.3 evaluator — October 25, 2026

Status: **built, not activated or fully qualified**. Independent exact-image
checks, Docker lifecycle smoke, malicious build isolation and host resource
cleanup are in progress. No official epoch baseline, final scale or calibrated
margin is claimed.

| Identity | Value |
| --- | --- |
| Local tag | `urnetwork/sim-latency-evaluator-base:ready-snapshot-0d58d0d9` |
| Docker image ID | `sha256:effe9ba5dc9cc337b4f250977c5f921ea42e4a336b30e3a5b54a9fd1bbe7f793` |
| Image bytes | `7513407652` |
| Frozen server | `0d58d0d91b069c62bcfb39cd72166c26925b786a` |
| Canonical ten-repository lock | `37555f58bbf1235f10600f8c1de7dafeef356985f719f5e029fdb8c2e69c9b4a` |
| Embedded lock-file SHA-256 | `2cb2ff2a9b1f64a61e165a194e72ad9ee07aae0d76cf43dddf47a3e3dce952ed` |
| Embedded source-config SHA-256 | `156aee96af328e2b4d4d838f2de2eecb0204650b8400504a0eca773b9838de2f` |
| Simulator/scorer SHA-256 | `4993081b4076693f231ca2e5d3ee4468646c54d2d6cbc89a01fe325fccf57dbc` |

The image contains the committed epoch-0 ledger from config main
`949e08c8c5412206e06709e36f8bfbe1e182aef3`. The other nine repository pins are
unchanged. The canonical model digest, raw embedded JSON hash and Docker image
ID are distinct identities; none is a published registry-manifest digest.

The build passed its actual Go **1.27.3** compiler assertion, offline vet,
database-recovery and error-classification gates, embedded source check and
final clean-source check. Build info reports Linux/amd64, CGO disabled,
greenteagc, the exact frozen server revision and `vcs.modified=false`.
The container identity independently emitted by the build and root readback
matches the simulator/config/lock-file hashes above. Independent qualification
results remain separate until they are terminal and retained.

## Provenance and limits

Retained build root, relative to the mono root:
`temp/ready-snapshot-evaluator-build-20261024.MCorfDyJ`.

- `image-record.json`: `cda30e8fcda9486b3cb435d535cb7675eb57d354285aefaa3acb37f05aadba0a`;
- `build.log`: `b5444ebe432b4410768dc5a53a1f7b44367bb47f1cf83cb904a957b8c87dd7bb`.

Trusted builder scripts came from clean main `9a3b7970a`, in the private
`temp/source-preflight-go1273-20261024.VSlJ4TE8/server` checkout. Its nine sibling
Git directories were **origin-discovery descriptors only**, not evaluated code.
The builder fetched and checked out all ten exact published revisions into a
fresh build context; those real clean checkouts are embedded in the image.
It did not overlay operator worktrees, copy config/main or mount vault/main.
The committed public source ledger is the only config file baked into this base.

The corrected installed source tool and its real remote proof are recorded in
[SOURCE-PREFLIGHT-2026-10-24.md](SOURCE-PREFLIGHT-2026-10-24.md).
The snapshot-isolation/startup regressions and failed earlier frontier are in
[STARTUP-AND-SNAPSHOT-2026-10-24.md](STARTUP-AND-SNAPSHOT-2026-10-24.md).

Main control programs remain independently maintained; this image does not pin
their commits season-wide. No active API policy, worker, supervisor, host
attestation, database row or staging round was changed. Image-bound containment,
host rebaseline, genuine provider-backed review/build qualification, fresh
frontier and reference calibration, hidden baseline, immutable MinIO retention
and live production readiness remain incomplete.
