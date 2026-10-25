# Approved margin units: control-plane correction

## Root cause and scope

Source manifests express margins in percent; evaluator policy and active source
pointers express them as fractions. Exact equality after multiplying a fraction
by 100 can reject an identical approved decimal policy: `29%` and `0.29` are
the deterministic example, **not a selected competition margin**.

The initial probe fixture retained unrelated legacy history at 16.1% while
changing its fresh baseline to 29%. That invocation failed before the target
comparison and is retained as an invalid fixture, not defect evidence. The
corrected probe clears legacy history for a fresh v2 baseline and validates the
manifest before reaching bootstrap. It then fails specifically at
`baseline manifest differs from the approved epoch-one policy`, while the
genuinely different `0.29001` control passes its expected rejection.
Corrected pre-fix log SHA-256:
`2f446bf9518ddb24c0307db5fad03b97f0fc0187326092d2107fa870217594bf`.

## Correction and adjacent audit

`source_margin.go` compares supported finite values using exact conversion in
either direction or exact equality of their shortest decimal representations
after unit conversion. No epsilon, rounding of measured recommendations, changed
scoring threshold or relaxed source signature is introduced. Zero and underflow
are distinguished; mode-specific staging-zero rules remain separate.

The same helper is applied to bootstrap, activation-manifest validation,
supervisor carry-forward and reviewed-image incumbent checks. An equal measured
recommendation cannot be falsely rejected as a decreased floor solely because
binary conversion rounded up. A genuinely lower or different margin remains
ineligible. This is main control code only; frozen evaluated server `c1f9dda60`,
its ten-repository source lock, qualified image `002c3904…`, workload, scorer
math and existing calibration evidence are unchanged.

## Deterministic qualification

Eight new top-level tests cover 501 quantized unit pairs, arbitrary measured
recommendations in both directions, nearby/different values, non-finite and
out-of-range values, zero-underflow, composed bootstrap/activation/reviewed-image
boundaries and equal-floor selection. The unit-pair loop is not 501 independent
tests or performance observations.

Independent Sol 6.1 low execution used exact main `06ca551b1` plus only the seven
margin Go files, actual Go 1.27.3/local, management CPUs 20,22, bounded Go
concurrency, synthetic disposable services and a private synthetic global Git
identity. Evaluated source and image were not changed.

| Invocation | Result | Full log SHA-256 |
| --- | --- | --- |
| Fixed targeted race smoke | 60 top-level passes; exit 0; whole 96 s, package 5.795 s | `090aa48066dbec2e181102cacdf7db32bb25aa1e4c0064868ed67b790beaec2c` |
| Five original predicates restored | Four behavioral failures: bootstrap, activation, reviewed-image incumbent and equal-floor selection; negative controls pass; exit 1 | `d9572ba7b139ac79c9a6e9eae69547d6a1f4fff15e00d74e410b6141a656183e` |
| Fixed files restored and rechecked | 60 top-level passes; exit 0; whole 12 s, package 5.839 s | `75ded081c215936dfc783e0a15216df27773ef370ea96fcccec8b11d9cc376ff` |

All five old-predicate files matched private HEAD exactly. The new helper and
test declarations were retained during the old-body proof, so compilation
failures were not counted as behavioral evidence. Every terminal cleanup was
0, only the original three containers/four networks remained, and the canonical
singleton slot was free.

Evidence roots: `temp/control-margin-smoke-20261025.YR6FyVbt/` and
`temp/source-margin-prefix-corrected-20261025.FBetSctw/`. The original invalid
fixture remains separately under `temp/source-margin-prefix-20261025.6IBaUqAc/`
with an explicit outcome correction beside the untouched logs.

Commit/pull/push smoke-qualified code before larger validation. Three full main
control simulator suites and actual runtime/provider/readiness qualification
remain separate gates; this smoke is not production activation or calibration.
