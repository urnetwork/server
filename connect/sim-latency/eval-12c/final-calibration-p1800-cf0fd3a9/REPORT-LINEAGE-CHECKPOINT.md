# Final report lineage checkpoint

The final report and calibration-document renderer now authenticate and
disclose both source roles:

- public patch-authoring tag `apex-season-1` at `eb697281…`;
- authoritative evaluator commit `5ca3d524…` and its digest-pinned image;
- the identical allowed-file blob connecting the two identities;
- season-base equivalence evidence SHA-256
  `6bce6a80cecfee0297bcc11afbaa390576d8f542980d8797e4da33046daa07b3`.

The completion audit requires the same lineage in the rendered HTML, Markdown,
and report-evidence JSON. Renderer and auditor deterministic self-tests pass,
and the live audit remains 6 passed / 4 pending / 0 failed.

- Renderer SHA-256:
  `cbbcc0d82e7576ae4e0d264e34ecec3dc360964cafd8e1497103c36d6d5b8b2f`.
- Auditor SHA-256:
  `b3c0e5288b2d45e3db78fd70e3b29c4cec76f95f93e092cfc61e9c456f3e3b1e`.
