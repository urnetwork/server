# Audit hardening checkpoint

Sealed during the v5 hidden screen on 27 August 2026. This checkpoint changes
only post-measurement verification and already-known handoff lineage; it does
not change the simulator, scorer, scale, source lock, or active measurement.

- The terminal audit now authenticates every file, byte count, and SHA-256
  entry inside all 15 evaluation evidence manifests.
- It rejects empty manifests, duplicate paths, traversal, hash/size mismatch,
  direct symlinks, and symlinked parent directories.
- Both auditor and renderer require the exact frozen set of 15 true security
  booleans plus three non-empty identity fields.
- Deterministic audit and renderer self-tests pass; the live audit remains
  6 passed, 4 pending, and 0 failed.
- Auditor SHA-256:
  `627d0336b6cea50066e8f9aa215e5d747d4d383ce562b23ecbb9876d2c495bb1`.
- Renderer SHA-256:
  `54a9993c17d2e46642b269d0969f061191c517ca9d998ee3592f10a4e56a4d3d`.

The production-staging and handoff scripts also bind the known v5 hidden
service, protocol, and pilot-qualification identities. Terminal-result hashes
remain deliberately fail-closed until the hidden campaign completes.
