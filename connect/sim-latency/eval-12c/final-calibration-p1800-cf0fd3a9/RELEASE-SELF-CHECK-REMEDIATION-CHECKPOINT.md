# Production release self-check remediation checkpoint

Recorded at 2026-08-28T00:55:40Z.

This checkpoint supersedes the release identity in `RELEASE-AND-SERVICE-CHECKPOINT.md`; that earlier checkpoint and its release artifacts remain retained under `5070445d`-qualified names.

## Defect and correction

Production staging attempt 4 stopped before round generation because the strict worker decoder did not declare the `irq_affinity_sha256` and `irq_policy_sha256` fields emitted by the pinned host self-check. The failed worker bootstrap log has SHA-256 `295c224d3f5fa4804a79ae0ae4d2a09281c874cba99f5e7fefe3bacc26f15a65`.

Control-plane commit `2ee4883f2b77cccfcbc69b3bcf1cb4ee613dad36` adds both fields, requires valid lowercase SHA-256 values for host eligibility, and keeps `DisallowUnknownFields`. Deterministic tests cover the exact emitted payload, missing/malformed hashes, and continued rejection of unrelated fields. The commit is pushed and remotely verified on `origin/finalization-control-plane`.

## Replacement release

- Source release: `90458a61e19259bba1bf1626b63567e92a06082d3944a070a8ea071b5f8bd5e7`
- Release amendment: `99d6010edcbc659d936e97cbc7cde48129d0af9146c6404a1bc03604d750ef5d`
- Release manifest: `17d2817a69f3bc506c98ba00f31b8cc15fc9e2b7e0a4e18b5ca0df9fc89bfc00`
- Release readiness check: `3fa3ca749a4718f31ed9ac17351c2b2695891f89117e794b7d3220e477d3b5cd`
- Service-backed check: `e3c168731edab3f0de0a823aaed23aa64e72b1ed7ea32e0dc5663806af7c4a08`
- API image: `sha256:923889fd2fb1ed398199bac89782df16e9acb4b5541d76c2394edcd513ca86e5`
- Worker image: `sha256:9942f9bf28cc0f7b690e717621e254bfaf19f7693fd9d8b4842475086454f437`

The release gate passed all 15 assertions. The service-backed gate passed all 11 FIFO, cache ACL, singleton-slot, lease-failover, retry, immutability, and cleanup assertions. Its log-streaming wrapper required the bounded KILL fallback; the gate then independently verified that the PostgreSQL/Redis containers, network, loopback alias, and managed hosts block were absent before publishing its pass record.

## Deployment and staging preflight

The authenticated `5070445d` API deployment and trusted-command tree were moved to root-owned commit-qualified archive paths. A fresh deployment for `2ee4883f` was installed with new credentials, authenticated twice for idempotence, and continues to expose only `config/local`, `vault/local`, and the two allowlisted host runtime databases to the control plane. Submitted evaluation containers still receive only the direct read-only local leaves.

Frozen measurement identity, evaluator image, simulator, scorer, p1800 scale, baseline, threshold, hidden seeds, and reference decision are unchanged. The amended staging driver SHA-256 is `da4a2270a6f513a08c845e578fb6b853e5282339d7fca5681f6b7f5d2665ca62`; its static self-test and root preflight both pass. The next phase is the production staging round.
