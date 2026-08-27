# Hidden reference seed 01 checkpoint

Captured at 2026-08-27T19:52:00Z from the frozen evaluator source commit
`5ca3d5242f4a7d40efe4415635608023b05a0956` and evaluator image
`sha256:cf0fd3a9e73385729ee8dcd8da7ea53eb59d5f372b9ff36789ec923056222038`.

The first precommitted hidden seed passed the required strict shared-baseline
ordering in exactly one attempt per reference:

| Reference | Candidate score (ms) | Ratio to 45,742.763 ms shared baseline | Placeable |
| --- | ---: | ---: | --- |
| better | 41,601.916 | 0.909475 | yes |
| no-op | 41,973.144 | 0.917591 | yes |
| worse | 70,826.486 | 1.548365 | no |

Thus `better < no-op < worse`, giving one ordering pass from one completed
seed against the launch requirement of at least four passes from five seeds.
The worse control's expected G1, G2, and G4 failures remain valid statistical
evidence and do not invalidate the ordering.

The three evidence manifests contain 166 authenticated entries totaling
39,888,847 bytes. Every worker result has the exact 15 true security booleans
and three non-empty isolation identifiers required by the evaluator schema.
The partial finalization audit reports six required passes, four pending checks,
and zero failures. At capture time, seed 2 was active and the campaign service
had zero restarts.

## Immutable bindings

- campaign commitment: `c79798ad1769fda861fd86da0cdec7bd2f12f11f5964e2582cfe84a69c9afd69`
- seed-01 result: `c87690bcd312f9eeb816917f3bca82d868ae1f465cbc9e5494165efde3b6f01d`
- 1/5 progress snapshot: `66e190a0af3ff5a7bf07c8be2c2c8363beea44c4838ce36b1fa554879e3c1adf`
- refreshed preview: `e72543d42c36c07e7a694aca71340b96a842cced1c4081f69775045d90013133`
- preview evidence: `de9ce97adcb934fc25c99766e3d8a7a228bf1d1e3bb04fd197362e3f9c799381`

No private seed, generated provider configuration, seed-reveal document, or raw
hidden seed material is included in this checkpoint.
