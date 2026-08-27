# Hidden reference seed 02 checkpoint

Captured at 2026-08-27T20:44:36Z from frozen evaluator source commit
`5ca3d5242f4a7d40efe4415635608023b05a0956` and evaluator image
`sha256:cf0fd3a9e73385729ee8dcd8da7ea53eb59d5f372b9ff36789ec923056222038`.

The second precommitted hidden seed passed the required strict shared-baseline
ordering in exactly one attempt per reference:

| Reference | Candidate score (ms) | Ratio to 45,054.257 ms shared baseline | Placeable |
| --- | ---: | ---: | --- |
| better | 44,927.219 | 0.997180 | yes |
| no-op | 47,871.762 | 1.062536 | yes |
| worse | 56,026.608 | 1.243536 | no |

Thus `better < no-op < worse`, bringing the hidden campaign to two ordering
passes from two completed seeds against the launch requirement of at least four
passes from five seeds. The worse control's expected G2 and G4 failures remain
valid statistical evidence and do not invalidate the ordering.

The three evidence manifests contain 172 authenticated entries totaling
39,340,662 bytes. Every entry was replayed against its declared path, byte
count, and SHA-256 digest. Every worker result has the exact 15 true security
booleans and three non-empty isolation identifiers required by the evaluator
schema. The partial finalization audit reports six required passes, four pending
checks, and zero failures. At capture time, seed 3 was active and the campaign
service had zero restarts.

## Immutable bindings

- campaign commitment: `c79798ad1769fda861fd86da0cdec7bd2f12f11f5964e2582cfe84a69c9afd69`
- seed-02 result: `4f919811f5b9a65c9817549c1308b08c7ce1576e26c5ed066fd2d0872aaed780`
- 2/5 progress snapshot: `a69ea111c5e89fb32df16f8261225bab67e6eb3fe69a651576fcca1a73ace40a`
- refreshed preview: `edb98758232bdbc14e10e55393e06de4d9b1216c2145a05fd9032ceeaaf1e39b`
- preview evidence: `7d9eba71fa3c549973bdd2f10c368a791d145dfb1ad02bda758ff6e57fcfcbd7`

No private seed, generated provider configuration, seed-reveal document, or raw
hidden seed material is included in this checkpoint.
