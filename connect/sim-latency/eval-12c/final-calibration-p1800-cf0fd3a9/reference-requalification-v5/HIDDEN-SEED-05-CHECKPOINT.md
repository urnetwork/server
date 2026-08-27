# Hidden reference seed 05 checkpoint

Captured at 2026-08-27T23:42:11Z from frozen evaluator source commit
`5ca3d5242f4a7d40efe4415635608023b05a0956` and evaluator image
`sha256:cf0fd3a9e73385729ee8dcd8da7ea53eb59d5f372b9ff36789ec923056222038`.

The fifth precommitted hidden seed passed the required strict shared-baseline
ordering in exactly one attempt per reference:

| Reference | Candidate score (ms) | Ratio to 37,330.061 ms shared baseline | Placeable |
| --- | ---: | ---: | --- |
| better | 31,380.156 | 0.840614 | yes |
| no-op | 33,442.431 | 0.895858 | no |
| worse | 45,875.515 | 1.228399 | no |

Thus `better < no-op < worse`, bringing the terminal campaign to four ordering
passes from five completed seeds and satisfying the unchanged four-of-five
launch gate. Placeability remains diagnostic-only: better passed all six gates
and was takeover-eligible, no-op failed G2, and worse failed G2 and G4. No
result was censored or retried.

The three seed-05 manifests contain 164 authenticated entries totaling
38,510,925 bytes. Every entry was replayed against its declared path, byte
count, and SHA-256 digest. Every worker result has the exact 15 true security
booleans and three non-empty isolation identifiers required by the evaluator
schema, with zero residual competition containers or networks.

## Immutable bindings

- campaign commitment: `c79798ad1769fda861fd86da0cdec7bd2f12f11f5964e2582cfe84a69c9afd69`
- seed-05 result: `d1f9d57766edda226a3ed8e4c71746b256ca580c77200d5135809b3448d473d9`
- terminal progress: `f2bbe8797dc463bb85d576cea29575579757df3eb7109a2170fd0566008d1e8b`
- terminal decision: `3e4cc70d783b01a87328736caf82f49016138c97ff384b26dc38864f8cede835`

No private seed, generated provider configuration, seed-reveal document, or raw
hidden seed material is included in this checkpoint.
