# Hidden reference seed 04 checkpoint

Captured at 2026-08-27T22:30:15Z from frozen evaluator source commit
`5ca3d5242f4a7d40efe4415635608023b05a0956` and evaluator image
`sha256:cf0fd3a9e73385729ee8dcd8da7ea53eb59d5f372b9ff36789ec923056222038`.

The fourth precommitted hidden seed passed the required strict shared-baseline
ordering in exactly one attempt per reference:

| Reference | Candidate score (ms) | Ratio to 35,386.419 ms shared baseline | Placeable |
| --- | ---: | ---: | --- |
| better | 34,471.129 | 0.974134 | no |
| no-op | 43,202.147 | 1.220868 | no |
| worse | 60,594.593 | 1.712369 | no |

Thus `better < no-op < worse`, bringing the campaign to three ordering passes
from four completed seeds. Placeability remains diagnostic-only for the sealed
reference protocol: better failed G2, no-op failed G4, and worse failed G1,
G2, and G4. Those results are retained and do not alter the strict ordering.
The final seed must pass to satisfy the unchanged four-of-five launch gate.

The three evidence manifests contain 172 authenticated entries totaling
39,797,073 bytes. Every entry was replayed against its declared path, byte
count, and SHA-256 digest. Every worker result has the exact 15 true security
booleans and three non-empty isolation identifiers required by the evaluator
schema. The partial finalization audit reports six required passes, four pending
checks, and zero failures. At capture time, seed 5 was active and the campaign
service had zero restarts.

## Immutable bindings

- campaign commitment: `c79798ad1769fda861fd86da0cdec7bd2f12f11f5964e2582cfe84a69c9afd69`
- seed-04 result: `e4602fa82217c5ea49a168ee59d386fe85df596ed914f65728969c851352c77e`
- 4/5 progress snapshot: `cf89fe0cb0e606f31fc818a6f86c4a3b23f4cf047ea839314db8d5b48098992a`
- refreshed preview: `21a10addb5e7672d6e4ddecf0f9219c81b56ae76b902c7f00010e47851e988b2`
- preview evidence: `13b8eb6cfe93d0daa4eab8b5fdcd3cb8cf3575e2caf26859b6d1c5111baa0925`

No private seed, generated provider configuration, seed-reveal document, or raw
hidden seed material is included in this checkpoint.
