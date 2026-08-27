# Season base equivalence checkpoint

The published `apex-season-1` tag remains the public patch-authoring base at
`eb697281cbe0a19a27d7771fe69fb24c2c3dab8c`. The authoritative evaluator
remains frozen at `5ca3d5242f4a7d40efe4415635608023b05a0956`.

This split is authenticated and does not require moving the published tag:

- the frozen policy SHA-256 is
  `2dba553cd94d6d901e0fc590fd147d3e39273b41c24317e987b1bbf479382460`;
- it permits exactly `connect/resident_contract_manager.go`;
- that file has Git blob `66e2d39956b958749dfdfd00f408d4c05f874833`
  at both commits;
- local performance reproduction uses the digest-pinned evaluator image, not
  the older tag's protected harness and dependencies;
- no seed material is included.

Equivalence evidence SHA-256:
`6bce6a80cecfee0297bcc11afbaa390576d8f542980d8797e4da33046daa07b3`.
