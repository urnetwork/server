# Corrected Go 1.27.3 evaluator — October 25, 2026

Status: **built; local qualification phases passed; not activated or fully
production-qualified**. Independent exact-image checks, Docker lifecycle smoke,
malicious build isolation and host resource cleanup passed. Fresh frontier work
is underway. No official epoch baseline, final scale or calibrated margin is
claimed.

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

## Independent basic and full-smoke evidence

Evidence root: `temp/ready-snapshot-independent-20261025.KlIwmzeC`.
Non-root, read-only, capability-dropped, networkless basic checks verified the
exact image, compiler, clean revision, simulator/config/lock hashes, complete
ten-repository tree and absence of main/all configuration or vault directories.

| Basic evidence | SHA-256 |
| --- | --- |
| `identity.json` | `2d82fffb2ea5f5c4b95b3b97c406017fef2e268f3195ec60bcf6cfe98ee0d4d4` |
| `filesystem-buildinfo.txt` | `53c5d9115c3f060d78f0703a37d2caaa7e157fcaa07cd718e490f68c77d5fe8d` |
| `go-version.txt` | `5707bf75dd54b4fdd5ac209444d48c9c4179ebf8510f80e2f7293dfbedc1012b` |

The full synthetic-local Docker smoke passed in **390 seconds**, including
offline comment-only candidate build, fresh PostgreSQL/Redis and migrations,
real 32-provider / 8-client simulation, accounting, authenticated resource
report, baseline/scoring entrypoints, inspected containment and cleanup.
The base tag remained bound to the exact image; only original persistent
services and networks remained. `smoke.log` SHA-256:
`46420c3763b5a7b7ae6b89e35a44bf342fa8df903d4cfbd2ef65eb1ca11f4fcb`.
The full smoke artifact tree is `urnetwork-container-smoke.5acZdRWN` in that root.

This short smoke uses a 2 GiB runner and calibration fixture. It is not the
production scale, a nine-run epoch control, a statistical-significance estimate
or image-bound host qualification.

## Isolation and adversarial cleanup

Malicious build isolation passed in **250 seconds** on the same exact base.
The protected simulator tree and six protected paths remained intact; tests ran
unprivileged and their writable check/build stages were discarded. The base tag
was unchanged. `isolation.log` in the same evidence root has SHA-256
`2f687581b4bfbc7669e3657061a753cde6dc8fdcd4b672bbe0ea0765cc9f5a58`.

The separate host-boundary resource test passed in **53 seconds**: CPU pressure
exercised all ten evaluation CPUs; the 72 GiB memory bomb was OOM-killed with
exit 137; management-core cleanup took **614 ms** against a 10-second ceiling.
No test containers or networks remained. This is a host resource-boundary proof,
not an image-bound host attestation. Boot ID:
`f0e880cb-c124-4ed6-9edb-43669191d84e`. `bomb.json` SHA-256:
`f1330f3c47856135823221fa153b6253d432d46be368baac9ea3a008bc781bfe`.
It used the proven immutable C5 fixture with its explicit matching module/workspace
and actual Go 1.27.3; no scoring overlapped any qualification phase.

The first fresh 15,000-provider impairment-on public-seed capacity point started
after all phases and cleanup completed. Its separate root is
`temp/ready-snapshot-frontier-20261025.oPXXTOEe`. It uses fresh services, all four
fleet shards, 200 clients and the exact new image to generate its workload.
The earlier failed source's rows are not reused. The point is exploratory and
has no accepted result until terminal completion and artifact checks.

Read-only maintenance checks at October 25, 00:35 UTC still returned HTTP 503
from the public `/competition/info` endpoint and connection refusal from
`172.28.208.177:23900/minio/health/live`. The dedicated
`vault/main/competition-review-openai.key` file was absent. No API/database
mutation, runtime activation or service restart was attempted. Maintenance
completion and the dedicated credential are prerequisites for the remaining
live-readiness and genuine provider-backed qualification work; local capacity
measurement continues independently.
