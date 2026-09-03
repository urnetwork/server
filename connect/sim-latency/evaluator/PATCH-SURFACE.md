# sim-latency patch-surface review

Status: pre-freeze review, 2026-08-17. The final source-lock step must repeat
the blob and import checks against the pushed season base before publishing the
policy digest.

## Editable surface

The season-one development policy permits exactly one existing regular file:

```text
connect/resident_contract_manager.go
```

No wildcard or directory prefix is accepted. New files, deletions, renames,
copies, mode changes, build constraints, binary patches, symlinks, and
submodules are rejected structurally before a build begins.

This file owns the resident-side active-contract lookup and its short-lived
cache. It is on the measured Connect path and is the target exercised by the
no-op, worse, and better reference patches. Its current direct imports are the
standard-library `context`, `sync`, and `time` packages plus the trusted server
root and model packages. Its constructor is called from `connect/resident.go`.

## Protected boundary

The literal allowlist keeps all of these outside miner control:

- `connect/sim-latency`, scorer and workload code;
- `stats`, accounting, migrations, configuration, vault, and site resources;
- the competition API, queue, worker, evaluator, patch validator, and Docker
  definitions;
- module metadata, vendored dependencies, generated/build files, and CI;
- SDK simulation helpers and every sibling repository in the source lock.

Candidate code can still use packages already present in the frozen module, so
the file allowlist is not the containment boundary by itself. The offline
unprivileged build, exact read-only local mounts, default-deny runtime network,
resource limits, immutable scorer, and G1-G6 gates remain mandatory.

## Freeze checks

Before the reference seed is drawn, record and verify all of the following in
the release evidence:

1. the pushed base commit and Git blob identity of the editable file;
2. the exact `policy.example.json` bytes and SHA-256;
3. the editable file's imports and constructor call sites;
4. successful structural validation and offline vet/compile for all three
   reference patches;
5. a clean source lock across `server`, `connect`, `sdk`, `proxy`, `glog`,
   `goidenticons`, `userwireguard`, and `sn`.

`TestExamplePatchPolicyMatchesReviewedSurface` fails if the literal surface is
widened or if protected local/all configuration and vault paths become
editable.
