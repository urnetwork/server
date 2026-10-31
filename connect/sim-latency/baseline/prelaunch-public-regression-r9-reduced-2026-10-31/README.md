# Accepted reduced-reference public R9/R9 pilot

This is public calibration evidence, **not the production epoch baseline or
complete launch acceptance**. The public workload seed is `20261030`; no fresh
hidden seed has been drawn at this checkpoint.

The one-use execution completed nine control and nine candidate runs, from
2026-10-30 21:29:26 UTC through 2026-10-31 00:21:55 UTC. Native verification
accepted the original artifacts at **2026-10-31 00:24:59.093728079 UTC**:

- all six evaluation gates passed;
- the approved 100 ns/byte regression was slower in both mean and median;
- both frozen-image replays completed with verified container cleanup; and
- original host container/network identities were restored and the evidence
  mount removed before verification.

The source remains `a9e8e1a710b24c749e6499a0606938252c83e136`, with the frozen
ten-repository graph and Go 1.27.3 evaluator image
`sha256:98c9061c4a43a25e7b581a5193ca6e7f48e49e7b7fc03b7c1b6190a029befee2`.
R9, alpha 0.05, the prospectively frozen 26.9% practical margin, all gates and
the three-hour per-job limit are unchanged. The earlier stronger reference's
complete failed pilot remains failed and preserved in the neighboring dataset.

Exact accepted artifacts:

| Artifact | SHA-256 |
|---|---|
| [Native proof](pilot-proof.json) | `983a4b554a85d04fcfbcded69b3c15a7ab8cdae8cbadd23544363f7c1d6e0436` |
| [Measured direction](pilot-direction.json) | `7dda458aa8ed8558529f87dbd3d36b7afa762dadb65e846e2db706999ba13b86` |
| [Nine-run control manifest](baseline.json) | `ee85182d88d4622b12fd9178065a06205fb8f64c5f1facd697e616ecb5369795` |
| [Candidate score and variance](score.json) | `c9a63fd3e964f22d86b1c8b777f83eea18331d8f78509508929e6c1feab9dae3` |

The original public plan is hash
`a22861dfd3d4e6bfd3e9a994b447fc535847cf7f3e4b92871dbce4ae97e79dd0`,
published before execution. The native operator is hash
`b1c3e311f76cafdc96327214065f431bda145005d8900563f9c32089e3c58a6c`.
The proof binds the complete raw artifact member census and both replay
observations at their original protected paths; these copies match it byte for
byte. No production job's private score or workload is published here.

The exact proof and direction were committed, pulled and pushed at server
`a1f50084a7dabc7204d87c926fd62b2f348b288e`; their protected publication
receipts were independently rehashed. The native after-public-pilot census
then completed with 13 sources and a second accepted two-replay check at
2026-10-31 00:32:58.433657992 UTC. The private exclusion catalog's digest is
`d5bdf06218e689141d3a0beb45a3fef9f30de018fd52fde7e4cf482ed2e7e899`;
its private seed/exclusion contents are deliberately not published.

Next: prepare the approved fresh hidden-seed screen. That screen, independent
review and new COMPLIANCE custody
remain required; this pilot alone does not release either production hold.
