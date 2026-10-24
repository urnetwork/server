# Bounded source preflight with branch and ancestry proof

This is a **main host-control correction**, not another evaluated-source change.
The tested ten-repository evaluation checkpoint remains server
`0d58d0d91b069c62bcfb39cd72166c26925b786a`. Its three full-suite passes remain
separate from the focused checks below. Nothing activates a round or approves
an evaluator image.

## Observed failures

The installed C5 `source-record` prepublication check exceeded its 300-second
operator bound while downloading gVisor's full branch history. It produced no
JSON, so remote graph validation was not declared successful. Its retained
stderr SHA-256 is
`8295882cbd93d1be764d045efce67c79b08370b03bb2f79533bbf62feddfc313`.

Adjacent deterministic Git fixtures proved two trust failures: an unrelated
commit object could satisfy the old presence-only check, and a tag named
`sim-latency` could substitute for the required branch in both verification
modes. Neither case is evidence that a live source repository was tampered with.

## Correction and verification

Initial metadata clones request one tip commit and no file blobs. Exact-tip
checks need no history; historical-checkpoint checks deepen the selected branch
only when necessary and require `merge-base --is-ancestor`. The cloned bare
`HEAD` must symbolically name the exact required branch, rejecting a tag alias.
Operator working branches/files are not checked out or modified.

The performance regression executes the real helper against local Git fixtures
with a shim that deterministically rejects an unbounded initial-history request.
The ancestry, real shallow-history deepening and tag regressions use actual Git.
Both original trust failures and the unbounded transfer failed before their
fixes. All four new tests plus adjacent source/launch guards then passed three
race repetitions: 21 tests, 63/63 executions.

Evidence root relative to the mono root:
`temp/provide-final-full-validation-20261024.sqVPtUoq`:

| Log | SHA-256 |
| --- | --- |
| `remote-preflight-prefix.log` | `0e20dd95114c95ed26c8730940fb6d394b2ef21b1bc58cee42ba8b3f91b75a95` |
| `remote-tag-prefix.log` | `842c00e871d77df62e851e587e7a0d9d44357b7a28c1a95deeff1d46de5f2ddd` |
| `remote-preflight-final.log` | `6fb751c5a3fb06a274e498b6888dfa236b310b88d8f5798c026875db6363920d` |

An updated clean Go 1.27.3 source-tool build and live remote verification must
still pass before this helper is used to publish an evaluator image. No provider,
main API, database, MinIO or scoring operation was used in the focused tests.
