# First-committed operator qualification

The named one-seed operator passed independent offline qualification. This is
not a measurement result, native execution authorization or production readiness.

Exactly 30 guards ran three consecutive times: 10 new first-position guards
and 20 focused legacy guards per pass. Durations were 1,230, 1,228 and 1,229
seconds, with 90 passes, zero skips and zero failures. The compiled old-dispatch
control built successfully and failed the exact expected assertion in 23.79
seconds. The original 134-guard three-pass qualification is reused explicitly;
it was **not** rerun, and this is not a fresh 144-guard full-suite claim.

- Actual executed binary SHA-256:
  `57ad96c70ef88a2f43248727a0f4b8632f50fa9e7b4409b64b8ba7e90bb4cc6a`.
- Binary size: 179,341,472 bytes; Go 1.27.3, race enabled.
- Original source base: `29b44380e2e521c2895429737b38c9e350842cd8`,
  its qualified nine fragments, and the four pinned overlays below. No moving
  main checkout was substituted.
- Qualification receipt:
  `/home/by/urnetwork/temp/first-committed-operator-qa-20261027.a21tBAze/offline-unit-qualification.json`,
  SHA-256 `83ce50e814b9388ce2fe20848a1ce0dd95eb0ef69fbbfe5fc35b7ef689f4752d`.
- Summary: `ebea897b06458e71aa5670c7415147fc52cb4921b0b28f93fdbb847a734acea5`.
- Source-pin array (47 entries):
  `14b39dfdd0d5b9b0a5fb10683bb25ae003497d3a234c3f6ad766b0a6ffa51ab7`.
- Evidence-pin array (three actual pass logs):
  `af3d4e0112e8d8673ece33e6209f5e659f76795cd75140db34a142c315b403d5`.

| Exact source overlay | SHA-256 |
| --- | --- |
| `held-out-execution.go.txt` | `dd432dcca0f83101968b58178068252939500274dfb67707df15af5d77ef8469` |
| `held-out-first-committed.go.txt` | `cb97bdf17b2e411b1063516757ac6851954edde3b2bb8ef83b826f3fcbc89c00` |
| `held-out-first-committed-tests.go.txt` | `1b493b58a55f2e93d479cfbd486c87c605e0e0a9bda5ddea1b6b6f2c981bee19` |
| `held-out-first-committed-dispatch-tests.go.txt` | `f0c4663fe93a267375a43a8ae0623e6428b0e6856b93dc14cddd2aaeaac7f840` |

Only the executor's closed terminal-dispatch branch changes existing code.
Every old test name remains present; the new named protocol does not weaken old
cardinalities. Source and binary pins passed before/after checks. Compilation
and test binaries used an approved root-private executable RAM tmpfs, bounded
to 3 GiB and CPUs 12,14. Logs, source pins and receipts are retained on disk.
An initial disk-floor stop occurred before the new compile; its old RED result
and original logs were retained. The new-only resume did not rerun that control.

Independent Sol 6.1 max read-only code review found no blocking defect in the
four-fragment scope or public declaration lineage. It confirmed exact first
position derivation, authentication of all six original requests, separate new
executable qualification, publication-before-execution enforcement, unchanged
gates/deadlines and the original shared execution-start fence. It did not run
tests or inspect private seeds. Root separately counted the actual 90 PASS / 0
SKIP / 0 FAIL entries and verified the executed binary hash.

The historical `analyzer_source` label `ebf0b61c…` remains release identity, not
the actual compiler base above. Native metadata preparation, public selected
plan publication, native verification and all three first-position measurements
remain separate steps. No real seed draw, provider call or campaign execution
occurred in this qualification. The second committed seed must stay unstarted;
the old two-seed executor remains prohibited before the shared start fence.
