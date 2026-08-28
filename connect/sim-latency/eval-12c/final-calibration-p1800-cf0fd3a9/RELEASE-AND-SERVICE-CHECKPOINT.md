# Release and service-backed gate checkpoint

Recorded: `2026-08-28T00:10:46Z`

Phase result: **PASS**

## Frozen identity

- Measurement commit: `5ca3d5242f4a7d40efe4415635608023b05a0956`
- Source lock: `0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838`
- Control-plane commit: `5070445ddb1764ad80f999102a9d71946e5a9e29`
- Evaluator image: `sha256:cf0fd3a9e73385729ee8dcd8da7ea53eb59d5f372b9ff36789ec923056222038`
- Boot ID: `34760d1b-a0b6-46a0-b8c1-264abd1affba`

## Release result

The release check passed all 15 assertions. Four CGO-disabled static Go
binaries contain `vcs.revision=5070445ddb1764ad80f999102a9d71946e5a9e29`
and `vcs.modified=false`. The standalone source checkout has a real `.git`
directory, no remotes, and the exact pushed control-plane commit.

- Release manifest:
  `e1405b3ca7c900e800c718917758a79d56c2392dc23c8ab1bd3bc17d8d5620db`
- Authenticated release check:
  `dd49f6b83b9761e52ca270e7d46706e38811ffa3b35997773b8fb5e36a12b7ca`
- Frozen release builder:
  `50d5c0a4c0b64c3b344270d74dfd724bb037982b92c0d53ac9cc6215aeee3887`
- OCI/Docker equivalence verifier:
  `b4a0316f591f1963110e5a328adee56a9a6d091a6c1deef8b0e6015d5f9cff2b`

| Component | Binary SHA-256 | Image ID | Attested platform manifest |
|---|---|---|---|
| API | `38366ec2312115193e3ab7afce7eedb327e2bcac8e5324f6f765c12fcaf4b4a6` | `sha256:da64be6fa93021d26dea5138b9fadc38e8b14762115f5298a1e6b58a218feb48` | `sha256:0853b4543129788b3db5bd1a53c9143f50ce0a3abe6ce7dfa45167bc3165bd9a` |
| Worker | `288ccf194fc626d20d2c5e982cca5de9d35d8ec36e4d416d843514b1947b3388` | `sha256:bacba185b8a345a01ed13c69a64b86f2821f8d453b70ada96e96bd231868ed50` | `sha256:42f5e79fac177363e99aa515c6f905bb5d4570a18ef9d8343945d9df626f2564` |
| Rebaseline | `8da786d3a058905f6eb40845dd30b950ea8ee568abb410f2c09275492c5d503a` | n/a | n/a |
| DB init | `85d3de5a23bb3cc027f28d8dc8d9783d8a93463b68c77314caaa04d95545ca8d` | n/a | n/a |

For each image, one no-cache BuildKit solve emitted an OCI index with SLSA v1
max provenance and an SPDX SBOM. A second solve reused that exact cached result
and emitted a loadable Docker archive with duplicate attestations disabled. The
verifier independently hashes every referenced image blob, rejects unsafe tar
paths and links, verifies both in-toto subjects, and requires the Docker archive
platform-manifest digest to equal the attested platform-manifest digest. The
final readiness sealer reruns this verification from the retained archives.

The large archives remain read-only in the authenticated evaluation evidence
root and are intentionally not duplicated in Git. Their hashes are bound by the
release manifest and release check.

## Service-backed result

The PostgreSQL/Redis gate passed all 11 assertions: dedicated service
addresses, origin migrations before local migrations, FIFO order, cache ACL,
single-slot ownership, lease failover, infrastructure retry, immutable terminal
fields, append-only events, and a zero test exit.

- Authenticated service check:
  `79b076c615357fe5eb2e105e00b4fd2703094860cf96153f0d65c5bc6f03d71c`
- Migration-order test source:
  `ca122814769ee0083fac951b1a931fa4bb942b3cb244aa7ddfd3e949a1ec2189`
- Store-integration test source:
  `5f60a86b0b77e7aac62f2ada1931d1762c22135afe7f6b4bbb7d989493c2fc3d`

The stack required TERM-to-KILL escalation during teardown; that cleanup path
worked. No BuildKit worker, competition-job container, PostgreSQL container,
Redis container, competition network, service alias, or service host entry
survived the phase.

## Rejected attempts retained

No rejected attempt was promoted:

1. `.release-build.yC5mhIRT` exposed a Go 1.26 VCS-stamping boundary: a linked
   worktree's `.git` file was not recognized as a repository root, so the API
   binary lacked `vcs.revision`. The fixed build uses an offline standalone
   clone with a real `.git` directory and no remotes.
2. `.release-build.is92sgAZ` proved that BuildKit's Docker exporter rejects the
   manifest list created by attached provenance/SBOM attestations. The fixed
   design retains an attested OCI archive and verifies a digest-equivalent
   loadable Docker archive.
3. `.release-build.cpLDzVkQ` built and verified both images, then stopped before
   promotion because BuildKit's root-owned exports could not be made read-only
   by the non-root release process. The fixed script transfers ownership of
   only the exact archive/metadata outputs before sealing them.

## Phase boundary

The shareable four-section `final-preview.html` now includes the passed release
and service gates. The local completion audit remains 7 pass / 3 pending / 0
fail because the fixed 6 h 03 min production staging round, final calibration
document, and final four-section report have not yet been sealed. Staging is the
next phase and must be independently committed and pushed when it passes.
