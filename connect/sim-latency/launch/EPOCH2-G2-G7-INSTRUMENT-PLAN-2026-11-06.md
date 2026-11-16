# Prospective G2+G7 instrument plan

Status: PLAN ONLY, NOT AN APPROVED PATCH, PRODUCT SELECTION OR IMAGE. Exact patch
bytes, protected-path census, approval digest, child commit, source lock, image
and simulator/scorer hashes are deliberately not materialized by this plan.

The retained October31 G2 approval is historical scorer2 evidence: its patch
SHA-256 is `6423d41b5f252ef0304adf6c513653fb5a7e37a55733e1bf56b9d59d4cda7819`.
It does not authorize labeling a G7 instrument as that same patch/approval.

The immutable provenance base remains epoch-one server
`a9e8e1a710b24c749e6499a0606938252c83e136`, complete ten-repository canonical
lock `ec322ed2db023e15ea5dc3eea60a6ca5989661bba737c340fea3477d8d1300cf`,
and evaluator image
`sha256:98c9061c4a43a25e7b581a5193ca6e7f48e49e7b7fc03b7c1b6190a029befee2`.
Never replace that provenance with current main or an ordinary service lock.

The separately approved numerical intent is prospective only: G2 independent
request-count/received-byte median floors of 95%, with no ceilings; G7 independent
page-coverage and byte-coverage ratios of at least 95% of the same-round baseline.
80% baseline coverage therefore means 76%, not 75% or 79%. Preserve the exact
`minimum_page_coverage_ratio` and `minimum_byte_coverage_ratio` fields.
Intentional useful-traffic suppression is still prohibited regardless of floors.

## Narrow patch assembly, for separate review

1. Retain the old approved G2 overlay as an immutable reference, not an editable
   approval. Prepare a new candidate patch against its authenticated frozen
   preimages. Keep unchanged product dependencies and the selected-product map
   separate from the deterministic server-only instrument child.
2. Review only necessary simulator/instrument hunks: offered-work descriptor and
   traversal/collector in `client.go`/`site.go`, exact run/result/scorer/CLI
   propagation, `work_coverage{,_plan}.go`, `score_work_coverage.go`, and their
   actual tests. Retain the 22 G7 roots selected by the new correctness protocol,
   the previous 48 roots and required helpers.
3. Add only the standalone model policy/summary/score-contract leaves and their
   tests, plus exact policy/provenance fields required by the existing protected
   source-manifest/trusted-instrument codecs. If a large controller/model/CLI
   file needs a type or capability hunk, extract that narrow hunk; do not copy
   the whole moving file or cherry-pick all main.
4. Review protected runner/entrypoint dispatch and the separate
   `evaluator-work-coverage.sh`/`prepare-round-baseline-work-coverage.sh`
   producers. Preserve historical evaluator and minimum-work producer bytes;
   do not route epoch one through scorer3. Include managed sub-evaluation
   coverage propagation only where the actual selected future mode requires it.
5. Close every imported-symbol/helper dependency against the exact frozen
   graph. Explicitly exclude unrelated recovery/operator classes, live SQL,
   migration/monitor/SN changes, ordinary service source locks and product
   implementation changes. Missing context or preimage conflict stops review.

This is a candidate scope, not a claim that its transitive closure already
compiles. A new exact sorted literal `ProtectedPaths` census, patch SHA and
full-policy approval must be independently reviewed after assembly. Rehashing
or relabeling an old G2 approval is not sufficient.

## Independent build/measurement boundary

A private synthetic or explicitly authorized qualification-only Goofe child can
check frozen preimages, compilation, all 70 correctness roots, full scorer3
capability, unchanged historical behavior and fail-closed admission. Such a
technical child/image must retain nonproduction identity, no epoch/round/winner
tuple and no source-publication authority. It is not incumbent selection.

Actual product selection waits for final epoch-one scores/seal, authentic frozen
review and explicit manual handoff. Reapply the exact newly approved instrument
patch to that selected product; an accepted patch touching a protected preimage
requires manual resolution, not rebase or fallback. Then bind the real child,
image and build receipt. Preserve all ten repositories; no `operator-proxy`.

Opening intake does not need to wait for evaluator runtime/host-equivalence
measurements or the fresh R9 control once genuine source/API intake authority
exists. Grading does require those exact real receipts and the immutable new
same-round baseline. Do not measure/build on Sille while epoch-one work or
approved recoveries are runnable, and do not alter epoch-one instrument bytes,
R9 baseline, policies or original clocks.
