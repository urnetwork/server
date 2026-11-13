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

## Installed tool and live remote proof

The corrected source tool was built from clean main
`9a3b7970ad1ad2b2faa6f06908a95ff0e8255a98`, Go 1.27.3, Linux/amd64, CGO
disabled, greenteagc and trimpath, with `vcs.modified=false`. Installed directory:
`/usr/local/libexec/urnetwork/competition-source-preflight-9a3b7970a-e27b5964`.
Its directory and `sim-latency` executable are root:root 0555. Binary SHA-256:
`e27b5964d25a166d2487e7ab35a62634b1a693f2fe381e9c0dee4c04dedd0a5c`.
This additive tool does not replace an active worker, supervisor or old bundle.

Independent readback and a new actual `source-record` invocation passed in
9 seconds. The real parser and remote graph returned epoch 0, production mode,
branch `sim-latency` and all ten exact commits. The canonical digest is
`37555f58bbf1235f10600f8c1de7dafeef356985f719f5e029fdb8c2e69c9b4a`.
No operator checkout changed and no new temporary clone/process remained.
`source-record-corrected.json` in the evidence root above has SHA-256
`7cc7c8586f8c50dc9ee54701a8b388382258ba861bc47b95120417e1346fab27`.
The earlier C5 timeout remains a separate failed attempt, not an overwritten log.

No provider, main API, database, MinIO or scoring operation was used in the
focused tests or live metadata check. Runtime qualification remains separate.
